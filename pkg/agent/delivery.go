package agent

import (
	"context"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/logger"
)

func (al *AgentLoop) publishAgentResponseIfNeeded(
	ctx context.Context,
	response agentResponse,
	defaultChannel, defaultChatID string,
) {
	channel, chatID := resolveDeliveryTarget(response, defaultChannel, defaultChatID)

	if al.tryEditFinalResponse(ctx, response, channel, chatID) {
		return
	}
	if al.handleSuppressedFinalSend(ctx, response, channel, chatID) {
		return
	}
	if al.handleEmptyFinalResponse(ctx, response, channel, chatID) {
		return
	}

	outbound := response.outboundMessage(channel, chatID)
	al.attachFinalReaction(ctx, &outbound, response)
	al.bus.PublishOutbound(ctx, outbound)
	logger.InfoCF("agent", "Published outbound response",
		map[string]any{
			"channel":             outbound.Channel,
			"chat_id":             outbound.ChatID,
			"content_len":         len(response.Content),
			"reply_to_message_id": outbound.ReplyToMessageID,
			"edit_message_id":     response.EditMessageID,
		})
}

func resolveDeliveryTarget(response agentResponse, defaultChannel, defaultChatID string) (string, string) {
	channel := response.Channel
	if channel == "" {
		channel = defaultChannel
	}
	chatID := response.ChatID
	if chatID == "" {
		chatID = defaultChatID
	}
	return channel, chatID
}

func (al *AgentLoop) tryEditFinalResponse(ctx context.Context, response agentResponse, channel, chatID string) bool {
	if response.EditMessageID == "" {
		return false
	}
	if al.channelManager == nil {
		logger.WarnCF("agent", "Cannot edit final message without channel manager; falling back to send", map[string]any{"channel": channel, "chat_id": chatID})
		return false
	}
	if err := al.channelManager.EditMessage(ctx, channel, chatID, response.EditMessageID, response.Content); err != nil {
		logger.WarnCF("agent", "Failed to edit final message; falling back to send", map[string]any{"channel": channel, "chat_id": chatID, "message_id": response.EditMessageID, "error": err.Error()})
		return false
	}
	al.applyFinalReaction(ctx, channel, chatID, response.Reaction)
	al.cleanupFinalDeliveryState(ctx, channel, chatID)
	if response.OnDelivered != nil {
		response.OnDelivered([]string{response.EditMessageID})
	}
	return true
}

func (al *AgentLoop) handleSuppressedFinalSend(ctx context.Context, response agentResponse, channel, chatID string) bool {
	if !response.SkipFinalSend || response.EditMessageID != "" {
		return false
	}
	if response.Reaction != nil {
		al.applyFinalReaction(ctx, channel, chatID, response.Reaction)
		al.cleanupFinalDeliveryState(ctx, channel, chatID)
		logger.DebugCF("agent", "Final text send suppressed by <meta> after applying reaction", map[string]any{"channel": channel, "chat_id": chatID})
		return true
	}
	logger.DebugCF("agent", "Final text send suppressed by <meta>", map[string]any{"channel": channel, "chat_id": chatID})
	return true
}

func (al *AgentLoop) handleEmptyFinalResponse(ctx context.Context, response agentResponse, channel, chatID string) bool {
	if response.Content != "" {
		return false
	}
	if response.Reaction != nil {
		al.applyFinalReaction(ctx, channel, chatID, response.Reaction)
		al.cleanupFinalDeliveryState(ctx, channel, chatID)
		return true
	}
	logger.DebugCF("agent", "Empty final response with no reaction, nothing sent", map[string]any{"channel": channel, "chat_id": chatID})
	return true
}

func (al *AgentLoop) attachFinalReaction(ctx context.Context, outbound *bus.OutboundMessage, response agentResponse) {
	if response.Reaction == nil || al.channelManager == nil {
		return
	}
	orig := outbound.OnDelivered
	outbound.OnDelivered = func(msgIDs []string) {
		_ = al.channelManager.SetMessageReaction(ctx, outbound.Channel, outbound.ChatID, response.Reaction.MessageID, response.Reaction.Emoji)
		if orig != nil {
			orig(msgIDs)
		}
	}
}

func (al *AgentLoop) applyFinalReaction(ctx context.Context, channel, chatID string, reaction *finalReactionAction) {
	if reaction == nil || al.channelManager == nil {
		return
	}
	if err := al.channelManager.SetMessageReaction(ctx, channel, chatID, reaction.MessageID, reaction.Emoji); err != nil {
		logger.WarnCF("agent", "Failed to apply final reaction", map[string]any{"channel": channel, "chat_id": chatID, "message_id": reaction.MessageID, "error": err.Error()})
	}
}

func (al *AgentLoop) cleanupFinalDeliveryState(ctx context.Context, channel, chatID string) {
	if al.channelManager != nil && channel != "" && chatID != "" {
		al.channelManager.CleanupState(ctx, channel, chatID)
	}
}
