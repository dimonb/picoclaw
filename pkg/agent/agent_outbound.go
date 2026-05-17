// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
	"github.com/sipeed/picoclaw/pkg/utils"
)

func (al *AgentLoop) maybePublishError(ctx context.Context, channel, chatID, sessionKey string, err error) error {
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return al.PublishResponseIfNeeded(
		ctx,
		channel,
		chatID,
		sessionKey,
		fmt.Sprintf("Error processing message: %v", err),
	)
}

func (al *AgentLoop) publishResponseOrError(
	ctx context.Context,
	channel, chatID, sessionKey string,
	response string,
	err error,
) error {
	if err != nil {
		if pubErr := al.maybePublishError(ctx, channel, chatID, sessionKey, err); pubErr != nil {
			return pubErr
		}
		return nil
	}
	return al.PublishResponseIfNeeded(ctx, channel, chatID, sessionKey, response)
}

func (al *AgentLoop) PublishResponseIfNeeded(ctx context.Context, channel, chatID, sessionKey, response string) error {
	if response == "" {
		return nil
	}

	alreadySentToSameChat := false
	defaultAgent := al.GetRegistry().GetDefaultAgent()
	if defaultAgent != nil {
		if tool, ok := defaultAgent.Tools.Get("message"); ok {
			if mt, ok := tool.(*tools.MessageTool); ok {
				alreadySentToSameChat = mt.HasSentTo(sessionKey, channel, chatID)
			}
		}
	}

	if alreadySentToSameChat {
		if al.channelManager != nil && channel != "" && chatID != "" {
			dismissCtx, dismissCancel := context.WithTimeout(ctx, 5*time.Second)
			al.channelManager.DismissToolFeedback(
				dismissCtx,
				channel,
				chatID,
				nil,
			)
			dismissCancel()
		}
		logger.DebugCF(
			"agent",
			"Skipped outbound (message tool already sent to same chat)",
			map[string]any{"channel": channel, "chat_id": chatID},
		)
		return nil
	}

	msg := bus.OutboundMessage{
		Context:    bus.NewOutboundContext(channel, chatID, ""),
		SessionKey: sessionKey,
		Content:    response,
	}
	if sessionKey != "" {
		msg.ContextUsage = computeContextUsage(al.agentForSession(sessionKey), sessionKey)
	}
	markFinalOutbound(&msg)

	// Synchronous send when the destination channel is registered with the
	// ChannelManager so we can capture the channel-native message ID and
	// stamp it onto the previously-persisted assistant row (deferred
	// delivery path used by processMessageSync / runTurnWithSteering).
	// Without a registered channel there is nothing to deliver to and no
	// ID to capture — fall back to fire-and-forget publish.
	if al.channelManager != nil {
		if _, ok := al.channelManager.GetChannel(channel); ok {
			res, err := al.PublishOutboundSync(ctx, msg)
			if err != nil {
				logger.WarnCF("agent", "Failed to publish outbound response",
					map[string]any{
						"channel": channel,
						"chat_id": chatID,
						"error":   err.Error(),
					})
				return err
			}
			if res.Err != nil {
				logger.WarnCF("agent", "Outbound response delivery error",
					map[string]any{
						"channel": channel,
						"chat_id": chatID,
						"error":   res.Err.Error(),
					})
				return res.Err
			}
			if len(res.IDs) > 0 && res.IDs[0] != "" && sessionKey != "" {
				al.stampPendingAssistantRef(ctx, sessionKey, res.IDs[0])
			}
			logger.InfoCF("agent", "Published outbound response",
				map[string]any{
					"channel":     channel,
					"chat_id":     chatID,
					"content_len": len(response),
				})
			return nil
		}
	}
	if err := al.bus.PublishOutbound(ctx, msg); err != nil {
		logger.WarnCF("agent", "Failed to publish outbound response",
			map[string]any{
				"channel": channel,
				"chat_id": chatID,
				"error":   err.Error(),
			})
		return err
	}
	logger.InfoCF("agent", "Published outbound response",
		map[string]any{
			"channel":     channel,
			"chat_id":     chatID,
			"content_len": len(response),
		})
	return nil
}

// stampPendingAssistantRef stamps the delivered channel-native ref onto the
// assistant row that the agent loop parked for this session in
// pendingAssistantStamps. No-op when nothing is pending (e.g. direct
// PublishResponseIfNeeded paths that don't go through a turn) or when the
// ContextManager is unavailable. Best-effort: stamp failure is logged.
func (al *AgentLoop) stampPendingAssistantRef(ctx context.Context, sessionKey, channelRef string) {
	v, ok := al.pendingAssistantStamps.LoadAndDelete(sessionKey)
	if !ok {
		return
	}
	rowID, _ := v.(int64)
	if rowID == 0 || al.contextManager == nil {
		return
	}
	stampCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := al.contextManager.UpdateChannelMessageID(stampCtx, sessionKey, rowID, channelRef); err != nil {
		logger.WarnCF("agent", "stamp assistant delivered ref failed", map[string]any{
			"session_key":        sessionKey,
			"message_id":         rowID,
			"channel_message_id": channelRef,
			"error":              err.Error(),
		})
	}
}

func (al *AgentLoop) targetReasoningChannelID(channelName string) (chatID string) {
	if al.channelManager == nil {
		return ""
	}
	if ch, ok := al.channelManager.GetChannel(channelName); ok {
		return ch.ReasoningChannelID()
	}
	return ""
}

func (al *AgentLoop) publishPicoReasoning(
	ctx context.Context,
	reasoningContent, chatID, sessionKey, modelName string,
) {
	if reasoningContent == "" || chatID == "" {
		return
	}

	if ctx.Err() != nil {
		return
	}

	pubCtx, pubCancel := context.WithTimeout(ctx, 5*time.Second)
	defer pubCancel()

	raw := map[string]string{metadataKeyMessageKind: messageKindThought}
	if trimmedModelName := strings.TrimSpace(modelName); trimmedModelName != "" {
		raw["model_name"] = trimmedModelName
	}

	if err := al.bus.PublishOutbound(pubCtx, bus.OutboundMessage{
		Context: bus.InboundContext{
			Channel: "pico",
			ChatID:  chatID,
			Raw:     raw,
		},
		SessionKey: sessionKey,
		Content:    reasoningContent,
	}); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
			errors.Is(err, bus.ErrBusClosed) {
			logger.DebugCF("agent", "Pico reasoning publish skipped (timeout/cancel)", map[string]any{
				"channel": "pico",
				"error":   err.Error(),
			})
		} else {
			logger.WarnCF("agent", "Failed to publish pico reasoning (best-effort)", map[string]any{
				"channel": "pico",
				"error":   err.Error(),
			})
		}
	}
}

func (al *AgentLoop) publishPicoToolCallInterim(
	ctx context.Context,
	ts *turnState,
	modelName string,
	reasoningContent string,
	content string,
	toolCalls []providers.ToolCall,
) {
	if ts == nil || ts.chatID == "" || al == nil || al.bus == nil {
		return
	}

	if strings.TrimSpace(reasoningContent) != "" {
		pubCtx, pubCancel := context.WithTimeout(ctx, 3*time.Second)
		err := al.bus.PublishOutbound(
			pubCtx,
			outboundMessageForTurnWithOptions(
				ts,
				reasoningContent,
				outboundTurnMessageOptions{
					kind:      messageKindThought,
					modelName: modelName,
				},
			),
		)
		pubCancel()
		if err != nil && !errors.Is(err, context.DeadlineExceeded) &&
			!errors.Is(err, context.Canceled) &&
			!errors.Is(err, bus.ErrBusClosed) {
			logger.WarnCF("agent", "Failed to publish pico reasoning", map[string]any{
				"channel": ts.channel,
				"chat_id": ts.chatID,
				"error":   err.Error(),
			})
		}
	}

	if !ts.opts.AllowInterimPicoPublish {
		return
	}

	visibleToolCalls := utils.BuildVisibleToolCalls(
		toolCalls,
		al.cfg.Agents.Defaults.GetToolFeedbackMaxArgsLength(),
	)
	duplicateToolCallContent := len(visibleToolCalls) > 0 &&
		utils.ToolCallExplanationDuplicatesContent(content, toolCalls)

	if strings.TrimSpace(content) != "" && !duplicateToolCallContent {
		pubCtx, pubCancel := context.WithTimeout(ctx, 3*time.Second)
		err := al.bus.PublishOutbound(
			pubCtx,
			outboundMessageForTurnWithOptions(ts, content, outboundTurnMessageOptions{
				modelName: modelName,
			}),
		)
		pubCancel()
		if err != nil && !errors.Is(err, context.DeadlineExceeded) &&
			!errors.Is(err, context.Canceled) &&
			!errors.Is(err, bus.ErrBusClosed) {
			logger.WarnCF("agent", "Failed to publish pico interim assistant content", map[string]any{
				"channel": ts.channel,
				"chat_id": ts.chatID,
				"error":   err.Error(),
			})
		}
	}

	if len(visibleToolCalls) == 0 {
		return
	}

	rawToolCalls, err := json.Marshal(visibleToolCalls)
	if err != nil {
		logger.WarnCF("agent", "Failed to serialize pico tool calls", map[string]any{
			"channel": ts.channel,
			"chat_id": ts.chatID,
			"error":   err.Error(),
		})
		return
	}

	msg := outboundMessageForTurnWithOptions(ts, "", outboundTurnMessageOptions{
		kind:      messageKindToolCalls,
		modelName: modelName,
		raw: map[string]string{
			metadataKeyToolCalls: string(rawToolCalls),
		},
	})

	pubCtx, pubCancel := context.WithTimeout(ctx, 3*time.Second)
	err = al.bus.PublishOutbound(pubCtx, msg)
	pubCancel()
	if err != nil && !errors.Is(err, context.DeadlineExceeded) &&
		!errors.Is(err, context.Canceled) &&
		!errors.Is(err, bus.ErrBusClosed) {
		logger.WarnCF("agent", "Failed to publish pico tool calls", map[string]any{
			"channel": ts.channel,
			"chat_id": ts.chatID,
			"error":   err.Error(),
		})
	}
}

func (al *AgentLoop) handleReasoning(
	ctx context.Context,
	reasoningContent, channelName, channelID string,
) {
	if reasoningContent == "" || channelName == "" || channelID == "" {
		return
	}

	// Check context cancellation before attempting to publish,
	// since PublishOutbound's select may race between send and ctx.Done().
	if ctx.Err() != nil {
		return
	}

	// Use a short timeout so the goroutine does not block indefinitely when
	// the outbound bus is full.  Reasoning output is best-effort; dropping it
	// is acceptable to avoid goroutine accumulation.
	pubCtx, pubCancel := context.WithTimeout(ctx, 5*time.Second)
	defer pubCancel()

	if err := al.bus.PublishOutbound(pubCtx, bus.OutboundMessage{
		Context: bus.NewOutboundContext(channelName, channelID, ""),
		Content: reasoningContent,
	}); err != nil {
		// Treat context.DeadlineExceeded / context.Canceled as expected
		// (bus full under load, or parent canceled).  Check the error
		// itself rather than ctx.Err(), because pubCtx may time out
		// (5 s) while the parent ctx is still active.
		// Also treat ErrBusClosed as expected — it occurs during normal
		// shutdown when the bus is closed before all goroutines finish.
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
			errors.Is(err, bus.ErrBusClosed) {
			logger.DebugCF("agent", "Reasoning publish skipped (timeout/cancel)", map[string]any{
				"channel": channelName,
				"error":   err.Error(),
			})
		} else {
			logger.WarnCF("agent", "Failed to publish reasoning (best-effort)", map[string]any{
				"channel": channelName,
				"error":   err.Error(),
			})
		}
	}
}

func (al *AgentLoop) PublishOutboundSync(ctx context.Context, msg bus.OutboundMessage) (bus.DeliveryResult, error) {
	if msg.Feedback == nil {
		msg.Feedback = make(chan bus.DeliveryResult, 1)
	}
	return awaitDelivery(ctx, msg.Feedback, al.bus.PublishOutbound(ctx, msg))
}

func (al *AgentLoop) PublishOutboundMediaSync(
	ctx context.Context,
	msg bus.OutboundMediaMessage,
) (bus.DeliveryResult, error) {
	if msg.Feedback == nil {
		msg.Feedback = make(chan bus.DeliveryResult, 1)
	}
	return awaitDelivery(ctx, msg.Feedback, al.bus.PublishOutboundMedia(ctx, msg))
}

// awaitDelivery is the shared blocking-on-feedback tail used by the
// PublishOutbound*Sync helpers. publishErr is the error returned by the
// publish call itself; when non-nil no feedback is expected. Otherwise we
// block on the feedback channel until a result arrives, the channel is
// closed (legacy failure signal), or ctx is canceled.
func awaitDelivery(
	ctx context.Context,
	feedback <-chan bus.DeliveryResult,
	publishErr error,
) (bus.DeliveryResult, error) {
	if publishErr != nil {
		return bus.DeliveryResult{}, publishErr
	}
	select {
	case res, ok := <-feedback:
		if !ok {
			return bus.DeliveryResult{}, fmt.Errorf("delivery failed: feedback channel closed")
		}
		return res, nil
	case <-ctx.Done():
		return bus.DeliveryResult{}, ctx.Err()
	}
}
