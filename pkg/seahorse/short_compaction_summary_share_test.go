package seahorse

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// seedLeafSummaries stores count leaf summaries of tokensEach into the
// conversation's context and returns their total token count.
func seedLeafSummaries(t *testing.T, s *Store, convID int64, count, tokensEach int) int {
	t.Helper()
	ctx := context.Background()

	var items []ContextItem
	ordinal := 100
	for i := 0; i < count; i++ {
		sum, err := s.CreateSummary(ctx, CreateSummaryInput{
			ConversationID: convID,
			Kind:           SummaryKindLeaf,
			Depth:          0,
			Content:        fmt.Sprintf("leaf summary %d covering an earlier stretch of talk", i),
			TokenCount:     tokensEach,
		})
		if err != nil {
			t.Fatalf("CreateSummary: %v", err)
		}
		items = append(items, ContextItem{
			Ordinal:    ordinal,
			ItemType:   "summary",
			SummaryID:  sum.SummaryID,
			TokenCount: tokensEach,
		})
		ordinal += 100
	}
	if err := s.UpsertContextItems(ctx, convID, items); err != nil {
		t.Fatalf("UpsertContextItems: %v", err)
	}
	return count * tokensEach
}

func TestGetContextSummaryTokenCount(t *testing.T) {
	_, s, convID := newTestCompactionEngine(t)
	ctx := context.Background()

	seedLeafSummaries(t, s, convID, 3, 100)

	// A message in context must not be counted.
	m, err := s.AddMessage(ctx, convID, "user", "hello", 5000)
	if err != nil {
		t.Fatalf("AddMessage: %v", err)
	}
	if err := s.AppendContextMessage(ctx, convID, m.ID); err != nil {
		t.Fatalf("AppendContextMessage: %v", err)
	}

	got, err := s.GetContextSummaryTokenCount(ctx, convID)
	if err != nil {
		t.Fatalf("GetContextSummaryTokenCount: %v", err)
	}
	if got != 300 {
		t.Errorf("summary tokens = %d, want 300 (messages must not be counted)", got)
	}

	total, err := s.GetContextTokenCount(ctx, convID)
	if err != nil {
		t.Fatalf("GetContextTokenCount: %v", err)
	}
	if total <= got {
		t.Errorf("total tokens = %d, want more than the summary-only %d", total, got)
	}
}

// The total-size trigger never fires in a healthy conversation: leaf compaction
// holds the context below ContextThreshold, so it never reaches the full budget.
// Without a trigger of its own the summary block grows without limit.
func TestShouldCompactCondensed(t *testing.T) {
	tests := []struct {
		name          string
		summaryTokens int
		tokensBefore  int
		budget        int
		force         bool
		want          bool
	}{
		{
			name:          "summary block over share while the context fits",
			summaryTokens: 5000, // share limit at budget 20000 is 3000
			tokensBefore:  8000,
			budget:        20000,
			want:          true,
		},
		{
			name:          "summary block within share",
			summaryTokens: 2000,
			tokensBefore:  8000,
			budget:        20000,
			want:          false,
		},
		{
			name:          "exactly at the share limit is not over it",
			summaryTokens: 3000,
			tokensBefore:  8000,
			budget:        20000,
			want:          false,
		},
		{
			name:          "whole context over budget still triggers",
			summaryTokens: 100,
			tokensBefore:  25000,
			budget:        20000,
			want:          true,
		},
		{
			name:          "force triggers regardless of budget",
			summaryTokens: 0,
			tokensBefore:  0,
			budget:        0,
			force:         true,
			want:          true,
		},
		{
			name:          "no budget means no opinion",
			summaryTokens: 999999,
			tokensBefore:  999999,
			budget:        0,
			want:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ce, s, convID := newTestCompactionEngine(t)
			ctx := context.Background()
			if tt.summaryTokens > 0 {
				seedLeafSummaries(t, s, convID, 1, tt.summaryTokens)
			}

			got := ce.shouldCompactCondensed(
				ctx, convID, CompactInput{Force: tt.force}, tt.tokensBefore, tt.budget,
			)
			if got != tt.want {
				t.Errorf("shouldCompactCondensed = %v, want %v", got, tt.want)
			}
		})
	}
}

// End to end: a conversation whose stored context fits its budget but whose
// summary block has outgrown its share must actually get rolled up. Before this
// trigger existed, such a conversation accumulated leaf summaries forever —
// across the whole production database not one conversation had more than a
// single condensed summary.
func TestCompactRollsUpWhenSummaryBlockOverShare(t *testing.T) {
	ce, s, convID := newTestCompactionEngine(t)
	ctx := context.Background()

	// 50 leaves x 300 tokens = 15000. Against a 30000 budget that is under the
	// full budget (so the total-size trigger stays quiet) and under the leaf
	// threshold of 22500 (so Phase 1 stays quiet), but well over the 4500-token
	// share limit — only the new trigger can fire here.
	const budget = 30000
	summaryTokens := seedLeafSummaries(t, s, convID, 50, 300)
	if summaryTokens <= int(float64(budget)*SummaryBudgetShare) {
		t.Fatalf("test setup: %d summary tokens is not over the share limit", summaryTokens)
	}
	if summaryTokens > int(float64(budget)*ContextThreshold) {
		t.Fatalf("test setup: %d summary tokens would also trip the leaf pass", summaryTokens)
	}

	b := budget
	if _, err := ce.Compact(ctx, convID, CompactInput{Budget: &b}); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	if !waitForCondensed(ce, convID, 10*time.Second) {
		t.Fatal("condensed compaction did not finish in time")
	}

	summaries, err := s.GetSummariesByConversation(ctx, convID)
	if err != nil {
		t.Fatalf("GetSummariesByConversation: %v", err)
	}
	condensed := 0
	for _, sum := range summaries {
		if sum.Kind == SummaryKindCondensed {
			condensed++
		}
	}
	if condensed == 0 {
		t.Fatal("no condensed summary was produced; the summary block would keep " +
			"growing and the assembler would shed compressed context every turn")
	}

	// The rollup has to actually shrink the block, otherwise the trigger would
	// re-fire on every subsequent turn.
	after, err := s.GetContextSummaryTokenCount(ctx, convID)
	if err != nil {
		t.Fatalf("GetContextSummaryTokenCount: %v", err)
	}
	if after >= summaryTokens {
		t.Errorf("summary tokens %d -> %d; rollup must reduce the block", summaryTokens, after)
	}
}
