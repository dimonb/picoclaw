package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
)

type SendCallback func(msg bus.OutboundMessage) error

type EditCallback func(ctx context.Context, channel, chatID, messageID, content string) error

type MessageTool struct {
	sendCallback SendCallback
	editCallback EditCallback
}

func NewMessageTool() *MessageTool {
	return &MessageTool{}
}

func (t *MessageTool) Name() string {
	return "message"
}

func (t *MessageTool) Description() string {
	return "Send a new message or edit an existing one. Use explicit platform message IDs for reply_to and edit_message_id."
}

func (t *MessageTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"content": map[string]any{
				"type":        "string",
				"description": "The message text to send or edit",
			},
			"channel": map[string]any{
				"type":        "string",
				"description": "Optional: target channel (telegram, whatsapp, etc.)",
			},
			"chat_id": map[string]any{
				"type":        "string",
				"description": "Optional: target chat/user ID",
			},
			"reply_to": map[string]any{
				"type":        "string",
				"description": "Optional platform message ID to reply to when sending a new message",
			},
			"edit_message_id": map[string]any{
				"type":        "string",
				"description": "Optional platform message ID to edit instead of sending a new message",
			},
			"wait_delivery": map[string]any{
				"type":        "boolean",
				"description": "Wait for delivery confirmation and return the platform message_id in the result. Use when you need the message ID for future edits.",
			},
		},
		"required": []string{"content"},
	}
}

func (t *MessageTool) SetSendCallback(callback SendCallback) {
	t.sendCallback = callback
}

func (t *MessageTool) SetEditCallback(callback EditCallback) {
	t.editCallback = callback
}

func (t *MessageTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	content, ok := args["content"].(string)
	if !ok {
		return &ToolResult{ForLLM: "content is required", IsError: true}
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return &ToolResult{ForLLM: "content is required", IsError: true}
	}

	channel, _ := args["channel"].(string)
	chatID, _ := args["chat_id"].(string)
	if channel == "" {
		channel = ToolChannel(ctx)
	}
	if chatID == "" {
		chatID = ToolChatID(ctx)
	}
	channel = strings.TrimSpace(channel)
	chatID = strings.TrimSpace(chatID)
	if channel == "" || chatID == "" {
		return &ToolResult{ForLLM: "No target channel/chat specified", IsError: true}
	}

	replyTo, _ := args["reply_to"].(string)
	replyTo = strings.TrimSpace(replyTo)
	editMessageID, _ := args["edit_message_id"].(string)
	editMessageID = strings.TrimSpace(editMessageID)
	if replyTo != "" && editMessageID != "" {
		return &ToolResult{ForLLM: "reply_to and edit_message_id cannot be used together", IsError: true}
	}

	if editMessageID != "" {
		if t.editCallback == nil {
			return &ToolResult{ForLLM: "Message editing not configured", IsError: true}
		}
		if err := t.editCallback(ctx, channel, chatID, editMessageID, content); err != nil {
			return &ToolResult{ForLLM: fmt.Sprintf("editing message: %v", err), IsError: true, Err: err}
		}
		MarkRoundSent(ctx)
		return SilentResult(fmt.Sprintf("Message edited in %s:%s (%s)", channel, chatID, editMessageID))
	}

	if t.sendCallback == nil {
		return &ToolResult{ForLLM: "Message sending not configured", IsError: true}
	}

	waitDelivery, _ := args["wait_delivery"].(bool)

	msg := bus.OutboundMessage{
		Channel:          channel,
		ChatID:           chatID,
		Content:          content,
		ReplyToMessageID: replyTo,
	}

	if waitDelivery {
		delivered := make(chan []string, 1)
		msg.OnDelivered = func(ids []string) {
			select {
			case delivered <- ids:
			default:
			}
		}
		if err := t.sendCallback(msg); err != nil {
			return &ToolResult{ForLLM: fmt.Sprintf("sending message: %v", err), IsError: true, Err: err}
		}
		select {
		case ids := <-delivered:
			if len(ids) > 0 {
				return SilentResult(fmt.Sprintf("Message sent to %s:%s, message_id: %s", channel, chatID, ids[0]))
			}
			return SilentResult(fmt.Sprintf("Message sent to %s:%s (no message_id returned)", channel, chatID))
		case <-time.After(30 * time.Second):
			return SilentResult(fmt.Sprintf("Message sent to %s:%s (delivery confirmation timeout)", channel, chatID))
		case <-ctx.Done():
			return SilentResult(fmt.Sprintf("Message sent to %s:%s (context cancelled before delivery)", channel, chatID))
		}
	}

	if err := t.sendCallback(msg); err != nil {
		return &ToolResult{ForLLM: fmt.Sprintf("sending message: %v", err), IsError: true, Err: err}
	}

	status := fmt.Sprintf("Message sent to %s:%s", channel, chatID)
	if replyTo != "" {
		status = fmt.Sprintf("%s in reply to %s", status, replyTo)
		MarkRoundSent(ctx)
	}
	return SilentResult(status)
}
