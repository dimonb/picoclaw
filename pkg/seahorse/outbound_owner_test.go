package seahorse

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
)

func TestOutboundDeliveryRefs(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	send := bus.OutboundOriginSend
	for _, d := range []OutboundDelivery{
		{Channel: "mattermost", ChatID: "chan1", MessageID: "chan1:root1:post1", SessionKey: "s", Origin: send},
		{Channel: "telegram", ChatID: "-100/42", MessageID: "-100:42:7", SessionKey: "s", Origin: send},
		// A sessionless send (cron, heartbeat): still the bot's own.
		{Channel: "telegram", ChatID: "-200", MessageID: "-200:7", Origin: send},
		{Channel: "telegram", ChatID: "-200", MessageID: "-200:177", Origin: send},
		// An edit record, and a row of unknown origin from an older version.
		{Channel: "telegram", ChatID: "-100", MessageID: "-300:7", Content: "edited", Origin: bus.OutboundOriginEdit},
		{Channel: "telegram", ChatID: "-100", MessageID: "-400:7", Content: "edited"},
		{Channel: "matrix", ChatID: "!r:example.org", MessageID: "!r:example.org $mine:example.org", Origin: send},
		{Channel: "matrix", ChatID: "!r:example.org", MessageID: "!r:example.org $other:example.org", Origin: send},
	} {
		if err := s.RecordOutboundDelivery(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		channel, chat, key string
		want               []string
	}{
		{"mattermost", "chan1", "post1", []string{"chan1:root1:post1"}},
		{"mattermost", "chan2", "post1", nil},
		{"telegram", "-100", "7", []string{"-100:42:7"}}, // topic row matches the chat; edit/unknown rows skipped
		{"telegram", "-200", "7", []string{"-200:7"}},    // sessionless send; "-200:177" is not "7"
		{"matrix", "!r:example.org", "$mine:example.org", []string{"!r:example.org $mine:example.org"}},
		{"matrix", "!r:example.org", "$theirs:example.org", nil},
	} {
		got, err := s.OutboundDeliveryRefs(ctx, tc.channel, tc.chat, tc.key, 0)
		sort.Strings(got)
		if err != nil || len(got) != len(tc.want) || (len(got) > 0 && got[0] != tc.want[0]) {
			t.Errorf("%+v: got %v err %v", tc, got, err)
		}
	}
}

// An edit only updates the content of an existing send: it keeps the row a
// send and never moves it to another chat or session. An edit of a message
// with no send record never becomes a send.
func TestOutboundDeliveryEditKeepsOrigin(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for _, d := range []OutboundDelivery{
		{Channel: "telegram", ChatID: "-100", MessageID: "-100:7", SessionKey: "s", Content: "a", Origin: bus.OutboundOriginSend},
		{Channel: "telegram", ChatID: "-200", MessageID: "-100:7", Content: "fixed", Origin: bus.OutboundOriginEdit},
		{Channel: "telegram", ChatID: "-100", MessageID: "-100:8", Content: "x", Origin: bus.OutboundOriginEdit},
	} {
		if err := s.RecordOutboundDelivery(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	var chat, session, content, origin string
	if err := s.db.QueryRowContext(ctx, `SELECT chat_id,session_key,content,origin FROM outbound_deliveries
 WHERE channel_message_id='-100:7'`).Scan(&chat, &session, &content, &origin); err != nil {
		t.Fatal(err)
	}
	if chat != "-100" || session != "s" || content != "fixed" || origin != bus.OutboundOriginSend {
		t.Errorf("after edit: chat=%q session=%q content=%q origin=%q", chat, session, content, origin)
	}
	if got, _ := s.OutboundDeliveryRefs(ctx, "telegram", "-100", "8", 0); len(got) != 0 {
		t.Errorf("edit-only row counted as own: %v", got)
	}
}

// Upgrading a database from before the origin column: rows with a session
// key are sends; sessionless rows (old edits and old sessionless sends, which
// cannot be told apart) are not trusted.
func TestOutboundDeliveryOriginMigration(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.Exec(
		`CREATE TABLE outbound_deliveries (delivery_id INTEGER PRIMARY KEY AUTOINCREMENT,channel TEXT NOT NULL,account TEXT NOT NULL DEFAULT '',channel_message_id TEXT NOT NULL,chat_id TEXT NOT NULL DEFAULT '',topic_id TEXT NOT NULL DEFAULT '',session_key TEXT NOT NULL DEFAULT '',content TEXT NOT NULL DEFAULT '',parts TEXT NOT NULL DEFAULT '[]',created_at TEXT NOT NULL DEFAULT (datetime('now')),UNIQUE(channel,account,channel_message_id))`,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO outbound_deliveries (channel,channel_message_id,chat_id,session_key,content) VALUES
 ('telegram','-100:7','-100','agent:main:telegram:-100','sent'),
 ('telegram','-100:8','-100','','edited or cron')`,
	); err != nil {
		t.Fatal(err)
	}
	if err := runSchema(db); err != nil {
		t.Fatalf("migration: %v", err)
	}
	if err := runSchema(db); err != nil { // idempotent
		t.Fatalf("second migration: %v", err)
	}
	s := &Store{db: db}
	ctx := context.Background()
	if got, _ := s.OutboundDeliveryRefs(ctx, "telegram", "-100", "7", 0); len(got) != 1 {
		t.Errorf("session row after migration: %v, want own", got)
	}
	if got, _ := s.OutboundDeliveryRefs(ctx, "telegram", "-100", "8", 0); len(got) != 0 {
		t.Errorf("sessionless legacy row after migration: %v, want not own", got)
	}
}

// Review 4, fix 2: a record of unknown origin (a receipt an older version
// queued for an edit) must not move a send row to another chat or session.
func TestOutboundDeliveryUnknownOriginKeepsSendRow(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for _, d := range []OutboundDelivery{
		{Channel: "telegram", ChatID: "-200", MessageID: "-200:45", SessionKey: "s-B", Content: "a", Origin: bus.OutboundOriginSend},
		{Channel: "telegram", ChatID: "-100", MessageID: "-200:45", Content: "edited from A"}, // no origin
	} {
		if err := s.RecordOutboundDelivery(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	var chat, session, origin string
	if err := s.db.QueryRowContext(ctx, `SELECT chat_id,session_key,origin FROM outbound_deliveries
 WHERE channel_message_id='-200:45'`).Scan(&chat, &session, &origin); err != nil {
		t.Fatal(err)
	}
	if chat != "-200" || session != "s-B" || origin != bus.OutboundOriginSend {
		t.Errorf("after unknown-origin record: chat=%q session=%q origin=%q", chat, session, origin)
	}
	if got, _ := s.OutboundDeliveryRefs(ctx, "telegram", "-100", "45", 0); len(got) != 0 {
		t.Errorf("row moved into chat A: %v", got)
	}
}

// Review 4, fix 2: receipts queued in the journal by a version from before
// the origin field are replayed by the migration's rule: with a session key
// a send, without one unknown (not trusted).
func TestJournalReplayOfLegacyReceipts(t *testing.T) {
	s := openTestStore(t)
	dir := t.TempDir()
	j, err := openOutboundJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i, raw := range []string{
		`{"delivery":{"Channel":"telegram","ChatID":"-100","SessionKey":"s","MessageID":"-100:7","Content":"sent"},"created_at":"2026-11-01 10:00:00"}`,
		`{"delivery":{"Channel":"telegram","ChatID":"-100","MessageID":"-100:8","Content":"edited"},"created_at":"2026-11-01 10:00:01"}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%020d.json", i+1)), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := j.replay(s); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if got, _ := s.OutboundDeliveryRefs(ctx, "telegram", "-100", "7", 0); len(got) != 1 {
		t.Errorf("legacy send receipt with a session: %v, want own", got)
	}
	if got, _ := s.OutboundDeliveryRefs(ctx, "telegram", "-100", "8", 0); len(got) != 0 {
		t.Errorf("legacy receipt without a session: %v, want not own", got)
	}
}
