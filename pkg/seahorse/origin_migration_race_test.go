package seahorse

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// Review 4, fix 3: several processes opening the same database at once
// (e.g. a gateway and a CLI command) all run the origin migration. Exactly
// one adds the column; the others must see it and succeed, not fail with
// "duplicate column name".
func TestOriginMigrationConcurrentOpeners(t *testing.T) {
	for round := range 10 {
		path := filepath.Join(t.TempDir(), fmt.Sprintf("race%d.db", round))
		open := func() *sql.DB {
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			db.SetMaxOpenConns(1)
			t.Cleanup(func() { _ = db.Close() })
			for _, p := range []string{"PRAGMA journal_mode = WAL;", "PRAGMA busy_timeout = 5000;"} {
				if _, err := db.Exec(p); err != nil {
					t.Fatal(err)
				}
			}
			return db
		}
		setup := open()
		if _, err := setup.Exec(
			`CREATE TABLE outbound_deliveries (delivery_id INTEGER PRIMARY KEY AUTOINCREMENT,channel TEXT NOT NULL,account TEXT NOT NULL DEFAULT '',channel_message_id TEXT NOT NULL,chat_id TEXT NOT NULL DEFAULT '',topic_id TEXT NOT NULL DEFAULT '',session_key TEXT NOT NULL DEFAULT '',content TEXT NOT NULL DEFAULT '',parts TEXT NOT NULL DEFAULT '[]',created_at TEXT NOT NULL DEFAULT (datetime('now')),UNIQUE(channel,account,channel_message_id));
INSERT INTO outbound_deliveries (channel,channel_message_id,chat_id,session_key) VALUES ('telegram','-100:7','-100','s');`,
		); err != nil {
			t.Fatal(err)
		}
		const openers = 8
		dbs := make([]*sql.DB, openers)
		for i := range dbs {
			dbs[i] = open()
		}
		var wg sync.WaitGroup
		errs := make([]error, openers)
		start := make(chan struct{})
		for i, db := range dbs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs[i] = ensureOutboundDeliveryOriginColumn(db)
			}()
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d, opener %d: %v", round, i, err)
			}
		}
		var origin string
		if err := setup.QueryRow(`SELECT origin FROM outbound_deliveries`).Scan(&origin); err != nil ||
			origin != "send" {
			t.Fatalf("round %d: origin=%q err=%v", round, origin, err)
		}
	}
}
