package seahorse

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// When the stored context no longer fits the budget the assembler has to drop
// something. Summaries are the oldest items by construction, so an oldest-first
// walk drops them first — throwing away the compacted record of the entire
// early conversation while keeping raw messages that cost far more per unit of
// information. This test pins the opposite priority.
func TestAssembleKeepsSummariesWhenOverBudget(t *testing.T) {
	s, convID := setupAssemblerStore(t)
	ctx := context.Background()

	var items []ContextItem
	ordinal := 100

	// Two cheap summaries covering the distant past.
	for i := 0; i < 2; i++ {
		sum, err := s.CreateSummary(ctx, CreateSummaryInput{
			ConversationID: convID,
			Kind:           SummaryKindLeaf,
			Depth:          0,
			Content:        fmt.Sprintf("summary %d of early messages", i),
			TokenCount:     20,
		})
		if err != nil {
			t.Fatalf("CreateSummary: %v", err)
		}
		items = append(items, ContextItem{
			Ordinal:    ordinal,
			ItemType:   "summary",
			SummaryID:  sum.SummaryID,
			TokenCount: 20,
		})
		ordinal += 100
	}

	// Expensive raw history that cannot fit alongside the fresh tail.
	const evictableMessages = 20
	for i := 0; i < evictableMessages; i++ {
		m, err := s.AddMessage(ctx, convID, "user", fmt.Sprintf("evictable %d", i), 100)
		if err != nil {
			t.Fatalf("AddMessage: %v", err)
		}
		items = append(items, ContextItem{
			Ordinal:    ordinal,
			ItemType:   "message",
			MessageID:  m.ID,
			TokenCount: 100,
		})
		ordinal += 100
	}

	// Fresh tail — protected from eviction.
	for i := 0; i < FreshTailCount; i++ {
		m, err := s.AddMessage(ctx, convID, "user", fmt.Sprintf("fresh %d", i), 10)
		if err != nil {
			t.Fatalf("AddMessage: %v", err)
		}
		items = append(items, ContextItem{
			Ordinal:    ordinal,
			ItemType:   "message",
			MessageID:  m.ID,
			TokenCount: 10,
		})
		ordinal += 100
	}

	if err := s.UpsertContextItems(ctx, convID, items); err != nil {
		t.Fatalf("UpsertContextItems: %v", err)
	}

	// Fresh tail costs 320; leave room for both summaries (40) and a few
	// evictable messages, but nowhere near all 2000 tokens of them.
	a := &Assembler{store: s, config: Config{}}
	result, err := a.Assemble(ctx, convID, AssembleInput{Budget: 700})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	for i := 0; i < 2; i++ {
		want := fmt.Sprintf("summary %d of early messages", i)
		if !strings.Contains(result.Summary, want) {
			t.Fatalf("summary %d was evicted; Summary = %q", i, result.Summary)
		}
	}

	// Eviction must still have happened — otherwise the test proves nothing.
	if len(result.Messages) >= evictableMessages+FreshTailCount {
		t.Fatalf("Messages = %d, expected raw history to be evicted", len(result.Messages))
	}

	// The surviving raw messages must be the newest ones, contiguous with the
	// fresh tail, so no tool-call sequence is torn apart.
	last := result.Messages[len(result.Messages)-1]
	if last.Content != fmt.Sprintf("fresh %d", FreshTailCount-1) {
		t.Fatalf("last message = %q, want the newest fresh-tail message", last.Content)
	}

	if !result.Evicted {
		t.Error("Evicted = false after dropping stored items; the caller would " +
			"see a prompt that fits and never trigger compaction")
	}
}

// The Evicted flag is the only signal that a conversation needs compacting:
// the assembler always returns something that fits, so a budget check on the
// built prompt cannot tell a healthy conversation from one silently shedding
// its oldest messages every turn. It must therefore stay false whenever
// nothing was dropped.
func TestAssembleDoesNotReportEvictionWhenEverythingFits(t *testing.T) {
	s, convID := setupAssemblerStore(t)
	ctx := context.Background()

	var items []ContextItem
	ordinal := 100
	for i := 0; i < 5; i++ {
		m, err := s.AddMessage(ctx, convID, "user", fmt.Sprintf("msg %d", i), 10)
		if err != nil {
			t.Fatalf("AddMessage: %v", err)
		}
		items = append(items, ContextItem{
			Ordinal:    ordinal,
			ItemType:   "message",
			MessageID:  m.ID,
			TokenCount: 10,
		})
		ordinal += 100
	}
	if err := s.UpsertContextItems(ctx, convID, items); err != nil {
		t.Fatalf("UpsertContextItems: %v", err)
	}

	a := &Assembler{store: s, config: Config{}}
	result, err := a.Assemble(ctx, convID, AssembleInput{Budget: 100_000})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if result.Evicted {
		t.Error("Evicted = true with a budget that fits everything; " +
			"proactive compaction would run on every turn for no reason")
	}
	if len(result.Messages) != 5 {
		t.Errorf("Messages = %d, want 5", len(result.Messages))
	}
}

func TestSelectEvictableWithinBudgetPrefersSummariesThenNewestMessages(t *testing.T) {
	t.Parallel()

	evictable := []resolvedItem{
		{itemType: "summary", tokenCount: 10, summary: &Summary{SummaryID: "sum_old"}},
		{itemType: "message", tokenCount: 50, message: &Message{Content: "old"}},
		{itemType: "message", tokenCount: 50, message: &Message{Content: "mid"}},
		{itemType: "message", tokenCount: 50, message: &Message{Content: "new"}},
	}

	kept := selectEvictableWithinBudget(evictable, 70)

	if len(kept) != 2 {
		t.Fatalf("kept %d items, want 2", len(kept))
	}
	if kept[0].itemType != "summary" {
		t.Fatalf("kept[0] = %q, want the summary to survive", kept[0].itemType)
	}
	if kept[1].message.Content != "new" {
		t.Fatalf("kept[1] = %q, want the newest message", kept[1].message.Content)
	}
}
