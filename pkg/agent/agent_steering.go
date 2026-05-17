// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"context"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/logger"
)

func (al *AgentLoop) processMessageSync(ctx context.Context, msg bus.InboundMessage) {
	if al.channelManager != nil {
		defer al.channelManager.InvokeTypingStop(msg.Channel, msg.ChatID)
	}

	response, err := al.processMessage(ctx, msg)
	if pubErr := al.publishResponseOrError(ctx, msg.Channel, msg.ChatID, msg.SessionKey, response, err); pubErr != nil {
		logger.ErrorCF("agent", "Failed to publish response in processMessageSync",
			map[string]any{
				"channel":     msg.Channel,
				"chat_id":     msg.ChatID,
				"session_key": msg.SessionKey,
				"error":       pubErr.Error(),
			})
	}
}

func (al *AgentLoop) runTurnWithSteering(ctx context.Context, initialMsg bus.InboundMessage) {
	// Process the initial message
	response, err := al.processMessage(ctx, initialMsg)
	if err != nil {
		if pubErr := al.maybePublishError(
			ctx,
			initialMsg.Channel,
			initialMsg.ChatID,
			initialMsg.SessionKey,
			err,
		); pubErr != nil {
			logger.ErrorCF("agent", "Failed to publish error in steering loop",
				map[string]any{
					"channel": initialMsg.Channel,
					"chat_id": initialMsg.ChatID,
					"error":   pubErr.Error(),
				})
			return
		}
		response = ""
	}
	finalResponse := response

	// Build continuation target
	target, targetErr := al.buildContinuationTarget(initialMsg)
	if targetErr != nil {
		logger.WarnCF("agent", "Failed to build steering continuation target",
			map[string]any{
				"channel": initialMsg.Channel,
				"error":   targetErr.Error(),
			})
		return
	}
	if target == nil {
		// System message or non-routable, response already published
		return
	}

	// Drain steering queue using existing Continue mechanism
	for al.pendingSteeringCountForScope(target.SessionKey) > 0 {
		// Check for context cancellation between iterations
		if ctx.Err() != nil {
			return
		}

		logger.InfoCF("agent", "Continuing queued steering after turn end",
			map[string]any{
				"channel":     target.Channel,
				"chat_id":     target.ChatID,
				"session_key": target.SessionKey,
				"queue_depth": al.pendingSteeringCountForScope(target.SessionKey),
			})

		continued, continueErr := al.Continue(ctx, target.SessionKey, target.Channel, target.ChatID)
		if continueErr != nil {
			logger.WarnCF("agent", "Failed to continue queued steering",
				map[string]any{
					"channel": target.Channel,
					"chat_id": target.ChatID,
					"error":   continueErr.Error(),
				})
			break
		}
		if continued == "" {
			break
		}
		finalResponse = continued
	}

	// Publish final response
	if finalResponse != "" {
		if err := al.PublishResponseIfNeeded(
			ctx,
			target.Channel,
			target.ChatID,
			target.SessionKey,
			finalResponse,
		); err != nil {
			logger.ErrorCF("agent", "Failed to publish final response in steering loop",
				map[string]any{
					"channel":     target.Channel,
					"chat_id":     target.ChatID,
					"session_key": target.SessionKey,
					"error":       err.Error(),
				})
		}
	}
}

func (al *AgentLoop) resolveSteeringTarget(msg bus.InboundMessage) (string, string, bool) {
	if msg.Channel == "system" {
		return "", "", false
	}

	route, agent, err := al.resolveMessageRoute(msg)
	if err != nil || agent == nil {
		return "", "", false
	}
	allocation := al.allocateRouteSession(route, msg)

	return resolveScopeKey(allocation.SessionKey, msg.SessionKey), agent.ID, true
}
