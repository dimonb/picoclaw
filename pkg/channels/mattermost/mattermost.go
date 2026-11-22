// Package mattermost implements a Mattermost channel: inbound events arrive
// over the v4 WebSocket API, outbound messages go through the v4 REST API.
// No Mattermost SDK is used; the handful of endpoints needed are called
// directly with net/http and gorilla/websocket.
package mattermost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
)

const (
	// maxMessageLength is the Mattermost post size limit (runes).
	maxMessageLength = 16383
	// maxFilesPerPost is the Mattermost limit of attachments on a single post.
	maxFilesPerPost = 10

	httpTimeout    = 60 * time.Second
	typingInterval = 5 * time.Second
	typingTimeout  = 5 * time.Minute
	reactionEmoji  = "eyes"
)

// MattermostChannel connects picoclaw to a Mattermost server as a bot user.
type MattermostChannel struct {
	*channels.BaseChannel
	bc         *config.Channel
	config     *config.MattermostSettings
	baseURL    string
	httpClient *http.Client
	dialer     *websocket.Dialer

	botUserID   string
	botUsername string
	teamID      string
	groupMode   string
	mentionRe   *regexp.Regexp

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	events chan wsEvent

	connMu sync.Mutex
	conn   *websocket.Conn
	seq    atomic.Int64 // client-side action sequence

	reconnectMin time.Duration
	reconnectMax time.Duration

	indMu      sync.Mutex
	indicators map[string]*threadIndicators // "<channel>/<root>" -> pending UX
	feedback   map[string]feedbackPost      // "<channel>/<root>" -> tool-feedback post
	seenPosts  postMemory                   // posts delivered to the agent (for post_edited)
	chanInfo   sync.Map                     // channel ID -> mmChannelInfo
	threads    threadQueue
	users      sync.Map // user id -> userInfo
	botThreads sync.Map // "<channel>/<root>" -> bool (bot takes part in the thread)
}

// NewMattermostChannel validates the settings and builds a channel instance.
// No network calls are made until Start.
func NewMattermostChannel(
	bc *config.Channel,
	cfg *config.MattermostSettings,
	messageBus *bus.MessageBus,
) (*MattermostChannel, error) {
	baseURL, err := normalizeServerURL(cfg.URL)
	if err != nil {
		return nil, err
	}
	if cfg.BotToken.String() == "" {
		return nil, fmt.Errorf("mattermost bot_token is required")
	}
	groupMode, err := normalizeGroupMode(cfg.GroupMode)
	if err != nil {
		return nil, err
	}

	base := channels.NewBaseChannel(config.ChannelMattermost, cfg, messageBus, bc.AllowFrom,
		channels.WithMaxMessageLength(maxMessageLength),
		channels.WithGroupTrigger(bc.GroupTrigger),
		channels.WithReasoningChannelID(bc.ReasoningChannelID),
	)

	return &MattermostChannel{
		BaseChannel:  base,
		bc:           bc,
		config:       cfg,
		baseURL:      baseURL,
		groupMode:    groupMode,
		httpClient:   &http.Client{Timeout: httpTimeout},
		dialer:       &websocket.Dialer{HandshakeTimeout: 30 * time.Second, Proxy: http.ProxyFromEnvironment},
		ctx:          context.Background(),
		reconnectMin: time.Second,
		reconnectMax: time.Minute,
		indicators:   make(map[string]*threadIndicators),
		feedback:     make(map[string]feedbackPost),
	}, nil
}

// mustMentionRe matches "@username" case-insensitively; boundaries are
// checked separately by findMentions.
func mustMentionRe(username string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)@` + regexp.QuoteMeta(username))
}

func normalizeServerURL(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "", fmt.Errorf("mattermost url is required")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("mattermost url must be an absolute http(s) URL, got %q", raw)
	}
	return raw, nil
}

// Start resolves the bot identity (and optional team), then runs the
// WebSocket loop in the background. WebSocket failures are retried with
// backoff and never fail Start; a bad token does, via GET /users/me.
func (c *MattermostChannel) Start(ctx context.Context) error {
	logger.InfoC("mattermost", "Starting Mattermost channel")

	c.ctx, c.cancel = context.WithCancel(ctx)

	var me struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}
	if err := c.apiJSON(c.ctx, http.MethodGet, "/api/v4/users/me", nil, &me); err != nil {
		c.cancel()
		return fmt.Errorf("mattermost: fetch bot identity: %w", err)
	}
	if me.ID == "" {
		c.cancel()
		return fmt.Errorf("mattermost: /users/me returned no user id")
	}
	c.botUserID = me.ID
	c.botUsername = strings.TrimPrefix(strings.TrimSpace(c.config.BotUsername), "@")
	if c.botUsername == "" {
		c.botUsername = me.Username
	}
	if c.botUsername != "" {
		c.mentionRe = mustMentionRe(c.botUsername)
	}

	if team := strings.TrimSpace(c.config.Team); team != "" {
		teamID, err := c.resolveTeam(c.ctx, team)
		if err != nil {
			c.cancel()
			return err
		}
		c.teamID = teamID
	}

	c.events = make(chan wsEvent, 64)
	c.wg.Add(2)
	go c.runWebSocket()
	go c.eventWorker()

	c.SetRunning(true)
	logger.InfoCF("mattermost", "Mattermost channel started", map[string]any{
		"bot_user_id":  c.botUserID,
		"bot_username": c.botUsername,
		"team_id":      c.teamID,
	})
	return nil
}

// Stop cancels the WebSocket loop, closes the connection, stops typing
// indicators and waits for background goroutines (bounded by ctx).
func (c *MattermostChannel) Stop(ctx context.Context) error {
	logger.InfoC("mattermost", "Stopping Mattermost channel")
	c.SetRunning(false)

	if c.cancel != nil {
		c.cancel()
	}
	c.closeConn()

	c.indMu.Lock()
	for key, ind := range c.indicators {
		ind.stop()
		delete(c.indicators, key)
	}
	c.indMu.Unlock()

	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	logger.InfoC("mattermost", "Mattermost channel stopped")
	return nil
}

func (c *MattermostChannel) resolveTeam(ctx context.Context, team string) (string, error) {
	var t struct {
		ID string `json:"id"`
	}
	err := c.apiJSON(ctx, http.MethodGet, "/api/v4/teams/name/"+url.PathEscape(team), nil, &t)
	if err == nil && t.ID != "" {
		return t.ID, nil
	}
	if err2 := c.apiJSON(ctx, http.MethodGet, "/api/v4/teams/"+url.PathEscape(team), nil, &t); err2 == nil &&
		t.ID != "" {
		return t.ID, nil
	}
	if err == nil {
		err = errors.New("team not found")
	}
	return "", fmt.Errorf("mattermost: cannot resolve team %q (by name or id): %w", team, err)
}

// --- outbound ----------------------------------------------------------------

// Send posts a message. The thread is taken from the per-turn context (the
// post being answered), from an explicit reply target, or from a
// "<channel>/<root>" chat ID; without any of them the post is top-level.
// Content over the Mattermost limit is split into several posts. A pending
// placeholder for the thread is edited into the first chunk.
func (c *MattermostChannel) Send(ctx context.Context, msg bus.OutboundMessage) ([]string, error) {
	if !c.IsRunning() {
		return nil, channels.ErrNotRunning
	}
	channelID, rootID := resolveTarget(msg.ChatID, &msg.Context, msg.ReplyToMessageID)
	if channelID == "" {
		return nil, fmt.Errorf("invalid mattermost chat ID %q: %w", msg.ChatID, channels.ErrSendFailed)
	}
	if isFinalOutbound(msg.Context) {
		defer c.threads.done(channelID, rootID)
	}
	if strings.TrimSpace(msg.Content) == "" {
		return nil, nil
	}
	if isToolFeedback(msg) {
		return c.sendToolFeedback(ctx, channelID, rootID, msg.Content)
	}

	reuse := c.takeReusablePost(ctx, channelID, rootID)
	chunks := channels.SplitMessage(msg.Content, maxMessageLength)
	ids := make([]string, 0, len(chunks))
	for i, chunk := range chunks {
		if i == 0 && reuse != "" {
			if err := c.patchPost(ctx, reuse, chunk); err == nil {
				ids = append(ids, encodeMessageID(channelID, rootID, reuse))
				continue
			}
			_ = c.deletePost(ctx, reuse)
		}
		postID, err := c.createPost(ctx, channelID, rootID, chunk, nil)
		if err != nil {
			return ids, err
		}
		ids = append(ids, encodeMessageID(channelID, rootID, postID))
	}
	return ids, nil
}

// EditMessage implements channels.MessageEditor via PUT /posts/{id}/patch.
// messageID may be a raw post ID or an encoded "<channel>:[<root>:]<post>".
// Like DeleteMessage it only touches the bot's own posts in chatID: a bot
// account with admin rights could otherwise edit other people's posts.
func (c *MattermostChannel) EditMessage(ctx context.Context, chatID, messageID, content string) error {
	postID, err := c.ownPost(ctx, chatID, messageID)
	if err != nil {
		return err
	}
	return c.patchPost(ctx, postID, content)
}

// DeleteMessage implements channels.MessageDeleter. Only the bot's own posts
// in chatID can be deleted (the bot account may have moderation rights).
func (c *MattermostChannel) DeleteMessage(ctx context.Context, chatID, messageID string) error {
	postID, err := c.ownPost(ctx, chatID, messageID)
	if err != nil {
		return err
	}
	return c.deletePost(ctx, postID)
}

// IsOwnMessage implements channels.OwnMessageVerifier.
func (c *MattermostChannel) IsOwnMessage(ctx context.Context, chatID, messageID string) (bool, error) {
	_, err := c.ownPost(ctx, chatID, messageID)
	if errors.Is(err, channels.ErrNotOwnMessage) {
		return false, nil
	}
	return err == nil, err
}

// ownPost resolves messageID to a post ID after checking with the server
// that the post was written by the bot in chatID's channel.
func (c *MattermostChannel) ownPost(ctx context.Context, chatID, messageID string) (string, error) {
	ref := parseMessageID(messageID)
	if ref.postID == "" {
		return "", fmt.Errorf("mattermost: empty message id: %w", channels.ErrSendFailed)
	}
	channelID, _, _ := strings.Cut(strings.TrimSpace(chatID), "/")
	if ref.channelID != "" && channelID != "" && ref.channelID != channelID {
		return "", fmt.Errorf("mattermost: post %s is not in channel %s: %w", ref.postID, channelID,
			channels.ErrNotOwnMessage)
	}
	var post struct {
		UserID    string `json:"user_id"`
		ChannelID string `json:"channel_id"`
	}
	if err := c.apiJSON(ctx, http.MethodGet, "/api/v4/posts/"+url.PathEscape(ref.postID), nil, &post); err != nil {
		return "", err
	}
	if post.UserID == "" || post.UserID != c.botUserID || (channelID != "" && post.ChannelID != channelID) {
		return "", fmt.Errorf("mattermost: post %s is not the bot's post in channel %s: %w", ref.postID, channelID,
			channels.ErrNotOwnMessage)
	}
	return ref.postID, nil
}

func (c *MattermostChannel) patchPost(ctx context.Context, postID, content string) error {
	body := map[string]any{"message": content}
	return c.apiJSON(ctx, http.MethodPut, "/api/v4/posts/"+url.PathEscape(postID)+"/patch", body, nil)
}

func (c *MattermostChannel) deletePost(ctx context.Context, postID string) error {
	if postID == "" {
		return nil
	}
	return c.apiJSON(ctx, http.MethodDelete, "/api/v4/posts/"+url.PathEscape(postID), nil, nil)
}

// startTyping shows "is typing…" in the thread until the returned (idempotent)
// stop is called or typingTimeout elapses. Mattermost clears the indicator
// after a few seconds, so it is refreshed periodically.
func (c *MattermostChannel) startTyping(channelID, rootID string) func() {
	stop := make(chan struct{})
	send := func() {
		reqCtx, cancel := context.WithTimeout(c.ctx, 10*time.Second)
		defer cancel()
		body := map[string]any{"channel_id": channelID, "parent_id": rootID}
		if err := c.apiJSON(reqCtx, http.MethodPost, "/api/v4/users/me/typing", body, nil); err != nil {
			logger.DebugCF("mattermost", "typing indicator failed", map[string]any{"error": err.Error()})
		}
	}
	go func() {
		send()
		ticker := time.NewTicker(typingInterval)
		defer ticker.Stop()
		timeout := time.NewTimer(typingTimeout)
		defer timeout.Stop()
		for {
			select {
			case <-stop:
				return
			case <-c.ctx.Done():
				return
			case <-timeout.C:
				return
			case <-ticker.C:
				send()
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(stop) }) }
}

func (c *MattermostChannel) addReaction(ctx context.Context, postID, emoji string) error {
	body := map[string]any{"user_id": c.botUserID, "post_id": postID, "emoji_name": emoji}
	return c.apiJSON(ctx, http.MethodPost, "/api/v4/reactions", body, nil)
}

func (c *MattermostChannel) removeReaction(ctx context.Context, postID, emoji string) error {
	path := fmt.Sprintf("/api/v4/users/%s/posts/%s/reactions/%s",
		url.PathEscape(c.botUserID), url.PathEscape(postID), url.PathEscape(emoji))
	return c.apiJSON(ctx, http.MethodDelete, path, nil, nil)
}

// SendMedia implements channels.MediaSender: files are uploaded to
// /api/v4/files and attached to posts (up to 10 per post), in the same
// thread Send would use.
func (c *MattermostChannel) SendMedia(ctx context.Context, msg bus.OutboundMediaMessage) ([]string, error) {
	if !c.IsRunning() {
		return nil, channels.ErrNotRunning
	}
	channelID, rootID := resolveTarget(msg.ChatID, &msg.Context, "")
	if channelID == "" {
		return nil, fmt.Errorf("invalid mattermost chat ID %q: %w", msg.ChatID, channels.ErrSendFailed)
	}
	store := c.GetMediaStore()
	if store == nil {
		return nil, fmt.Errorf("no media store available: %w", channels.ErrSendFailed)
	}
	c.discardPending(ctx, channelID, rootID)

	fileIDs := make([]string, 0, len(msg.Parts))
	caption := ""
	for _, part := range msg.Parts {
		if caption == "" {
			caption = strings.TrimSpace(part.Caption)
		}
		localPath, err := store.Resolve(part.Ref)
		if err != nil {
			logger.ErrorCF("mattermost", "Failed to resolve media ref", map[string]any{
				"ref": part.Ref, "error": err.Error(),
			})
			continue
		}
		name := part.Filename
		if name == "" {
			name = filepath.Base(localPath)
		}
		id, err := c.uploadFile(ctx, channelID, localPath, name)
		if err != nil {
			return nil, err
		}
		fileIDs = append(fileIDs, id)
	}
	if len(fileIDs) == 0 {
		return nil, fmt.Errorf("mattermost: no media could be uploaded: %w", channels.ErrSendFailed)
	}

	ids := make([]string, 0, (len(fileIDs)+maxFilesPerPost-1)/maxFilesPerPost)
	for start := 0; start < len(fileIDs); start += maxFilesPerPost {
		end := min(start+maxFilesPerPost, len(fileIDs))
		text := ""
		if start == 0 {
			text = caption
		}
		postID, err := c.createPost(ctx, channelID, rootID, text, fileIDs[start:end])
		if err != nil {
			return ids, err
		}
		ids = append(ids, encodeMessageID(channelID, rootID, postID))
	}
	return ids, nil
}

func (c *MattermostChannel) createPost(
	ctx context.Context,
	channelID, rootID, message string,
	fileIDs []string,
) (string, error) {
	body := map[string]any{"channel_id": channelID, "message": message}
	if rootID != "" {
		body["root_id"] = rootID
	}
	if len(fileIDs) > 0 {
		body["file_ids"] = fileIDs
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := c.apiJSON(ctx, http.MethodPost, "/api/v4/posts", body, &created); err != nil {
		return "", err
	}
	if rootID != "" {
		c.markBotThread(channelID, rootID)
	} else {
		c.markBotThread(channelID, created.ID) // a top-level bot post roots a thread
	}
	return created.ID, nil
}

func (c *MattermostChannel) uploadFile(ctx context.Context, channelID, localPath, name string) (string, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return "", fmt.Errorf("mattermost upload: %w: %w", channels.ErrSendFailed, err)
	}
	defer f.Close()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err = mw.WriteField("channel_id", channelID); err != nil {
		return "", err
	}
	fw, err := mw.CreateFormFile("files", name)
	if err != nil {
		return "", err
	}
	if _, err = io.Copy(fw, f); err != nil {
		return "", fmt.Errorf("mattermost upload: %w: %w", channels.ErrSendFailed, err)
	}
	if err = mw.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v4/files", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	var resp struct {
		FileInfos []struct {
			ID string `json:"id"`
		} `json:"file_infos"`
	}
	if err := c.do(req, &resp); err != nil {
		return "", err
	}
	if len(resp.FileInfos) == 0 || resp.FileInfos[0].ID == "" {
		return "", fmt.Errorf("mattermost upload: empty file_infos: %w", channels.ErrSendFailed)
	}
	return resp.FileInfos[0].ID, nil
}

// --- REST helpers ------------------------------------------------------------

func (c *MattermostChannel) apiJSON(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.do(req, out)
}

// do authenticates and executes req, mapping HTTP failures onto the channel
// error sentinels so the Manager retries only what is retryable.
func (c *MattermostChannel) do(req *http.Request, out any) error {
	req.Header.Set("Authorization", "Bearer "+c.config.BotToken.String())
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return err
		}
		return channels.ClassifyNetError(fmt.Errorf("mattermost %s %s: %w", req.Method, req.URL.Path, err))
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var apiErr struct {
			ID      string `json:"id"`
			Message string `json:"message"`
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		detail := strings.TrimSpace(string(raw))
		if json.Unmarshal(raw, &apiErr) == nil && apiErr.Message != "" {
			detail = apiErr.Message
		}
		return channels.ClassifySendError(resp.StatusCode,
			fmt.Errorf("mattermost %s %s: HTTP %d: %s", req.Method, req.URL.Path, resp.StatusCode, detail))
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("mattermost %s %s: decode response: %w", req.Method, req.URL.Path, err)
	}
	return nil
}
