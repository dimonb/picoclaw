package channels

import (
	"context"
	"errors"
	"testing"

	"golang.org/x/time/rate"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
)

// matrixLikeChannel edits any message it is asked to (as a Matrix homeserver
// accepts m.replace for any event) and can delete (redact) any message.
type matrixLikeChannel struct {
	deletingChannel
	edited []string
}

func (c *matrixLikeChannel) EditMessage(_ context.Context, chatID, messageID, _ string) error {
	c.edited = append(c.edited, chatID+"|"+messageID)
	return nil
}

// Review 2, fix 1: an edit must not make a message the bot's own. Editing a
// foreign event and then deleting it is refused, and the edit is not
// recorded as a delivery.
func TestEditDoesNotMakeMessageOwn_MatrixLike(t *testing.T) {
	ctx := context.Background()
	m := newTestManager()
	ch := &matrixLikeChannel{}
	m.channels["matrix"] = ch
	var recorded []string
	m.SetOutboundRecorder(func(_ context.Context, d bus.OutboundDelivery) error {
		recorded = append(recorded, d.ChatID+"|"+d.MessageID+"|"+d.Content)
		return nil
	})

	const room, foreign = "!room:example.org", "$foreign:example.org"
	if err := m.EditMessage(ctx, "matrix", room, foreign, "hijacked"); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteMessage(ctx, "matrix", room, foreign); !errors.Is(err, ErrNotOwnMessage) {
		t.Errorf("delete after edit: err = %v, want ErrNotOwnMessage", err)
	}
	if len(ch.deleted) != 0 || len(recorded) != 0 {
		t.Errorf("deleted=%v recorded=%v, want nothing", ch.deleted, recorded)
	}

	// Editing the bot's own message still updates its stored content.
	_ = m.recordTextDelivery(
		ctx,
		"matrix",
		bus.OutboundMessage{ChatID: room, SessionKey: "s"},
		[]string{"$mine:example.org"},
	)
	recorded = nil
	if err := m.EditMessage(ctx, "matrix", room, "$mine:example.org", "fixed"); err != nil {
		t.Fatal(err)
	}
	if len(recorded) != 1 || recorded[0] != room+"|$mine:example.org|fixed" {
		t.Errorf("own edit recorded = %v", recorded)
	}
}

// Review 2, fix 4: Matrix room v1–2 event IDs ("$x:server") must not collapse
// to the server name: owning one event does not make others own.
func TestOwnRefsUseFullMatrixEventIDs(t *testing.T) {
	ctx := context.Background()
	m := newTestManager()
	ch := &deletingChannel{}
	m.channels["matrix"] = ch
	_ = m.recordTextDelivery(
		ctx,
		"matrix",
		bus.OutboundMessage{ChatID: "!r:example.org"},
		[]string{"$mine:example.org"},
	)

	if err := m.DeleteMessage(ctx, "matrix", "!r:example.org", "$theirs:example.org"); !errors.Is(
		err,
		ErrNotOwnMessage,
	) {
		t.Errorf("other event on the same server: err = %v, want ErrNotOwnMessage", err)
	}
	if err := m.DeleteMessage(ctx, "matrix", "!r:example.org", "$mine:example.org"); err != nil {
		t.Errorf("own event: %v", err)
	}
}

// telegramLikeChannel canonicalizes refs like the Telegram channel and edits
// in the chat embedded in an opaque ref.
type telegramLikeChannel struct{ matrixLikeChannel }

func (c *telegramLikeChannel) CanonicalMessageRef(chatID, messageID string) string {
	switch parts := splitRef(messageID); len(parts) {
	case 2:
		return parts[0] + ":" + parts[1]
	case 3:
		return parts[0] + ":" + parts[2]
	case 1:
		return chatID + ":" + parts[0]
	}
	return ""
}

func splitRef(ref string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(ref); i++ {
		if ref[i] == ':' {
			parts = append(parts, ref[start:i])
			start = i + 1
		}
	}
	return append(parts, ref[start:])
}

// Review 2, fix 1 (Telegram): editing the bot's own message C:N while
// handling group G must not make message N in G (someone else's) deletable,
// neither in memory nor through the durable lookup.
func TestEditCrossChatDoesNotEnableDelete_TelegramLike(t *testing.T) {
	ctx := context.Background()
	m := newTestManager()
	m.config = &config.Config{Channels: config.ChannelsConfig{"telegram": &config.Channel{Type: "telegram"}}}
	ch := &telegramLikeChannel{}
	m.channels["telegram"] = ch

	// The bot's own message 77 in chat C (-100).
	_ = m.recordTextDelivery(ctx, "telegram", bus.OutboundMessage{ChatID: "-100", SessionKey: "s"}, []string{"77"})
	// Edit it while handling group G (-200).
	if err := m.EditMessage(ctx, "telegram", "-200", "-100:77", "fixed"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"77", "-200:77"} {
		if err := m.DeleteMessage(ctx, "telegram", "-200", id); !errors.Is(err, ErrNotOwnMessage) {
			t.Errorf("delete %q in G: err = %v, want ErrNotOwnMessage", id, err)
		}
	}
	// After a restart only the durable store knows: a row polluted by an old
	// edit record ("-100:77" stored under chat G) must not match "77" in G.
	m.ownRefs = deliveredRefs{}
	m.SetDeliveryLookup(func(_ context.Context, _, chatID, _ string) ([]string, error) {
		if chatID == "-200" {
			return []string{"-100:77", "-200:5:78"}, nil
		}
		return nil, nil
	})
	if err := m.DeleteMessage(ctx, "telegram", "-200", "77"); !errors.Is(err, ErrNotOwnMessage) {
		t.Errorf("durable lookup: delete 77 in G: err = %v, want ErrNotOwnMessage", err)
	}
	// A genuine own message in G (stored with its topic) is still deletable.
	if err := m.DeleteMessage(ctx, "telegram", "-200", "78"); err != nil {
		t.Errorf("own message in G: %v", err)
	}
	if len(ch.deleted) != 1 || ch.deleted[0] != "-200|78" {
		t.Errorf("deleted = %v", ch.deleted)
	}
}

// Review 2, fix 5: media receipts are normalized like text receipts.
func TestSendMediaWithRetryNormalizesTelegramIDs(t *testing.T) {
	m := newTestManager()
	m.config = &config.Config{Channels: config.ChannelsConfig{"telegram": &config.Channel{Type: "telegram"}}}
	ch := &mockMediaChannel{sendMediaFn: func(context.Context, bus.OutboundMediaMessage) ([]string, error) {
		return []string{"456"}, nil
	}}
	w := newTestWorker(ch)
	ids, err := m.sendMediaWithRetry(context.Background(), "telegram", w, bus.OutboundMediaMessage{
		Channel: "telegram", ChatID: "-100", Context: bus.NewOutboundContext("telegram", "-100", ""),
	})
	if err != nil || len(ids) != 1 || ids[0] != "-100:456" {
		t.Errorf("ids = %v err = %v, want [-100:456]", ids, err)
	}
}

func newTestWorker(ch Channel) *channelWorker {
	return &channelWorker{ch: ch, limiter: rate.NewLimiter(rate.Inf, 1)}
}
