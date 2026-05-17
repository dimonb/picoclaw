package integrationtools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
)

func TestReactionTool_Execute_UsesContextMessageIDByDefault(t *testing.T) {
	tool := NewReactionTool()

	var gotChannel, gotChatID, gotMessageID, gotEmoji string
	tool.SetReactionCallback(func(ctx context.Context, channel, chatID, messageID, emoji string) error {
		gotChannel = channel
		gotChatID = chatID
		gotMessageID = messageID
		gotEmoji = emoji
		return nil
	})

	ctx := WithToolInboundContext(context.Background(), "telegram", "chat-1", "msg-100", "")
	result := tool.Execute(ctx, map[string]any{"emoji": "👍"})
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.ForLLM)
	}
	if gotChannel != "telegram" || gotChatID != "chat-1" || gotMessageID != "msg-100" || gotEmoji != "👍" {
		t.Fatalf("unexpected callback args: channel=%q chatID=%q messageID=%q emoji=%q",
			gotChannel, gotChatID, gotMessageID, gotEmoji)
	}
}

func TestReactionTool_Execute_AllowsExplicitMessageIDOverride(t *testing.T) {
	tool := NewReactionTool()

	var gotMessageID string
	tool.SetReactionCallback(func(ctx context.Context, channel, chatID, messageID, emoji string) error {
		gotMessageID = messageID
		return nil
	})

	ctx := WithToolInboundContext(context.Background(), "telegram", "chat-1", "msg-context", "")
	result := tool.Execute(ctx, map[string]any{"message_id": "msg-explicit", "emoji": "👍"})
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
	reactionTool.SetReactionCallback(func(ctx context.Context, channel, chatID, messageID, emoji string) error {
		gotMessageID = messageID
		return nil
	})
	reactionResult := reactionTool.Execute(
		WithToolContext(context.Background(), "telegram", "12345"),
		map[string]any{"message_id": deliveredRef, "emoji": "👍"},
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
	tool.SetReactionCallback(func(ctx context.Context, channel, chatID, messageID, emoji string) error {
		return nil
	})

	ctx := WithToolContext(context.Background(), "telegram", "chat-1")
	result := tool.Execute(ctx, map[string]any{"emoji": "👍"})
	if !result.IsError {
		t.Fatal("expected error")
	}
	if result.ForLLM != "message_id is required" {
		t.Fatalf("unexpected error message: %q", result.ForLLM)
	}
}

func TestReactionTool_Execute_MissingEmoji(t *testing.T) {
	tool := NewReactionTool()
	tool.SetReactionCallback(func(ctx context.Context, channel, chatID, messageID, emoji string) error {
		return nil
	})

	ctx := WithToolInboundContext(context.Background(), "telegram", "chat-1", "msg-1", "")
	result := tool.Execute(ctx, map[string]any{})
	if !result.IsError {
		t.Fatal("expected error")
	}
	if result.ForLLM != "emoji is required" {
		t.Fatalf("unexpected error message: %q", result.ForLLM)
	}
}

func TestReactionTool_Execute_CallbackError(t *testing.T) {
	tool := NewReactionTool()
	tool.SetReactionCallback(func(ctx context.Context, channel, chatID, messageID, emoji string) error {
		return errors.New("unsupported")
	})

	ctx := WithToolInboundContext(context.Background(), "telegram", "chat-1", "msg-100", "")
	result := tool.Execute(ctx, map[string]any{"emoji": "👍"})
	if !result.IsError {
		t.Fatal("expected error")
	}
	if result.Err == nil {
		t.Fatal("expected wrapped error")
	}
}

func TestReactionTool_Execute_RejectsEmojiNotInAllowList(t *testing.T) {
	tool := NewReactionTool()
	var called bool
	tool.SetReactionCallback(func(ctx context.Context, channel, chatID, messageID, emoji string) error {
		called = true
		return nil
	})
	tool.SetSupportFunc(func(ctx context.Context, channel, chatID string) channels.ReactionSupport {
		return channels.ReactionSupport{Allowed: []string{"👍", "❤️"}}
	})

	ctx := WithToolInboundContext(context.Background(), "telegram", "chat-1", "msg-1", "")
	result := tool.Execute(ctx, map[string]any{"emoji": "🌚"})
	if !result.IsError {
		t.Fatal("expected error for unsupported emoji")
	}
	if called {
		t.Fatal("callback should not be invoked for unsupported emoji")
	}
}

func TestReactionTool_Execute_RemoveCallsRemoveCallback(t *testing.T) {
	tool := NewReactionTool()
	var addCalled, removeCalled bool
	tool.SetReactionCallback(func(ctx context.Context, channel, chatID, messageID, emoji string) error {
		addCalled = true
		return nil
	})
	tool.SetRemoveReactionCallback(func(ctx context.Context, channel, chatID, messageID, emoji string) error {
		removeCalled = true
		return nil
	})

	ctx := WithToolInboundContext(context.Background(), "telegram", "chat-1", "msg-1", "")
	result := tool.Execute(ctx, map[string]any{"emoji": "👍", "remove": true})
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.ForLLM)
	}
	if addCalled {
		t.Fatal("add callback should not be invoked when remove=true")
	}
	if !removeCalled {
		t.Fatal("remove callback was not invoked")
	}
}

func TestReactionTool_Parameters(t *testing.T) {
	tool := NewReactionTool()
	params := tool.Parameters()

	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatal("expected properties map")
	}
	for _, key := range []string{"emoji", "message_id", "channel", "chat_id", "remove"} {
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
	if !contains(required, "emoji") || !contains(required, "message_id") {
		t.Errorf("required missing emoji/message_id: %v", required)
	}
}

func contains(slice []string, target string) bool {
	for _, s := range slice {
		if s == target {
			return true
		}
	}
	return false
}
