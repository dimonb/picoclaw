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
	return "Edit (or delete) a message you sent earlier in the current chat, by ID. Your own delivered messages " +
		"show their ID in the msgs:# annotation in the conversation. Like a person in chat, when you made a " +
		"mistake or a status changed, you may fix or update your earlier message instead of sending a new one. " +
		"Only your own messages in the chat you are handling can be edited or deleted."
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
				"description": "If true, delete the message instead of editing it.",
			},
			"channel": map[string]any{
				"type":        "string",
				"description": "Optional; must be the current channel (messages in other chats cannot be edited or deleted).",
			},
			"chat_id": map[string]any{
				"type":        "string",
				"description": "Optional; must be the current chat (messages in other chats cannot be edited or deleted).",
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

	// Editing and deleting are limited to the conversation being handled:
	// the model must not reach into another chat by passing channel/chat_id
	// (or by a reference into another chat, which the Manager refuses as not
	// the bot's own message in this chat).
	action := "edit"
	if remove {
		action = "delete"
	}
	channel, _ := args["channel"].(string)
	chatID, _ := args["chat_id"].(string)
	channel, chatID = strings.TrimSpace(channel), strings.TrimSpace(chatID)
	curChannel, curChat := strings.TrimSpace(ToolChannel(ctx)), strings.TrimSpace(ToolChatID(ctx))
	if curChannel == "" || curChat == "" {
		return &ToolResult{
			ForLLM:  fmt.Sprintf("%s is only available while handling a chat conversation", action),
			IsError: true,
		}
	}
	if (channel != "" && channel != curChannel) || (chatID != "" && chatID != curChat) {
		return &ToolResult{
			ForLLM: fmt.Sprintf(
				"you can only %s your own messages in the current chat (%s:%s), not in %s:%s",
				action, curChannel, curChat, firstNonEmpty(channel, curChannel), firstNonEmpty(chatID, curChat),
			),
			IsError: true,
		}
	}
	channel, chatID = curChannel, curChat

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

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
