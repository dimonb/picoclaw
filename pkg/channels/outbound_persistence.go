package channels

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/logger"
)

// SetOutboundRecorder installs the durable delivery sink. It runs after a
// confirmed channel send, before feedback reaches a synchronous caller. Failure
// must never retry a channel send that already succeeded.
func (m *Manager) SetOutboundRecorder(fn func(context.Context, bus.OutboundDelivery) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.outboundRecorder = fn
}

func (m *Manager) recordTextDelivery(ctx context.Context, channel string, msg bus.OutboundMessage, ids []string) error {
	return m.recordDelivery(
		ctx,
		bus.OutboundDelivery{
			Channel:    channel,
			Account:    msg.Context.Account,
			ChatID:     outboundMessageChatID(msg),
			TopicID:    msg.Context.TopicID,
			SessionKey: msg.SessionKey,
			Content:    msg.Content,
		},
		ids,
	)
}

func (m *Manager) recordMediaDelivery(
	ctx context.Context,
	channel string,
	msg bus.OutboundMediaMessage,
	ids []string,
) error {
	return m.recordDelivery(
		ctx,
		bus.OutboundDelivery{
			Channel:    channel,
			Account:    msg.Context.Account,
			ChatID:     outboundMediaChatID(msg),
			TopicID:    msg.Context.TopicID,
			SessionKey: msg.SessionKey,
			Parts:      msg.Parts,
		},
		ids,
	)
}

func (m *Manager) recordDelivery(ctx context.Context, d bus.OutboundDelivery, ids []string) error {
	m.mu.RLock()
	recorder := m.outboundRecorder
	m.mu.RUnlock()
	if recorder == nil || len(ids) == 0 {
		return nil
	}
	// Once the channel accepted the payload, turn cancellation must not discard
	// its receipt. Bound the detached DB write, including on gateway shutdown.
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	for _, id := range ids {
		if id == "" {
			continue
		}
		d.MessageID = id
		// Telegram Send returns a bare numeric ID. Match the inbound opaque ref,
		// preserving topic routing, but never prefix an already-opaque receipt.
		if channelTypeForEvent(m, d.Channel) == "telegram" && !strings.Contains(id, ":") {
			chat, topic, _ := strings.Cut(d.ChatID, "/")
			if topic == "" {
				topic = d.TopicID
			}
			d.MessageID = chat + ":" + id
			if topic != "" {
				d.MessageID = chat + ":" + topic + ":" + id
			}
		}
		if err := recorder(persistCtx, d); err != nil {
			logger.ErrorCF(
				"channels",
				"Delivered message persistence failed",
				map[string]any{"channel": d.Channel, "message_id": d.MessageID, "error": err.Error()},
			)
		}
	}
	return nil
}

// observeDelivery persists native receipts immediately. The per-request set also
// protects exact payloads from later logical-payload fallback overwrites.
func (m *Manager) observeDelivery(
	ctx context.Context,
	base bus.OutboundDelivery,
) (context.Context, func([]string) ([]string, []string)) {
	var mu sync.Mutex
	seen := make(map[string]bool)
	var observed []string
	key := func(id string) string {
		if i := strings.LastIndex(id, ":"); i >= 0 {
			return id[i+1:]
		}
		return id
	}
	observedCtx := bus.WithOutboundReceiptObserver(ctx, func(d bus.OutboundDelivery) {
		if d.MessageID == "" {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		k := key(d.MessageID)
		if seen[k] {
			return
		}
		seen[k] = true
		observed = append(observed, d.MessageID)
		payload := base
		payload.Content, payload.Parts = d.Content, append([]bus.MediaPart(nil), d.Parts...)
		_ = m.recordDelivery(ctx, payload, []string{d.MessageID})
	})
	return observedCtx, func(ids []string) ([]string, []string) {
		mu.Lock()
		defer mu.Unlock()
		all := append([]string(nil), ids...)
		returned := make(map[string]bool)
		var fallback []string
		for _, id := range ids {
			k := key(id)
			if id == "" || returned[k] {
				continue
			}
			returned[k] = true
			if !seen[k] {
				fallback = append(fallback, id)
			}
		}
		for _, id := range observed {
			if !returned[key(id)] {
				all = append(all, id)
				returned[key(id)] = true
			}
		}
		return all, fallback
	}
}
