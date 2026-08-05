//go:build !mipsle && !netbsd && !(freebsd && arm)

package agent

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/seahorse"
	"github.com/sipeed/picoclaw/pkg/session"
)

// bootstrapTestSessions is a SessionStore that counts history reads, so tests
// can tell a skipped reconcile from a repeated one.
type bootstrapTestSessions struct {
	session.SessionStore

	mu        sync.Mutex
	history   map[string][]providers.Message
	revisions map[string]string
	reads     map[string]int
}

func newBootstrapTestSessions() *bootstrapTestSessions {
	return &bootstrapTestSessions{
		history:   map[string][]providers.Message{},
		revisions: map[string]string{},
		reads:     map[string]int{},
	}
}

func (s *bootstrapTestSessions) put(key string, msgs []providers.Message, revision string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history[key] = msgs
	s.revisions[key] = revision
}

func (s *bootstrapTestSessions) readCount(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads[key]
}

func (s *bootstrapTestSessions) GetHistory(key string) []providers.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads[key]++
	return s.history[key]
}

func (s *bootstrapTestSessions) ListSessions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.history))
	for k := range s.history {
		keys = append(keys, k)
	}
	return keys
}

func (s *bootstrapTestSessions) HistoryRevision(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revisions[key]
}

func (s *bootstrapTestSessions) SetHistory(key string, history []providers.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history[key] = history
	s.revisions[key] = "cleared"
}

func (s *bootstrapTestSessions) SetSummary(string, string) {}

func (s *bootstrapTestSessions) Save(string) error { return nil }

func newBootstrapTestManager(t *testing.T, cfg seahorse.Config) (*seahorseContextManager, *bootstrapTestSessions) {
	t.Helper()
	if cfg.DBPath == "" {
		cfg.DBPath = t.TempDir() + "/seahorse.db"
	}
	engine, err := seahorse.NewEngine(cfg, nil)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })

	sessions := newBootstrapTestSessions()
	return &seahorseContextManager{engine: engine, sessions: sessions}, sessions
}

func bootstrapTestHistory(n int) []providers.Message {
	msgs := make([]providers.Message, 0, n)
	for i := 0; i < n; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, providers.Message{Role: role, Content: fmt.Sprintf("message %d", i)})
	}
	return msgs
}

// TestLiveSessionStoreReportsHistoryRevision keeps the sweep's skip reachable
// in production: it degrades silently to re-reading every session when the
// store the agent actually builds does not advertise the capability.
func TestLiveSessionStoreReportsHistoryRevision(t *testing.T) {
	store := initSessionStore(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })

	revStore, ok := store.(session.HistoryRevisionStore)
	if !ok {
		t.Fatalf("%T does not implement session.HistoryRevisionStore; the bootstrap "+
			"sweep would re-read every session on every startup", store)
	}

	store.AddMessage("test:revision", "user", "hello")
	first := revStore.HistoryRevision("test:revision")
	if first == "" {
		t.Fatal("HistoryRevision is empty for a session that has messages")
	}
	store.AddMessage("test:revision", "assistant", "hi")
	if second := revStore.HistoryRevision("test:revision"); second == first {
		t.Errorf("HistoryRevision did not change after an append: %q", second)
	}
}

// TestEnsureBootstrappedReconcilesBeforeFirstUse covers the guarantee the
// startup sweep used to provide by running inline: whatever a turn touches has
// already been reconciled with the JSONL, even if the sweep has not reached it.
func TestEnsureBootstrappedReconcilesBeforeFirstUse(t *testing.T) {
	mgr, sessions := newBootstrapTestManager(t, seahorse.Config{})
	ctx := context.Background()
	sessionKey := "test:not-swept-yet"
	sessions.put(sessionKey, bootstrapTestHistory(4), "rev-1")

	// No sweep has run; the turn arrives first.
	resp, err := mgr.Assemble(ctx, &AssembleRequest{SessionKey: sessionKey, HistoryBudget: 10000})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(resp.History) != 4 {
		t.Fatalf("assembled %d messages, want 4: the session was used before it was reconciled",
			len(resp.History))
	}
}

// TestEnsureBootstrappedRunsOncePerSession pins the latch: concurrent turns for
// one session must not each rebuild it.
func TestEnsureBootstrappedRunsOncePerSession(t *testing.T) {
	mgr, sessions := newBootstrapTestManager(t, seahorse.Config{})
	ctx := context.Background()
	sessionKey := "test:concurrent"
	sessions.put(sessionKey, bootstrapTestHistory(4), "rev-1")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mgr.ensureBootstrapped(ctx, sessionKey)
		}()
	}
	wg.Wait()

	if got := sessions.readCount(sessionKey); got != 1 {
		t.Errorf("history read %d times, want 1", got)
	}

	// And the sweep that arrives later skips it.
	mgr.bootstrapAllSessions(ctx)
	if got := sessions.readCount(sessionKey); got != 1 {
		t.Errorf("history read %d times after the sweep, want 1", got)
	}
}

// TestBootstrapSweepSkipsUnchangedSessions covers the cost of the sweep itself.
//
// Reading a session back means parsing its whole JSONL. A workspace that
// accumulates one archived session per cron run has thousands of them, none of
// which will ever change again — re-parsing all of them on every restart is the
// bulk of the sweep, and none of it does anything.
func TestBootstrapSweepSkipsUnchangedSessions(t *testing.T) {
	dbPath := t.TempDir() + "/seahorse.db"
	mgr, sessions := newBootstrapTestManager(t, seahorse.Config{DBPath: dbPath})
	ctx := context.Background()
	sessionKey := "test:archived"
	sessions.put(sessionKey, bootstrapTestHistory(6), "rev-1")

	mgr.bootstrapAllSessions(ctx)
	if got := sessions.readCount(sessionKey); got != 1 {
		t.Fatalf("first sweep read the history %d times, want 1", got)
	}

	// Restart: same engine state, same revision, fresh latch.
	restarted := &seahorseContextManager{engine: mgr.engine, sessions: sessions}
	restarted.bootstrapAllSessions(ctx)
	if got := sessions.readCount(sessionKey); got != 1 {
		t.Errorf("history re-read on restart (%d reads) although its revision did not change", got)
	}

	// A revision change must bring the reconcile back.
	sessions.put(sessionKey, bootstrapTestHistory(8), "rev-2")
	moved := &seahorseContextManager{engine: mgr.engine, sessions: sessions}
	moved.bootstrapAllSessions(ctx)
	if got := sessions.readCount(sessionKey); got != 2 {
		t.Errorf("history read %d times, want 2: an edited session was skipped", got)
	}

	resp, err := moved.Assemble(ctx, &AssembleRequest{SessionKey: sessionKey, HistoryBudget: 10000})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(resp.History) != 8 {
		t.Errorf("assembled %d messages, want 8: the delta was not ingested", len(resp.History))
	}
}

// TestBootstrapSweepSkipsIgnoredSessions makes ignoring a class of session
// actually cheap: no history read for something the engine would discard.
func TestBootstrapSweepSkipsIgnoredSessions(t *testing.T) {
	mgr, sessions := newBootstrapTestManager(t, seahorse.Config{
		IgnoreSessionPatterns: []string{"agent_cron-**"},
	})
	ctx := context.Background()
	sessions.put("agent_cron-abc123", bootstrapTestHistory(4), "rev-1")
	sessions.put("agent_main_telegram_group_42", bootstrapTestHistory(4), "rev-1")

	mgr.bootstrapAllSessions(ctx)

	if got := sessions.readCount("agent_cron-abc123"); got != 0 {
		t.Errorf("ignored session's history was read %d times, want 0", got)
	}
	if got := sessions.readCount("agent_main_telegram_group_42"); got != 1 {
		t.Errorf("live session's history was read %d times, want 1", got)
	}
}

// TestClearSkipsLaterBootstrap: /clear wipes both sides, so a sweep that
// reaches the session afterwards must not re-ingest the history from a JSONL
// the clear already emptied.
func TestClearSkipsLaterBootstrap(t *testing.T) {
	mgr, sessions := newBootstrapTestManager(t, seahorse.Config{})
	ctx := context.Background()
	sessionKey := "test:cleared"
	sessions.put(sessionKey, bootstrapTestHistory(4), "rev-1")

	if err := mgr.Clear(ctx, sessionKey); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	mgr.bootstrapAllSessions(ctx)

	if got := sessions.readCount(sessionKey); got != 0 {
		t.Errorf("history read %d times after Clear, want 0", got)
	}
	resp, err := mgr.Assemble(ctx, &AssembleRequest{SessionKey: sessionKey, HistoryBudget: 10000})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(resp.History) != 0 {
		t.Errorf("assembled %d messages after Clear, want 0", len(resp.History))
	}
}
