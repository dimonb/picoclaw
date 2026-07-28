package seahorse

import "time"

// Short-term memory configuration constants — all are experience-based defaults.

const (
	// OrdinalStep is the gap between ordinals in context_items.
	// Insert at midpoint; resequence only when precision exhausted.
	OrdinalStep = 100

	// ContextThreshold is the compaction trigger for the context window.
	ContextThreshold float64 = 0.75 // Compact at 75% of context window
	FreshTailCount   int     = 32   // Recent messages protected from compaction

	// LeafMinFanout is the fanout parameter.
	LeafMinFanout          int = 8 // Min messages per leaf summary
	CondensedMinFanout     int = 4 // Min summaries per condensed
	CondensedMinFanoutHard int = 2 // Min for forced compaction

	// SummaryBudgetShare bounds the injected summary block's share of the
	// assemble budget.
	//
	// Summaries are never recycled the way messages are: the assembler reserves
	// every one of them ahead of raw history, and leaf compaction only ever adds
	// more. Nothing collapses them in normal operation either — condensed
	// compaction triggers once stored context passes the full budget, while leaf
	// compaction triggers at ContextThreshold and keeps the conversation below
	// that line, so the two never meet and the block ratchets upward for the life
	// of the chat. Observed on a 28-day topic: 116 leaf summaries, 83k tokens,
	// injected into all 294KB of every system prompt it built.
	//
	// Capping trades always-on recall for retrieval on demand. Summaries past the
	// allowance stay in SQLite and remain reachable through short_grep; nothing
	// is deleted.
	SummaryBudgetShare float64 = 0.15

	// LeafChunkTokens is the token target.
	LeafChunkTokens       int = 20000 // Max tokens per leaf chunk
	LeafTargetTokens      int = 1200  // Target tokens for leaf summaries
	CondensedTargetTokens int = 2000  // Target tokens for condensed summaries
	MaxExpandTokens       int = 4000  // Token cap for expansion queries

	// MaxStoredToolResultTokens caps a single tool_result at ingest. Tool
	// output is unbounded — a shell command that dumps a TUI screen or a log
	// file lands tens of thousands of tokens in one message, and that message
	// then sits in history forever, crowding out real conversation. The cap
	// stays well under LeafChunkTokens so that no single stored message can
	// fill a leaf chunk by itself. The model still sees the untruncated
	// output during the turn that produced it; only the stored copy is cut.
	MaxStoredToolResultTokens int = 8000

	// CondensedCompactTimeout bounds one condensed compaction pass.
	//
	// Rollup is the only compaction that runs detached: leaf compaction executes
	// inline on the turn's context and dies with it, while rollup runs on a
	// background goroutine against a context that lives as long as the process.
	// It also holds a per-conversation guard for its whole run. An unbounded
	// provider call therefore does not merely stall one pass — it wedges rollup
	// for that conversation until restart, because every later trigger is
	// deduplicated against the guard the stuck goroutine still holds, and a
	// goroutine that never returns never runs its deferred release.
	//
	// Observed in production: a rollup launched and was never heard from again —
	// no summary, no error, and none of the loop's debug exits, with debug
	// logging enabled. The pass is generous because summarizing a full
	// LeafChunkTokens chunk is slow; it exists to break a wedge, not to pace
	// normal work.
	CondensedCompactTimeout = 5 * time.Minute

	// MaxCompactIterations caps CompactUntilUnder to prevent infinite loops.
	// Each iteration reduces ~4x tokens via leaf (8:1) or condensed (4:1) compaction.
	// With a 200k token context window and 75% threshold, ~20 iterations is enough
	// for any realistic scenario. If exceeded, the issue is logged as a warning.
	MaxCompactIterations int = 20
)

// Leaf summary compression modes for Config.LeafSummaryCompression.
const (
	// LeafCompressionRelaxed (default) accepts any leaf summary smaller than its
	// source segment, preserving richer summaries. Empty string maps here.
	LeafCompressionRelaxed = "relaxed"
	// LeafCompressionStrict enforces the hard token target for leaf summaries
	// (upstream behavior), escalating to the aggressive prompt or deterministic
	// truncation whenever the LLM overshoots the target.
	LeafCompressionStrict = "strict"
)
