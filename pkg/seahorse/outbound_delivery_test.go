package seahorse

import (
	"context"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
)

func TestOutboundDeliveryUpsertWithoutDuplicateModelTurns(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	d := OutboundDelivery{
		Channel:   "telegram",
		MessageID: "-100:42:7",
		ChatID:    "-100",
		TopicID:   "42",
		Content:   "original",
	}
	if err := s.RecordOutboundDelivery(ctx, d); err != nil {
		t.Fatal(err)
	}
	d.Content = "edited"
	if err := s.RecordOutboundDelivery(ctx, d); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetMessageByChannelMessageID(ctx, d.MessageID)
	if err != nil || got == nil || got.Content != "edited" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM messages").Scan(&n); err != nil || n != 0 {
		t.Fatalf("model rows=%d err=%v", n, err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM outbound_deliveries").Scan(&n); err != nil || n != 1 {
		t.Fatalf("delivery rows=%d err=%v", n, err)
	}
	d.Account = "second"
	if err := s.RecordOutboundDelivery(ctx, d); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetMessageByChannelMessageID(ctx, d.MessageID); err == nil {
		t.Fatal("ambiguous unscoped lookup accepted")
	}
	if got, err := s.GetMessageByChannelMessageIDScoped(
		ctx,
		d.MessageID,
		"telegram",
		"second",
	); err != nil ||
		got == nil {
		t.Fatalf("scoped got=%+v err=%v", got, err)
	}
}

func TestOutboundEditPreservesSession(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	conv, err := s.GetOrCreateConversation(ctx, "telegram:chat")
	if err != nil {
		t.Fatal(err)
	}
	d := OutboundDelivery{
		Channel:    "telegram",
		MessageID:  "chat:7",
		SessionKey: "telegram:chat",
		ChatID:     "chat",
		Content:    "original",
	}
	if err = s.RecordOutboundDelivery(ctx, d); err != nil {
		t.Fatal(err)
	}
	d.SessionKey = ""
	d.ChatID = ""
	d.Content = "edited needle"
	if err = s.RecordOutboundDelivery(ctx, d); err != nil {
		t.Fatal(err)
	}
	found, err := s.SearchMessages(ctx, SearchInput{Pattern: "needle", ConversationID: conv.ConversationID})
	if err != nil || len(found) != 1 {
		t.Fatalf("session lost: %+v %v", found, err)
	}
}

func TestOutboundScopedLegacyFallback(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	c, err := s.GetOrCreateConversation(ctx, "telegram:chat")
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.AddMessage(ctx, c.ConversationID, "user", "legacy", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.UpdateMessageChannelMessageID(ctx, m.ID, "chat:9"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetMessageByChannelMessageIDScoped(ctx, "chat:9", "telegram", "")
	if err != nil || got == nil || got.ID != m.ID {
		t.Fatalf("legacy=%+v err=%v", got, err)
	}
	for _, scope := range [][2]string{{"matrix", ""}, {"telegram", "unproven-account"}} {
		got, err := s.GetMessageByChannelMessageIDScoped(ctx, "chat:9", scope[0], scope[1])
		if err != nil || got != nil {
			t.Fatalf("unsafe fallback=%+v err=%v", got, err)
		}
	}
}

func TestOutboundMediaCaptionGrepExpand(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	d := OutboundDelivery{
		Channel:   "telegram",
		MessageID: "chat:99",
		Parts:     []bus.MediaPart{{Type: "file", Ref: "media://receipt.pdf", Caption: "UniqueReceiptMarker"}},
	}
	if err := s.RecordOutboundDelivery(ctx, d); err != nil {
		t.Fatal(err)
	}
	r := &RetrievalEngine{store: s}
	got, err := r.Grep(ctx, GrepInput{Pattern: "UniqueReceiptMarker", Scope: "message", AllConversations: true})
	if err != nil || len(got.Messages) != 1 {
		t.Fatalf("grep=%+v err=%v", got, err)
	}
	expanded, err := r.ExpandMessages(ctx, []int64{got.Messages[0].ID})
	if err != nil || len(expanded.Messages) != 1 || expanded.Messages[0].Content != "UniqueReceiptMarker" ||
		len(expanded.Messages[0].Attachments) != 1 {
		t.Fatalf("expand=%+v err=%v", expanded, err)
	}
	rows, err := s.SearchMessages(
		ctx,
		SearchInput{Mode: "like", Pattern: "UniqueReceiptMarker", AllConversations: true},
	)
	if err != nil || len(rows) != 1 {
		t.Fatalf("like=%+v err=%v", rows, err)
	}
}
