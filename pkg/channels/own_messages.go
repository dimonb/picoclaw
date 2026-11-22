package channels

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/logger"
)

// OwnMessageVerifier is implemented by channels that can check with the
// platform whether messageID is a message the bot itself sent in chatID
// (e.g. Mattermost: the post's user_id and channel_id).
type OwnMessageVerifier interface {
	IsOwnMessage(ctx context.Context, chatID, messageID string) (bool, error)
}

// DeliveryLookup reports whether a durable delivery record exists for the
// message in the chat (e.g. seahorse outbound_deliveries). Installed by the
// agent loop next to the outbound recorder.
type DeliveryLookup func(ctx context.Context, channel, chatID, messageID string) (bool, error)

// SetDeliveryLookup installs the durable own-message lookup used by
// DeleteMessage when a message is not in the in-memory delivery record.
func (m *Manager) SetDeliveryLookup(fn DeliveryLookup) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deliveryLookup = fn
}

// isOwnMessage checks, in order: the in-memory record of confirmed
// deliveries, the channel's own verification, and the durable delivery
// lookup. Unknown means "not own".
func (m *Manager) isOwnMessage(ctx context.Context, channel string, ch Channel, chatID, messageID string) bool {
	if m.ownRefs.has(channel, chatID, messageID) {
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
	if lookup != nil {
		ref := m.normalizeDeliveredID(channel, chatID, "", messageID)
		if own, err := lookup(ctx, channel, chatID, ref); err == nil && own {
			return true
		}
	}
	return false
}

const (
	ownRefsMax = 20000
	ownRefsTTL = 7 * 24 * time.Hour
)

// deliveredRefs remembers the bot's confirmed deliveries per chat, keyed by
// the last segment of the message ref (the platform message/post ID), so
// "chat:msg", "chat:topic:msg" and a bare "msg" match the same delivery.
type deliveredRefs struct {
	mu    sync.Mutex
	refs  map[string]time.Time
	order []string // insertion order, for oldest-first eviction
}

func ownRefKey(channel, chatID, messageID string) string {
	chat, _, _ := strings.Cut(strings.TrimSpace(chatID), "/")
	id := strings.TrimSpace(messageID)
	if i := strings.LastIndexAny(id, ": "); i >= 0 {
		id = id[i+1:]
	}
	if channel == "" || chat == "" || id == "" {
		return ""
	}
	return channel + "|" + chat + "|" + id
}

func (r *deliveredRefs) add(channel, chatID, messageID string) {
	key := ownRefKey(channel, chatID, messageID)
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

func (r *deliveredRefs) has(channel, chatID, messageID string) bool {
	key := ownRefKey(channel, chatID, messageID)
	if key == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	at, ok := r.refs[key]
	return ok && time.Since(at) < ownRefsTTL
}
