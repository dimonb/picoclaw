package integrationtools

import (
	"context"
	"fmt"
	"strings"
)

// EditMessageCallback edits an existing message on the given channel/chat.
type EditMessageCallback func(ctx context.Context, channel, chatID, messageID, content string) error

// DeleteMessageCallback deletes an existing message on the given channel/chat.
type DeleteMessageCallback func(ctx context.Context, channel, chatID, messageID string) error

type EditMessageTool struct {
	editCallback   EditMessageCallback
	deleteCallback DeleteMessageCallback
}

func NewEditMessageTool() *EditMessageTool {
	return &EditMessageTool{}
}

func (t *EditMessageTool) Name() string {
	return "edit_message"
}

func (t *EditMessageTool) Description() string {
	return "Edit (or delete) a message you sent earlier, by ID. Your own delivered messages show their ID in the " +
		"msgs:# annotation in the conversation. Like a person in chat, when you made a mistake or a status " +
		"changed, you may fix or update your earlier message instead of sending a new one."
}

func (t *EditMessageTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"message_id": map[string]any{
				"type":        "string",
				"description": "Target message ID. Format depends on channel — Telegram: \"chat_id:msg_id\" or \"chat_id:topic_id:msg_id\"; Matrix: \"room_id event_id\"; Mattermost: \"channel_id:post_id\" or \"channel_id:root_id:post_id\".",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "New message content (required unless delete is true). Markdown is supported on channels that render it.",
			},
			"delete": map[string]any{
				"type":        "boolean",
				"description": "If true, delete the message instead of editing it. Only your own messages in the current chat can be deleted.",
			},
			"channel": map[string]any{
				"type":        "string",
				"description": "Optional target channel; defaults to current inbound channel.",
			},
			"chat_id": map[string]any{
				"type":        "string",
				"description": "Optional target chat ID; defaults to current inbound chat.",
			},
		},
		"required": []string{"message_id"},
	}
}

func (t *EditMessageTool) SetEditCallback(cb EditMessageCallback) {
	t.editCallback = cb
}

func (t *EditMessageTool) SetDeleteCallback(cb DeleteMessageCallback) {
	t.deleteCallback = cb
}

func (t *EditMessageTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	messageID, _ := args["message_id"].(string)
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return &ToolResult{ForLLM: "message_id is required", IsError: true}
	}

	remove, _ := args["delete"].(bool)
	content, _ := args["content"].(string)
	if !remove && strings.TrimSpace(content) == "" {
		return &ToolResult{ForLLM: "content is required", IsError: true}
	}

	channel, _ := args["channel"].(string)
	chatID, _ := args["chat_id"].(string)
	if remove {
		// Deleting is limited to the conversation being handled; the model
		// must not delete in another chat by passing channel/chat_id.
		curChannel, curChat := strings.TrimSpace(ToolChannel(ctx)), strings.TrimSpace(ToolChatID(ctx))
		if curChannel == "" || curChat == "" {
			return &ToolResult{ForLLM: "delete is only available while handling a chat conversation", IsError: true}
		}
		if (strings.TrimSpace(channel) != "" && strings.TrimSpace(channel) != curChannel) ||
			(strings.TrimSpace(chatID) != "" && strings.TrimSpace(chatID) != curChat) {
			return &ToolResult{ForLLM: "you can only delete your own messages in the current chat", IsError: true}
		}
		channel, chatID = curChannel, curChat
	}
	if strings.TrimSpace(channel) == "" {
		channel = ToolChannel(ctx)
	}
	if strings.TrimSpace(chatID) == "" {
		chatID = ToolChatID(ctx)
	}
	channel = strings.TrimSpace(channel)
	chatID = strings.TrimSpace(chatID)
	if channel == "" || chatID == "" {
		return &ToolResult{ForLLM: "No target channel/chat specified", IsError: true}
	}

	if remove {
		if t.deleteCallback == nil {
			return &ToolResult{ForLLM: "Message deletion not configured", IsError: true}
		}
		if err := t.deleteCallback(ctx, channel, chatID, messageID); err != nil {
			return &ToolResult{ForLLM: fmt.Sprintf("deleting message: %v", err), IsError: true, Err: err}
		}
		return &ToolResult{ForLLM: fmt.Sprintf("Message %s on %s:%s deleted", messageID, channel, chatID), Silent: true}
	}

	if t.editCallback == nil {
		return &ToolResult{ForLLM: "Message edit not configured", IsError: true}
	}

	if err := t.editCallback(ctx, channel, chatID, messageID, content); err != nil {
		return &ToolResult{
			ForLLM:  fmt.Sprintf("editing message: %v", err),
			IsError: true,
			Err:     err,
		}
	}

	return &ToolResult{
		ForLLM: fmt.Sprintf("Message %s on %s:%s edited", messageID, channel, chatID),
		Silent: true,
	}
}
