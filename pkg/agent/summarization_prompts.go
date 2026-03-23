package agent

import (
	"fmt"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/providers"
)

type compactionNoteMetadata struct {
	SessionKey      string
	Channel         string
	ChatID          string
	SourceMessages  int
	OmittedMessages bool
}

// messageThreadAnnotation returns the delivery/thread annotation prefix for a
// message, e.g. "[msg:#5, reply_to:#3, react_to:#5=❤️] " or "" if absent.
func messageThreadAnnotation(msg providers.Message) string {
	parts := make([]string, 0, 2+len(msg.Reactions))
	if len(msg.MessageIDs) > 0 && strings.TrimSpace(msg.MessageIDs[0]) != "" {
		parts = append(parts, fmt.Sprintf("msg:#%s", msg.MessageIDs[0]))
	}
	if msg.ReplyToMessageID != "" {
		parts = append(parts, fmt.Sprintf("reply_to:#%s", msg.ReplyToMessageID))
	}
	for _, reaction := range msg.Reactions {
		if reaction.TargetMessageID == "" || reaction.Emoji == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("react_to:#%s=%s", reaction.TargetMessageID, reaction.Emoji))
	}
	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf("[%s] ", strings.Join(parts, ", "))
}
func formatConversationMessages(batch []providers.Message) string {
	var sb strings.Builder
	for _, m := range batch {
		switch {
		case len(m.MessageIDs) > 0 && m.ReplyToMessageID != "":
			fmt.Fprintf(&sb, "%s [msg:#%s, reply_to:#%s]: %s\n", m.Role, m.MessageIDs[0], m.ReplyToMessageID, m.Content)
		case len(m.MessageIDs) > 0:
			fmt.Fprintf(&sb, "%s [msg:#%s]: %s\n", m.Role, m.MessageIDs[0], m.Content)
		case m.ReplyToMessageID != "":
			fmt.Fprintf(&sb, "%s [reply_to:#%s]: %s\n", m.Role, m.ReplyToMessageID, m.Content)
		default:
			fmt.Fprintf(&sb, "%s: %s\n", m.Role, m.Content)
		}
	}
	return strings.TrimSpace(sb.String())
}

func buildRunningSummaryPrompt(
	existingSummary string,
	batch []providers.Message,
) string {
	if strings.TrimSpace(existingSummary) == "" {
		existingSummary = "(none)"
	}

	return fmt.Sprintf(`<task>
Update the running conversation summary for future context injection.
</task>

<instructions>
Preserve only information likely to matter in future turns.
Prioritize:
- confirmed decisions and commitments
- unresolved questions, blockers, or follow-ups
- the latest explicit user instructions
- user preferences, constraints, and working style
- important corrections, changed assumptions, and reversals of plan
- exact technical references that may matter later: files, paths, URLs, identifiers, versions, function names, config keys, commands, branch names, and named entities
- action items with owner and timeframe if present

Distinguish clearly between confirmed facts or decisions, tentative ideas, and unresolved proposals.
Omit small talk, repetition, exploratory dead ends, and details that are interesting but do not change future behavior.
If newer statements conflict with older ones, prefer the newer statement and note the change briefly.
When summarizing preferences, plans, or output requirements, prefer the latest explicit user instruction.
Messages may carry [msg:#ID], [reply_to:#PARENT], and [react_to:#ID=EMOJI] annotations showing thread structure and silent acknowledgements. Mention thread structure only if it remains unresolved or operationally relevant for future context.
Do not invent facts.
Write in the dominant language of the conversation.
Keep the result under 180 words.
</instructions>

<format>
Return Markdown with exactly these sections:
## Key Context
## Decisions
## Open Loops
## Tentative Ideas / Alternatives
## Preferences / Constraints

Use short bullet points.
Avoid repeating the same item across sections.
Put stable background in "Key Context" and settled choices in "Decisions".
For "Open Loops", include the next expected action or blocker if known.
"Open Loops" should contain items that still require action, resolution, confirmation, or follow-up.
Put unresolved but still potentially useful ideas, alternatives, or proposals in "Tentative Ideas / Alternatives".
"Tentative Ideas / Alternatives" should contain non-actionable options that may be useful later; do not duplicate active open loops there.
Prefer action-oriented bullets in "Open Loops" (for example: decide, verify, inspect, confirm, wait for).
Prefer option-oriented bullets in "Tentative Ideas / Alternatives" (for example: possible alternative, fallback option, optional refinement, if needed).
Omit tentative ideas that are stale or no longer relevant.
If a section is empty, write "- none".
</format>

<existing_summary>
%s
</existing_summary>

<conversation>
%s
</conversation>`, existingSummary, formatConversationMessages(batch))
}

func buildRunningSummaryMergePrompt(
	existingSummary string,
	partialSummaries []string,
) string {
	if strings.TrimSpace(existingSummary) == "" {
		existingSummary = "(none)"
	}

	var sb strings.Builder
	sb.WriteString(`<task>
Merge the existing running summary and the new partial summaries into one updated running summary.
</task>

<instructions>
Keep the exact section structure below.
Deduplicate aggressively.
Preserve unresolved items until they are resolved.
Prefer newer information when facts conflict.
Do not preserve both old and new versions of the same fact unless the change itself matters.
Keep only future-relevant context.
Do not invent facts.
Keep the result under 180 words.
</instructions>

<format>
Return Markdown with exactly these sections:
## Key Context
## Decisions
## Open Loops
## Tentative Ideas / Alternatives
## Preferences / Constraints

Use short bullet points.
"Open Loops" should contain items that still require action, resolution, confirmation, or follow-up.
Put unresolved but still potentially useful ideas, alternatives, or proposals in "Tentative Ideas / Alternatives".
"Tentative Ideas / Alternatives" should contain non-actionable options that may be useful later; do not duplicate active open loops there.
If a section is empty, write "- none".
</format>

<existing_summary>
`)
	sb.WriteString(existingSummary)
	sb.WriteString(`
</existing_summary>

<partial_summaries>
`)
	for i, summary := range partialSummaries {
		if strings.TrimSpace(summary) == "" {
			continue
		}
		fmt.Fprintf(&sb, "<summary index=\"%d\">\n%s\n</summary>\n", i+1, summary)
	}
	sb.WriteString(`</partial_summaries>`)
	return sb.String()
}

func buildDetailedCompactionPrompt(
	runningSummary string,
	batch []providers.Message,
	meta compactionNoteMetadata,
) string {
	if strings.TrimSpace(runningSummary) == "" {
		runningSummary = "(none)"
	}

	omitted := "no"
	if meta.OmittedMessages {
		omitted = "yes"
	}

	return fmt.Sprintf(`<task>
Write a detailed compaction memory note for this conversation segment.
</task>

<metadata>
<session_key>%s</session_key>
<channel>%s</channel>
<chat_id>%s</chat_id>
<source_messages>%d</source_messages>
<oversized_messages_omitted>%s</oversized_messages_omitted>
</metadata>

<instructions>
Create a faithful, high-signal summary of this segment.
Include:
- what the user wanted
- what was done or decided
- unresolved follow-ups
- notable files, commands, paths, URLs, entities, versions, and deadlines
- stable preferences or working style signals
- important corrections and changes of plan

Separate completed work from proposed work.
Separate confirmed changes from ideas that were only discussed.
Separate confirmed facts from tentative ideas when needed.
In "Artifacts Mentioned", prefer exact references and indicate whether each artifact was changed, inspected, or merely referenced if that is clear.
Omit filler and repetition.
Do not invent details.
Write in the dominant language of the conversation.
Target 250-500 words.
</instructions>

<format>
Return Markdown with exactly these sections:
## Session
## What Happened
## Decisions
## Action Items
## Open Questions
## Artifacts Mentioned
## Preferences / Working Style

Use bullet points when helpful.
If a section is empty, write "- none".
</format>

<running_summary>
%s
</running_summary>

<conversation>
%s
</conversation>`,
		meta.SessionKey,
		meta.Channel,
		meta.ChatID,
		meta.SourceMessages,
		omitted,
		runningSummary,
		formatConversationMessages(batch),
	)
}

func buildDetailedCompactionMergePrompt(
	runningSummary string,
	partialNotes []string,
	meta compactionNoteMetadata,
) string {
	if strings.TrimSpace(runningSummary) == "" {
		runningSummary = "(none)"
	}

	omitted := "no"
	if meta.OmittedMessages {
		omitted = "yes"
	}

	var sb strings.Builder
	sb.WriteString(`<task>
Merge these detailed compaction notes into one cohesive daily memory entry.
</task>

<metadata>
`)
	fmt.Fprintf(&sb, "<session_key>%s</session_key>\n", meta.SessionKey)
	fmt.Fprintf(&sb, "<channel>%s</channel>\n", meta.Channel)
	fmt.Fprintf(&sb, "<chat_id>%s</chat_id>\n", meta.ChatID)
	fmt.Fprintf(&sb, "<source_messages>%d</source_messages>\n", meta.SourceMessages)
	fmt.Fprintf(&sb, "<oversized_messages_omitted>%s</oversized_messages_omitted>\n", omitted)
	sb.WriteString(`</metadata>

<instructions>
Preserve important details, deduplicate aggressively, and prefer newer facts when notes conflict.
Maintain the exact section structure below.
Keep enough detail for a future reader to reconstruct the work without replaying the full conversation.
Do not invent facts.
Write in the dominant language of the source notes.
</instructions>

<format>
Return Markdown with exactly these sections:
## Session
## What Happened
## Decisions
## Action Items
## Open Questions
## Artifacts Mentioned
## Preferences / Working Style

Use bullet points when helpful.
If a section is empty, write "- none".
</format>

<running_summary>
`)
	sb.WriteString(runningSummary)
	sb.WriteString(`
</running_summary>

<partial_notes>
`)
	for i, note := range partialNotes {
		if strings.TrimSpace(note) == "" {
			continue
		}
		fmt.Fprintf(&sb, "<note index=\"%d\">\n%s\n</note>\n", i+1, note)
	}
	sb.WriteString(`</partial_notes>`)
	return sb.String()
}

func buildCompactionFileContent(
	timestamp time.Time,
	meta compactionNoteMetadata,
	runningSummary string,
	detail string,
) string {
	omitted := "no"
	if meta.OmittedMessages {
		omitted = "yes"
	}

	channel := meta.Channel
	if channel == "" {
		channel = "n/a"
	}
	chatID := meta.ChatID
	if chatID == "" {
		chatID = "n/a"
	}

	var sb strings.Builder
	sb.WriteString("# Compaction Summary ")
	sb.WriteString(timestamp.Format("2006-01-02 15:04:05"))
	sb.WriteString("\n\n")
	fmt.Fprintf(&sb, "- Session: `%s`\n", meta.SessionKey)
	fmt.Fprintf(&sb, "- Channel: `%s`\n", channel)
	fmt.Fprintf(&sb, "- Chat ID: `%s`\n", chatID)
	fmt.Fprintf(&sb, "- Source messages summarized: %d\n", meta.SourceMessages)
	fmt.Fprintf(&sb, "- Oversized messages omitted: %s\n", omitted)

	if strings.TrimSpace(runningSummary) != "" {
		sb.WriteString("\n## Running Summary Snapshot\n\n")
		sb.WriteString(strings.TrimSpace(runningSummary))
		sb.WriteString("\n")
	}

	sb.WriteString("\n---\n\n")
	if strings.TrimSpace(detail) != "" {
		sb.WriteString(strings.TrimSpace(detail))
		sb.WriteString("\n")
	} else {
		sb.WriteString("## Detailed Summary\n\n- unavailable; see running summary snapshot above.\n")
	}

	return sb.String()
}
