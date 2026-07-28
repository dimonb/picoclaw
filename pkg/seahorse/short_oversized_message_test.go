package seahorse

import (
	"context"
	"strings"
	"testing"
)

// TestCompactLeafWithOversizedMessageInHead reproduces the layout that wedged
// leaf compaction on the beta bot and pins the fix.
//
// A single tool_result of ~40k tokens sat sixth in the compactable region.
// compactLeaf accumulated 105+33+81+100+82 tokens, then added the 40k message
// and broke on `accumTokens >= LeafChunkTokens` with only six messages in the
// chunk — below LeafMinFanout, so the chunk was rejected and nil returned.
// Because nothing was compacted the head of the queue never moved, so every
// later turn hit the same break at the same message: leaf compaction was
// permanently declined and the conversation grew without bound (281 messages /
// 224k tokens by the time it was found). The token cap must not short-circuit
// the chunk before it is large enough to summarize.
func TestCompactLeafWithOversizedMessageInHead(t *testing.T) {
	ce, s, convID := newTestCompactionEngine(t)
	ctx := context.Background()

	small := []int{105, 33, 81, 100, 82}
	for _, tokens := range small {
		m, err := s.AddMessage(ctx, convID, "user", "small", tokens)
		if err != nil {
			t.Fatalf("AddMessage: %v", err)
		}
		if err := s.AppendContextMessage(ctx, convID, m.ID); err != nil {
			t.Fatalf("AppendContextMessage: %v", err)
		}
	}

	// The oversized tool result: on its own it blows past LeafChunkTokens.
	oversized, err := s.AddMessage(ctx, convID, "tool", "huge tool output", 40811)
	if err != nil {
		t.Fatalf("AddMessage: %v", err)
	}
	if err := s.AppendContextMessage(ctx, convID, oversized.ID); err != nil {
		t.Fatalf("AppendContextMessage: %v", err)
	}

	// Enough trailing messages that the chunk above sits outside the fresh
	// tail and is genuinely eligible for compaction.
	for i := 0; i < FreshTailCount+LeafMinFanout; i++ {
		m, err := s.AddMessage(ctx, convID, "user", "tail", 20)
		if err != nil {
			t.Fatalf("AddMessage: %v", err)
		}
		if err := s.AppendContextMessage(ctx, convID, m.ID); err != nil {
			t.Fatalf("AppendContextMessage: %v", err)
		}
	}

	summaryID, err := ce.compactLeaf(ctx, convID)
	if err != nil {
		t.Fatalf("compactLeaf: %v", err)
	}
	if summaryID == nil {
		t.Fatal("compactLeaf declined to compact: an oversized message in the head " +
			"still wedges leaf compaction, so the conversation can never shrink")
	}

	// The chunk must have swallowed the oversized message, otherwise it stays
	// at the head and wedges the next call just the same.
	linked, err := s.GetSummarySourceMessages(ctx, *summaryID)
	if err != nil {
		t.Fatalf("GetSummarySourceMessages: %v", err)
	}
	if len(linked) < LeafMinFanout {
		t.Errorf("summary covers %d messages, want >= %d (LeafMinFanout)", len(linked), LeafMinFanout)
	}
	var covered bool
	for _, m := range linked {
		if m.ID == oversized.ID {
			covered = true
			break
		}
	}
	if !covered {
		t.Error("the oversized message was left out of the chunk; it would wedge the next call again")
	}
}

func TestCapToolResultForStorage(t *testing.T) {
	huge := strings.Repeat("x", MaxStoredToolResultTokens*40)
	hugeTokens := estimateTextTokens(huge)
	if hugeTokens <= MaxStoredToolResultTokens {
		t.Fatalf("test fixture is not over the cap: %d tokens", hugeTokens)
	}

	t.Run("truncates an oversized tool message", func(t *testing.T) {
		msg := &Message{Role: "tool", Content: huge, TokenCount: hugeTokens}
		if !capToolResultForStorage(msg) {
			t.Fatal("expected the message to be truncated")
		}
		if got := estimateTextTokens(msg.Content); got > MaxStoredToolResultTokens {
			t.Errorf("stored content is %d tokens, want <= %d", got, MaxStoredToolResultTokens)
		}
		if !strings.Contains(msg.Content, "truncated at ingest") {
			t.Error("truncation is not marked in the stored content")
		}
		// TokenCount is written through to context_items verbatim, so a stale
		// value would keep the compaction and assemble maths wrong.
		if msg.TokenCount > MaxStoredToolResultTokens {
			t.Errorf("TokenCount = %d, want <= %d", msg.TokenCount, MaxStoredToolResultTokens)
		}
		if msg.TokenCount < 1 {
			t.Errorf("TokenCount = %d, want >= 1", msg.TokenCount)
		}
	})

	t.Run("truncates an oversized tool_result part", func(t *testing.T) {
		msg := &Message{
			Role:       "assistant",
			Parts:      []MessagePart{{Type: "text", Text: "ok"}, {Type: "tool_result", Text: huge}},
			TokenCount: hugeTokens,
		}
		if !capToolResultForStorage(msg) {
			t.Fatal("expected the part to be truncated")
		}
		if got := estimateTextTokens(msg.Parts[1].Text); got > MaxStoredToolResultTokens {
			t.Errorf("stored part is %d tokens, want <= %d", got, MaxStoredToolResultTokens)
		}
		if msg.Parts[0].Text != "ok" {
			t.Errorf("non tool_result part was modified: %q", msg.Parts[0].Text)
		}
	})

	t.Run("keeps both ends of the output", func(t *testing.T) {
		body := strings.Repeat("m", MaxStoredToolResultTokens*40)
		msg := &Message{Role: "tool", Content: "HEAD" + body + "TAIL"}
		msg.TokenCount = estimateTextTokens(msg.Content)
		if !capToolResultForStorage(msg) {
			t.Fatal("expected truncation")
		}
		// The tail is where a tool reports its exit status and error.
		if !strings.HasPrefix(msg.Content, "HEAD") {
			t.Error("head of the output was dropped")
		}
		if !strings.HasSuffix(msg.Content, "TAIL") {
			t.Error("tail of the output was dropped")
		}
	})

	t.Run("leaves messages under the cap alone", func(t *testing.T) {
		for _, msg := range []*Message{
			{Role: "tool", Content: "short output", TokenCount: 3},
			{Role: "user", Content: huge, TokenCount: hugeTokens},
			{Role: "assistant", Parts: []MessagePart{{Type: "text", Text: huge}}, TokenCount: hugeTokens},
		} {
			before := *msg
			if capToolResultForStorage(msg) {
				t.Errorf("role=%q parts=%d was truncated but should not have been", msg.Role, len(msg.Parts))
			}
			if msg.TokenCount != before.TokenCount {
				t.Errorf("TokenCount changed without truncation: %d → %d", before.TokenCount, msg.TokenCount)
			}
		}
	})
}

// TestCompactSkipsLeafWellUnderBudget pins the end-of-turn leaf trigger to the
// budget.
//
// Phase 1 used to run on every turn no matter how small the conversation was.
// That costs a summarization LLM call per turn, folds live conversation into
// summaries long before anything needs compressing, and — because it shrinks
// the history — makes codex-ws replay the whole session every turn
// (reason=history_shrank), which is the provider-cache thrash the compaction
// design explicitly tries to avoid. It also never stopped: on the beta bot it
// took the Pico chat from 226k tokens to 57k against a 181k budget and was
// still going, heading for the ~40 messages that FreshTailCount and
// LeafMinFanout leave it.
func TestCompactSkipsLeafWellUnderBudget(t *testing.T) {
	ctx := context.Background()

	// Plenty of compactable messages, so only the budget check can hold leaf back.
	seed := func(ce *CompactionEngine, s *Store, convID int64) int {
		total := 0
		for i := 0; i < FreshTailCount+LeafMinFanout*3; i++ {
			m, err := s.AddMessage(ctx, convID, "user", "message body", 100)
			if err != nil {
				t.Fatalf("AddMessage: %v", err)
			}
			if err := s.AppendContextMessage(ctx, convID, m.ID); err != nil {
				t.Fatalf("AppendContextMessage: %v", err)
			}
			total += 100
		}
		return total
	}

	t.Run("skips leaf when the context fits comfortably", func(t *testing.T) {
		ce, s, convID := newTestCompactionEngine(t)
		total := seed(ce, s, convID)

		// Three times the stored size: nothing needs compressing.
		budget := total * 3
		result, err := ce.Compact(ctx, convID, CompactInput{Budget: &budget})
		if err != nil {
			t.Fatalf("Compact: %v", err)
		}
		if result.LeafSummaries != 0 {
			t.Errorf("LeafSummaries = %d, want 0: compacting a conversation that fits "+
				"burns an LLM call and forces a full provider session replay every turn",
				result.LeafSummaries)
		}
	})

	t.Run("compacts once over the threshold", func(t *testing.T) {
		ce, s, convID := newTestCompactionEngine(t)
		total := seed(ce, s, convID)

		// Stored context sits above ContextThreshold of the budget.
		budget := int(float64(total) / ContextThreshold * 0.9)
		result, err := ce.Compact(ctx, convID, CompactInput{Budget: &budget})
		if err != nil {
			t.Fatalf("Compact: %v", err)
		}
		if result.LeafSummaries == 0 {
			t.Error("LeafSummaries = 0 while over the threshold; the conversation would grow unbounded")
		}
	})

	t.Run("compacts when the caller gives no budget", func(t *testing.T) {
		ce, s, convID := newTestCompactionEngine(t)
		seed(ce, s, convID)

		result, err := ce.Compact(ctx, convID, CompactInput{})
		if err != nil {
			t.Fatalf("Compact: %v", err)
		}
		if result.LeafSummaries == 0 {
			t.Error("LeafSummaries = 0 with no budget given; unbounded growth is the worse failure")
		}
	})
}
