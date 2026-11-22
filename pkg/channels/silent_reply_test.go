package channels

import (
	"context"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/sipeed/picoclaw/pkg/bus"
)

// silentTestChannel records what the manager does with a silent reply.
type silentTestChannel struct {
	mockChannel
	mu        sync.Mutex
	deleted   []string
	silentFor []bus.OutboundMessage
	typing    int
	reactions int
}

func (c *silentTestChannel) DeleteMessage(_ context.Context, _, messageID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deleted = append(c.deleted, messageID)
	return nil
}

func (c *silentTestChannel) HandleSilentReply(_ context.Context, msg bus.OutboundMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.silentFor = append(c.silentFor, msg)
}

func (c *silentTestChannel) StartTyping(context.Context, string) (func(), error) {
	c.mu.Lock()
	c.typing++
	c.mu.Unlock()
	return func() {}, nil
}

func (c *silentTestChannel) ReactToMessage(context.Context, string, string) (func(), error) {
	c.mu.Lock()
	c.reactions++
	c.mu.Unlock()
	return func() {}, nil
}

// A silent final (NO_REPLY) is never sent: typing stops, the reaction is
// undone, the placeholder deleted and the channel's own handler notified.
func TestRunWorker_SilentReplyCleansUpWithoutSending(t *testing.T) {
	m := newTestManager()
	ch := &silentTestChannel{}
	var typingStopped, reactionUndone bool
	m.RecordTypingStop("test", "chat1", func() { typingStopped = true })
	m.RecordReactionUndo("test", "chat1", func() { reactionUndone = true })
	m.RecordPlaceholder("test", "chat1", "ph-1")

	w := &channelWorker{
		ch:      ch,
		queue:   make(chan bus.OutboundMessage, 1),
		done:    make(chan struct{}),
		limiter: rate.NewLimiter(rate.Inf, 1),
	}
	go m.runWorker(t.Context(), "test", w)

	feedback := make(chan bus.DeliveryResult, 1)
	msg := testOutboundMessage(bus.OutboundMessage{Channel: "test", ChatID: "chat1", Feedback: feedback})
	msg.Context.Raw = map[string]string{"message_kind": bus.MessageKindSilent, "outbound_kind": "final"}
	w.queue <- msg

	select {
	case res := <-feedback:
		if res.Err != nil || len(res.IDs) != 0 {
			t.Fatalf("feedback = %+v, want empty success", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no feedback for silent reply")
	}
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if len(ch.sentMessages) != 0 {
		t.Errorf("silent reply was sent: %+v", ch.sentMessages)
	}
	if !typingStopped || !reactionUndone {
		t.Errorf("typingStopped=%v reactionUndone=%v", typingStopped, reactionUndone)
	}
	if len(ch.deleted) != 1 || ch.deleted[0] != "ph-1" {
		t.Errorf("placeholder deletes = %v", ch.deleted)
	}
	if len(ch.silentFor) != 1 {
		t.Errorf("SilentReplyHandler calls = %d", len(ch.silentFor))
	}
}

// Group messages that do not address the bot get no typing/reaction up
// front (they may end in NO_REPLY); addressed and direct messages do.
func TestHandleInbound_IndicatorsOnlyForAddressedMessages(t *testing.T) {
	tests := []struct {
		name      string
		chatType  string
		mentioned bool
		want      int
	}{
		{"direct", "direct", false, 1},
		{"group mentioned", "group", true, 1},
		{"group not addressed", "group", false, 0},
		{"channel not addressed", "channel", false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msgBus := bus.NewMessageBus()
			defer msgBus.Close()
			ch := &silentTestChannel{}
			ch.BaseChannel = *NewBaseChannel("test", nil, msgBus, nil)
			ch.SetOwner(ch)
			ch.SetPlaceholderRecorder(newTestManager())

			ch.HandleInboundContext(context.Background(), "chat1", "hi", nil, bus.InboundContext{
				Channel: "test", ChatID: "chat1", ChatType: tt.chatType, SenderID: "u1",
				MessageID: "m1", Mentioned: tt.mentioned,
			})
			<-msgBus.InboundChan()
			if ch.typing != tt.want || ch.reactions != tt.want {
				t.Errorf("typing=%d reactions=%d, want %d", ch.typing, ch.reactions, tt.want)
			}
		})
	}
}
