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
	return al.maybePublishErrorFor(ctx, nil, channel, chatID, sessionKey, err)
}

// maybePublishErrorFor is maybePublishError addressed to the message being
// answered (see publishFinalResponse).
func (al *AgentLoop) maybePublishErrorFor(
	ctx context.Context,
	inbound *bus.InboundContext,
	channel, chatID, sessionKey string,
	err error,
) error {
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return al.publishFinalResponse(
		ctx,
		inbound,
		channel,
		chatID,
		sessionKey,
		fmt.Sprintf("Error processing message: %v", err),
	)
}

func (al *AgentLoop) publishResponseOrError(
	ctx context.Context,
	inbound *bus.InboundContext,
	channel, chatID, sessionKey string,
	response string,
	err error,
) error {
	if err != nil {
		if pubErr := al.maybePublishErrorFor(ctx, inbound, channel, chatID, sessionKey, err); pubErr != nil {
			return pubErr
		}
		return nil
	}
	return al.publishFinalResponse(ctx, inbound, channel, chatID, sessionKey, response)
}

func (al *AgentLoop) PublishResponseIfNeeded(ctx context.Context, channel, chatID, sessionKey, response string) error {
	return al.publishFinalResponse(ctx, nil, channel, chatID, sessionKey, response)
}

// isDuplicateOfSent reports whether response repeats one of the texts the
// message tool sent to the same chat this round: equal after normalizing case,
// whitespace and trailing punctuation, or one containing the other with
// nearly the same length (formatting/wrapping differences).
func isDuplicateOfSent(response string, sent []string) bool {
	resp := normalizeForDuplicate(response)
	if resp == "" {
		return false
	}
	for _, s := range sent {
		other := normalizeForDuplicate(s)
		if other == "" {
			continue
		}
		if resp == other {
			return true
		}
		short, long := resp, other
		if len([]rune(short)) > len([]rune(long)) {
			short, long = long, short
		}
		if len([]rune(short)) >= 20 && strings.Contains(long, short) &&
			float64(len([]rune(short))) >= 0.9*float64(len([]rune(long))) {
			return true
		}
	}
	return false
}

func normalizeForDuplicate(s string) string {
	s = strings.ToLower(strings.Join(strings.Fields(s), " "))
	return strings.TrimRight(s, " .!?…,;:")
}

// finalOutboundContext addresses a final response. When the turn's inbound
// context belongs to the same chat it is attached, so channels can tell which
// message is being answered (Mattermost replies into that post's thread,
// Slack into its thread_ts via TopicID). ReplyToMessageID is cleared: the
// final answer is not a quote-reply to whatever the user was replying to,
// which keeps Telegram/Discord/Slack/VK reply behavior unchanged.
func finalOutboundContext(inbound *bus.InboundContext, channel, chatID string) bus.InboundContext {
	if inbound == nil || strings.TrimSpace(inbound.ChatID) != strings.TrimSpace(chatID) ||
		(inbound.Channel != "" && inbound.Channel != channel) {
		return bus.NewOutboundContext(channel, chatID, "")
	}
	outboundCtx := outboundContextFromInbound(inbound, channel, chatID, "")
	outboundCtx.ReplyToMessageID = ""
	return outboundCtx
}

// publishFinalResponse publishes a turn's final response. inbound (optional)
// is the context of the message being answered.
func (al *AgentLoop) publishFinalResponse(
	ctx context.Context,
	inbound *bus.InboundContext,
	channel, chatID, sessionKey, response string,
) error {
	// Trim, not just an empty check: a model told to stay silent unless it has
	// something to report answers with a space rather than an empty string, and
	// " " is not deliverable — Telegram rejects it as RICH_MESSAGE_EMPTY and
	// then as "text must be non-empty", which the channel reports as a
	// temporary failure and retries. Silence is a result, not a message.
	if strings.TrimSpace(response) == "" {
		return nil
	}
	if bus.IsSilentReply(response) {
		al.publishSilentReply(ctx, inbound, channel, chatID, sessionKey)
		return nil
	}

	// The message tool may already have delivered this answer during the
	// turn (models sometimes send it and then repeat it as the final). Only
	// such a duplicate is suppressed; a different final (e.g. "done" after
	// drafting through the message tool) is still delivered.
	duplicateOfSent := false
	defaultAgent := al.GetRegistry().GetDefaultAgent()
	if defaultAgent != nil {
		if tool, ok := defaultAgent.Tools.Get("message"); ok {
			if mt, ok := tool.(*tools.MessageTool); ok {
				duplicateOfSent = isDuplicateOfSent(response, mt.SentContentTo(sessionKey, channel, chatID))
			}
		}
	}

	if duplicateOfSent {
		logger.DebugCF(
			"agent",
			"Skipped outbound (duplicates what the message tool already sent to this chat)",
			map[string]any{"channel": channel, "chat_id": chatID},
		)
		if inbound != nil && al.channelManager != nil {
			// The agent delivered its answer itself (possibly somewhere else
			// than the default, e.g. top level instead of the thread). Treat
			// the final answer as silent so the channel clears what it
			// showed for this turn (placeholder, typing, tool feedback).
			al.publishSilentReply(ctx, inbound, channel, chatID, sessionKey)
			return nil
		}
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
		return nil
	}

	msg := bus.OutboundMessage{
		Context:    finalOutboundContext(inbound, channel, chatID),
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

// publishSilentReply handles a turn whose final answer is NO_REPLY: nothing
// is delivered. A content-less outbound marked bus.MessageKindSilent goes
// through the normal outbound queue (after anything the turn already queued)
// so the channel manager removes typing, reactions, placeholders and tool
// feedback shown for the turn. The assistant NO_REPLY stays in history.
func (al *AgentLoop) publishSilentReply(
	ctx context.Context,
	inbound *bus.InboundContext,
	channel, chatID, sessionKey string,
) {
	if channel == "" || chatID == "" {
		return
	}
	logger.InfoCF("agent", "Silent final reply: nothing more delivered",
		map[string]any{"channel": channel, "chat_id": chatID, "session_key": sessionKey})
	msg := bus.OutboundMessage{
		Channel:    channel,
		ChatID:     chatID,
		Context:    finalOutboundContext(inbound, channel, chatID),
		SessionKey: sessionKey,
	}
	if msg.Context.Raw == nil {
		msg.Context.Raw = make(map[string]string, 2)
	}
	msg.Context.Raw[metadataKeyMessageKind] = bus.MessageKindSilent
	markFinalOutbound(&msg)
	if al.channelManager != nil {
		if _, ok := al.channelManager.GetChannel(channel); ok {
			if _, err := al.PublishOutboundSync(ctx, msg); err != nil {
				logger.DebugCF("agent", "Silent reply cleanup not delivered",
					map[string]any{"channel": channel, "error": err.Error()})
			}
			return
		}
	}
	_ = al.bus.PublishOutbound(ctx, msg)
}
