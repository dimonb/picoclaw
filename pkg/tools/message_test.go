package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
)

func TestMessageTool_Execute_SendSuccess(t *testing.T) {
	tool := NewMessageTool()

	var sent bus.OutboundMessage
	tool.SetSendCallback(func(msg bus.OutboundMessage) error {
		sent = msg
		return nil
	})

	ctx := WithToolContext(context.Background(), "test-channel", "test-chat-id")
	result := tool.Execute(ctx, map[string]any{"content": "Hello, world!"})

	if sent.Channel != "test-channel" || sent.ChatID != "test-chat-id" || sent.Content != "Hello, world!" {
		t.Fatalf("unexpected outbound message: %#v", sent)
	}
	if !result.Silent || result.IsError {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestMessageTool_Execute_SendReply(t *testing.T) {
	tool := NewMessageTool()
	var sent bus.OutboundMessage
	tool.SetSendCallback(func(msg bus.OutboundMessage) error {
		sent = msg
		return nil
	})

	ctx := WithToolContext(context.Background(), "telegram", "chat-1")
	result := tool.Execute(ctx, map[string]any{"content": "reply", "reply_to": "123"})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
	if sent.ReplyToMessageID != "123" {
		t.Fatalf("reply_to_message_id = %q", sent.ReplyToMessageID)
	}
}

func TestMessageTool_Execute_EditSuccess(t *testing.T) {
	tool := NewMessageTool()
	called := false
	tool.SetEditCallback(func(_ context.Context, channel, chatID, messageID, content string) error {
		called = true
		if channel != "telegram" || chatID != "chat-1" || messageID != "321" || content != "updated" {
			t.Fatalf("unexpected edit args: %q %q %q %q", channel, chatID, messageID, content)
		}
		return nil
	})

	ctx := WithToolContext(context.Background(), "telegram", "chat-1")
	result := tool.Execute(ctx, map[string]any{"content": "updated", "edit_message_id": "321"})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
	if !called {
		t.Fatal("expected edit callback")
	}
}

func TestMessageTool_Execute_SendFailure(t *testing.T) {
	tool := NewMessageTool()
	sendErr := errors.New("network error")
	tool.SetSendCallback(func(msg bus.OutboundMessage) error { return sendErr })

	ctx := WithToolContext(context.Background(), "test-channel", "test-chat-id")
	result := tool.Execute(ctx, map[string]any{"content": "Test message"})
	if !result.IsError || result.Err != sendErr {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestMessageTool_Execute_EditFailure(t *testing.T) {
	tool := NewMessageTool()
	editErr := errors.New("edit error")
	tool.SetEditCallback(func(_ context.Context, _, _, _, _ string) error { return editErr })

	ctx := WithToolContext(context.Background(), "test-channel", "test-chat-id")
	result := tool.Execute(ctx, map[string]any{"content": "Test message", "edit_message_id": "123"})
	if !result.IsError || result.Err != editErr {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestMessageTool_Execute_MissingContent(t *testing.T) {
	tool := NewMessageTool()
	ctx := WithToolContext(context.Background(), "test-channel", "test-chat-id")
	result := tool.Execute(ctx, map[string]any{})
	if !result.IsError || result.ForLLM != "content is required" {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestMessageTool_Execute_NoTargetChannel(t *testing.T) {
	tool := NewMessageTool()
	tool.SetSendCallback(func(msg bus.OutboundMessage) error { return nil })
	result := tool.Execute(context.Background(), map[string]any{"content": "Test message"})
	if !result.IsError || result.ForLLM != "No target channel/chat specified" {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestMessageTool_Execute_NotConfigured(t *testing.T) {
	tool := NewMessageTool()
	ctx := WithToolContext(context.Background(), "test-channel", "test-chat-id")
	result := tool.Execute(ctx, map[string]any{"content": "Test message"})
	if !result.IsError || result.ForLLM != "Message sending not configured" {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestMessageTool_Execute_RejectsMixedReplyAndEdit(t *testing.T) {
	tool := NewMessageTool()
	ctx := WithToolContext(context.Background(), "test-channel", "test-chat-id")
	result := tool.Execute(ctx, map[string]any{"content": "Test message", "reply_to": "1", "edit_message_id": "2"})
	if !result.IsError {
		t.Fatal("expected error")
	}
}

func TestMessageTool_Execute_WaitDelivery(t *testing.T) {
	tool := NewMessageTool()

	tool.SetSendCallback(func(msg bus.OutboundMessage) error {
		if msg.OnDelivered != nil {
			go msg.OnDelivered([]string{"platform-msg-42"})
		}
		return nil
	})

	ctx := WithToolContext(context.Background(), "telegram", "chat-1")
	result := tool.Execute(ctx, map[string]any{"content": "hello", "wait_delivery": true})

	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "platform-msg-42") {
		t.Fatalf("expected message_id in result, got: %s", result.ForLLM)
	}
}

func TestMessageTool_Execute_WaitDelivery_NoID(t *testing.T) {
	tool := NewMessageTool()

	tool.SetSendCallback(func(msg bus.OutboundMessage) error {
		if msg.OnDelivered != nil {
			go msg.OnDelivered([]string{})
		}
		return nil
	})

	ctx := WithToolContext(context.Background(), "telegram", "chat-1")
	result := tool.Execute(ctx, map[string]any{"content": "hello", "wait_delivery": true})

	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "no message_id") {
		t.Fatalf("expected 'no message_id' in result, got: %s", result.ForLLM)
	}
}
