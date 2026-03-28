package agent

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/routing"
)

func TestResolveDispatchSessionKey_UsesResolvedScopeKey(t *testing.T) {
	al, _, _, _, cleanup := newTestAgentLoop(t)
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
	al, _, _, _, cleanup := newTestAgentLoop(t)
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
	select {
	case out := <-msgBus.OutboundChan():
		if out.Content != "Mock response" {
			t.Fatalf("outbound content = %q, want %q", out.Content, "Mock response")
		}
	case <-ctx.Done():
		t.Fatal("expected outbound message from replacement worker")
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
	select {
	case <-msgBus.OutboundChan():
	case <-ctx.Done():
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
	select {
	case <-msgBus.OutboundChan():
	case <-ctx.Done():
		t.Fatal("expected second outbound message after worker recreation")
	}

	al.dispatcher.Wait()
}

func TestProcessDirectWithMessage_SerializesWithInboundOnSameSession(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "agent-dispatcher-*")
	if err != nil {
		t.Fatalf("MkdirTemp() error = %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         tmpDir,
				ModelName:         "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
			},
		},
	}

	msgBus := bus.NewMessageBus()
	provider := &blockingDirectProvider{
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
		firstResp:    "first response",
		finalResp:    "second response",
	}
	al := NewAgentLoop(cfg, msgBus, provider)

	directDone := make(chan struct{})
	var directResp string
	var directErr error
	go func() {
		defer close(directDone)
		directResp, directErr = al.ProcessDirectWithMessage(context.Background(), bus.InboundMessage{
			Channel:    "telegram",
			ChatID:     "-1003717341079/17",
			SenderID:   "cron",
			Content:    "from cron",
			SessionKey: "agent:main:telegram:group:-1003717341079/17",
		})
	}()

	select {
	case <-provider.firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first direct request to start")
	}

	inbound := bus.InboundMessage{
		Channel:  "telegram",
		ChatID:   "-1003717341079/17",
		SenderID: "user-1",
		Content:  "hello",
		Peer: bus.Peer{
			Kind: "group",
			ID:   "-1003717341079/17",
		},
	}
	al.dispatcher.Dispatch(context.Background(), inbound)

	select {
	case <-msgBus.OutboundChan():
		t.Fatal("inbound message should not run before the direct request finishes")
	case <-time.After(150 * time.Millisecond):
	}

	close(provider.releaseFirst)

	select {
	case <-directDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for direct request to finish")
	}
	if directErr != nil {
		t.Fatalf("ProcessDirectWithMessage() error = %v", directErr)
	}
	if directResp != "first response" {
		t.Fatalf("direct response = %q, want %q", directResp, "first response")
	}

	select {
	case out := <-msgBus.OutboundChan():
		if out.Content != "second response" {
			t.Fatalf("outbound content = %q, want %q", out.Content, "second response")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for queued inbound response")
	}

	al.dispatcher.Wait()
}
