package channels

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/logger"
)

// Own-message checks for Manager.DeleteMessage.
//
// A message counts as the bot's own in a chat only when the bot itself
// delivered it there (a confirmed send, never an edit: platforms such as
// Matrix accept edit events for any message, and an edit may target another
// chat than the one passed along). Messages are compared by their full
// reference in canonical form, never by a last segment, so IDs from another
// chat (Telegram "C:N" vs "G:N") or Matrix event IDs ("$x:server") cannot
// collide.

// OwnMessageVerifier is implemented by channels that can check with the
// platform whether messageID is a message the bot itself sent in chatID
// (e.g. Mattermost: the post's user_id and channel_id).
type OwnMessageVerifier interface {
	IsOwnMessage(ctx context.Context, chatID, messageID string) (bool, error)
}

// MessageRefCanonicalizer is implemented by channels whose message references
// come in several forms. CanonicalMessageRef returns one form for messageID as
// seen from chatID, including the chat the message lives in (Telegram
// "chat:msg" for "chat:topic:msg" or a bare "msg"; Mattermost "channel:post"
// for "channel:root:post" or a bare "post"), or "" if it cannot be parsed.
type MessageRefCanonicalizer interface {
	CanonicalMessageRef(chatID, messageID string) string
}

// DeliveryLookup returns the durable references of the bot's confirmed
// deliveries in the chat whose reference ends with refSuffix (a prefilter;
// the Manager compares canonical forms exactly). Installed by the agent loop
// next to the outbound recorder (e.g. seahorse outbound_deliveries).
type DeliveryLookup func(ctx context.Context, channel, chatID, refSuffix string) ([]string, error)

// SetDeliveryLookup installs the durable own-message lookup used by
// DeleteMessage when a message is not in the in-memory delivery record.
func (m *Manager) SetDeliveryLookup(fn DeliveryLookup) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deliveryLookup = fn
}

// canonicalMessageRef returns the channel's canonical form of messageID in
// chatID, or the trimmed reference itself.
func canonicalMessageRef(ch Channel, chatID, messageID string) string {
	ref := strings.TrimSpace(messageID)
	if c, ok := ch.(MessageRefCanonicalizer); ok && ref != "" {
		if canonical := c.CanonicalMessageRef(chatID, ref); canonical != "" {
			return canonical
		}
	}
	return ref
}

// refSuffix is the prefilter for DeliveryLookup: the part after the last
// ':' (Telegram message ID, Mattermost post ID), which every stored form of
// the same message ends with.
func refSuffix(ref string) string {
	if i := strings.LastIndex(ref, ":"); i >= 0 && i < len(ref)-1 {
		return ref[i+1:]
	}
	return ref
}

// rememberOwnDelivery records a confirmed send in the in-memory record.
func (m *Manager) rememberOwnDelivery(channel, chatID, messageID string) {
	ch, _ := m.GetChannel(channel)
	m.ownRefs.add(channel, chatID, canonicalMessageRef(ch, chatID, messageID))
}

// isOwnMessage checks, in order: the in-memory record of confirmed
// deliveries, the channel's own verification, and the durable delivery
// lookup. Unknown means "not own".
func (m *Manager) isOwnMessage(ctx context.Context, channel string, ch Channel, chatID, messageID string) bool {
	want := canonicalMessageRef(ch, chatID, messageID)
	if want == "" {
		return false
	}
	if m.ownRefs.has(channel, chatID, want) {
		return true
	}
	if verifier, ok := ch.(OwnMessageVerifier); ok {
		own, err := verifier.IsOwnMessage(ctx, chatID, messageID)
		if err == nil {
			return own
		}
		logger.DebugCF("channels", "Own-message check failed", map[string]any{
			"channel": channel, "message_id": messageID, "error": err.Error(),
		})
	}
	m.mu.RLock()
	lookup := m.deliveryLookup
	m.mu.RUnlock()
	if lookup == nil {
		return false
	}
	refs, err := lookup(ctx, channel, chatID, refSuffix(want))
	if err != nil {
		return false
	}
	for _, ref := range refs {
		if canonicalMessageRef(ch, chatID, ref) == want {
			return true
		}
	}
	return false
}

const (
	ownRefsMax = 20000
	ownRefsTTL = 7 * 24 * time.Hour
)

// deliveredRefs remembers the bot's confirmed deliveries, keyed by channel,
// chat and the full canonical message reference.
type deliveredRefs struct {
	mu    sync.Mutex
	refs  map[string]time.Time
	order []string // insertion order, for oldest-first eviction
}

func ownRefKey(channel, chatID, canonicalRef string) string {
	chat, _, _ := strings.Cut(strings.TrimSpace(chatID), "/")
	ref := strings.TrimSpace(canonicalRef)
	if channel == "" || chat == "" || ref == "" {
		return ""
	}
	return channel + "|" + chat + "|" + ref
}

func (r *deliveredRefs) add(channel, chatID, canonicalRef string) {
	key := ownRefKey(channel, chatID, canonicalRef)
	if key == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.refs == nil {
		r.refs = make(map[string]time.Time)
	}
	if _, exists := r.refs[key]; !exists {
		r.order = append(r.order, key)
	}
	r.refs[key] = time.Now()
	for len(r.order) > ownRefsMax {
		delete(r.refs, r.order[0])
		r.order = r.order[1:]
	}
}

func (r *deliveredRefs) has(channel, chatID, canonicalRef string) bool {
	key := ownRefKey(channel, chatID, canonicalRef)
	if key == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	at, ok := r.refs[key]
	return ok && time.Since(at) < ownRefsTTL
}
