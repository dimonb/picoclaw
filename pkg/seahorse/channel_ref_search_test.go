package seahorse

import (
	"context"
	"strings"
	"testing"
	"time"
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
		ctx, conv.ConversationID, "user", bodyContent, "", "", ref, nil, nil, 0, time.Time{},
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

// TestAssembledMessageCarriesChannelRef pins that the LLM-visible Assemble
// output for a message with a channel_message_id wraps it as
// <msg id="…">…</msg>, even though messages.content stores the raw body.
// This is the path through which the LLM gets to see and re-target refs.
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

	if _, err := engine.Ingest(ctx, sessionKey, []Message{{
		Role:             "user",
		Content:          body,
		ChannelMessageID: ref,
	}}); err != nil {
		t.Fatalf("Ingest: %v", err)
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
	found := false
	wantAttr := `id="` + ref + `"`
	for _, m := range res.Messages {
		if strings.Contains(m.Content, wantAttr) && strings.Contains(m.Content, body) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Assemble output did not wrap message with %s and body %q; got %+v",
			wantAttr, body, res.Messages)
	}
}
