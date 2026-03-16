package agent

import (
	"context"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/routing"
)

func TestResolveDispatchSessionKey_UsesResolvedScopeKey(t *testing.T) {
	al, _, _, _, cleanup := newTestAgentLoop(t) //nolint:dogsled
	defer cleanup()

	msg := bus.InboundMessage{
		Channel:    "cli",
		ChatID:     "direct",
		SenderID:   "cron",
		Content:    "hello",
		SessionKey: "cron-job-42",
	}

	route, _, err := al.resolveMessageRoute(msg)
	if err != nil {
		t.Fatalf("resolveMessageRoute failed: %v", err)
	}

	got := al.resolveDispatchSessionKey(msg)
	want := resolveScopeKey(route, msg.SessionKey)
	if got != want {
		t.Fatalf("resolveDispatchSessionKey() = %q, want %q", got, want)
	}
	if got == msg.SessionKey {
		t.Fatalf("expected dispatch key to ignore non-agent session key %q", msg.SessionKey)
	}
}

func TestResolveDispatchSessionKey_SystemMessagesUseDefaultMainSession(t *testing.T) {
	al, _, _, _, cleanup := newTestAgentLoop(t) //nolint:dogsled
	defer cleanup()

	got := al.resolveDispatchSessionKey(bus.InboundMessage{
		Channel:  "system",
		ChatID:   "telegram:chat-1",
		SenderID: "async:spawn",
		Content:  "Task complete",
	})

	defaultAgent := al.registry.GetDefaultAgent()
	if defaultAgent == nil {
		t.Fatal("expected default agent")
	}
	want := routing.BuildAgentMainSessionKey(defaultAgent.ID)
	if got != want {
		t.Fatalf("resolveDispatchSessionKey(system) = %q, want %q", got, want)
	}
}

func TestSessionDispatcher_DispatchReplacesClosingWorker(t *testing.T) {
	al, _, msgBus, _, cleanup := newTestAgentLoop(t)
	defer cleanup()

	msg := bus.InboundMessage{
		Channel:  "cli",
		ChatID:   "direct",
		SenderID: "user-1",
		Content:  "hello",
	}
	key := al.resolveDispatchSessionKey(msg)

	stale := &sessionWorker{
		ch:      make(chan workerTask, 1),
		closing: true,
	}

	al.dispatcher.mu.Lock()
	al.dispatcher.workers[key] = stale
	al.dispatcher.mu.Unlock()

	al.dispatcher.Dispatch(context.Background(), msg)

	al.dispatcher.mu.Lock()
	replacement := al.dispatcher.workers[key]
	al.dispatcher.mu.Unlock()
	if replacement == nil {
		t.Fatal("expected replacement worker to be registered")
	}
	if replacement == stale {
		t.Fatal("expected stale closing worker to be replaced")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, ok := msgBus.SubscribeOutbound(ctx)
	if !ok {
		t.Fatal("expected outbound message from replacement worker")
	}
	if out.Content != "Mock response" {
		t.Fatalf("outbound content = %q, want %q", out.Content, "Mock response")
	}

	al.dispatcher.Wait()
}

func TestSessionDispatcher_IdleWorkerExpiresAndNextMessageStillProcesses(t *testing.T) {
	oldTimeout := sessionWorkerIdleTimeout
	sessionWorkerIdleTimeout = 25 * time.Millisecond
	defer func() { sessionWorkerIdleTimeout = oldTimeout }()

	al, _, msgBus, _, cleanup := newTestAgentLoop(t)
	defer cleanup()

	msg := bus.InboundMessage{
		Channel:  "cli",
		ChatID:   "direct",
		SenderID: "user-1",
		Content:  "hello",
	}
	key := al.resolveDispatchSessionKey(msg)

	al.dispatcher.Dispatch(context.Background(), msg)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, ok := msgBus.SubscribeOutbound(ctx); !ok {
		t.Fatal("expected first outbound message")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		al.dispatcher.mu.Lock()
		_, ok := al.dispatcher.workers[key]
		al.dispatcher.mu.Unlock()
		if !ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	al.dispatcher.mu.Lock()
	_, ok := al.dispatcher.workers[key]
	al.dispatcher.mu.Unlock()
	if ok {
		t.Fatal("expected idle worker to be removed after timeout")
	}

	al.dispatcher.Dispatch(context.Background(), msg)
	if _, ok := msgBus.SubscribeOutbound(ctx); !ok {
		t.Fatal("expected second outbound message after worker recreation")
	}

	al.dispatcher.Wait()
}
