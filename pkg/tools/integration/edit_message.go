package integrationtools

import (
	"context"
	"fmt"
	"strings"
)

// EditMessageCallback edits an existing message on the given channel/chat.
type EditMessageCallback func(ctx context.Context, channel, chatID, messageID, content string) error

type EditMessageTool struct {
	editCallback EditMessageCallback
}

func NewEditMessageTool() *EditMessageTool {
	return &EditMessageTool{}
}

func (t *EditMessageTool) Name() string {
	return "edit_message"
}

func (t *EditMessageTool) Description() string {
	return "Edit a previously sent message by ID. Use to revise or correct content already delivered."
}

func (t *EditMessageTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"message_id": map[string]any{
				"type":        "string",
				"description": "Target message ID. Format depends on channel — Telegram: \"chat_id:msg_id\" or \"chat_id:topic_id:msg_id\"; Matrix: \"room_id event_id\".",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "New message content. Markdown is supported on channels that render it.",
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
		"required": []string{"message_id", "content"},
	}
}

func (t *EditMessageTool) SetEditCallback(cb EditMessageCallback) {
	t.editCallback = cb
}

func (t *EditMessageTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	messageID, _ := args["message_id"].(string)
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return &ToolResult{ForLLM: "message_id is required", IsError: true}
	}

	content, _ := args["content"].(string)
	if strings.TrimSpace(content) == "" {
		return &ToolResult{ForLLM: "content is required", IsError: true}
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
		return &ToolResult{ForLLM: "No target channel/chat specified", IsError: true}
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
