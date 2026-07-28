package agent

import (
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// The bug this guards: the history budget used to be contextWindow - maxTokens,
// ignoring the system prompt and tool definitions. With a large system prompt
// the ContextManager then filled a budget that could not possibly fit, so every
// turn went over budget, ran compaction, and got trimmed again.
func TestHistoryTokenBudgetReservesSystemAndTools(t *testing.T) {
	t.Parallel()

	const (
		contextWindow = 240000
		maxTokens     = 60000
		systemTokens  = 80000
		toolTokens    = 8000
	)

	budget := historyTokenBudget(contextWindow, maxTokens, systemTokens, toolTokens)

	want := contextWindow - maxTokens - systemTokens - toolTokens - contextBudgetSafetyMargin
	if budget != want {
		t.Fatalf("historyTokenBudget = %d, want %d", budget, want)
	}

	// A request that exactly fills the budget must still fit the window.
	history := []providers.Message{{Role: "user", Content: fillTokens(budget)}}
	messages := append([]providers.Message{{Role: "system", Content: fillTokens(systemTokens)}}, history...)
	total := 0
	for _, m := range messages {
		total += EstimateMessageTokens(m)
	}
	if got := total + toolTokens + maxTokens; got > contextWindow {
		t.Fatalf("budget-filling request needs %d tokens, over the %d window", got, contextWindow)
	}
}

func TestHistoryTokenBudgetFloorsInsteadOfGoingNegative(t *testing.T) {
	t.Parallel()

	// Pathological config: the system prompt and output reserve alone exceed
	// the window. The budget must stay usable (the caller's trim fallback
	// handles the overflow) rather than collapse to zero or negative.
	budget := historyTokenBudget(100000, 60000, 90000, 5000)

	if budget != 10000 {
		t.Fatalf("historyTokenBudget = %d, want the 10%% floor (10000)", budget)
	}
}

func TestEstimateSummaryTokens(t *testing.T) {
	t.Parallel()

	if got := estimateSummaryTokens("   "); got != 0 {
		t.Fatalf("estimateSummaryTokens(blank) = %d, want 0", got)
	}
	if got := estimateSummaryTokens("a summary of prior conversation"); got <= 0 {
		t.Fatalf("estimateSummaryTokens(text) = %d, want > 0", got)
	}
}

// fillTokens returns text whose estimated token count is approximately n.
func fillTokens(n int) string {
	// EstimateMessageTokens uses ~2/5 tokens per rune, so 5 runes ≈ 2 tokens.
	runes := make([]byte, n*5/2)
	for i := range runes {
		runes[i] = 'a'
	}
	return string(runes)
}
