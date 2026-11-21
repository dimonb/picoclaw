package seahorse

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestOutboundSearchReopenGrepExpand(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "archive.db")
	open := func() *Store {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		if err = runSchema(db); err != nil {
			t.Fatal(err)
		}
		return &Store{db: db}
	}
	s := open()
	conv, err := s.GetOrCreateConversation(ctx, "telegram:test")
	if err != nil {
		t.Fatal(err)
	}
	d := OutboundDelivery{Channel: "telegram", MessageID: "-100:42:7", SessionKey: "telegram:test", Content: "archive alpha beta"}
	if err = s.RecordOutboundDelivery(ctx, d); err != nil {
		t.Fatal(err)
	}
	m, err := s.AddMessage(ctx, conv.ConversationID, "assistant", d.Content, 5)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.UpdateMessageChannelMessageID(ctx, m.ID, d.MessageID); err != nil {
		t.Fatal(err)
	}
	// Exercise actual MATCH, including operators (not the LIKE fallback).
	rows, err := s.searchMessagesFTS(ctx, SearchInput{Mode: "match", Pattern: "alpha AND beta", ConversationID: conv.ConversationID})
	if err != nil || len(rows) != 1 || rows[0].MessageID >= 0 || rows[0].TotalCount != 1 {
		t.Fatalf("fts=%+v err=%v", rows, err)
	}
	id := rows[0].MessageID
	if err = s.db.Close(); err != nil {
		t.Fatal(err)
	}
	s = open()
	defer s.db.Close()
	r := &RetrievalEngine{store: s}
	got, err := r.Grep(ctx, GrepInput{Pattern: "alpha", Scope: "message", AllConversations: true})
	if err != nil || len(got.Messages) != 1 || got.Messages[0].ID != id {
		t.Fatalf("grep=%+v err=%v", got, err)
	}
	expanded, err := r.ExpandMessages(ctx, []int64{id})
	if err != nil || len(expanded.Messages) != 1 || expanded.Messages[0].ChannelMessageID != d.MessageID || expanded.Messages[0].Content != d.Content {
		t.Fatalf("expand=%+v err=%v", expanded, err)
	}
	d.Content = "updated gamma delta"
	if err = s.RecordOutboundDelivery(ctx, d); err != nil {
		t.Fatal(err)
	}
	rows, err = s.searchMessagesFTS(ctx, SearchInput{Mode: "match", Pattern: "gamma NOT alpha"})
	if err != nil || len(rows) != 1 || rows[0].MessageID != id {
		t.Fatalf("edit=%+v err=%v", rows, err)
	}
	old, err := s.SearchMessages(ctx, SearchInput{Pattern: "alpha"})
	if err != nil || len(old) != 0 {
		t.Fatalf("stale edit=%+v err=%v", old, err)
	}
	expanded, err = r.ExpandMessages(ctx, []int64{id})
	if err != nil || expanded.Messages[0].Content != d.Content {
		t.Fatalf("edited expand=%+v err=%v", expanded, err)
	}
	var n int
	if err = s.db.QueryRow(`SELECT count(*) FROM messages`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("history=%d err=%v", n, err)
	}
}

func TestOutboundSearchFiltersAndBackground(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	conv, err := s.GetOrCreateConversation(ctx, "known")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []OutboundDelivery{{Channel: "telegram", MessageID: "known:1", SessionKey: "known", Content: "filter needle"}, {Channel: "telegram", MessageID: "background:2", SessionKey: "not-created", Content: "filter needle"}} {
		if err = s.RecordOutboundDelivery(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().UTC().Add(-time.Hour)
	future := time.Now().UTC().Add(time.Hour)
	for _, mode := range []string{"full_text", "like"} {
		for _, tc := range []struct {
			in   SearchInput
			want int
		}{
			{SearchInput{Pattern: "needle", ConversationID: conv.ConversationID}, 1},
			{SearchInput{Pattern: "needle", ConversationID: conv.ConversationID, AllConversations: true}, 2},
			{SearchInput{Pattern: "needle", Role: "user", AllConversations: true}, 0},
			{SearchInput{Pattern: "needle", Role: "assistant", Since: &past, Before: &future, AllConversations: true}, 2},
			{SearchInput{Pattern: "needle", Since: &future, AllConversations: true}, 0},
			{SearchInput{Pattern: "needle", Before: &past, AllConversations: true}, 0},
		} {
			tc.in.Mode = mode
			rows, err := s.SearchMessages(ctx, tc.in)
			if err != nil || len(rows) != tc.want {
				t.Fatalf("mode=%s in=%+v rows=%+v err=%v", mode, tc.in, rows, err)
			}
		}
	}
	var n int
	if err = s.db.QueryRow(`SELECT count(*) FROM conversations`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("fake sessions=%d err=%v", n, err)
	}
}
