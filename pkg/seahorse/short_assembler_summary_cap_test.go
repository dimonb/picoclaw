package seahorse

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// seedSummaryConversation stores summaryCount summaries of summaryTokens each,
// followed by a cheap fresh tail of messages. Sizes are chosen by the callers so
// the whole conversation fits the budget under test — these tests exercise the
// path where nothing is over budget.
func seedSummaryConversation(
	t *testing.T,
	s *Store,
	convID int64,
	summaryCount int,
	summaryTokens int,
) {
	t.Helper()
	ctx := context.Background()

	var items []ContextItem
	ordinal := 100

	for i := 0; i < summaryCount; i++ {
		sum, err := s.CreateSummary(ctx, CreateSummaryInput{
			ConversationID: convID,
			Kind:           SummaryKindLeaf,
			Depth:          0,
			Content:        fmt.Sprintf("summary %d", i),
			TokenCount:     summaryTokens,
		})
		if err != nil {
			t.Fatalf("CreateSummary: %v", err)
		}
		items = append(items, ContextItem{
			Ordinal:    ordinal,
			ItemType:   "summary",
			SummaryID:  sum.SummaryID,
			TokenCount: summaryTokens,
		})
		ordinal += 100
	}

	for i := 0; i < FreshTailCount; i++ {
		m, err := s.AddMessage(ctx, convID, "user", fmt.Sprintf("fresh %d", i), 1)
		if err != nil {
			t.Fatalf("AddMessage: %v", err)
		}
		items = append(items, ContextItem{
			Ordinal:    ordinal,
			ItemType:   "message",
			MessageID:  m.ID,
			TokenCount: 1,
		})
		ordinal += 100
	}

	if err := s.UpsertContextItems(ctx, convID, items); err != nil {
		t.Fatalf("UpsertContextItems: %v", err)
	}
}

// The expensive case is not the over-budget one. A long-lived conversation
// accumulates leaf summaries that are never collapsed (condensed compaction
// triggers above the budget, leaf compaction keeps the conversation below it)
// and never evicted (the assembler reserves summaries ahead of messages). Every
// one of them is rebuilt into the system prompt on every turn even though the
// stored context fits comfortably — which is how a 28-day topic reached a 294KB
// system prompt that was 88% summary.
func TestAssembleCapsSummaryBlockWhenEverythingFits(t *testing.T) {
	s, convID := setupAssemblerStore(t)
	ctx := context.Background()

	// 100 summaries x 100 tokens = 10000 tokens, well inside a 20000 budget:
	// nothing here is over budget, so eviction never runs.
	const summaryCount = 100
	const summaryTokens = 100
	const budget = 20000
	seedSummaryConversation(t, s, convID, summaryCount, summaryTokens)

	a := &Assembler{store: s, config: Config{}}
	result, err := a.Assemble(ctx, convID, AssembleInput{Budget: budget})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	allowance := int(float64(budget) * SummaryBudgetShare)
	wantKept := allowance / summaryTokens

	if got := strings.Count(result.Summary, "<summary "); got != wantKept {
		t.Fatalf("kept %d summaries, want %d (allowance %d tokens at %d each)",
			got, wantKept, allowance, summaryTokens)
	}

	// The newest survive; the oldest are the ones dropped.
	for i := summaryCount - wantKept; i < summaryCount; i++ {
		if !strings.Contains(result.Summary, fmt.Sprintf("summary %d\n", i)) {
			t.Errorf("summary %d was dropped; the newest summaries must survive", i)
		}
	}
	if strings.Contains(result.Summary, "summary 0\n") {
		t.Error("oldest summary survived; the cap must drop from the old end")
	}

	// Dropping summaries to hold a steady-state share is not eviction. Evicted
	// forces a proactive compaction on the very turn it is reported, so setting
	// it here would compact on every single turn for the rest of the
	// conversation's life.
	if result.Evicted {
		t.Error("Evicted = true after capping the summary block; the caller " +
			"would run a proactive compaction pass on every turn")
	}
}

// Bounding accumulation must not erase the record that earlier conversation
// existed. A lone summary larger than the allowance is still the only
// compressed trace of everything before it, and it fits the budget fine.
func TestAssembleCapKeepsLoneSummaryOverAllowance(t *testing.T) {
	s, convID := setupAssemblerStore(t)
	ctx := context.Background()

	// Allowance is 3000; the summary is 5000 but the budget is 20000, so only
	// the cap could drop it.
	const budget = 20000
	seedSummaryConversation(t, s, convID, 1, 5000)

	a := &Assembler{store: s, config: Config{}}
	result, err := a.Assemble(ctx, convID, AssembleInput{Budget: budget})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	if !strings.Contains(result.Summary, "summary 0\n") {
		t.Fatalf("the only summary was dropped for exceeding the allowance; "+
			"Summary = %q", result.Summary)
	}
	if result.Evicted {
		t.Error("Evicted = true although the conversation fits the budget")
	}
}

// Summaries sitting in the protected fresh tail cannot be dropped, but they are
// still part of the block the model pays for. They must spend the allowance, or
// the cap bounds only the evictable half of what it claims to bound.
func TestAssembleCapCountsFreshTailSummariesAgainstAllowance(t *testing.T) {
	s, convID := setupAssemblerStore(t)
	ctx := context.Background()

	const budget = 20000
	const summaryTokens = 100
	allowance := int(float64(budget) * SummaryBudgetShare)

	var items []ContextItem
	ordinal := 100

	addSummary := func(label string) {
		sum, err := s.CreateSummary(ctx, CreateSummaryInput{
			ConversationID: convID,
			Kind:           SummaryKindLeaf,
			Depth:          0,
			Content:        label,
			TokenCount:     summaryTokens,
		})
		if err != nil {
			t.Fatalf("CreateSummary: %v", err)
		}
		items = append(items, ContextItem{
			Ordinal:    ordinal,
			ItemType:   "summary",
			SummaryID:  sum.SummaryID,
			TokenCount: summaryTokens,
		})
		ordinal += 100
	}

	for i := 0; i < 100; i++ {
		addSummary(fmt.Sprintf("old %d", i))
	}
	// Fresh tail made entirely of summaries — protected, but not free.
	for i := 0; i < FreshTailCount; i++ {
		addSummary(fmt.Sprintf("tail %d", i))
	}

	if err := s.UpsertContextItems(ctx, convID, items); err != nil {
		t.Fatalf("UpsertContextItems: %v", err)
	}

	a := &Assembler{store: s, config: Config{}}
	result, err := a.Assemble(ctx, convID, AssembleInput{Budget: budget})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	// The fresh tail alone (32 x 100 = 3200) already exceeds the 3000-token
	// allowance, so every evictable summary must go and no floor may re-admit
	// one — the tail already guarantees a summary survives.
	if got := strings.Count(result.Summary, "<summary "); got != FreshTailCount {
		t.Fatalf("kept %d summaries, want %d (fresh tail only)", got, FreshTailCount)
	}
	if strings.Contains(result.Summary, "old ") {
		t.Errorf("evictable summaries survived although the fresh tail (%d tokens) "+
			"already exceeds the allowance (%d)", FreshTailCount*summaryTokens, allowance)
	}
}

func summaryItem(ordinal, tokens int) resolvedItem {
	return resolvedItem{
		ordinal:    ordinal,
		itemType:   "summary",
		summary:    &Summary{SummaryID: fmt.Sprintf("sum_%d", ordinal), TokenCount: tokens},
		tokenCount: tokens,
	}
}

func messageItem(ordinal, tokens int) resolvedItem {
	return resolvedItem{
		ordinal:    ordinal,
		itemType:   "message",
		message:    &Message{ID: int64(ordinal)},
		tokenCount: tokens,
	}
}

func TestCapSummariesWithinBudget(t *testing.T) {
	tests := []struct {
		name        string
		items       []resolvedItem
		allowance   int
		haveSummary bool
		wantKept    []int // ordinals, in chronological order
		wantDropped int
	}{
		{
			name:        "keeps newest summaries within allowance",
			items:       []resolvedItem{summaryItem(1, 100), summaryItem(2, 100), summaryItem(3, 100)},
			allowance:   250,
			haveSummary: true,
			wantKept:    []int{2, 3},
			wantDropped: 1,
		},
		{
			name:        "messages pass through untouched",
			items:       []resolvedItem{summaryItem(1, 100), messageItem(2, 9000), summaryItem(3, 100)},
			allowance:   100,
			haveSummary: true,
			wantKept:    []int{2, 3},
			wantDropped: 1,
		},
		{
			name:        "floor admits one summary when none is guaranteed",
			items:       []resolvedItem{summaryItem(1, 100), summaryItem(2, 5000)},
			allowance:   0,
			haveSummary: false,
			wantKept:    []int{2},
			wantDropped: 1,
		},
		{
			name:        "no floor when the caller already has a summary",
			items:       []resolvedItem{summaryItem(1, 100), summaryItem(2, 5000)},
			allowance:   0,
			haveSummary: true,
			wantKept:    nil,
			wantDropped: 2,
		},
		{
			name:        "everything fits",
			items:       []resolvedItem{summaryItem(1, 100), summaryItem(2, 100)},
			allowance:   1000,
			haveSummary: true,
			wantKept:    []int{1, 2},
			wantDropped: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kept, droppedCount, droppedTokens := capSummariesWithinBudget(tt.items, tt.allowance, tt.haveSummary)

			var gotOrdinals []int
			for _, k := range kept {
				gotOrdinals = append(gotOrdinals, k.ordinal)
			}
			if fmt.Sprint(gotOrdinals) != fmt.Sprint(tt.wantKept) {
				t.Errorf("kept ordinals = %v, want %v", gotOrdinals, tt.wantKept)
			}
			if droppedCount != tt.wantDropped {
				t.Errorf("droppedCount = %d, want %d", droppedCount, tt.wantDropped)
			}

			var wantDroppedTokens int
			for _, it := range tt.items {
				wantDroppedTokens += it.tokenCount
			}
			for _, k := range kept {
				wantDroppedTokens -= k.tokenCount
			}
			if droppedTokens != wantDroppedTokens {
				t.Errorf("droppedTokens = %d, want %d", droppedTokens, wantDroppedTokens)
			}
		})
	}
}
