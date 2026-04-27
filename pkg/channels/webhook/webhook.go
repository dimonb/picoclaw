package webhook

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/identity"
	"github.com/sipeed/picoclaw/pkg/logger"
)

const (
	channelName    = "webhook"
	maxBodySize    = 1 << 20 // 1 MiB
	defaultTimeout = 10 * time.Minute
	defaultTTL     = 5 * time.Minute
	janitorPeriod  = 30 * time.Second

	statusProcessing = "processing"
	statusDone       = "done"
	statusExpired    = "expired"
)

var sessionNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

type ticket struct {
	id        string
	session   string
	status    string
	content   string
	createdAt time.Time
	doneAt    time.Time
}

type sessionQueue struct {
	mu    sync.Mutex
	queue []string // ticket IDs in FIFO order
}

// WebhookChannel implements a generic HTTP webhook provider.
// Inbound: POST /webhook/<session>      → returns ticket ID, publishes inbound to bus.
// Outbound: GET /webhook/<session>/<ticket> → returns ticket status / agent response.
type WebhookChannel struct {
	*channels.BaseChannel

	cfg     config.WebhookConfig
	timeout time.Duration
	ttl     time.Duration

	ctx    context.Context
	cancel context.CancelFunc

	ticketsMu sync.RWMutex
	tickets   map[string]*ticket // ticketID → ticket

	sessionsMu sync.Mutex
	sessions   map[string]*sessionQueue // session → FIFO queue of ticket IDs
}

// NewWebhookChannel constructs a Webhook channel from config.
func NewWebhookChannel(cfg config.WebhookConfig, messageBus *bus.MessageBus) (*WebhookChannel, error) {
	base := channels.NewBaseChannel(channelName, cfg, messageBus, cfg.AllowFrom,
		channels.WithReasoningChannelID(cfg.ReasoningChannelID),
	)

	timeout := time.Duration(cfg.RequestTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ttl := time.Duration(cfg.ResultTTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = defaultTTL
	}

	return &WebhookChannel{
		BaseChannel: base,
		cfg:         cfg,
		timeout:     timeout,
		ttl:         ttl,
		tickets:     make(map[string]*ticket),
		sessions:    make(map[string]*sessionQueue),
	}, nil
}

// Start activates the channel.
func (c *WebhookChannel) Start(ctx context.Context) error {
	logger.InfoC(channelName, "Starting Webhook channel")
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.SetRunning(true)
	go c.runJanitor(c.ctx)
	return nil
}

// Stop deactivates the channel.
func (c *WebhookChannel) Stop(ctx context.Context) error {
	logger.InfoC(channelName, "Stopping Webhook channel")
	if c.cancel != nil {
		c.cancel()
	}
	c.SetRunning(false)
	return nil
}

// WebhookPath registers the prefix on the shared HTTP server. Trailing slash
// makes it a subtree pattern in net/http, so it matches /webhook/<anything>
// while leaving more specific exact paths (e.g. /webhook/line) to other channels.
func (c *WebhookChannel) WebhookPath() string {
	return "/webhook/"
}

// ServeHTTP routes inbound POST and ticket polling GET requests.
func (c *WebhookChannel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !c.IsRunning() {
		http.Error(w, "channel not running", http.StatusServiceUnavailable)
		return
	}

	rest := strings.TrimPrefix(r.URL.Path, "/webhook/")
	parts := strings.Split(rest, "/")

	switch len(parts) {
	case 1:
		c.handleInbound(w, r, parts[0])
	case 2:
		c.handleResult(w, r, parts[0], parts[1])
	default:
		http.NotFound(w, r)
	}
}

func (c *WebhookChannel) handleInbound(w http.ResponseWriter, r *http.Request, session string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !sessionNameRe.MatchString(session) {
		http.Error(w, "invalid session name", http.StatusBadRequest)
		return
	}
	if !c.checkAuth(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize+1))
	if err != nil {
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}
	if int64(len(body)) > maxBodySize {
		http.Error(w, "request entity too large", http.StatusRequestEntityTooLarge)
		return
	}

	var payload struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(payload.Content) == "" {
		http.Error(w, "content is required", http.StatusBadRequest)
		return
	}

	sender := bus.SenderInfo{
		Platform:    channelName,
		PlatformID:  session,
		CanonicalID: identity.BuildCanonicalID(channelName, session),
	}
	if !c.IsAllowedSender(sender) {
		http.Error(w, "sender not allowed", http.StatusForbidden)
		return
	}

	tid, err := newTicketID()
	if err != nil {
		logger.ErrorCF(channelName, "Failed to generate ticket id", map[string]any{"error": err.Error()})
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	t := &ticket{
		id:        tid,
		session:   session,
		status:    statusProcessing,
		createdAt: time.Now(),
	}
	c.ticketsMu.Lock()
	c.tickets[tid] = t
	c.ticketsMu.Unlock()
	c.enqueueTicket(session, tid)

	peer := bus.Peer{Kind: "direct", ID: session}
	metadata := map[string]string{
		"platform":   channelName,
		"ticket_id":  tid,
		"session":    session,
	}

	// Use the ticket id as the inbound message ID so that downstream tracing
	// can correlate the agent turn with the originating webhook request.
	go c.HandleMessage(c.ctx, peer, tid, session, session, payload.Content, nil, metadata, sender)

	respondJSON(w, http.StatusAccepted, map[string]string{
		"ticket":  tid,
		"session": session,
		"status":  statusProcessing,
	})
}

func (c *WebhookChannel) handleResult(w http.ResponseWriter, r *http.Request, session, tid string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !c.checkAuth(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	c.ticketsMu.RLock()
	t, ok := c.tickets[tid]
	c.ticketsMu.RUnlock()
	if !ok || t.session != session {
		http.NotFound(w, r)
		return
	}

	resp := map[string]string{
		"ticket":  t.id,
		"session": t.session,
		"status":  t.status,
		"content": t.content,
	}
	respondJSON(w, http.StatusOK, resp)
}

func (c *WebhookChannel) checkAuth(r *http.Request) bool {
	secret := c.cfg.SharedSecret()
	if secret == "" {
		return true
	}
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	provided := header[len(prefix):]
	return subtle.ConstantTimeCompare([]byte(provided), []byte(secret)) == 1
}

// Send is invoked by the channel manager when the agent produces an outbound
// message. We pop the next pending ticket for the chat (= session) and
// attach the response to it.
func (c *WebhookChannel) Send(ctx context.Context, msg bus.OutboundMessage) ([]string, error) {
	if !c.IsRunning() {
		return nil, channels.ErrNotRunning
	}

	tid := c.dequeueTicket(msg.ChatID)
	if tid == "" {
		logger.WarnCF(channelName, "No pending ticket for outbound message", map[string]any{
			"session": msg.ChatID,
		})
		return nil, nil
	}

	c.ticketsMu.Lock()
	t, ok := c.tickets[tid]
	if ok {
		t.status = statusDone
		t.content = msg.Content
		t.doneAt = time.Now()
	}
	c.ticketsMu.Unlock()

	if !ok {
		return nil, nil
	}
	return []string{tid}, nil
}

func (c *WebhookChannel) enqueueTicket(session, tid string) {
	c.sessionsMu.Lock()
	q, ok := c.sessions[session]
	if !ok {
		q = &sessionQueue{}
		c.sessions[session] = q
	}
	c.sessionsMu.Unlock()

	q.mu.Lock()
	q.queue = append(q.queue, tid)
	q.mu.Unlock()
}

func (c *WebhookChannel) dequeueTicket(session string) string {
	c.sessionsMu.Lock()
	q, ok := c.sessions[session]
	c.sessionsMu.Unlock()
	if !ok {
		return ""
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.queue) == 0 {
		return ""
	}
	tid := q.queue[0]
	q.queue = q.queue[1:]
	return tid
}

func (c *WebhookChannel) runJanitor(ctx context.Context) {
	ticker := time.NewTicker(janitorPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			c.evict(now)
		}
	}
}

func (c *WebhookChannel) evict(now time.Time) {
	c.ticketsMu.Lock()
	defer c.ticketsMu.Unlock()
	for id, t := range c.tickets {
		switch t.status {
		case statusDone, statusExpired:
			if now.Sub(t.doneAt) > c.ttl {
				delete(c.tickets, id)
			}
		case statusProcessing:
			if now.Sub(t.createdAt) > c.timeout {
				t.status = statusExpired
				t.doneAt = now
			}
		}
	}
}

func newTicketID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("ticket id rand: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func respondJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
