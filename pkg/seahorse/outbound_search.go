package seahorse

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/providers/protocoltypes"
)

// Archive IDs are negative, keeping delivered copies out of model history.
// Resolve session keys dynamically: a background send must never create a session.
const archiveMessagesCTE = `WITH search_messages AS (
 SELECT message_id,conversation_id,role,content,created_at FROM messages m
 WHERE NOT EXISTS (SELECT 1 FROM outbound_deliveries d
 LEFT JOIN conversations c ON c.session_key=d.session_key
 WHERE m.role='assistant' AND m.channel_message_id=d.channel_message_id
 AND c.conversation_id=m.conversation_id)
 UNION ALL
 SELECT -d.delivery_id,COALESCE(c.conversation_id,0),'assistant',d.content,d.created_at
 FROM outbound_deliveries d LEFT JOIN conversations c ON c.session_key=d.session_key
)`
const archiveSearchCTE = archiveMessagesCTE + `, search_matches AS (
 SELECT rowid,bm25(messages_fts) AS rank FROM messages_fts WHERE messages_fts MATCH ?
 UNION ALL
 SELECT -rowid,bm25(outbound_deliveries_fts) AS rank FROM outbound_deliveries_fts WHERE outbound_deliveries_fts MATCH ?
)`

func ensureOutboundSearch(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Upgrade the initial archive schema while preserving existing row IDs.
	var hasID int
	if err = tx.QueryRow(`SELECT count(*) FROM pragma_table_info('outbound_deliveries') WHERE name='delivery_id'`).Scan(&hasID); err != nil {
		return err
	}
	if hasID == 0 {
		for _, q := range []string{
			`ALTER TABLE outbound_deliveries RENAME TO outbound_deliveries_legacy`,
			`CREATE TABLE outbound_deliveries (delivery_id INTEGER PRIMARY KEY AUTOINCREMENT,channel TEXT NOT NULL,account TEXT NOT NULL DEFAULT '',channel_message_id TEXT NOT NULL,chat_id TEXT NOT NULL DEFAULT '',topic_id TEXT NOT NULL DEFAULT '',session_key TEXT NOT NULL DEFAULT '',content TEXT NOT NULL DEFAULT '',parts TEXT NOT NULL DEFAULT '[]',created_at TEXT NOT NULL DEFAULT (datetime('now')),UNIQUE(channel,account,channel_message_id))`,
			`INSERT INTO outbound_deliveries SELECT rowid,* FROM outbound_deliveries_legacy`,
			`DROP TABLE outbound_deliveries_legacy`,
			`CREATE INDEX idx_outbound_delivery_ref ON outbound_deliveries(channel_message_id)`,
		} {
			if _, err = tx.Exec(q); err != nil {
				return fmt.Errorf("upgrade delivery archive: %w", err)
			}
		}
	}
	var exists int
	if err = tx.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='outbound_deliveries_fts'`).Scan(&exists); err != nil {
		return err
	}
	for _, q := range []string{
		`CREATE VIRTUAL TABLE IF NOT EXISTS outbound_deliveries_fts USING fts5(content,content='outbound_deliveries',content_rowid='delivery_id',tokenize='trigram')`,
		`CREATE TRIGGER IF NOT EXISTS outbound_search_ai AFTER INSERT ON outbound_deliveries BEGIN INSERT INTO outbound_deliveries_fts(rowid,content) VALUES(new.delivery_id,new.content); END`,
		`CREATE TRIGGER IF NOT EXISTS outbound_search_ad AFTER DELETE ON outbound_deliveries BEGIN INSERT INTO outbound_deliveries_fts(outbound_deliveries_fts,rowid,content) VALUES('delete',old.delivery_id,old.content); END`,
		`CREATE TRIGGER IF NOT EXISTS outbound_search_au AFTER UPDATE ON outbound_deliveries BEGIN INSERT INTO outbound_deliveries_fts(outbound_deliveries_fts,rowid,content) VALUES('delete',old.delivery_id,old.content); INSERT INTO outbound_deliveries_fts(rowid,content) VALUES(new.delivery_id,new.content); END`,
	} {
		if _, err = tx.Exec(q); err != nil {
			return fmt.Errorf("delivery search schema: %w", err)
		}
	}
	if exists == 0 {
		if _, err = tx.Exec(`INSERT INTO outbound_deliveries_fts(outbound_deliveries_fts) VALUES('rebuild')`); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) getArchivedDelivery(ctx context.Context, id int64) (*Message, error) {
	var m Message
	var raw, created string
	err := s.db.QueryRowContext(ctx, `SELECT -d.delivery_id,COALESCE(c.conversation_id,0),d.content,d.parts,d.created_at,d.channel_message_id FROM outbound_deliveries d LEFT JOIN conversations c ON c.session_key=d.session_key WHERE d.delivery_id=?`, id).Scan(&m.ID, &m.ConversationID, &m.Content, &raw, &created, &m.ChannelMessageID)
	if err != nil {
		return nil, err
	}
	m.Role = "assistant"
	m.CreatedAt = parseSQLiteTime(created)
	var parts []bus.MediaPart
	if err = json.Unmarshal([]byte(raw), &parts); err != nil {
		return nil, err
	}
	for _, p := range parts {
		m.Attachments = append(m.Attachments, protocoltypes.Attachment{Type: p.Type, Ref: p.Ref, Filename: p.Filename, ContentType: p.ContentType})
		m.Parts = append(m.Parts, MessagePart{Type: "media", MediaURI: p.Ref, MimeType: p.ContentType, Text: p.Caption})
	}
	m.TokenCount = EstimateMessageTokens(m)
	return &m, nil
}
