package channels

import (
	"context"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
)

type receiptStreamer struct {
	mockStreamer
	ids []string
}

func (s *receiptStreamer) DeliveredMessageIDs() []string { return s.ids }

// A streamed final answer is delivered by the streamer; the turn's final
// outbound is then only a duplicate. It must report the stream's post IDs so
// the agent can stamp them onto its reply (and the model can edit it later).
func TestPreSend_StreamedFinalReportsDeliveredIDs(t *testing.T) {
	m := newTestManager()
	ch := &mockStreamingChannel{streamer: &receiptStreamer{ids: []string{"chan:p1", "chan:p2"}}}
	m.channels["test"] = ch

	streamer, ok := m.GetStreamer(context.Background(), "test", "123", "s1")
	if !ok {
		t.Fatal("expected streamer")
	}
	if err := streamer.Finalize(context.Background(), "streamed reply"); err != nil {
		t.Fatal(err)
	}
	final := testOutboundMessage(bus.OutboundMessage{
		Channel: "test", ChatID: "123", SessionKey: "s1", Content: "streamed reply",
		Context: bus.InboundContext{Channel: "test", ChatID: "123", Raw: map[string]string{"outbound_kind": "final"}},
	})
	ids, handled := m.preSend(context.Background(), "test", final, ch)
	if !handled {
		t.Fatal("duplicate final must be handled (not re-sent)")
	}
	if len(ids) != 2 || ids[0] != "chan:p1" {
		t.Errorf("ids = %v, want the stream's posts", ids)
	}
	if _, left := m.streamDelivered.Load(streamSuppressionKey("test", "123", "s1")); left {
		t.Error("delivered IDs must be consumed")
	}
}
