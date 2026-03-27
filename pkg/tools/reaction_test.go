package tools

import (
	"context"
	"testing"

	"github.com/sipeed/picoclaw/pkg/channels"
)

func TestReactionTool_Execute_Success(t *testing.T) {
	tool := NewReactionTool()

	var gotChannel, gotChatID, gotMessageID, gotEmoji string
	tool.SetReactionCallback(func(_ context.Context, channel, chatID, messageID, emoji string) error {
		gotChannel = channel
		gotChatID = chatID
		gotMessageID = messageID
		gotEmoji = emoji
		return nil
	})
	tool.SetSupportFunc(func(_ context.Context, channel, chatID string) channels.ReactionSupport {
		return channels.ReactionSupport{Allowed: []string{"✅", "❤️"}}
	})

	ctx := WithToolContext(context.Background(), "telegram", "chat-1")
	result := tool.Execute(ctx, map[string]any{"emoji": "✅", "message_id": "123"})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
	if !result.Silent {
		t.Fatal("expected silent result")
	}
	if !result.UserVisibleSideEffect {
		t.Fatal("expected reaction success to mark user-visible side effect")
	}
	if gotChannel != "telegram" || gotChatID != "chat-1" || gotMessageID != "123" || gotEmoji != "✅" {
		t.Fatalf("unexpected callback args: %q %q %q %q", gotChannel, gotChatID, gotMessageID, gotEmoji)
	}
}

func TestReactionTool_Execute_RejectsUnsupportedEmoji(t *testing.T) {
	tool := NewReactionTool()
	tool.SetReactionCallback(func(_ context.Context, _, _, _, _ string) error { return nil })
	tool.SetSupportFunc(func(_ context.Context, _, _ string) channels.ReactionSupport {
		return channels.ReactionSupport{Allowed: []string{"✅"}}
	})

	ctx := WithToolContext(context.Background(), "telegram", "chat-1")
	result := tool.Execute(ctx, map[string]any{"emoji": "🔥", "message_id": "123"})
	if !result.IsError {
		t.Fatal("expected error")
	}
}
