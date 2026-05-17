// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"context"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/providers"
)

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
		ts.setPhase(TurnPhaseCompleted)
		return turnResult{
			finalContent: finalContent,
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
			ReasoningContent: responseReasoningContent(exec.response),
		}
	}

	if ts.opts.EnableSummary {
		al.contextManager.Compact(
			turnCtx,
			&CompactRequest{
				SessionKey: ts.sessionKey,
				Reason:     ContextCompressReasonSummarize,
				Budget:     ts.agent.ContextWindow,
			},
		)
	}

	ts.setPhase(TurnPhaseCompleted)
	return turnResult{
		finalContent:     finalContent,
		status:           turnStatus,
		followUps:        append([]bus.InboundMessage(nil), ts.followUps...),
		assistantMessage: assistantMsg,
	}, nil
}
