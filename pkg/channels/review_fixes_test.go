package channels

import (
	"context"
	"errors"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
)

type deletingChannel struct {
	mockChannel
	deleted  []string
	verifyFn func(chatID, messageID string) (bool, error)
}

func (c *deletingChannel) DeleteMessage(_ context.Context, chatID, messageID string) error {
	c.deleted = append(c.deleted, chatID+"|"+messageID)
	return nil
}

type verifyingChannel struct{ deletingChannel }

func (c *verifyingChannel) IsOwnMessage(_ context.Context, chatID, messageID string) (bool, error) {
	return c.verifyFn(chatID, messageID)
}

// DeleteMessage only deletes messages the bot itself delivered in that chat.
func TestManagerDeleteMessage_OnlyOwnMessagesInChat(t *testing.T) {
	ctx := context.Background()
	m := newTestManager()
	ch := &deletingChannel{}
	m.channels["test"] = ch

	_ = m.recordTextDelivery(ctx, "test", bus.OutboundMessage{ChatID: "chat1"}, []string{"chat1:42"})

	if err := m.DeleteMessage(ctx, "test", "chat1", "chat1:42"); err != nil {
		t.Fatalf("own message: %v", err)
	}
	for _, tc := range []struct{ chatID, messageID string }{
		{"chat1", "chat1:43"}, // someone else's message in the chat
		{"chat2", "chat2:42"}, // same ID, other chat
		{"chat2", "42"},
		{"chat1", "42"}, // no canonicalizer: only the full reference matches
	} {
		err := m.DeleteMessage(ctx, "test", tc.chatID, tc.messageID)
		if !errors.Is(err, ErrNotOwnMessage) {
			t.Errorf("%+v: err = %v, want ErrNotOwnMessage", tc, err)
		}
	}
	if len(ch.deleted) != 1 {
		t.Errorf("deleted = %v, want only the own delete", ch.deleted)
	}
}

// Unknown to the in-memory record (e.g. after a restart): the channel's own
// check and then the durable delivery lookup decide.
func TestManagerDeleteMessage_VerifierAndLookup(t *testing.T) {
	ctx := context.Background()
	m := newTestManager()
	vch := &verifyingChannel{}
	vch.verifyFn = func(chatID, messageID string) (bool, error) { return messageID == "c:mine", nil }
	m.channels["mm"] = vch
	if err := m.DeleteMessage(ctx, "mm", "c", "c:mine"); err != nil {
		t.Errorf("verified own: %v", err)
	}
	if err := m.DeleteMessage(ctx, "mm", "c", "c:theirs"); !errors.Is(err, ErrNotOwnMessage) {
		t.Errorf("verified foreign: %v", err)
	}

	dch := &deletingChannel{}
	m.channels["tg"] = dch
	var looked []string
	m.SetDeliveryLookup(func(_ context.Context, channel, chatID, suffix string) ([]string, error) {
		looked = append(looked, channel+"|"+chatID+"|"+suffix)
		return []string{"100:7"}, nil
	})
	if err := m.DeleteMessage(ctx, "tg", "100", "100:7"); err != nil {
		t.Errorf("durable own: %v", err)
	}
	if err := m.DeleteMessage(ctx, "tg", "100", "100:8"); !errors.Is(err, ErrNotOwnMessage) {
		t.Errorf("durable unknown: %v", err)
	}
	if len(looked) != 2 || len(dch.deleted) != 1 {
		t.Errorf("looked=%v deleted=%v", looked, dch.deleted)
	}
}

// Telegram Send receipts are bare IDs; the Manager reports them as the opaque
// refs the agent sees (they end up as msgs:#… in history).
func TestNormalizeDeliveredIDs_Telegram(t *testing.T) {
	m := newTestManager()
	m.config = &config.Config{}
	m.config.Channels = config.ChannelsConfig{"tg": &config.Channel{Type: "telegram"}}
	got := m.normalizeDeliveredIDs("tg", bus.OutboundMessage{ChatID: "-100/5"}, []string{"7", "-100:5:8"})
	if len(got) != 2 || got[0] != "-100:5:7" || got[1] != "-100:5:8" {
		t.Errorf("telegram ids = %v", got)
	}
	if got := m.normalizeDeliveredIDs("mm", bus.OutboundMessage{ChatID: "c"}, []string{"p1"}); got[0] != "p1" {
		t.Errorf("non-telegram ids must stay as they are: %v", got)
	}
}

// A silent final clears the stream marker and streamed IDs, so they cannot
// swallow or label the next turn's final reply.
func TestSilentReplyClearsStaleStreamState(t *testing.T) {
	ctx := context.Background()
	m := newTestManager()
	ch := &mockStreamingChannel{streamer: &receiptStreamer{ids: []string{"chan:old"}}}
	m.channels["test"] = ch

	st, _ := m.GetStreamer(ctx, "test", "123", "s1")
	if err := st.Finalize(ctx, "streamed"); err != nil {
		t.Fatal(err)
	}
	silent := testOutboundMessage(bus.OutboundMessage{
		Channel: "test", ChatID: "123", SessionKey: "s1",
		Context: bus.InboundContext{Channel: "test", ChatID: "123", Raw: map[string]string{
			"message_kind": bus.MessageKindSilent, "outbound_kind": "final",
		}},
	})
	m.handleSilentReply(ctx, "test", ch, silent)

	next := testOutboundMessage(bus.OutboundMessage{
		Channel: "test", ChatID: "123", SessionKey: "s1", Content: "next turn",
		Context: bus.InboundContext{Channel: "test", ChatID: "123", Raw: map[string]string{"outbound_kind": "final"}},
	})
	if ids, handled := m.preSend(ctx, "test", next, ch); handled || len(ids) != 0 {
		t.Errorf("next final swallowed by stale stream state: handled=%v ids=%v", handled, ids)
	}
}

// A new stream drops IDs left by an earlier, unclaimed stream.
func TestGetStreamerDropsStaleDeliveredIDs(t *testing.T) {
	m := newTestManager()
	key := streamSuppressionKey("test", "123", "s1")
	m.streamDelivered.Store(key, []string{"chan:stale"})
	m.channels["test"] = &mockStreamingChannel{streamer: &receiptStreamer{ids: []string{"chan:new"}}}

	st, _ := m.GetStreamer(context.Background(), "test", "123", "s1")
	if err := st.Finalize(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if ids := m.takeStreamDelivered(key); len(ids) != 1 || ids[0] != "chan:new" {
		t.Errorf("ids = %v, want only the new stream's", ids)
	}
}
