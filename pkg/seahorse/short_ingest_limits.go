package seahorse

import (
	"fmt"

	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tokenizer"
)

// truncationMarker is spliced between the kept head and tail of a cut tool
// result. It names the cost so a later reader (human or model) can tell the
// gap apart from output the tool never produced.
const truncationMarker = "\n\n… [%d characters truncated at ingest: tool output exceeded the storage cap] …\n\n"

// capToolResultForStorage trims oversized tool output on a message about to be
// persisted, and reports whether anything was cut.
//
// Tool output is the one message kind with no natural size bound: a shell
// command that dumps a TUI frame or tails a log writes tens of thousands of
// tokens into a single message that then sits in history forever. Beyond
// crowding out real conversation, a message larger than LeafChunkTokens used to
// wedge leaf compaction outright — see the chunk accumulation loop in
// compactLeaf. The cap keeps any single stored message summarizable.
//
// Only the stored copy is affected. The turn that produced the output already
// handed the untruncated text to the model.
func capToolResultForStorage(msg *Message) bool {
	if msg == nil {
		return false
	}

	changed := false
	saved := 0

	if msg.Role == "tool" {
		if trimmed, cut := truncateToolTextForStorage(msg.Content); cut {
			saved += estimateTextTokens(msg.Content) - estimateTextTokens(trimmed)
			msg.Content = trimmed
			changed = true
		}
	}

	for i := range msg.Parts {
		if msg.Parts[i].Type != "tool_result" {
			continue
		}
		if trimmed, cut := truncateToolTextForStorage(msg.Parts[i].Text); cut {
			saved += estimateTextTokens(msg.Parts[i].Text) - estimateTextTokens(trimmed)
			msg.Parts[i].Text = trimmed
			changed = true
		}
	}

	if changed {
		// The caller estimated TokenCount from the untruncated message, and the
		// store writes that number through verbatim into context_items. Adjust
		// it by what was actually removed rather than re-deriving the whole
		// estimate, which would have to replicate the caller's model.
		msg.TokenCount -= saved
		if msg.TokenCount < 1 {
			msg.TokenCount = 1
		}
	}

	return changed
}

// storageShape returns the message as Ingest would persist it, without
// touching the caller's copy.
//
// Bootstrap must compare the canonical JSONL against this shape rather than
// against the raw message. Ingest caps oversized tool output, so the stored row
// is a shorter string than the history it came from; comparing raw text reads
// that cap as a history edit, deletes the conversation tail and re-ingests it —
// which caps it again, so the next startup finds the same "edit". Every boot
// then rebuilds every conversation that ever stored a large tool result.
//
// Capping is idempotent: text already under the cap comes back unchanged, so
// the shaped message is also what a re-ingest would store.
//
// The Parts slice is cloned first: capToolResultForStorage rewrites part text
// in place, and a Message copy still shares its caller's backing array.
func storageShape(msg Message) (Message, bool) {
	shaped := msg
	if len(msg.Parts) > 0 {
		shaped.Parts = append([]MessagePart(nil), msg.Parts...)
	}
	return shaped, capToolResultForStorage(&shaped)
}

// storageShapes maps storageShape over a slice.
func storageShapes(messages []Message) []Message {
	shaped := make([]Message, len(messages))
	for i := range messages {
		shaped[i], _ = storageShape(messages[i])
	}
	return shaped
}

// truncateToolTextForStorage cuts text down to roughly MaxStoredToolResultTokens,
// keeping both ends. The head carries the command and the start of its output;
// the tail carries the exit status and whatever error the tool ended on, which
// is usually the part worth reading.
func truncateToolTextForStorage(text string) (string, bool) {
	if text == "" {
		return text, false
	}
	tokens := estimateTextTokens(text)
	if tokens <= MaxStoredToolResultTokens {
		return text, false
	}

	runes := []rune(text)

	build := func(budget int) string {
		head := budget * 3 / 4
		tail := budget - head
		if head < 1 {
			head = 1
		}
		if tail < 1 {
			tail = 1
		}
		dropped := len(runes) - head - tail
		return string(runes[:head]) +
			fmt.Sprintf(truncationMarker, dropped) +
			string(runes[len(runes)-tail:])
	}

	// Size the first cut from the blob's own tokens-per-rune ratio, leaving
	// room for the marker itself. The ratio is only an estimate, so the loop
	// below enforces the bound rather than trusting the arithmetic.
	markerTokens := estimateTextTokens(fmt.Sprintf(truncationMarker, len(runes)))
	contentTokens := MaxStoredToolResultTokens - markerTokens
	if contentTokens < 1 {
		contentTokens = 1
	}
	budget := len(runes) * contentTokens / tokens
	if budget < 2 {
		budget = 2
	}
	if budget+markerTokens >= len(runes) {
		return text, false
	}

	out := build(budget)
	for i := 0; i < 8 && estimateTextTokens(out) > MaxStoredToolResultTokens && budget > 2; i++ {
		budget = budget * 9 / 10
		if budget < 2 {
			budget = 2
		}
		out = build(budget)
	}
	if len(out) >= len(text) {
		return text, false
	}
	return out, true
}

func estimateTextTokens(text string) int {
	return tokenizer.EstimateMessageTokens(providers.Message{Content: text})
}
