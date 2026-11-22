package channels

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
)

// Review 3, fix 5: editing follows the same rule as deleting: only the bot's
// own messages, only in the chat being handled.
func TestEditOnlyOwnMessagesInCurrentChat(t *testing.T) {
	ctx := context.Background()
	m := newTestManager()
	m.config = &config.Config{Channels: config.ChannelsConfig{"telegram": &config.Channel{Type: "telegram"}}}
	ch := &telegramLikeChannel{}
	m.channels["telegram"] = ch
	var recorded []bus.OutboundDelivery
	m.SetOutboundRecorder(func(_ context.Context, d bus.OutboundDelivery) error {
		recorded = append(recorded, d)
		return nil
	})

	// The bot's own messages: 77 in chat C (-100) and 5 in group G (-200).
	_ = m.recordTextDelivery(ctx, "telegram", bus.OutboundMessage{ChatID: "-100"}, []string{"77"})
	_ = m.recordTextDelivery(ctx, "telegram", bus.OutboundMessage{ChatID: "-200"}, []string{"5"})
	recorded = nil

	// Own message in the current chat: allowed, by bare or full reference.
	for _, id := range []string{"5", "-200:5"} {
		if err := m.EditMessage(ctx, "telegram", "-200", id, "fixed"); err != nil {
			t.Errorf("edit own %q in G: %v", id, err)
		}
	}
	// The bot's own message in another chat, while handling G: refused.
	// Someone else's message in G: refused.
	for _, id := range []string{"-100:77", "77", "-200:6"} {
		if err := m.EditMessage(ctx, "telegram", "-200", id, "x"); !errors.Is(err, ErrNotOwnMessage) {
			t.Errorf("edit %q in G: err = %v, want ErrNotOwnMessage", id, err)
		}
	}
	if len(ch.edited) != 2 || ch.edited[0] != "-200|5" || ch.edited[1] != "-200|-200:5" {
		t.Errorf("edited = %v, want only the two own edits in G", ch.edited)
	}
	// Edits are recorded as edits: they update content, never ownership.
	for _, d := range recorded {
		if d.Origin != bus.OutboundOriginEdit || d.MessageID != "-200:5" {
			t.Errorf("edit record = %+v", d)
		}
	}
}

// Review 3, fix 1: sends are recorded with origin "send", whatever the
// session (cron, heartbeat and other sessionless sends included).
func TestSendsAreRecordedAsSends(t *testing.T) {
	ctx := context.Background()
	m := newTestManager()
	m.channels["test"] = &mockChannel{}
	var recorded []bus.OutboundDelivery
	m.SetOutboundRecorder(func(_ context.Context, d bus.OutboundDelivery) error {
		recorded = append(recorded, d)
		return nil
	})
	_ = m.recordTextDelivery(ctx, "test", bus.OutboundMessage{ChatID: "c"}, []string{"c:1"})
	_ = m.recordMediaDelivery(ctx, "test", bus.OutboundMediaMessage{ChatID: "c", SessionKey: "s"}, []string{"c:2"})
	if len(recorded) != 2 {
		t.Fatalf("recorded = %+v", recorded)
	}
	for _, d := range recorded {
		if d.Origin != bus.OutboundOriginSend {
			t.Errorf("send recorded with origin %q: %+v", d.Origin, d)
		}
	}
}

// matrixRefChannel canonicalizes refs like the Matrix channel ("room event").
type matrixRefChannel struct{ deletingChannel }

func (c *matrixRefChannel) CanonicalMessageRef(chatID, messageID string) string {
	if len(messageID) > 0 && messageID[0] == '$' {
		return chatID + " " + messageID
	}
	return messageID
}

// Review 3, fix 2: the durable lookup key never cuts a Matrix event ID at
// its ':' (that yields the server name).
func TestLookupKey(t *testing.T) {
	for _, tc := range []struct {
		ch   Channel
		ref  string
		want string
	}{
		{&matrixRefChannel{}, "!r:example.org $e1:example.org", "$e1:example.org"},
		{&deletingChannel{}, "!r:example.org $e1:example.org", "$e1:example.org"},
		{&deletingChannel{}, "$e1:example.org", "$e1:example.org"},
		{&telegramLikeChannel{}, "-100:7", "7"},
		{&deletingChannel{}, "abc:def", "abc:def"},
	} {
		if got := lookupKey(tc.ch, tc.ref); got != tc.want {
			t.Errorf("lookupKey(%T, %q) = %q, want %q", tc.ch, tc.ref, got, tc.want)
		}
	}

	// Through the Manager: the lookup receives the event ID.
	m := newTestManager()
	m.channels["matrix"] = &matrixRefChannel{}
	var keys []string
	m.SetDeliveryLookup(func(_ context.Context, _, _, key string) ([]string, error) {
		keys = append(keys, key)
		return []string{"!r:example.org $mine:example.org"}, nil
	})
	if err := m.DeleteMessage(context.Background(), "matrix", "!r:example.org", "$mine:example.org"); err != nil {
		t.Errorf("own event by bare ID: %v", err)
	}
	if len(keys) != 1 || keys[0] != "$mine:example.org" {
		t.Errorf("lookup keys = %v, want [$mine:example.org]", keys)
	}
}

// Review 3, item 4: StopAll must not hold m.mu while it waits for workers to
// drain. A send in flight records its delivery under m.mu (read lock), so a
// worker finishing a send during StopAll used to deadlock it (seen under
// load as pkg/channels hanging until the test timeout).
func TestStopAllDoesNotDeadlockWithSendInFlight(t *testing.T) {
	m := newTestManager()
	started, release := make(chan struct{}), make(chan struct{})
	m.channels["slow"] = &mockChannel{sendFn: func(context.Context, bus.OutboundMessage) error {
		close(started)
		<-release
		return nil
	}}
	if err := m.StartAll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := m.SendToChannel(t.Context(), "slow", "chat-1", "hello"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not start sending")
	}

	stopped := make(chan error, 1)
	go func() { stopped <- m.StopAll(context.Background()) }()
	waitStopping(t, m)
	close(release)
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("StopAll deadlocked with a send in flight")
	}
}

// UnregisterChannel drains its worker the same way.
func TestUnregisterChannelDoesNotDeadlockWithSendInFlight(t *testing.T) {
	m := newTestManager()
	started, release := make(chan struct{}), make(chan struct{})
	m.channels["slow"] = &mockChannel{sendFn: func(context.Context, bus.OutboundMessage) error {
		close(started)
		<-release
		return nil
	}}
	if err := m.StartAll(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.StopAll(context.Background()) })
	if err := m.SendToChannel(t.Context(), "slow", "chat-1", "hello"); err != nil {
		t.Fatal(err)
	}
	<-started
	done := make(chan struct{})
	go func() { m.UnregisterChannel("slow"); close(done) }()
	waitStopping(t, m)
	close(release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("UnregisterChannel deadlocked with a send in flight")
	}
}

// waitStopping waits until a stop in progress either holds m.mu (and would
// wait for the worker under it) or has detached the workers and released it.
// Only then does the in-flight send finish and need the read lock.
func waitStopping(t *testing.T, m *Manager) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if !m.mu.TryRLock() {
			return // held by the stop
		}
		detached := len(m.workers) == 0
		m.mu.RUnlock()
		if detached {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("stop did not begin")
		}
		time.Sleep(time.Millisecond)
	}
}
