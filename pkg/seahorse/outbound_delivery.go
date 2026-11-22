package seahorse

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/providers/protocoltypes"
)

// OutboundDelivery is the durable, channel-scoped representation of a confirmed send.
// Keeping it separate from messages avoids duplicate turns and compaction loss.
type OutboundDelivery = bus.OutboundDelivery

// RecordOutboundDelivery upserts one successfully delivered native reference.
// Callers must not call this for failed sends or an empty/unknown reference.
func (s *Store) RecordOutboundDelivery(ctx context.Context, d OutboundDelivery) error {
	if d.Channel == "" || d.MessageID == "" {
		return fmt.Errorf("delivery channel and message ID are required")
	}
	if s.outboundJournal != nil {
		return s.outboundJournal.record(s, d)
	}
	ctx, cancel := context.WithTimeout(ctx, outboundReceiptTimeout)
	defer cancel()
	return s.insertOutboundDelivery(ctx, d, time.Now().UTC().Format(sqliteTimeLayout))
}

func (s *Store) insertOutboundDelivery(ctx context.Context, d OutboundDelivery, created string) error {
	// Captions belong to accepted media receipts, not to the enclosing logical
	// send. Include them in indexed text so native media is discoverable as well.
	if len(d.Parts) > 0 {
		text := []string{}
		if d.Content != "" {
			text = append(text, d.Content)
		}
		for _, part := range d.Parts {
			if caption := strings.TrimSpace(part.Caption); caption != "" && !strings.Contains(d.Content, caption) {
				text = append(text, caption)
			}
		}
		d.Content = strings.Join(text, "\n")
	}
	parts, err := json.Marshal(d.Parts)
	if err != nil {
		return fmt.Errorf("encode delivery parts: %w", err)
	}
	// A send makes the row "send" for good. Anything else (an edit, or a
	// record of unknown origin) only updates the content and never moves an
	// existing row to another chat, topic or session.
	_, err = s.db.ExecContext(ctx, `INSERT INTO outbound_deliveries
 (channel,account,channel_message_id,chat_id,topic_id,session_key,content,parts,created_at,origin)
 VALUES (?,?,?,?,?,?,?,?,?,?) ON CONFLICT(channel,account,channel_message_id) DO UPDATE SET
 chat_id=CASE WHEN excluded.chat_id='' OR excluded.origin<>'send' THEN outbound_deliveries.chat_id ELSE excluded.chat_id END,
 topic_id=CASE WHEN excluded.topic_id='' OR excluded.origin<>'send' THEN outbound_deliveries.topic_id ELSE excluded.topic_id END,
 session_key=CASE WHEN excluded.session_key='' OR excluded.origin<>'send' THEN outbound_deliveries.session_key ELSE excluded.session_key END,
 origin=CASE WHEN outbound_deliveries.origin='send' OR excluded.origin='send' THEN 'send' ELSE excluded.origin END,
 content=excluded.content,parts=excluded.parts`, d.Channel, d.Account, d.MessageID, d.ChatID, d.TopicID, d.SessionKey, d.Content, string(parts), created, d.Origin)
	return err
}

// GetMessageByChannelMessageIDScoped prefers confirmed delivery content. Empty
// channel/account are wildcards; collisions must be resolved explicitly.
func (s *Store) GetMessageByChannelMessageIDScoped(
	ctx context.Context,
	ref, channel, account string,
) (*Message, error) {
	if ref == "" {
		return nil, nil
	}
	found, err := func() (*Message, error) {
		rows, err := s.db.QueryContext(ctx, `SELECT content,parts,created_at FROM outbound_deliveries
	 WHERE channel_message_id=? AND (?='' OR channel=?) AND (?='' OR account=?) LIMIT 2`, ref, channel, channel, account, account)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var found *Message
		for rows.Next() {
			if found != nil {
				return nil, fmt.Errorf("ambiguous message reference %q; specify channel and account", ref)
			}
			var content, raw, created string
			if err = rows.Scan(&content, &raw, &created); err != nil {
				return nil, err
			}
			var parts []bus.MediaPart
			if err = json.Unmarshal([]byte(raw), &parts); err != nil {
				return nil, fmt.Errorf("decode delivery parts: %w", err)
			}
			found = &Message{Role: "assistant", Content: content, ChannelMessageID: ref}
			found.CreatedAt, _ = time.Parse(sqliteTimeLayout, created)
			for _, p := range parts {
				found.Attachments = append(
					found.Attachments,
					protocoltypes.Attachment{
						Type:        p.Type,
						Ref:         p.Ref,
						Filename:    p.Filename,
						ContentType: p.ContentType,
					},
				)
				found.Parts = append(
					found.Parts,
					MessagePart{Type: "media", MediaURI: p.Ref, MimeType: p.ContentType, Text: p.Caption},
				)
			}
		}
		err = rows.Err()
		if err != nil {
			return nil, err
		}
		return found, nil
	}()
	if err != nil {
		return nil, err
	}
	if found != nil {
		return found, nil
	}
	// Old messages have no channel/account column. Only accept a scoped
	// fallback if the readable session key itself proves that identity. Opaque
	// hashed session keys cannot be safely inferred from the reference alone.
	if channel != "" || account != "" {
		matched, lookupErr := func() (int64, error) {
			rows, err := s.db.QueryContext(
				ctx,
				`SELECT m.message_id,c.session_key FROM messages m JOIN conversations c ON c.conversation_id=m.conversation_id WHERE m.channel_message_id=? ORDER BY m.message_id`,
				ref,
			)
			if err != nil {
				return 0, err
			}
			defer rows.Close()
			var matched int64
			for rows.Next() {
				var id int64
				var session string
				if err = rows.Scan(&id, &session); err != nil {
					return 0, err
				}
				fields := strings.SplitN(session, ":", 3)
				if channel != "" && (len(fields) < 2 || fields[0] != channel) {
					continue
				}
				// Legacy keys encode channel:chat, not account: never invent an account.
				if account != "" {
					continue
				}
				if matched != 0 {
					return 0, fmt.Errorf("ambiguous legacy message reference %q", ref)
				}
				matched = id
			}
			err = rows.Err()
			if err != nil {
				return 0, err
			}
			return matched, nil
		}()
		if lookupErr != nil {
			return nil, lookupErr
		}
		if matched == 0 {
			return nil, nil
		}
		return s.GetMessageByID(ctx, matched)
	}
	return s.getLegacyMessageByChannelMessageID(ctx, ref)
}

// OutboundDeliveryRefs returns the references of the bot's own sends in the
// chat (chat_id, or chat_id/<topic>) that equal refKey or end with ":" or
// " " + refKey (Matrix "room event"),
// newest first, at most limit. The caller compares canonical forms exactly;
// refKey is only a prefilter. Only rows recorded as sends (origin "send")
// count: edits never make a message the bot's own, and rows from older
// versions without an origin are not trusted, except those the migration
// could prove to be sends (see ensureOutboundDeliveryOriginColumn).
func (s *Store) OutboundDeliveryRefs(
	ctx context.Context,
	channel, chatID, refKey string,
	limit int,
) ([]string, error) {
	refKey = strings.TrimSpace(refKey)
	chat, _, _ := strings.Cut(strings.TrimSpace(chatID), "/")
	if channel == "" || chat == "" || refKey == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 200
	}
	ctx, cancel := context.WithTimeout(ctx, outboundReceiptTimeout)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT channel_message_id FROM outbound_deliveries
 WHERE channel=? AND (chat_id=? OR chat_id LIKE ? ESCAPE '\') AND origin='send'
 AND (channel_message_id=? OR channel_message_id LIKE ? ESCAPE '\'
 OR channel_message_id LIKE ? ESCAPE '\')
 ORDER BY created_at DESC LIMIT ?`,
		channel, chat, likeEscape(chat)+"/%", refKey, "%:"+likeEscape(refKey), "% "+likeEscape(refKey), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
