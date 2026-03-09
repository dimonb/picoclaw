package tools

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/sipeed/picoclaw/pkg/bus"
)

type SendCallback func(msg bus.OutboundMessage) error

const (
	replyModeChat    = "chat"
	replyModeCurrent = "current"
	replyModeParent  = "parent"
)

type MessageTool struct {
	sendCallback SendCallback
	sentInRound  atomic.Bool // Tracks whether a message was sent in the current processing round
}

func NewMessageTool() *MessageTool {
	return &MessageTool{}
}

func (t *MessageTool) Name() string {
	return "message"
}

func (t *MessageTool) Description() string {
	return "Send a message to the user on a chat channel. Use this when you want to communicate something or explicitly control reply threading."
}

func (t *MessageTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"content": map[string]any{
				"type":        "string",
				"description": "The message content to send",
			},
			"channel": map[string]any{
				"type":        "string",
				"description": "Optional: target channel (telegram, whatsapp, etc.)",
			},
			"chat_id": map[string]any{
				"type":        "string",
				"description": "Optional: target chat/user ID",
			},
			"reply_mode": map[string]any{
				"type":        "string",
				"enum":        []string{replyModeChat, replyModeCurrent, replyModeParent},
				"description": "Optional: threading mode. chat sends a normal message, current replies to the current inbound message, parent replies to the parent/replied-to inbound message.",
			},
			"reply_to_message_id": map[string]any{
				"type":        "string",
				"description": "Optional: explicit platform message ID to reply to. Overrides reply_mode when provided.",
			},
		},
		"required": []string{"content"},
	}
}

// ResetSentInRound resets the per-round send tracker.
// Called by the agent loop at the start of each inbound message processing round.
func (t *MessageTool) ResetSentInRound() {
	t.sentInRound.Store(false)
}

// HasSentInRound returns true if the message tool sent a message during the current round.
func (t *MessageTool) HasSentInRound() bool {
	return t.sentInRound.Load()
}

func (t *MessageTool) SetSendCallback(callback SendCallback) {
	t.sendCallback = callback
}

func (t *MessageTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	content, ok := args["content"].(string)
	if !ok {
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

	if channel == "" || chatID == "" {
		return &ToolResult{ForLLM: "No target channel/chat specified", IsError: true}
	}

	replyToMessageID, err := resolveReplyTarget(ctx, args)
	if err != nil {
		return &ToolResult{
			ForLLM:  err.Error(),
			IsError: true,
			Err:     err,
		}
	}

	if t.sendCallback == nil {
		return &ToolResult{ForLLM: "Message sending not configured", IsError: true}
	}

	msg := bus.OutboundMessage{
		Channel:          channel,
		ChatID:           chatID,
		Content:          content,
		ReplyToMessageID: replyToMessageID,
	}
	if err := t.sendCallback(msg); err != nil {
		return &ToolResult{
			ForLLM:  fmt.Sprintf("sending message: %v", err),
			IsError: true,
			Err:     err,
		}
	}

	t.sentInRound.Store(true)
	// Silent: user already received the message directly
	status := fmt.Sprintf("Message sent to %s:%s", channel, chatID)
	if replyToMessageID != "" {
		status = fmt.Sprintf("%s in reply to %s", status, replyToMessageID)
	}
	return &ToolResult{
		ForLLM: status,
		Silent: true,
	}
}

func resolveReplyTarget(ctx context.Context, args map[string]any) (string, error) {
	replyToMessageID, _ := args["reply_to_message_id"].(string)
	replyToMessageID = strings.TrimSpace(replyToMessageID)
	if replyToMessageID != "" {
		return replyToMessageID, nil
	}

	replyMode, _ := args["reply_mode"].(string)
	replyMode = strings.ToLower(strings.TrimSpace(replyMode))

	switch replyMode {
	case "", replyModeChat:
		return "", nil
	case replyModeCurrent:
		if id := strings.TrimSpace(ToolCurrentMessageID(ctx)); id != "" {
			return id, nil
		}
		return "", fmt.Errorf("reply_mode=current requested but current message id is unavailable")
	case replyModeParent:
		if id := strings.TrimSpace(ToolParentMessageID(ctx)); id != "" {
			return id, nil
		}
		return "", fmt.Errorf("reply_mode=parent requested but parent message id is unavailable")
	default:
		return "", fmt.Errorf("unsupported reply_mode %q", replyMode)
	}
}
