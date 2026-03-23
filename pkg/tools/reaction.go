package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/sipeed/picoclaw/pkg/channels"
)

type ReactionCallback func(ctx context.Context, channel, chatID, messageID, emoji string) error

type ReactionSupportFunc func(ctx context.Context, channel, chatID string) channels.ReactionSupport

type ReactionTool struct {
	reactCallback ReactionCallback
	supportFunc   ReactionSupportFunc
}

func NewReactionTool() *ReactionTool {
	return &ReactionTool{}
}

func (t *ReactionTool) Name() string {
	return "reaction"
}

func (t *ReactionTool) Description() string {
	return "Add an emoji reaction to a specific message. Use explicit message IDs from context."
}

func (t *ReactionTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"emoji": map[string]any{
				"type":        "string",
				"description": "Emoji reaction to add",
			},
			"message_id": map[string]any{
				"type":        "string",
				"description": "Target platform message ID to react to",
			},
			"channel": map[string]any{
				"type":        "string",
				"description": "Optional target channel; defaults to current tool context channel",
			},
			"chat_id": map[string]any{
				"type":        "string",
				"description": "Optional target chat ID; defaults to current tool context chat",
			},
		},
		"required": []string{"emoji", "message_id"},
	}
}

func (t *ReactionTool) SetReactionCallback(callback ReactionCallback) {
	t.reactCallback = callback
}

func (t *ReactionTool) SetSupportFunc(fn ReactionSupportFunc) {
	t.supportFunc = fn
}

func (t *ReactionTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	emoji, _ := args["emoji"].(string)
	emoji = strings.TrimSpace(emoji)
	if emoji == "" {
		return ErrorResult("emoji is required")
	}

	messageID, _ := args["message_id"].(string)
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return ErrorResult("message_id is required")
	}

	channel, _ := args["channel"].(string)
	chatID, _ := args["chat_id"].(string)
	if strings.TrimSpace(channel) == "" {
		channel = ToolChannel(ctx)
	}
	if strings.TrimSpace(chatID) == "" {
		chatID = ToolChatID(ctx)
	}
	channel = strings.TrimSpace(channel)
	chatID = strings.TrimSpace(chatID)
	if channel == "" || chatID == "" {
		return ErrorResult("reaction tool requires a target channel/chat")
	}
	if t.reactCallback == nil {
		return ErrorResult("reaction sending not configured")
	}

	support := channels.ReactionSupport{}
	if t.supportFunc != nil {
		support = t.supportFunc(ctx, channel, chatID)
	}
	if !support.AnyUnicode && len(support.Allowed) > 0 {
		allowed := false
		for _, candidate := range support.Allowed {
			if strings.TrimSpace(candidate) == emoji {
				allowed = true
				break
			}
		}
		if !allowed {
			return ErrorResult(fmt.Sprintf("emoji %q is not supported by channel; allowed: %s", emoji, strings.Join(support.Allowed, " ")))
		}
	}

	if err := t.reactCallback(ctx, channel, chatID, messageID, emoji); err != nil {
		return ErrorResult(fmt.Sprintf("adding reaction: %v", err)).WithError(err)
	}

	return SilentResult(fmt.Sprintf("Reaction %s added to %s:%s message %s", emoji, channel, chatID, messageID))
}
