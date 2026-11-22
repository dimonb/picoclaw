package agent

import (
	"context"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
)

// The final reply of a regular turn (SendResponse=false path through
// runTurnWithSteering) must carry the inbound context of the message being
// answered, so channels like Mattermost can reply into that post's thread.
// ReplyToMessageID is not propagated (no quote-reply to the user's target).
func TestRunTurnWithSteering_FinalReplyCarriesInboundContext(t *testing.T) {
	al, _, msgBus, _, cleanup := newTestAgentLoop(t)
	defer cleanup()

	inbound := bus.InboundMessage{
		Context: bus.InboundContext{
			Channel:          "mattermost",
			ChatID:           "chan1",
			ChatType:         "channel",
			SenderID:         "user1",
			MessageID:        "chan1:root1:post2",
			ReplyToMessageID: "chan1:root1",
			Mentioned:        true,
			Raw:              map[string]string{"thread_root": "root1"},
		},
		Content: "hello",
	}
	inbound = bus.NormalizeInboundMessage(inbound)

	al.runTurnWithSteering(context.Background(), inbound)

	select {
	case out := <-msgBus.OutboundChan():
		if out.Content != "Mock response" {
			t.Fatalf("content = %q", out.Content)
		}
		if out.ChatID != "chan1" || out.Context.MessageID != "chan1:root1:post2" {
			t.Errorf("chat=%q messageID=%q, want inbound message context", out.ChatID, out.Context.MessageID)
		}
		if out.Context.ReplyToMessageID != "" || out.ReplyToMessageID != "" {
			t.Errorf("reply_to must not be propagated, got %q / %q", out.Context.ReplyToMessageID, out.ReplyToMessageID)
		}
		if out.Context.Raw[metadataKeyOutboundKind] != outboundKindFinal || out.Context.Raw["thread_root"] != "root1" {
			t.Errorf("raw = %v", out.Context.Raw)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no final outbound")
	}
}

func TestFinalOutboundContext_IgnoresOtherChat(t *testing.T) {
	inbound := &bus.InboundContext{Channel: "mattermost", ChatID: "chan1", MessageID: "chan1:p1"}
	if got := finalOutboundContext(inbound, "mattermost", "chan2"); got.MessageID != "" || got.ChatID != "chan2" {
		t.Errorf("other chat: %+v", got)
	}
	if got := finalOutboundContext(inbound, "telegram", "chan1"); got.MessageID != "" {
		t.Errorf("other channel: %+v", got)
	}
	if got := finalOutboundContext(nil, "mattermost", "chan1"); got.ChatID != "chan1" || got.MessageID != "" {
		t.Errorf("nil inbound: %+v", got)
	}
}
