package seahorse

import (
	"context"
	"strings"
	"testing"
)

// ftsIntegrityCheck asks FTS5 to compare the index against its content table.
func ftsIntegrityCheck(t *testing.T, s *Store, table string) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO ` + table + ` (` + table + `) VALUES ('integrity-check')`); err != nil {
		t.Fatalf("%s integrity-check: %v", table, err)
	}
}

func countMessageMatches(t *testing.T, s *Store, pattern string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM messages_fts WHERE messages_fts MATCH ?`, pattern).
		Scan(&n); err != nil {
		t.Fatalf("match %q: %v", pattern, err)
	}
	return n
}

func triggerSQL(t *testing.T, s *Store, name string) string {
	t.Helper()
	var body string
	if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = ?`, name).
		Scan(&body); err != nil {
		t.Fatalf("read trigger %s: %v", name, err)
	}
	return body
}

// The delete path must address the index by rowid. A WHERE on an FTS5 column
// is a full scan of the index per deleted row, which made /clear of a large
// conversation hold the write lock for minutes.
func TestMessagesFTSTriggersAddressRowsByRowid(t *testing.T) {
	s := openTestStore(t)

	for _, name := range []string{"messages_ad", "messages_au"} {
		body := triggerSQL(t, s, name)
		if strings.Contains(body, "WHERE") {
			t.Errorf("%s locates FTS rows with a WHERE scan:\n%s", name, body)
		}
		if !strings.Contains(body, "'delete', old.message_id") {
			t.Errorf("%s does not use the rowid 'delete' command:\n%s", name, body)
		}
	}
	if body := triggerSQL(t, s, "messages_au"); !strings.Contains(body, "UPDATE OF content") {
		t.Errorf("messages_au fires on any column update:\n%s", body)
	}

	// External content: no second copy of every message in messages_fts_content.
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = 'messages_fts_content'`).
		Scan(&n); err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	if n != 0 {
		t.Error("messages_fts still keeps its own content copy")
	}
}

func TestMessagesFTSStaysInSyncThroughClear(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	keep, err := s.GetOrCreateConversation(ctx, "agent:keep")
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	drop, err := s.GetOrCreateConversation(ctx, "agent:drop")
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	for i := 0; i < 50; i++ {
		if _, err = s.AddMessage(ctx, keep.ConversationID, "user", "keeper payload", 3); err != nil {
			t.Fatalf("add keep message: %v", err)
		}
		if _, err = s.AddMessage(ctx, drop.ConversationID, "user", "dropped payload", 3); err != nil {
			t.Fatalf("add drop message: %v", err)
		}
	}

	if err = s.ClearConversation(ctx, drop.ConversationID); err != nil {
		t.Fatalf("ClearConversation: %v", err)
	}

	if got := countMessageMatches(t, s, "dropped"); got != 0 {
		t.Errorf("cleared messages still indexed: %d", got)
	}
	if got := countMessageMatches(t, s, "keeper"); got != 50 {
		t.Errorf("other conversation's index rows = %d, want 50", got)
	}
	ftsIntegrityCheck(t, s, "messages_fts")

	res, err := s.searchMessagesFTS(ctx, SearchInput{Pattern: "keeper", AllConversations: true})
	if err != nil {
		t.Fatalf("searchMessagesFTS: %v", err)
	}
	if len(res) != 50 {
		t.Errorf("searchMessagesFTS returned %d results, want 50", len(res))
	}
	for _, r := range res {
		if r.ConversationID != keep.ConversationID {
			t.Errorf("search hit from conversation %d, want %d", r.ConversationID, keep.ConversationID)
		}
	}
}

// Stamping channel_message_id after every send must not re-index the message;
// only a content change does.
func TestMessagesFTSReindexesOnlyOnContentChange(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	conv, err := s.GetOrCreateConversation(ctx, "agent:stamp")
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	msg, err := s.AddMessage(ctx, conv.ConversationID, "assistant", "original words", 3)
	if err != nil {
		t.Fatalf("add message: %v", err)
	}

	// Every FTS5 write appends a segment to the _data shadow table, so an
	// unchanged row count means the trigger did not touch the index.
	segments := func() int {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM messages_fts_data`).Scan(&n); err != nil {
			t.Fatalf("count messages_fts_data: %v", err)
		}
		return n
	}
	before := segments()
	if _, err := s.db.Exec(
		`UPDATE messages SET channel_message_id = 'tg:42' WHERE message_id = ?`,
		msg.ID,
	); err != nil {
		t.Fatalf("stamp channel_message_id: %v", err)
	}
	if after := segments(); after != before {
		t.Errorf("channel_message_id update wrote to messages_fts (%d -> %d segments)", before, after)
	}

	if _, err := s.db.Exec(
		`UPDATE messages SET content = 'replacement text' WHERE message_id = ?`,
		msg.ID,
	); err != nil {
		t.Fatalf("update content: %v", err)
	}
	if got := countMessageMatches(t, s, "original"); got != 0 {
		t.Errorf("old content still indexed: %d", got)
	}
	if got := countMessageMatches(t, s, "replacement"); got != 1 {
		t.Errorf("new content indexed %d times, want 1", got)
	}
	ftsIntegrityCheck(t, s, "messages_fts")
}

// A DB created before the external-content layout has messages_fts rowids that
// do not match message_id; runSchema must replace it and rebuild from messages.
func TestRunSchemaRebuildsLegacyMessagesFTS(t *testing.T) {
	db := openTestDB(t)
	if err := runSchema(db); err != nil {
		t.Fatalf("runSchema: %v", err)
	}
	s := &Store{db: db}

	legacy := []string{
		`DROP TRIGGER messages_ai`,
		`DROP TRIGGER messages_ad`,
		`DROP TRIGGER messages_au`,
		`DROP TABLE messages_fts`,
		`CREATE VIRTUAL TABLE messages_fts USING fts5(message_id, content, tokenize="trigram")`,
		`CREATE TRIGGER messages_ai AFTER INSERT ON messages BEGIN
			INSERT INTO messages_fts (message_id, content) VALUES (new.message_id, new.content);
		END`,
		`CREATE TRIGGER messages_ad AFTER DELETE ON messages BEGIN
			DELETE FROM messages_fts WHERE message_id = old.message_id;
		END`,
		`CREATE TRIGGER messages_au AFTER UPDATE ON messages BEGIN
			DELETE FROM messages_fts WHERE message_id = old.message_id;
			INSERT INTO messages_fts (message_id, content) VALUES (new.message_id, new.content);
		END`,
		`INSERT INTO conversations (session_key) VALUES ('legacy')`,
	}
	for _, q := range legacy {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("set up legacy schema: %v\n%s", err, q)
		}
	}
	// Skew the legacy rowids away from message_id, as on a long-lived DB.
	if _, err := db.Exec(`INSERT INTO messages_fts (message_id, content) VALUES (999, 'stale filler')`); err != nil {
		t.Fatalf("insert filler: %v", err)
	}
	for _, c := range []string{"alpha legacy text", "bravo legacy text", "charlie legacy text"} {
		if _, err := db.Exec(
			`INSERT INTO messages (conversation_id, role, content) VALUES (1, 'user', ?)`,
			c,
		); err != nil {
			t.Fatalf("insert legacy message: %v", err)
		}
	}

	if err := runSchema(db); err != nil {
		t.Fatalf("runSchema over legacy DB: %v", err)
	}

	var ddl string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE name = 'messages_fts'`).Scan(&ddl); err != nil {
		t.Fatalf("read messages_fts ddl: %v", err)
	}
	if !strings.Contains(ddl, "content_rowid") {
		t.Fatalf("messages_fts not migrated:\n%s", ddl)
	}
	if got := countMessageMatches(t, s, "stale"); got != 0 {
		t.Errorf("legacy-only index rows survived the rebuild: %d", got)
	}
	if got := countMessageMatches(t, s, "legacy"); got != 3 {
		t.Errorf("rebuilt index has %d legacy messages, want 3", got)
	}
	var mismatched int
	if err := db.QueryRow(`SELECT count(*) FROM messages_fts f
		JOIN messages m ON m.message_id = f.rowid
		WHERE messages_fts MATCH 'bravo' AND m.content != 'bravo legacy text'`).Scan(&mismatched); err != nil {
		t.Fatalf("join check: %v", err)
	}
	if mismatched != 0 {
		t.Errorf("rowid does not map to message_id after rebuild")
	}

	// The rebuilt index must accept deletes of pre-existing rows.
	if _, err := db.Exec(`DELETE FROM messages WHERE content = 'alpha legacy text'`); err != nil {
		t.Fatalf("delete migrated message: %v", err)
	}
	if got := countMessageMatches(t, s, "legacy"); got != 2 {
		t.Errorf("after delete, %d legacy messages indexed, want 2", got)
	}
	ftsIntegrityCheck(t, s, "messages_fts")

	// A second run is a no-op on the migrated layout.
	if err := runSchema(db); err != nil {
		t.Fatalf("runSchema on migrated DB: %v", err)
	}
	if got := countMessageMatches(t, s, "legacy"); got != 2 {
		t.Errorf("after re-run, %d legacy messages indexed, want 2", got)
	}
}

func countSummaryRows(t *testing.T, s *Store, summaryID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM summaries_fts WHERE summary_id = ?`, summaryID).
		Scan(&n); err != nil {
		t.Fatalf("count summaries_fts rows for %q: %v", summaryID, err)
	}
	return n
}

// summaries_fts keeps its own rows (summaries rowids are not VACUUM-stable),
// so its delete path must reach the row through the FTS index, not by
// scanning every row for a matching summary_id.
func TestSummariesFTSDeleteUsesIndex(t *testing.T) {
	s := openTestStore(t)

	body := strings.ReplaceAll(sqlDeleteSummaryFTS, "old.summary_id", "?")
	first := body[:strings.Index(body, ";")]
	id := "sum_18a2b3c4d5e6f708"
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN `+first, id, id, id)
	if err != nil {
		t.Fatalf("explain summaries_fts delete: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var rid, parent, notUsed int
		var detail string
		if err := rows.Scan(&rid, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		plan.WriteString(detail + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	// FTS5 reports a MATCH-driven scan as "INDEX 0:M<col>" and a rowid lookup
	// as "INDEX 0:="; a bare "INDEX 0:" is a full scan of the table.
	for _, line := range strings.Split(strings.TrimSpace(plan.String()), "\n") {
		if strings.HasSuffix(line, "VIRTUAL TABLE INDEX 0:") {
			t.Errorf("summaries_fts delete scans the whole index:\n%s", plan.String())
		}
	}
	if !strings.Contains(plan.String(), ":M") {
		t.Errorf("summaries_fts delete does not use MATCH:\n%s", plan.String())
	}

	if body := triggerSQL(t, s, "summaries_au"); !strings.Contains(body, "UPDATE OF content") {
		t.Errorf("summaries_au fires on any column update:\n%s", body)
	}
}

func TestSummariesFTSStaysInSync(t *testing.T) {
	s := openTestStore(t)

	if _, err := s.db.Exec(`INSERT INTO conversations (session_key) VALUES ('sums')`); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	ids := []string{
		"sum_18a2b3c4d5e6f701", "sum_18a2b3c4d5e6f702", "sum_18a2b3c4d5e6f703",
		`sum_"quoted"`, "ab",
	}
	for _, id := range ids {
		if _, err := s.db.Exec(`INSERT INTO summaries (summary_id, conversation_id, kind, content)
			VALUES (?, 1, 'leaf', 'summary body ' || ?)`, id, id); err != nil {
			t.Fatalf("insert summary %q: %v", id, err)
		}
	}

	// Ids sharing a prefix must not take each other's rows with them.
	if _, err := s.db.Exec(`DELETE FROM summaries WHERE summary_id = ?`, "sum_18a2b3c4d5e6f702"); err != nil {
		t.Fatalf("delete summary: %v", err)
	}
	for _, id := range ids {
		want := 1
		if id == "sum_18a2b3c4d5e6f702" {
			want = 0
		}
		if got := countSummaryRows(t, s, id); got != want {
			t.Errorf("summaries_fts rows for %q = %d, want %d", id, got, want)
		}
	}

	// Quoted and too-short-for-trigram ids are deleted too.
	for _, id := range []string{`sum_"quoted"`, "ab"} {
		if _, err := s.db.Exec(`DELETE FROM summaries WHERE summary_id = ?`, id); err != nil {
			t.Fatalf("delete summary %q: %v", id, err)
		}
		if got := countSummaryRows(t, s, id); got != 0 {
			t.Errorf("summaries_fts still has %d rows for deleted %q", got, id)
		}
	}

	segments := func() int {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM summaries_fts_data`).Scan(&n); err != nil {
			t.Fatalf("count summaries_fts_data: %v", err)
		}
		return n
	}
	before := segments()
	if _, err := s.db.Exec(
		`UPDATE summaries SET model = 'm' WHERE summary_id = ?`,
		"sum_18a2b3c4d5e6f701",
	); err != nil {
		t.Fatalf("update model: %v", err)
	}
	if after := segments(); after != before {
		t.Errorf("non-content update wrote to summaries_fts (%d -> %d segments)", before, after)
	}

	if _, err := s.db.Exec(
		`UPDATE summaries SET content = 'rewritten digest' WHERE summary_id = ?`,
		"sum_18a2b3c4d5e6f701",
	); err != nil {
		t.Fatalf("update content: %v", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM summaries_fts WHERE summaries_fts MATCH 'rewritten'`).
		Scan(&n); err != nil {
		t.Fatalf("match rewritten: %v", err)
	}
	if n != 1 {
		t.Errorf("rewritten summary indexed %d times, want 1", n)
	}
	if got := countSummaryRows(t, s, "sum_18a2b3c4d5e6f701"); got != 1 {
		t.Errorf("summaries_fts rows after content update = %d, want 1", got)
	}
	ftsIntegrityCheck(t, s, "summaries_fts")
}
