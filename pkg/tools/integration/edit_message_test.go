package integrationtools

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestEditMessageTool_Execute_HappyPath(t *testing.T) {
	tool := NewEditMessageTool()

	var gotChannel, gotChatID, gotMessageID, gotContent string
	tool.SetEditCallback(func(ctx context.Context, channel, chatID, messageID, content string) error {
		gotChannel = channel
		gotChatID = chatID
		gotMessageID = messageID
		gotContent = content
		return nil
	})

	ctx := WithToolContext(context.Background(), "telegram", "chat-1")
	result := tool.Execute(ctx, map[string]any{
		"message_id": "12345:67",
		"content":    "updated text",
	})
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.ForLLM)
	}
	if !result.Silent {
		t.Fatal("expected silent result")
	}
	if gotChannel != "telegram" || gotChatID != "chat-1" ||
		gotMessageID != "12345:67" || gotContent != "updated text" {
		t.Fatalf("unexpected callback args: channel=%q chatID=%q messageID=%q content=%q",
			gotChannel, gotChatID, gotMessageID, gotContent)
	}
}

func TestEditMessageTool_Execute_ExplicitChannelChatOverridesContext(t *testing.T) {
	tool := NewEditMessageTool()

	var gotChannel, gotChatID string
	tool.SetEditCallback(func(ctx context.Context, channel, chatID, messageID, content string) error {
		gotChannel = channel
		gotChatID = chatID
		return nil
	})

	ctx := WithToolContext(context.Background(), "telegram", "chat-1")
	result := tool.Execute(ctx, map[string]any{
		"message_id": "12345:67",
		"content":    "hi",
		"channel":    "discord",
		"chat_id":    "chat-9",
	})
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.ForLLM)
	}
	if gotChannel != "discord" || gotChatID != "chat-9" {
		t.Fatalf("expected explicit channel/chat, got %q/%q", gotChannel, gotChatID)
	}
}

func TestEditMessageTool_Execute_MissingMessageID(t *testing.T) {
	tool := NewEditMessageTool()
	tool.SetEditCallback(func(ctx context.Context, channel, chatID, messageID, content string) error {
		return nil
	})

	ctx := WithToolContext(context.Background(), "telegram", "chat-1")
	result := tool.Execute(ctx, map[string]any{"content": "hi"})
	if !result.IsError {
		t.Fatal("expected error")
	}
	if result.ForLLM != "message_id is required" {
		t.Fatalf("unexpected error message: %q", result.ForLLM)
	}
}

func TestEditMessageTool_Execute_MissingContent(t *testing.T) {
	tool := NewEditMessageTool()
	tool.SetEditCallback(func(ctx context.Context, channel, chatID, messageID, content string) error {
		return nil
	})

	ctx := WithToolContext(context.Background(), "telegram", "chat-1")
	result := tool.Execute(ctx, map[string]any{"message_id": "12345:67"})
	if !result.IsError {
		t.Fatal("expected error")
	}
	if result.ForLLM != "content is required" {
		t.Fatalf("unexpected error message: %q", result.ForLLM)
	}
}

func TestEditMessageTool_Execute_WhitespaceContent(t *testing.T) {
	tool := NewEditMessageTool()
	tool.SetEditCallback(func(ctx context.Context, channel, chatID, messageID, content string) error {
		return nil
	})

	ctx := WithToolContext(context.Background(), "telegram", "chat-1")
	result := tool.Execute(ctx, map[string]any{"message_id": "12345:67", "content": "   "})
	if !result.IsError {
		t.Fatal("expected error for whitespace-only content")
	}
}

func TestEditMessageTool_Execute_NoChannelOrChatID(t *testing.T) {
	tool := NewEditMessageTool()
	tool.SetEditCallback(func(ctx context.Context, channel, chatID, messageID, content string) error {
		return nil
	})

	result := tool.Execute(context.Background(), map[string]any{
		"message_id": "12345:67",
		"content":    "hi",
	})
	if !result.IsError {
		t.Fatal("expected error when channel/chat unspecified")
	}
}

func TestEditMessageTool_Execute_NilCallback(t *testing.T) {
	tool := NewEditMessageTool()

	ctx := WithToolContext(context.Background(), "telegram", "chat-1")
	result := tool.Execute(ctx, map[string]any{
		"message_id": "12345:67",
		"content":    "hi",
	})
	if !result.IsError {
		t.Fatal("expected error when callback not configured")
	}
	if result.ForLLM != "Message edit not configured" {
		t.Fatalf("unexpected error message: %q", result.ForLLM)
	}
}

func TestEditMessageTool_Execute_CallbackError(t *testing.T) {
	tool := NewEditMessageTool()
	wantErr := errors.New("channel does not support editing")
	tool.SetEditCallback(func(ctx context.Context, channel, chatID, messageID, content string) error {
		return wantErr
	})

	ctx := WithToolContext(context.Background(), "telegram", "chat-1")
	result := tool.Execute(ctx, map[string]any{
		"message_id": "12345:67",
		"content":    "hi",
	})
	if !result.IsError {
		t.Fatal("expected error")
	}
	if !errors.Is(result.Err, wantErr) {
		t.Fatalf("expected wrapped err to match, got %v", result.Err)
	}
}

func TestEditMessageTool_Parameters(t *testing.T) {
	tool := NewEditMessageTool()
	params := tool.Parameters()

	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatal("expected properties map")
	}
	for _, key := range []string{"message_id", "content", "channel", "chat_id"} {
		if _, ok := props[key]; !ok {
			t.Fatalf("expected %s parameter", key)
		}
	}
	messageIDProp := props["message_id"].(map[string]any)
	messageIDDesc, _ := messageIDProp["description"].(string)
	for _, want := range []string{"chat_id:msg_id", "chat_id:topic_id:msg_id", "room_id event_id"} {
		if !strings.Contains(messageIDDesc, want) {
			t.Errorf("message_id description missing %q: %q", want, messageIDDesc)
		}
	}
	required, _ := params["required"].([]string)
	if !contains(required, "message_id") || !contains(required, "content") {
		t.Errorf("required missing message_id/content: %v", required)
	}
}
