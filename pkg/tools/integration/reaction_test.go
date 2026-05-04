package integrationtools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
)

func TestReactionTool_Execute_UsesContextMessageIDByDefault(t *testing.T) {
	tool := NewReactionTool()

	var gotChannel, gotChatID, gotMessageID string
	tool.SetReactionCallback(func(ctx context.Context, channel, chatID, messageID string) error {
		gotChannel = channel
		gotChatID = chatID
		gotMessageID = messageID
		return nil
	})

	ctx := WithToolInboundContext(context.Background(), "telegram", "chat-1", "msg-100", "")
	result := tool.Execute(ctx, map[string]any{})
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.ForLLM)
	}
	if gotChannel != "telegram" || gotChatID != "chat-1" || gotMessageID != "msg-100" {
		t.Fatalf("unexpected callback args: channel=%q chatID=%q messageID=%q", gotChannel, gotChatID, gotMessageID)
	}
}

func TestReactionTool_Execute_AllowsExplicitMessageIDOverride(t *testing.T) {
	tool := NewReactionTool()

	var gotMessageID string
	tool.SetReactionCallback(func(ctx context.Context, channel, chatID, messageID string) error {
		gotMessageID = messageID
		return nil
	})

	ctx := WithToolInboundContext(context.Background(), "telegram", "chat-1", "msg-context", "")
	result := tool.Execute(ctx, map[string]any{"message_id": "msg-explicit"})
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.ForLLM)
	}
	if gotMessageID != "msg-explicit" {
		t.Fatalf("expected explicit message id, got %q", gotMessageID)
	}
}

func TestReactionTool_Execute_TargetsDeliveredRefFromMessageTool(t *testing.T) {
	const deliveredRef = "12345:9:67"

	messageTool := NewMessageTool()
	messageTool.SetSendCallback(func(
		ctx context.Context,
		channel, chatID, content, replyToMessageID string,
		mediaParts []bus.MediaPart,
	) ([]string, error) {
		return []string{deliveredRef}, nil
	})
	messageResult := messageTool.Execute(
		WithToolContext(context.Background(), "telegram", "12345"),
		map[string]any{"content": "hello", "wait_delivery": true},
	)
	if messageResult.IsError {
		t.Fatalf("message tool failed: %s", messageResult.ForLLM)
	}
	if !strings.Contains(messageResult.ForLLM, deliveredRef) {
		t.Fatalf("message tool result did not expose delivered ref %q: %q", deliveredRef, messageResult.ForLLM)
	}

	reactionTool := NewReactionTool()
	var gotMessageID string
	reactionTool.SetReactionCallback(func(ctx context.Context, channel, chatID, messageID string) error {
		gotMessageID = messageID
		return nil
	})
	reactionResult := reactionTool.Execute(
		WithToolContext(context.Background(), "telegram", "12345"),
		map[string]any{"message_id": deliveredRef},
	)
	if reactionResult.IsError {
		t.Fatalf("reaction tool failed: %s", reactionResult.ForLLM)
	}
	if gotMessageID != deliveredRef {
		t.Fatalf("reaction targeted %q, want delivered ref %q", gotMessageID, deliveredRef)
	}
}

func TestReactionTool_Execute_MissingMessageID(t *testing.T) {
	tool := NewReactionTool()
	tool.SetReactionCallback(func(ctx context.Context, channel, chatID, messageID string) error { return nil })

	ctx := WithToolContext(context.Background(), "telegram", "chat-1")
	result := tool.Execute(ctx, map[string]any{})
	if !result.IsError {
		t.Fatal("expected error")
	}
	if result.ForLLM != "message_id is required" {
		t.Fatalf("unexpected error message: %q", result.ForLLM)
	}
}

func TestReactionTool_Execute_CallbackError(t *testing.T) {
	tool := NewReactionTool()
	tool.SetReactionCallback(func(ctx context.Context, channel, chatID, messageID string) error {
		return errors.New("unsupported")
	})

	ctx := WithToolInboundContext(context.Background(), "telegram", "chat-1", "msg-100", "")
	result := tool.Execute(ctx, map[string]any{})
	if !result.IsError {
		t.Fatal("expected error")
	}
	if result.Err == nil {
		t.Fatal("expected wrapped error")
	}
}

func TestReactionTool_Parameters(t *testing.T) {
	tool := NewReactionTool()
	params := tool.Parameters()

	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatal("expected properties map")
	}
	if _, ok := props["message_id"]; !ok {
		t.Fatal("expected message_id parameter")
	}
	messageIDProp := props["message_id"].(map[string]any)
	messageIDDesc, _ := messageIDProp["description"].(string)
	for _, want := range []string{"chat_id:msg_id", "chat_id:topic_id:msg_id", "room_id event_id"} {
		if !strings.Contains(messageIDDesc, want) {
			t.Errorf("message_id description missing %q: %q", want, messageIDDesc)
		}
	}
	if _, ok := props["channel"]; !ok {
		t.Fatal("expected channel parameter")
	}
	if _, ok := props["chat_id"]; !ok {
		t.Fatal("expected chat_id parameter")
	}
}
