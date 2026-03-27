package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func formatMessageCapabilityNote(
	channel, chatID, messageID, replyToMessageID string,
	support channels.ReactionSupport,
) string {
	var parts []string
	if strings.TrimSpace(messageID) != "" {
		parts = append(parts, fmt.Sprintf("Current message ID: %s", strings.TrimSpace(messageID)))
	}
	if strings.TrimSpace(replyToMessageID) != "" {
		parts = append(parts, fmt.Sprintf("Parent message ID: %s", strings.TrimSpace(replyToMessageID)))
	}
	if channel != "" && chatID != "" {
		parts = append(
			parts,
			"Use explicit platform message IDs for message.reply_to, message.edit_message_id, reaction.message_id, and final <meta>{...}</meta> JSON. If you already sent the visible text with the message tool, set meta.send_final=false to suppress an extra final text message.",
		)
	}
	if support.AnyUnicode {
		parts = append(parts, "Current channel reactions: any Unicode emoji supported.")
	} else if len(support.Allowed) > 0 {
		parts = append(parts, fmt.Sprintf("Current channel reactions: %s", strings.Join(support.Allowed, " ")))
	}
	if len(parts) == 0 {
		return ""
	}
	return "## Current Message Capabilities\n" + strings.Join(parts, "\n")
}

func (al *AgentLoop) appendMessageCapabilityNote(
	ctx context.Context,
	messages []providers.Message,
	channel, chatID, messageID, replyToMessageID string,
) []providers.Message {
	if len(messages) == 0 || messages[0].Role != "system" {
		return messages
	}
	support := channels.ReactionSupport{}
	if al.channelManager != nil && channel != "" && chatID != "" {
		support = al.channelManager.GetReactionSupport(ctx, channel, chatID)
	}
	if note := formatMessageCapabilityNote(channel, chatID, messageID, replyToMessageID, support); note != "" {
		messages[0].Content += "\n\n" + note
		if len(messages[0].SystemParts) > 0 {
			messages[0].SystemParts = append(messages[0].SystemParts, providers.ContentBlock{Type: "text", Text: note})
		}
	}
	return messages
}
