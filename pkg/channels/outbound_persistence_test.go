package channels

import (
	"context"
	"errors"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
)

func TestOutboundPersistenceNativeRefsAndFailureIsNotDeliveryFailure(t *testing.T) {
	m := &Manager{}
	var got []bus.OutboundDelivery
	m.SetOutboundRecorder(func(ctx context.Context, d bus.OutboundDelivery) error {
		if ctx.Err() != nil {
			t.Fatal("delivery receipt inherited canceled context")
		}
		got = append(got, d)
		return errors.New("database unavailable")
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	msg := bus.NormalizeOutboundMessage(
		bus.OutboundMessage{
			ChatID:  "-100/42",
			Context: bus.NewOutboundContext("telegram", "-100/42", ""),
			Content: "confirmed",
		},
	)
	if err := m.recordTextDelivery(ctx, "telegram", msg, []string{"7", "-100:42:8", ""}); err != nil {
		t.Fatalf("confirmed send must not be reported failed: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("receipts=%d", len(got))
	}
	if got[0].MessageID != "-100:42:7" || got[1].MessageID != "-100:42:8" {
		t.Fatalf("refs=%+v", got)
	}
}

func TestNativeReceiptsWinOverFallbackAndSurvivePartialError(t *testing.T) {
	m := &Manager{}
	var got []bus.OutboundDelivery
	m.SetOutboundRecorder(func(_ context.Context, d bus.OutboundDelivery) error { got = append(got, d); return nil })
	ctx, finish := m.observeDelivery(
		context.Background(),
		bus.OutboundDelivery{Channel: "telegram", ChatID: "-100/42", SessionKey: "session"},
	)
	bus.ObserveOutboundReceipt(ctx, bus.OutboundDelivery{MessageID: "7", Content: "first chunk"})
	bus.ObserveOutboundReceipt(ctx, bus.OutboundDelivery{MessageID: "-100:42:7", Content: "duplicate"})
	ids, fallback := finish([]string{"-100:42:7", "8", "8"})
	if len(ids) != 3 || len(fallback) != 1 || fallback[0] != "8" {
		t.Fatalf("ids=%v fallback=%v", ids, fallback)
	}
	_ = m.recordTextDelivery(
		ctx,
		"telegram",
		bus.OutboundMessage{ChatID: "-100/42", Content: "logical fallback"},
		fallback,
	)
	if len(got) != 2 || got[0].Content != "first chunk" || got[0].MessageID != "-100:42:7" ||
		got[0].SessionKey != "session" ||
		got[1].Content != "logical fallback" {
		t.Fatalf("deliveries=%+v", got)
	}
	// Even a wrapper which loses partial IDs must not cause a resend.
	ids, _ = finish(nil)
	if len(ids) != 1 {
		t.Fatalf("lost observed receipt: %v", ids)
	}
	_, independent := m.observeDelivery(context.Background(), bus.OutboundDelivery{})
	_, fallback = independent([]string{"7"})
	if len(fallback) != 1 {
		t.Fatal("receipt state leaked between requests")
	}
}
