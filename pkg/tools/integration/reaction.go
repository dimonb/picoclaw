package integrationtools

import (
	"context"
	"fmt"
	"strings"

	"github.com/sipeed/picoclaw/pkg/channels"
)

// ReactionCallback sets or removes an emoji reaction on a target message.
type ReactionCallback func(ctx context.Context, channel, chatID, messageID, emoji string) error

// ReactionSupportFunc reports the channel's emoji policy so the tool can
// validate against an allow-list before calling the channel.
type ReactionSupportFunc func(ctx context.Context, channel, chatID string) channels.ReactionSupport

type ReactionTool struct {
	reactCallback  ReactionCallback
	removeCallback ReactionCallback
	supportFunc    ReactionSupportFunc
}

func NewReactionTool() *ReactionTool {
	return &ReactionTool{}
}

func (t *ReactionTool) Name() string {
	return "reaction"
}

func (t *ReactionTool) Description() string {
	return "Add or remove an emoji reaction on a specific message. Use explicit message IDs from context."
}

func (t *ReactionTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"emoji": map[string]any{
				"type":        "string",
				"description": "Emoji reaction to add (e.g. \"👍\", \"❤️\"). Required.",
			},
			"message_id": map[string]any{
				"type":        "string",
				"description": "Target message ID to react to. Format depends on channel — Telegram: \"chat_id:msg_id\" or \"chat_id:topic_id:msg_id\"; Matrix: \"room_id event_id\".",
			},
			"channel": map[string]any{
				"type":        "string",
				"description": "Optional target channel; defaults to current inbound channel.",
			},
			"chat_id": map[string]any{
				"type":        "string",
				"description": "Optional target chat ID; defaults to current inbound chat.",
			},
			"remove": map[string]any{
				"type":        "boolean",
				"description": "If true, remove the reaction instead of adding it. Defaults to false.",
			},
		},
		"required": []string{"emoji", "message_id"},
	}
}

func (t *ReactionTool) SetReactionCallback(callback ReactionCallback) {
	t.reactCallback = callback
}

func (t *ReactionTool) SetRemoveReactionCallback(callback ReactionCallback) {
	t.removeCallback = callback
}

func (t *ReactionTool) SetSupportFunc(fn ReactionSupportFunc) {
	t.supportFunc = fn
}

func (t *ReactionTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	emoji, _ := args["emoji"].(string)
	emoji = strings.TrimSpace(emoji)
	if emoji == "" {
		return &ToolResult{ForLLM: "emoji is required", IsError: true}
	}

	messageID, _ := args["message_id"].(string)
	messageID = strings.TrimSpace(messageID)

	channel, _ := args["channel"].(string)
	chatID, _ := args["chat_id"].(string)
	if strings.TrimSpace(channel) == "" {
		channel = ToolChannel(ctx)
	}
	if strings.TrimSpace(chatID) == "" {
		chatID = ToolChatID(ctx)
	}
	if messageID == "" {
		messageID = ToolMessageID(ctx)
	}
	channel = strings.TrimSpace(channel)
	chatID = strings.TrimSpace(chatID)
	if channel == "" || chatID == "" {
		return &ToolResult{ForLLM: "No target channel/chat specified", IsError: true}
	}
	if messageID == "" {
		return &ToolResult{ForLLM: "message_id is required", IsError: true}
	}

	remove, _ := args["remove"].(bool)

	if remove {
		if t.removeCallback == nil {
			return &ToolResult{ForLLM: "Reaction removal not configured", IsError: true}
		}
		if err := t.removeCallback(ctx, channel, chatID, messageID, emoji); err != nil {
			return &ToolResult{
				ForLLM:  fmt.Sprintf("removing reaction: %v", err),
				IsError: true,
				Err:     err,
			}
		}
		return &ToolResult{
			ForLLM: fmt.Sprintf("Reaction %s removed from %s:%s message %s", emoji, channel, chatID, messageID),
			Silent: true,
		}
	}

	if t.reactCallback == nil {
		return &ToolResult{ForLLM: "Reaction not configured", IsError: true}
	}

	if t.supportFunc != nil {
		support := t.supportFunc(ctx, channel, chatID)
		if !support.AnyUnicode && len(support.Allowed) > 0 {
			allowed := false
			for _, candidate := range support.Allowed {
				if strings.TrimSpace(candidate) == emoji {
					allowed = true
					break
				}
			}
			if !allowed {
				return &ToolResult{
					ForLLM: fmt.Sprintf(
						"emoji %q is not supported by channel; allowed: %s",
						emoji, strings.Join(support.Allowed, " "),
					),
					IsError: true,
				}
			}
		}
	}

	if err := t.reactCallback(ctx, channel, chatID, messageID, emoji); err != nil {
		return &ToolResult{
			ForLLM:  fmt.Sprintf("adding reaction: %v", err),
			IsError: true,
			Err:     err,
		}
	}

	return &ToolResult{
		ForLLM: fmt.Sprintf("Reaction %s added to %s:%s message %s", emoji, channel, chatID, messageID),
		Silent: true,
	}
}
