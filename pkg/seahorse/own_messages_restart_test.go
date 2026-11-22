package seahorse

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
)

// restartChannel is a fake channel for the Manager → store path. Send
// returns the next ID from ids; edits and deletes are recorded.
type restartChannel struct {
	name    string
	ids     []string
	edited  []string
	deleted []string
}

func (c *restartChannel) Name() string                        { return c.name }
func (c *restartChannel) Start(context.Context) error         { return nil }
func (c *restartChannel) Stop(context.Context) error          { return nil }
func (c *restartChannel) IsRunning() bool                     { return true }
func (c *restartChannel) IsAllowed(string) bool               { return true }
func (c *restartChannel) IsAllowedSender(bus.SenderInfo) bool { return true }
func (c *restartChannel) ReasoningChannelID() string          { return "" }
func (c *restartChannel) Send(context.Context, bus.OutboundMessage) ([]string, error) {
	id := c.ids[0]
	c.ids = c.ids[1:]
	return []string{id}, nil
}

func (c *restartChannel) EditMessage(_ context.Context, chatID, messageID, _ string) error {
	c.edited = append(c.edited, chatID+"|"+messageID)
	return nil
}

func (c *restartChannel) DeleteMessage(_ context.Context, chatID, messageID string) error {
	c.deleted = append(c.deleted, chatID+"|"+messageID)
	return nil
}

// telegramRefChannel canonicalizes like the Telegram channel ("chat:msg").
type telegramRefChannel struct{ restartChannel }

func (c *telegramRefChannel) CanonicalMessageRef(chatID, messageID string) string {
	parts := strings.Split(messageID, ":")
	switch len(parts) {
	case 1:
		chat, _, _ := strings.Cut(chatID, "/") // "chat/topic"
		return chat + ":" + parts[0]
	case 2:
		return messageID
	case 3:
		return parts[0] + ":" + parts[2]
	}
	return ""
}

// matrixRefChannel canonicalizes like the Matrix channel ("room event").
type matrixRefChannel struct{ restartChannel }

func (c *matrixRefChannel) CanonicalMessageRef(chatID, messageID string) string {
	if strings.HasPrefix(messageID, "$") {
		return chatID + " " + messageID
	}
	return messageID
}

// newStoreManager returns a Manager wired to the store like the agent loop
// does (agent_inject.go): the store records deliveries and answers the
// own-message lookup. A new Manager over the same store is a restart.
func newStoreManager(t *testing.T, s *Store, chType string, ch channels.Channel) *channels.Manager {
	t.Helper()
	cfg := &config.Config{Channels: config.ChannelsConfig{chType: &config.Channel{Type: chType}}}
	m, err := channels.NewManager(cfg, bus.NewMessageBus(), nil)
	if err != nil {
		t.Fatal(err)
	}
	m.RegisterChannel(chType, ch)
	m.SetOutboundRecorder(s.RecordOutboundDelivery)
	m.SetDeliveryLookup(func(ctx context.Context, channel, chatID, key string) ([]string, error) {
		return s.OutboundDeliveryRefs(ctx, channel, chatID, key, 200)
	})
	return m
}

// Review 3, fix 1: a send without a session key (cron, heartbeat,
// SendToChannel) stays deletable after a restart; a row an older version
// wrote for an edit of someone else's message does not become deletable.
func TestSessionlessSendDeletableAfterRestart(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	ch := &telegramRefChannel{restartChannel{name: "telegram", ids: []string{"77"}}}
	m := newStoreManager(t, s, "telegram", ch)
	if err := m.SendToChannel(ctx, "telegram", "-100", "cron output"); err != nil {
		t.Fatal(err)
	}
	// Written by an older version for an edit of someone else's message 88.
	if err := s.RecordOutboundDelivery(ctx, OutboundDelivery{
		Channel: "telegram", ChatID: "-100", MessageID: "-100:88", Content: "edited",
	}); err != nil {
		t.Fatal(err)
	}

	m = newStoreManager(t, s, "telegram", ch) // restart: empty in-memory record
	if err := m.DeleteMessage(ctx, "telegram", "-100", "77"); err != nil {
		t.Errorf("own sessionless send after restart: %v", err)
	}
	for _, id := range []string{"88", "-100:88"} {
		if err := m.DeleteMessage(ctx, "telegram", "-100", id); !errors.Is(err, channels.ErrNotOwnMessage) {
			t.Errorf("legacy edit row %q: err = %v, want ErrNotOwnMessage", id, err)
		}
	}
	if len(ch.deleted) != 1 || ch.deleted[0] != "-100|77" {
		t.Errorf("deleted = %v", ch.deleted)
	}
}

// Review 3, fix 2: after a restart a Matrix message older than the lookup
// limit is still found: the prefilter is the event ID, not the server name
// every event ID ends with.
func TestMatrixOwnMessageBeyondLookupLimitAfterRestart(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	const room = "!r:example.org"
	ids := []string{room + " $mine:example.org"}
	for i := range 250 {
		ids = append(ids, fmt.Sprintf("%s $e%d:example.org", room, i))
	}
	ch := &matrixRefChannel{restartChannel{name: "matrix", ids: ids}}
	m := newStoreManager(t, s, "matrix", ch)
	for range ids {
		if err := m.SendToChannel(ctx, "matrix", room, "hi"); err != nil {
			t.Fatal(err)
		}
	}
	// The first message is the oldest (sends within one second share a
	// timestamp, so make the order explicit).
	if _, err := s.db.ExecContext(ctx, `UPDATE outbound_deliveries SET created_at='2026-01-01 00:00:00'
 WHERE channel_message_id=?`, room+" $mine:example.org"); err != nil {
		t.Fatal(err)
	}

	m = newStoreManager(t, s, "matrix", ch) // restart
	for _, id := range []string{"$mine:example.org", room + " $mine:example.org"} {
		if err := m.EditMessage(ctx, "matrix", room, id, "fixed"); err != nil {
			t.Errorf("edit own old event %q after restart: %v", id, err)
		}
	}
	if err := m.DeleteMessage(ctx, "matrix", room, "$mine:example.org"); err != nil {
		t.Errorf("delete own old event after restart: %v", err)
	}
	if err := m.DeleteMessage(ctx, "matrix", room, "$theirs:example.org"); !errors.Is(err, channels.ErrNotOwnMessage) {
		t.Errorf("foreign event: err = %v, want ErrNotOwnMessage", err)
	}
}

// openLegacyDeliveryStore returns a store over a database from before the
// origin column, holding rows, then migrated (as on upgrade).
func openLegacyDeliveryStore(t *testing.T, rows string) *Store {
	t.Helper()
	db := openTestDB(t)
	if _, err := db.Exec(`CREATE TABLE outbound_deliveries (delivery_id INTEGER PRIMARY KEY AUTOINCREMENT,channel TEXT NOT NULL,account TEXT NOT NULL DEFAULT '',channel_message_id TEXT NOT NULL,chat_id TEXT NOT NULL DEFAULT '',topic_id TEXT NOT NULL DEFAULT '',session_key TEXT NOT NULL DEFAULT '',content TEXT NOT NULL DEFAULT '',parts TEXT NOT NULL DEFAULT '[]',created_at TEXT NOT NULL DEFAULT (datetime('now')),UNIQUE(channel,account,channel_message_id))`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO outbound_deliveries (channel,channel_message_id,chat_id,session_key,content) VALUES ` + rows); err != nil {
		t.Fatal(err)
	}
	if err := runSchema(db); err != nil {
		t.Fatalf("migration: %v", err)
	}
	return &Store{db: db}
}

// Review 4, fix 1: in older versions an edit of the bot's message B:45 made
// from chat A moved its row to chat A (keeping B's session), so after the
// upgrade the row is a "send" listed under A. From chat A the message must
// still be neither editable nor deletable: the channel would act in chat B.
// The bot's own messages in A stay editable and deletable.
func TestOldEditRowDoesNotOpenAnotherChat_TelegramLike(t *testing.T) {
	s := openLegacyDeliveryStore(t, `
 ('telegram','-200:45','-100','agent:main:telegram:-200','moved by an old edit'),
 ('telegram','-100:45','-100','agent:main:telegram:-100','own in A'),
 ('telegram','-100:5:46','-100/5','agent:main:telegram:-100/5','own in a topic of A')`)
	ctx := context.Background()
	ch := &telegramRefChannel{restartChannel{name: "telegram"}}
	m := newStoreManager(t, s, "telegram", ch)

	for _, id := range []string{"-200:45", "-200:7:45"} {
		if err := m.EditMessage(ctx, "telegram", "-100", id, "x"); !errors.Is(err, channels.ErrNotOwnMessage) {
			t.Errorf("edit %q from A: err = %v, want ErrNotOwnMessage", id, err)
		}
		if err := m.DeleteMessage(ctx, "telegram", "-100", id); !errors.Is(err, channels.ErrNotOwnMessage) {
			t.Errorf("delete %q from A: err = %v, want ErrNotOwnMessage", id, err)
		}
	}
	if len(ch.edited) != 0 || len(ch.deleted) != 0 {
		t.Fatalf("reached the channel: edited=%v deleted=%v", ch.edited, ch.deleted)
	}
	for _, id := range []string{"45", "-100:45"} {
		if err := m.EditMessage(ctx, "telegram", "-100", id, "fixed"); err != nil {
			t.Errorf("edit own %q in A: %v", id, err)
		}
	}
	if err := m.EditMessage(ctx, "telegram", "-100/5", "46", "fixed"); err != nil {
		t.Errorf("edit own topic message: %v", err)
	}
	if err := m.DeleteMessage(ctx, "telegram", "-100/5", "-100:5:46"); err != nil {
		t.Errorf("delete own topic message: %v", err)
	}
	if len(ch.edited) != 3 || len(ch.deleted) != 1 {
		t.Errorf("edited=%v deleted=%v", ch.edited, ch.deleted)
	}
}

// The same for Matrix "room event" references.
func TestOldEditRowDoesNotOpenAnotherChat_MatrixLike(t *testing.T) {
	s := openLegacyDeliveryStore(t, `
 ('matrix','!B:example.org $e:example.org','!A:example.org','agent:main:matrix:!B','moved by an old edit'),
 ('matrix','!A:example.org $mine:example.org','!A:example.org','agent:main:matrix:!A','own in A')`)
	ctx := context.Background()
	ch := &matrixRefChannel{restartChannel{name: "matrix"}}
	m := newStoreManager(t, s, "matrix", ch)
	const roomA = "!A:example.org"

	if err := m.EditMessage(ctx, "matrix", roomA, "!B:example.org $e:example.org", "x"); !errors.Is(
		err,
		channels.ErrNotOwnMessage,
	) {
		t.Errorf("edit B's event from A: err = %v", err)
	}
	if err := m.DeleteMessage(ctx, "matrix", roomA, "!B:example.org $e:example.org"); !errors.Is(
		err,
		channels.ErrNotOwnMessage,
	) {
		t.Errorf("delete B's event from A: err = %v", err)
	}
	if len(ch.edited) != 0 || len(ch.deleted) != 0 {
		t.Fatalf("reached the channel: edited=%v deleted=%v", ch.edited, ch.deleted)
	}
	for _, id := range []string{"$mine:example.org", roomA + " $mine:example.org"} {
		if err := m.EditMessage(ctx, "matrix", roomA, id, "fixed"); err != nil {
			t.Errorf("edit own %q: %v", id, err)
		}
	}
	if err := m.DeleteMessage(ctx, "matrix", roomA, "$mine:example.org"); err != nil {
		t.Errorf("delete own: %v", err)
	}
}
