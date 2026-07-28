// PicoClaw - Ultra-lightweight personal AI agent
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors

package agent

import (
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tokenizer"
)

// parseTurnBoundaries returns the starting index of each Turn in the history.
// A Turn is a complete "user input → LLM iterations → final response" cycle
// (as defined in #1316). Each Turn begins at a user message and extends
// through all subsequent assistant/tool messages until the next user message.
//
// Cutting at a Turn boundary guarantees that no tool-call sequence
// (assistant+ToolCalls → tool results) is split across the cut.
func parseTurnBoundaries(history []providers.Message) []int {
	var starts []int
	for i, msg := range history {
		if msg.Role == "user" {
			starts = append(starts, i)
		}
	}
	return starts
}

// isSafeBoundary reports whether index is a valid Turn boundary — i.e.,
// a position where the kept portion (history[index:]) begins at a user
// message, so no tool-call sequence is torn apart.
func isSafeBoundary(history []providers.Message, index int) bool {
	if index <= 0 || index >= len(history) {
		return true
	}
	return history[index].Role == "user"
}

// findSafeBoundary locates the nearest Turn boundary to targetIndex.
// It prefers the boundary at or before targetIndex (preserving more recent
// context). Falls back to the nearest boundary after targetIndex, and
// returns targetIndex unchanged only when no Turn boundary exists at all.
func findSafeBoundary(history []providers.Message, targetIndex int) int {
	if len(history) == 0 {
		return 0
	}
	if targetIndex <= 0 {
		return 0
	}
	if targetIndex >= len(history) {
		return len(history)
	}

	turns := parseTurnBoundaries(history)
	if len(turns) == 0 {
		return targetIndex
	}

	// Find the last Turn boundary at or before targetIndex.
	// Prefer backward: keeps more recent messages.
	backward := -1
	for _, t := range turns {
		if t <= targetIndex {
			backward = t
		}
	}
	if backward > 0 {
		return backward
	}

	// No valid Turn boundary before target (or only at index 0 which
	// would keep everything). Use the first Turn after targetIndex.
	for _, t := range turns {
		if t > targetIndex {
			return t
		}
	}

	// No Turn boundary after targetIndex either. The only boundary is at
	// index 0, meaning the entire history is a single Turn. Return 0 to
	// signal that safe compression is not possible — callers check for
	// mid <= 0 and skip compression in that case.
	return 0
}

// EstimateMessageTokens estimates the token count for a single message.
// Delegates to the shared tokenizer package for consistency across agent and seahorse.
func EstimateMessageTokens(msg providers.Message) int {
	return tokenizer.EstimateMessageTokens(msg)
}

// EstimateToolDefsTokens estimates the total token cost of tool definitions
// as they appear in the LLM request. Delegates to the shared tokenizer package.
func EstimateToolDefsTokens(defs []providers.ToolDefinition) int {
	return tokenizer.EstimateToolDefsTokens(defs)
}

type contextBudgetStats struct {
	MessageCount    int
	ToolCount       int
	MessageTokens   int
	SystemTokens    int
	NonSystemTokens int
	ToolTokens      int
	MaxTokens       int
	TotalTokens     int
	ContextWindow   int
	RemainingTokens int
	OverBudget      bool
}

func estimateContextBudgetStats(
	contextWindow int,
	messages []providers.Message,
	toolDefs []providers.ToolDefinition,
	maxTokens int,
) contextBudgetStats {
	stats := contextBudgetStats{
		MessageCount:  len(messages),
		ToolCount:     len(toolDefs),
		MaxTokens:     maxTokens,
		ContextWindow: contextWindow,
	}
	for _, m := range messages {
		tokens := EstimateMessageTokens(m)
		stats.MessageTokens += tokens
		if m.Role == "system" {
			stats.SystemTokens += tokens
		} else {
			stats.NonSystemTokens += tokens
		}
	}
	stats.ToolTokens = EstimateToolDefsTokens(toolDefs)
	stats.TotalTokens = stats.MessageTokens + stats.ToolTokens + maxTokens
	stats.RemainingTokens = contextWindow - stats.TotalTokens
	stats.OverBudget = stats.TotalTokens > contextWindow
	return stats
}

func contextBudgetStatsFields(stats contextBudgetStats) map[string]any {
	return map[string]any{
		"message_count":     stats.MessageCount,
		"tool_count":        stats.ToolCount,
		"message_tokens":    stats.MessageTokens,
		"system_tokens":     stats.SystemTokens,
		"non_system_tokens": stats.NonSystemTokens,
		"tool_tokens":       stats.ToolTokens,
		"max_tokens":        stats.MaxTokens,
		"total_tokens":      stats.TotalTokens,
		"context_window":    stats.ContextWindow,
		"remaining_tokens":  stats.RemainingTokens,
		"over_budget":       stats.OverBudget,
	}
}

// isOverContextBudget checks whether the assembled messages plus tool definitions
// and output reserve would exceed the model's context window. This enables
// proactive compression before calling the LLM, rather than reacting to 400 errors.
func isOverContextBudget(
	contextWindow int,
	messages []providers.Message,
	toolDefs []providers.ToolDefinition,
	maxTokens int,
) bool {
	return estimateContextBudgetStats(contextWindow, messages, toolDefs, maxTokens).OverBudget
}

// trimHistoryToFitContextWindow rebuilds the prompt from progressively newer
// history slices until it fits within the context window. Oldest complete turns
// are dropped first so tool-call sequences remain intact.
func trimHistoryToFitContextWindow(
	history []providers.Message,
	build func([]providers.Message) []providers.Message,
	contextWindow int,
	toolDefs []providers.ToolDefinition,
	maxTokens int,
) ([]providers.Message, []providers.Message, bool) {
	messages := build(history)
	if !isOverContextBudget(contextWindow, messages, toolDefs, maxTokens) {
		return history, messages, true
	}

	trimmedHistory := append([]providers.Message(nil), history...)
	for len(trimmedHistory) > 0 {
		dropUntil := nextHistoryTrimStart(trimmedHistory)
		if dropUntil <= 0 || dropUntil >= len(trimmedHistory) {
			trimmedHistory = nil
		} else {
			trimmedHistory = append([]providers.Message(nil), trimmedHistory[dropUntil:]...)
		}

		messages = build(trimmedHistory)
		if !isOverContextBudget(contextWindow, messages, toolDefs, maxTokens) {
			return trimmedHistory, messages, true
		}
	}

	return nil, messages, false
}

func nextHistoryTrimStart(history []providers.Message) int {
	if len(history) == 0 {
		return 0
	}

	turns := parseTurnBoundaries(history)
	if len(turns) >= 2 {
		return turns[1]
	}
	if len(turns) == 1 {
		if turns[0] > 0 {
			return turns[0]
		}
		return len(history)
	}

	return len(history)
}
