// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"context"
	"strings"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// SetupTurn extracts the one-time initialization phase, returning a
// turnExecution populated with history, messages, and candidate selection.
// It replaces lines 56-145 of the original runTurn.
func (p *Pipeline) SetupTurn(ctx context.Context, ts *turnState) (*turnExecution, error) {
	cfg := p.Cfg
	maxMediaSize := cfg.Agents.Defaults.GetMaxMediaSize()

	contextualSkills := ts.activeSkills
	if ts.agent.ContextBuilder != nil {
		contextualSkills = ts.agent.ContextBuilder.ResolveActiveSkillsForContext(ts.activeSkills)
	}
	var toolDefs []providers.ToolDefinition
	if ts.agent.Tools != nil {
		toolDefs = filterToolsByTurnProfile(ts.agent.Tools.ToProviderDefs(), ts.profile)
	}

	var history []providers.Message
	var summary string
	var assembleEvicted bool
	if !ts.opts.NoHistory {
		if resp, err := p.ContextManager.Assemble(ctx, &AssembleRequest{
			SessionKey:    ts.sessionKey,
			HistoryBudget: agentHistoryBudget(ts.agent, toolDefs, contextualSkills),
		}); err == nil && resp != nil {
			history = resp.History
			summary = resp.Summary
			assembleEvicted = resp.Evicted
		}
	}
	ts.captureRestorePoint(history, summary)

	ts.recordSkillContextSnapshot(skillContextTriggerInitialBuild, contextualSkills)
	initialPromptReq := promptBuildRequestForTurn(ts, history, summary, ts.userMessage, ts.media, cfg)
	initialPromptReq.ActiveSkills = append([]string(nil), contextualSkills...)
	messages := ts.agent.ContextBuilder.BuildMessagesFromPrompt(initialPromptReq)
	currentTurnStart := len(messages)
	if strings.TrimSpace(ts.userMessage) != "" || len(ts.media) > 0 {
		currentTurnStart = len(messages) - 1
	}

	messages = resolveMediaRefs(messages, p.MediaStore, maxMediaSize, currentTurnStart)

	if !ts.opts.NoHistory {
		initialBudgetStats := estimateContextBudgetStats(ts.agent.ContextWindow, messages, toolDefs, ts.agent.MaxTokens)
		// assembleEvicted is a trigger in its own right. When the stored context
		// no longer fits, the manager drops its oldest items and hands back a
		// prompt that does fit — so OverBudget stays false and, without this,
		// compaction is never asked for. The conversation then loses its oldest
		// messages on every turn instead of summarizing them, forever.
		if initialBudgetStats.OverBudget || assembleEvicted {
			// The estimate that produced the assemble budget was too optimistic
			// (usually a summary-heavy or skill-heavy system prompt). Recompute
			// the budget from what the prompt actually costs, so compaction and
			// the next assemble aim at a number that really fits.
			measuredBudget := historyTokenBudget(
				ts.agent.ContextWindow,
				ts.agent.MaxTokens,
				initialBudgetStats.SystemTokens-estimateSummaryTokens(summary),
				initialBudgetStats.ToolTokens,
			)
			fields := contextBudgetStatsFields(initialBudgetStats)
			fields["session_key"] = ts.sessionKey
			fields["history_msgs"] = len(history)
			fields["summary_chars"] = len(summary)
			fields["history_budget"] = measuredBudget
			fields["assemble_evicted"] = assembleEvicted
			fields["trigger"] = "over_budget"
			if !initialBudgetStats.OverBudget {
				fields["trigger"] = "assemble_evicted"
			}
			logger.WarnCF("agent", "Proactive compression: context budget exceeded before LLM call", fields)
			if err := p.ContextManager.Compact(ctx, &CompactRequest{
				SessionKey:    ts.sessionKey,
				Reason:        ContextCompressReasonProactive,
				HistoryBudget: measuredBudget,
			}); err != nil {
				logger.WarnCF("agent", "Proactive compact failed", map[string]any{
					"session_key": ts.sessionKey,
					"error":       err.Error(),
				})
			}
			ts.refreshRestorePointFromSession(ts.agent)
			if resp, err := p.ContextManager.Assemble(ctx, &AssembleRequest{
				SessionKey:    ts.sessionKey,
				HistoryBudget: measuredBudget,
			}); err == nil && resp != nil {
				history = resp.History
				summary = resp.Summary
			}
			originalHistoryCount := len(history)
			reassembledPromptReq := promptBuildRequestForTurn(ts, history, summary, ts.userMessage, ts.media, cfg)
			reassembledPromptReq.ActiveSkills = append([]string(nil), contextualSkills...)
			reassembledMessages := ts.agent.ContextBuilder.BuildMessagesFromPrompt(reassembledPromptReq)
			reassembledCurrentTurnStart := len(reassembledMessages)
			if strings.TrimSpace(ts.userMessage) != "" || len(ts.media) > 0 {
				reassembledCurrentTurnStart = len(reassembledMessages) - 1
			}
			reassembledMessages = resolveMediaRefs(reassembledMessages, p.MediaStore, maxMediaSize, reassembledCurrentTurnStart)
			reassembledBudgetStats := estimateContextBudgetStats(ts.agent.ContextWindow, reassembledMessages, toolDefs, ts.agent.MaxTokens)
			reassembledFields := contextBudgetStatsFields(reassembledBudgetStats)
			reassembledFields["session_key"] = ts.sessionKey
			reassembledFields["history_msgs"] = len(history)
			reassembledFields["summary_chars"] = len(summary)
			logger.WarnCF("agent", "Context budget after proactive compact reassemble", reassembledFields)
			var fit bool
			history, messages, fit = trimHistoryToFitContextWindow(
				history,
				func(trimmedHistory []providers.Message) []providers.Message {
					rebuildPromptReq := promptBuildRequestForTurn(
						ts,
						trimmedHistory,
						summary,
						ts.userMessage,
						ts.media,
						cfg,
					)
					rebuildPromptReq.ActiveSkills = append([]string(nil), contextualSkills...)
					rebuilt := ts.agent.ContextBuilder.BuildMessagesFromPrompt(rebuildPromptReq)
					rebuiltCurrentTurnStart := len(rebuilt)
					if strings.TrimSpace(ts.userMessage) != "" || len(ts.media) > 0 {
						rebuiltCurrentTurnStart = len(rebuilt) - 1
					}
					return resolveMediaRefs(rebuilt, p.MediaStore, maxMediaSize, rebuiltCurrentTurnStart)
				},
				ts.agent.ContextWindow,
				toolDefs,
				ts.agent.MaxTokens,
			)
			finalBudgetStats := estimateContextBudgetStats(ts.agent.ContextWindow, messages, toolDefs, ts.agent.MaxTokens)
			if dropped := originalHistoryCount - len(history); dropped > 0 {
				fields := contextBudgetStatsFields(finalBudgetStats)
				fields["session_key"] = ts.sessionKey
				fields["dropped_msgs"] = dropped
				fields["remaining_msgs"] = len(history)
				fields["still_overlimit"] = !fit
				logger.WarnCF("agent", "Trimmed rebuilt history after proactive compaction", fields)
			} else if !fit {
				logger.WarnCF("agent", "Context still exceeds budget "+
					"after proactive compaction rebuild", map[string]any{
					"session_key":    ts.sessionKey,
					"history_msgs":   len(history),
					"context_window": ts.agent.ContextWindow,
					"max_tokens":     ts.agent.MaxTokens,
				})
			}
		}
	}

	if !ts.opts.NoHistory && (strings.TrimSpace(ts.userMessage) != "" || len(ts.media) > 0) {
		rootMsg := userPromptMessage(
			ts.userMessage,
			ts.media,
			ts.opts.Dispatch.MessageID(),
			inboundMessageMetadata(ts.opts),
		)
		if len(rootMsg.Media) > 0 || rootMsg.MessageID != "" || !rootMsg.Metadata.IsEmpty() {
			ts.agent.Sessions.AddFullMessage(ts.sessionKey, rootMsg)
		} else {
			ts.agent.Sessions.AddMessage(ts.sessionKey, rootMsg.Role, rootMsg.Content)
		}
		ts.recordPersistedMessage(rootMsg)
		ts.ingestMessage(ctx, p.al, rootMsg)
	}

	activeCandidates, activeModel, usedLight := p.al.selectCandidates(ts.agent, ts.userMessage, messages)
	activeProvider := ts.agent.Provider
	if usedLight && ts.agent.LightProvider != nil {
		activeProvider = ts.agent.LightProvider
	}
	activeModelName := strings.TrimSpace(ts.agent.Model)
	if usedLight {
		activeModelName = strings.TrimSpace(sideQuestionModelName(ts.agent, true))
	}
	activeModelName = resolvedCandidateModelName(activeCandidates, activeModelName)

	exec := newTurnExecution(
		ts.agent,
		ts.opts,
		history,
		summary,
		messages,
	)
	exec.currentTurnStart = currentTurnStart
	exec.activeCandidates = activeCandidates
	exec.activeModel = activeModel
	exec.activeModelConfig = resolveActiveModelConfig(
		p.Cfg,
		ts.agent.Workspace,
		activeCandidates,
		activeModel,
		p.Cfg.Agents.Defaults.Provider,
	)
	exec.llmModelName = activeModelName
	exec.activeProvider = activeProvider
	exec.usedLight = usedLight

	return exec, nil
}
