package seahorse

import (
	"context"
	"strings"
	"testing"
)

// TestFTSAndChannelRefCoexist pins that a message stored with a
// channel_message_id is reachable by both paths simultaneously: the FTS5
// content index returns the row for a content match, and a direct ref
// lookup via GetMessageByChannelMessageID returns the same row. Today
// the wrapped <msg id="…"> form is generated only at assemble time and
// is NOT persisted into messages.content, so FTS matches the raw body
// while the ref is queried via its own column.
func TestFTSAndChannelRefCoexist(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	conv, err := s.GetOrCreateConversation(ctx, "test:fts-and-ref")
	if err != nil {
		t.Fatalf("GetOrCreateConversation: %v", err)
	}

	const (
		ref         = "telegram:chat-1:42"
		uniqueWord  = "marmalade"
		bodyContent = "the marmalade jar is on the third shelf"
	)

	added, err := s.AddMessageWithReasoning(
		ctx, conv.ConversationID, "user", bodyContent, "", ref, nil, 0,
	)
	if err != nil {
		t.Fatalf("AddMessageWithReasoning: %v", err)
	}

	got, err := s.GetMessageByChannelMessageID(ctx, ref)
	if err != nil {
		t.Fatalf("GetMessageByChannelMessageID: %v", err)
	}
	if got == nil || got.ID != added.ID {
		t.Fatalf("ref lookup returned wrong row: got=%+v want id=%d", got, added.ID)
	}

	results, err := s.SearchMessages(ctx, SearchInput{
		Pattern:        uniqueWord,
		Mode:           "full_text",
		ConversationID: conv.ConversationID,
		Limit:          10,
	})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(results) == 0 {
		t.Fatalf("FTS found no results for %q", uniqueWord)
	}

	matched := false
	for _, r := range results {
		if r.MessageID == added.ID {
			matched = true
			if !strings.Contains(r.Snippet, "marmalade") {
				t.Errorf("expected snippet to contain %q, got %q", uniqueWord, r.Snippet)
			}
			break
		}
	}
	if !matched {
		t.Errorf("FTS results did not include the row matched by channel_message_id (id=%d): %+v",
			added.ID, results)
	}
}

// TestAssembledMessageCarriesChannelRef pins that Assemble preserves the
// channel_message_id alongside the raw body. The seahorse layer is purely
// storage/retrieval — the agent layer is responsible for the LLM-visible
// `<msg id="…">…</msg>` envelope at prompt-build time.
func TestAssembledMessageCarriesChannelRef(t *testing.T) {
	engine, err := NewEngine(Config{DBPath: t.TempDir() + "/test.db"}, nil)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer engine.Close()

	ctx := context.Background()
	const sessionKey = "test:assembled-ref"
	const ref = "telegram:chat-1:42"
	const body = "raw body without any xml wrapping"

	if _, ingestErr := engine.Ingest(ctx, sessionKey, []Message{{
		Role:             "user",
		Content:          body,
		ChannelMessageID: ref,
	}}); ingestErr != nil {
		t.Fatalf("Ingest: %v", ingestErr)
	}

	got, err := engine.GetRetrieval().Store().GetMessageByChannelMessageID(ctx, ref)
	if err != nil {
		t.Fatalf("ref lookup: %v", err)
	}
	if got == nil {
		t.Fatalf("ref lookup returned nil")
	}
	if got.Content != body {
		t.Errorf("stored content was wrapped; expected raw %q, got %q", body, got.Content)
	}

	res, err := engine.Assemble(ctx, sessionKey, AssembleInput{Budget: 4096})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	var assembled *Message
	for i := range res.Messages {
		if res.Messages[i].ChannelMessageID == ref {
			assembled = &res.Messages[i]
			break
		}
	}
	if assembled == nil {
		t.Fatalf("Assemble output did not return message with ChannelMessageID=%q; got %+v", ref, res.Messages)
	}
	if assembled.Content != body {
		t.Errorf("Assemble Content = %q, want raw %q", assembled.Content, body)
	}
}
