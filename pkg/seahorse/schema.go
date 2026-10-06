package seahorse

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/sipeed/picoclaw/pkg/logger"
)

// SQL statements for FTS5 tables with trigram tokenizer.
const (
	sqlCreateSummariesFTS = `CREATE VIRTUAL TABLE IF NOT EXISTS summaries_fts USING fts5(
		summary_id,
		content,
		tokenize="trigram"
	)`
	// messages_fts is an external-content index over messages.content keyed by
	// rowid = message_id, so the sync triggers address rows by rowid instead of
	// scanning the whole index for a matching column value.
	sqlCreateMessagesFTS = `CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
		content,
		content='messages',
		content_rowid='message_id',
		tokenize="trigram"
	)`
	// sqlDeleteSummaryFTS removes old.summary_id's row from summaries_fts
	// inside a summaries trigger.
	sqlDeleteSummaryFTS = `DELETE FROM summaries_fts WHERE rowid IN (
				SELECT rowid FROM summaries_fts
				WHERE summaries_fts MATCH 'summary_id:"' || replace(old.summary_id, '"', '""') || '"'
					AND summary_id = old.summary_id
			) AND length(old.summary_id) >= 3;
			DELETE FROM summaries_fts WHERE length(old.summary_id) < 3 AND summary_id = old.summary_id;`
	sqlCheckFTS5Available    = `CREATE VIRTUAL TABLE IF NOT EXISTS _fts5_check USING fts5(content)`
	sqlCheckTrigramAvailable = `CREATE VIRTUAL TABLE IF NOT EXISTS _trigram_check USING fts5(content, tokenize="trigram")`
	sqlDropFTS5Check         = `DROP TABLE IF EXISTS _fts5_check`
	sqlDropTrigramCheck      = `DROP TABLE IF EXISTS _trigram_check`
)

// runSchema creates or upgrades the database schema.
// All schemas are idempotent (safe to run multiple times).
func runSchema(db *sql.DB) error {
	// Check FTS5 support before creating tables
	if err := checkFTS5Support(db); err != nil {
		return fmt.Errorf("FTS5 check: %w", err)
	}

	stmts := []string{
		`CREATE TABLE IF NOT EXISTS conversations (
			conversation_id  INTEGER PRIMARY KEY AUTOINCREMENT,
			session_key      TEXT NOT NULL UNIQUE,
			history_revision TEXT NOT NULL DEFAULT '',
			created_at       TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at       TEXT NOT NULL DEFAULT (datetime('now'))
		)`,

		`CREATE TABLE IF NOT EXISTS messages (
			message_id      INTEGER PRIMARY KEY AUTOINCREMENT,
			conversation_id INTEGER NOT NULL REFERENCES conversations(conversation_id),
			role            TEXT NOT NULL,
			content         TEXT NOT NULL DEFAULT '',
			model_name      TEXT NOT NULL DEFAULT '',
			reasoning_content TEXT NOT NULL DEFAULT '',
			channel_message_id TEXT,
			metadata        TEXT,
			attachments     TEXT,
			token_count     INTEGER NOT NULL DEFAULT 0,
			created_at      TEXT NOT NULL DEFAULT (datetime('now'))
		)`,

		`CREATE TABLE IF NOT EXISTS message_parts (
			part_id     INTEGER PRIMARY KEY AUTOINCREMENT,
			message_id  INTEGER NOT NULL REFERENCES messages(message_id),
			type        TEXT NOT NULL,
			text        TEXT,
			name        TEXT,
			arguments   TEXT,
			tool_call_id TEXT,
			media_uri   TEXT,
			mime_type   TEXT,
			ordinal     INTEGER NOT NULL DEFAULT 0
		)`,

		`CREATE TABLE IF NOT EXISTS summaries (
			summary_id                TEXT PRIMARY KEY,
			conversation_id           INTEGER NOT NULL REFERENCES conversations(conversation_id),
			kind                      TEXT NOT NULL,
			depth                     INTEGER NOT NULL DEFAULT 0,
			content                   TEXT NOT NULL,
			token_count               INTEGER NOT NULL DEFAULT 0,
			earliest_at               TEXT,
			latest_at                 TEXT,
			descendant_count          INTEGER NOT NULL DEFAULT 0,
			descendant_token_count    INTEGER NOT NULL DEFAULT 0,
			source_message_token_count INTEGER NOT NULL DEFAULT 0,
			model                     TEXT,
			created_at                TEXT NOT NULL DEFAULT (datetime('now'))
		)`,

		`CREATE TABLE IF NOT EXISTS summary_parents (
			summary_id        TEXT NOT NULL,
			parent_summary_id TEXT NOT NULL,
			PRIMARY KEY (summary_id, parent_summary_id)
		)`,

		`CREATE TABLE IF NOT EXISTS summary_messages (
			summary_id TEXT NOT NULL,
			message_id INTEGER NOT NULL,
			ordinal    INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (summary_id, message_id)
		)`,

		`CREATE TABLE IF NOT EXISTS context_items (
			conversation_id INTEGER NOT NULL,
			ordinal         INTEGER NOT NULL,
			item_type       TEXT NOT NULL,
			summary_id      TEXT,
			message_id      INTEGER,
			token_count     INTEGER NOT NULL DEFAULT 0,
			created_at      TEXT NOT NULL DEFAULT (datetime('now')),
			PRIMARY KEY (conversation_id, ordinal)
		)`,

		// FTS5 virtual table with trigram tokenizer for CJK support
		sqlCreateSummariesFTS,

		// Indexes for common query patterns
		`CREATE INDEX IF NOT EXISTS idx_messages_conversation ON messages(conversation_id)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_created ON messages(conversation_id, created_at)`,
		// Parts are always looked up by message_id and returned in ordinal
		// order; without this index every lookup degrades to a full scan of
		// message_parts, which dominates startup bootstrap on large DBs.
		`CREATE INDEX IF NOT EXISTS idx_message_parts_message ON message_parts(message_id, ordinal)`,
		`CREATE INDEX IF NOT EXISTS idx_summaries_conversation ON summaries(conversation_id)`,
		`CREATE INDEX IF NOT EXISTS idx_summaries_kind_depth ON summaries(conversation_id, kind, depth)`,
		`CREATE INDEX IF NOT EXISTS idx_summary_parents_parent ON summary_parents(parent_summary_id)`,
		`CREATE INDEX IF NOT EXISTS idx_summary_messages_message ON summary_messages(message_id)`,
		`CREATE INDEX IF NOT EXISTS idx_context_items_conv ON context_items(conversation_id, ordinal)`,

		// Drop old triggers before creating new ones so existing DBs get updated bodies.
		// (CREATE TRIGGER IF NOT EXISTS does NOT replace an existing trigger body.)
		`DROP TRIGGER IF EXISTS summaries_ai`,
		`DROP TRIGGER IF EXISTS summaries_ad`,
		`DROP TRIGGER IF EXISTS summaries_au`,

		// FTS5 triggers to keep summaries_fts in sync with summaries table.
		// summaries has no INTEGER PRIMARY KEY, so its rowids are not stable
		// across VACUUM and summaries_fts keeps its own rows. A plain
		// WHERE summary_id = ... scans every FTS row; the delete instead finds
		// the row through the index with a phrase match on summary_id and then
		// filters exactly. Trigram cannot match ids shorter than 3 characters,
		// so those fall back to the scan (generated ids are far longer).
		`CREATE TRIGGER summaries_ai AFTER INSERT ON summaries BEGIN
			INSERT INTO summaries_fts (summary_id, content) VALUES (new.summary_id, new.content);
		END`,
		`CREATE TRIGGER summaries_ad AFTER DELETE ON summaries BEGIN
			` + sqlDeleteSummaryFTS + `
		END`,
		`CREATE TRIGGER summaries_au AFTER UPDATE OF content ON summaries BEGIN
			` + sqlDeleteSummaryFTS + `
			INSERT INTO summaries_fts (summary_id, content) VALUES (new.summary_id, new.content);
		END`,
	}

	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return err
		}
	}

	if err := ensureMessagesFTS(db); err != nil {
		return err
	}

	if err := ensureConversationsHistoryRevisionColumn(db); err != nil {
		return err
	}
	if err := ensureMessagesReasoningContentColumn(db); err != nil {
		return err
	}
	if err := ensureMessagesModelNameColumn(db); err != nil {
		return err
	}
	if err := ensureMessagesChannelIDColumn(db); err != nil {
		return err
	}
	if err := ensureMessagesMetadataColumn(db); err != nil {
		return err
	}
	if err := ensureMessagesAttachmentsColumn(db); err != nil {
		return err
	}
	return nil
}

// messagesFTSTriggers keep the external-content messages_fts in step with
// messages. Rows are addressed by rowid (= message_id), and an external-content
// delete must be given the exact content that was indexed, which old.content is.
// The update trigger fires only when content changes: stamping
// channel_message_id or metadata after a send must not re-index the message.
var messagesFTSTriggers = []string{
	`CREATE TRIGGER messages_ai AFTER INSERT ON messages BEGIN
		INSERT INTO messages_fts (rowid, content) VALUES (new.message_id, new.content);
	END`,
	`CREATE TRIGGER messages_ad AFTER DELETE ON messages BEGIN
		INSERT INTO messages_fts (messages_fts, rowid, content) VALUES ('delete', old.message_id, old.content);
	END`,
	`CREATE TRIGGER messages_au AFTER UPDATE OF content ON messages BEGIN
		INSERT INTO messages_fts (messages_fts, rowid, content) VALUES ('delete', old.message_id, old.content);
		INSERT INTO messages_fts (rowid, content) VALUES (new.message_id, new.content);
	END`,
}

// ensureMessagesFTS creates messages_fts as an external-content table and
// replaces the legacy layout, which stored its own copy of every message with
// message_id as a plain column: deleting one row scanned the whole index, so
// clearing a large conversation held the write lock for minutes. The legacy
// rowids do not match message_id, so the index is dropped and rebuilt from
// messages. Everything runs in one transaction: a failure leaves the old
// table and triggers in place.
func ensureMessagesFTS(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("messages_fts: begin: %w", err)
	}
	defer tx.Rollback()

	var ddl string
	err = tx.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'messages_fts'`).Scan(&ddl)
	switch {
	case err == sql.ErrNoRows:
		ddl = ""
	case err != nil:
		return fmt.Errorf("messages_fts: read schema: %w", err)
	}
	legacy := ddl != "" && !strings.Contains(ddl, "content_rowid")

	stmts := []string{
		`DROP TRIGGER IF EXISTS messages_ai`,
		`DROP TRIGGER IF EXISTS messages_ad`,
		`DROP TRIGGER IF EXISTS messages_au`,
	}
	if legacy {
		stmts = append(stmts, `DROP TABLE messages_fts`)
	}
	if ddl == "" || legacy {
		stmts = append(stmts,
			sqlCreateMessagesFTS,
			`INSERT INTO messages_fts (messages_fts) VALUES ('rebuild')`,
		)
	}
	stmts = append(stmts, messagesFTSTriggers...)

	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("messages_fts: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("messages_fts: commit: %w", err)
	}
	if legacy {
		logger.InfoCF(
			"seahorse",
			"Rebuilt messages_fts as an external-content index; run VACUUM to reclaim the space of the old copy",
			nil,
		)
	}
	return nil
}

// ensureConversationsHistoryRevisionColumn adds the marker the bootstrap sweep
// uses to skip sessions whose JSONL has not moved since it last reconciled
// them. Existing rows start empty, which reads as "unknown" and reconciles once.
func ensureConversationsHistoryRevisionColumn(db *sql.DB) error {
	hasColumn, err := tableHasColumn(db, "conversations", "history_revision")
	if err != nil {
		return fmt.Errorf("check conversations.history_revision: %w", err)
	}
	if hasColumn {
		return nil
	}
	if _, err := db.Exec(
		`ALTER TABLE conversations ADD COLUMN history_revision TEXT NOT NULL DEFAULT ''`,
	); err != nil {
		return fmt.Errorf("add conversations.history_revision: %w", err)
	}
	return nil
}

func ensureMessagesAttachmentsColumn(db *sql.DB) error {
	hasColumn, err := tableHasColumn(db, "messages", "attachments")
	if err != nil {
		return fmt.Errorf("check messages.attachments: %w", err)
	}
	if hasColumn {
		return nil
	}
	if _, err := db.Exec(`ALTER TABLE messages ADD COLUMN attachments TEXT`); err != nil {
		return fmt.Errorf("add messages.attachments: %w", err)
	}
	return nil
}

func ensureMessagesReasoningContentColumn(db *sql.DB) error {
	hasColumn, err := tableHasColumn(db, "messages", "reasoning_content")
	if err != nil {
		return fmt.Errorf("check messages.reasoning_content: %w", err)
	}
	if hasColumn {
		return nil
	}

	if _, err := db.Exec(`ALTER TABLE messages ADD COLUMN reasoning_content TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("add messages.reasoning_content: %w", err)
	}
	return nil
}

func ensureMessagesModelNameColumn(db *sql.DB) error {
	hasColumn, err := tableHasColumn(db, "messages", "model_name")
	if err != nil {
		return fmt.Errorf("check messages.model_name: %w", err)
	}
	if hasColumn {
		return nil
	}

	if _, err := db.Exec(`ALTER TABLE messages ADD COLUMN model_name TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("add messages.model_name: %w", err)
	}
	return nil
}

func ensureMessagesMetadataColumn(db *sql.DB) error {
	hasColumn, err := tableHasColumn(db, "messages", "metadata")
	if err != nil {
		return fmt.Errorf("check messages.metadata: %w", err)
	}
	if hasColumn {
		return nil
	}

	if _, err := db.Exec(`ALTER TABLE messages ADD COLUMN metadata TEXT`); err != nil {
		return fmt.Errorf("add messages.metadata: %w", err)
	}
	return nil
}

func ensureMessagesChannelIDColumn(db *sql.DB) error {
	hasColumn, err := tableHasColumn(db, "messages", "channel_message_id")
	if err != nil {
		return fmt.Errorf("check messages.channel_message_id: %w", err)
	}
	if !hasColumn {
		if _, err := db.Exec(`ALTER TABLE messages ADD COLUMN channel_message_id TEXT`); err != nil {
			return fmt.Errorf("add messages.channel_message_id: %w", err)
		}
	}
	if _, err := db.Exec(
		`CREATE INDEX IF NOT EXISTS idx_messages_channel_id ON messages(channel_message_id)`,
	); err != nil {
		return fmt.Errorf("create index idx_messages_channel_id: %w", err)
	}
	return nil
}

func tableHasColumn(db *sql.DB, tableName, columnName string) (bool, error) {
	rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, tableName))
	if err != nil {
		return false, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid        int
			name       string
			columnType string
			notNull    int
			defaultVal sql.NullString
			pk         int
		)
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultVal, &pk); err != nil {
			return false, err
		}
		if name == columnName {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return false, nil
}

// checkFTS5Support verifies that SQLite has FTS5 with trigram tokenizer enabled.
// This is required for full-text search with CJK (Chinese, Japanese, Korean) support.
func checkFTS5Support(db *sql.DB) error {
	// Check if FTS5 is compiled in
	var fts5Enabled int
	err := db.QueryRow(`SELECT sqlite_compileoption_used('ENABLE_FTS5')`).Scan(&fts5Enabled)
	if err != nil {
		// sqlite_compileoption_used might not exist in older SQLite
		// Try a different approach: create a test FTS5 table
		_, testErr := db.Exec(sqlCheckFTS5Available)
		if testErr != nil {
			return fmt.Errorf("SQLite FTS5 not available: %w (required for full-text search)", testErr)
		}
		db.Exec(sqlDropFTS5Check)
	} else if fts5Enabled == 0 {
		return fmt.Errorf("SQLite was compiled without FTS5 support (required for full-text search)")
	}

	// Check if trigram tokenizer is available by trying to create a test table
	// Not all SQLite builds include the trigram tokenizer
	_, err = db.Exec(sqlCheckTrigramAvailable)
	if err != nil {
		logger.WarnCF("seahorse", "SQLite trigram tokenizer not available, CJK search may be limited",
			map[string]any{"error": err.Error()})
		// Trigram is not strictly required, just better for CJK
		// Don't return error, just log warning
	} else {
		db.Exec(sqlDropTrigramCheck)
	}

	return nil
}
