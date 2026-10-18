// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"context"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// backgroundCompactTimeout bounds the end-of-turn compaction goroutine. A leaf
// pass is one summarization call — tens of seconds on a large chat — so this is
// a backstop against a wedged provider, not a normal deadline.
const backgroundCompactTimeout = 5 * time.Minute

// Finalize handles turn finalization, either:
//   - Early return when allResponsesHandled=true (ExecuteTools already finalized)
//   - Normal finalization for allResponsesHandled=false (sets finalContent, persists assistant
//     message, runs Compact)
func (p *Pipeline) Finalize(
	ctx context.Context,
	turnCtx context.Context,
	ts *turnState,
	exec *turnExecution,
	turnStatus TurnEndStatus,
	finalContent string,
) (turnResult, error) {
	al := p.al

	// When allResponsesHandled=true, ExecuteTools already finalized
	// (added handledToolResponseSummary, saved session, set phase to Completed).
	if exec.allResponsesHandled {
		if ts.hardAbortRequested() {
			return al.abortTurn(ts)
		}
		if !ts.opts.NoHistory && exec.deferredCompactBudget > 0 {
			p.scheduleContextCompaction(turnCtx, &CompactRequest{
				SessionKey:    ts.sessionKey,
				Reason:        ContextCompressReasonSummarize,
				HistoryBudget: exec.deferredCompactBudget,
			})
		}
		ts.setPhase(TurnPhaseCompleted)
		return turnResult{
			finalContent: finalContent,
			modelName:    exec.llmModelName,
			status:       turnStatus,
			followUps:    append([]bus.InboundMessage(nil), ts.followUps...),
		}, nil
	}

	ts.setPhase(TurnPhaseFinalizing)
	ts.setFinalContent(finalContent)
	var assistantMsg *providers.Message
	if !ts.opts.NoHistory && finalContent != "" {
		assistantMsg = &providers.Message{
			Role:             "assistant",
			Content:          finalContent,
			ModelName:        exec.llmModelName,
			ReasoningContent: responseReasoningContent(exec.response),
		}
	}

	if !ts.opts.NoHistory && (ts.opts.EnableSummary || exec.deferredCompactBudget > 0) {
		// Off the turn: the summary this produces is for later turns, not this
		// one — the answer is already written. Run inline and the user waits out
		// a summarization LLM call (measured at 20-35s on a large chat) before
		// the reply is published a few lines below.
		//
		// Request fields are read here, on the turn goroutine, because ts and
		// exec keep being mutated after Finalize returns.
		req := &CompactRequest{
			SessionKey:    ts.sessionKey,
			Reason:        ContextCompressReasonSummarize,
			HistoryBudget: agentHistoryBudget(ts.agent, exec.providerToolDefs, ts.activeSkills),
		}
		if exec.deferredCompactBudget > 0 {
			req.HistoryBudget = exec.deferredCompactBudget
		}
		p.scheduleContextCompaction(turnCtx, req)
	}

	contextUsage := computeContextUsage(ts.agent, ts.sessionKey)
	streamErr := finalizeConfiguredStreamingLLM(turnCtx, ts, exec, finalContent, contextUsage)
	// If streaming never became visible, keep the legacy Pico interim publish path
	// so the final answer is still delivered outside normal SendResponse.
	if ((streamErr != nil && !isConfiguredStreamingVisibleError(streamErr)) || exec.streamingFallback) &&
		!ts.opts.SendResponse && ts.opts.AllowInterimPicoPublish && finalContent != "" {
		msg := outboundMessageForTurnWithOptions(ts, finalContent, outboundTurnMessageOptions{
			modelName: exec.llmModelName,
		})
		msg.ContextUsage = contextUsage
		markFinalOutbound(&msg)
		_ = al.bus.PublishOutbound(turnCtx, msg)
	}
	if streamErr != nil && isConfiguredStreamingVisibleError(streamErr) {
		ts.setPhase(TurnPhaseCompleted)
		return turnResult{
			finalContent: finalContent,
			status:       TurnEndStatusError,
			followUps:    append([]bus.InboundMessage(nil), ts.followUps...),
		}, streamErr
	}
	ts.setPhase(TurnPhaseCompleted)
	return turnResult{
		finalContent:     finalContent,
		modelName:        exec.llmModelName,
		status:           turnStatus,
		followUps:        append([]bus.InboundMessage(nil), ts.followUps...),
		assistantMessage: assistantMsg,
	}, nil
}

// scheduleContextCompaction runs existing memory maintenance independently of
// answer publication. req is a turn-local snapshot, never mutable turn state.
func (p *Pipeline) scheduleContextCompaction(turnCtx context.Context, req *CompactRequest) {
	// WithoutCancel keeps the logging and tracing values while dropping the
	// turn's cancellation, which fires as soon as the turn ends and would
	// kill this mid-summary. The timeout stops a hung provider call from
	// parking a goroutine forever.
	compactCtx, cancelCompact := context.WithTimeout(
		context.WithoutCancel(turnCtx),
		backgroundCompactTimeout,
	)
	go func() {
		defer cancelCompact()
		// A failure here is not fatal for the turn, but it is why a
		// conversation silently stops being compacted and grows until every
		// turn goes over budget — so it must not be swallowed. Later turns retry
		// maintenance; only an actually over-budget
		// request needs the bounded synchronous fallback.
		if err := p.ContextManager.Compact(compactCtx, req); err != nil {
			logger.WarnCF("agent", "End-of-turn compaction failed", map[string]any{
				"session_key": req.SessionKey,
				"error":       err.Error(),
			})
		}
	}()
}
