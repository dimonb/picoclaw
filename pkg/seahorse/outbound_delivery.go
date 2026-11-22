package seahorse

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	_, err = s.db.ExecContext(ctx, `INSERT INTO outbound_deliveries
 (channel,account,channel_message_id,chat_id,topic_id,session_key,content,parts,created_at)
 VALUES (?,?,?,?,?,?,?,?,?) ON CONFLICT(channel,account,channel_message_id) DO UPDATE SET
 chat_id=CASE WHEN excluded.chat_id='' THEN outbound_deliveries.chat_id ELSE excluded.chat_id END,
 topic_id=CASE WHEN excluded.topic_id='' THEN outbound_deliveries.topic_id ELSE excluded.topic_id END,
 session_key=CASE WHEN excluded.session_key='' THEN outbound_deliveries.session_key ELSE excluded.session_key END,
 content=excluded.content,parts=excluded.parts`, d.Channel, d.Account, d.MessageID, d.ChatID, d.TopicID, d.SessionKey, d.Content, string(parts), created)
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

// HasOutboundDelivery reports whether a confirmed delivery of ref exists in
// the chat. ref may be the full opaque ref or one whose last segment is the
// platform message ID (e.g. Mattermost "<channel>:<post>" for a stored
// "<channel>:<root>:<post>"). Used to allow deleting only the bot's own
// messages.
func (s *Store) HasOutboundDelivery(ctx context.Context, channel, chatID, ref string) (bool, error) {
	ref = strings.TrimSpace(ref)
	if channel == "" || chatID == "" || ref == "" {
		return false, nil
	}
	last := ref
	if i := strings.LastIndex(last, ":"); i >= 0 {
		last = last[i+1:]
	}
	chat, _, _ := strings.Cut(chatID, "/")
	ctx, cancel := context.WithTimeout(ctx, outboundReceiptTimeout)
	defer cancel()
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM outbound_deliveries
 WHERE channel=? AND (chat_id=? OR chat_id LIKE ? ESCAPE '\')
 AND (channel_message_id=? OR channel_message_id=? OR channel_message_id LIKE ? ESCAPE '\') LIMIT 1`,
		channel, chat, likeEscape(chat)+"/%", ref, last, "%:"+likeEscape(last)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
