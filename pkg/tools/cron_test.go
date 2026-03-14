package tools

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/cron"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func newTestCronTool(t *testing.T) *CronTool {
	t.Helper()
	storePath := filepath.Join(t.TempDir(), "cron.json")
	cronService := cron.NewCronService(storePath, nil)
	msgBus := bus.NewMessageBus()
	cfg := config.DefaultConfig()
	tool, err := NewCronTool(cronService, nil, msgBus, t.TempDir(), true, 0, cfg)
	if err != nil {
		t.Fatalf("NewCronTool() error: %v", err)
	}
	return tool
}

// TestCronTool_CommandBlockedFromRemoteChannel verifies command scheduling is restricted to internal channels
func TestCronTool_CommandBlockedFromRemoteChannel(t *testing.T) {
	tool := newTestCronTool(t)
	ctx := WithToolContext(context.Background(), "telegram", "chat-1")
	result := tool.Execute(ctx, map[string]any{
		"action":          "add",
		"message":         "check disk",
		"command":         "df -h",
		"command_confirm": true,
		"at_seconds":      float64(60),
	})

	if !result.IsError {
		t.Fatal("expected command scheduling to be blocked from remote channel")
	}
	if !strings.Contains(result.ForLLM, "restricted to internal channels") {
		t.Errorf("expected 'restricted to internal channels', got: %s", result.ForLLM)
	}
}

// TestCronTool_CommandRequiresConfirm verifies command_confirm=true is required
func TestCronTool_CommandRequiresConfirm(t *testing.T) {
	tool := newTestCronTool(t)
	ctx := WithToolContext(context.Background(), "cli", "direct")
	result := tool.Execute(ctx, map[string]any{
		"action":     "add",
		"message":    "check disk",
		"command":    "df -h",
		"at_seconds": float64(60),
	})

	if !result.IsError {
		t.Fatal("expected error when command_confirm is missing")
	}
	if !strings.Contains(result.ForLLM, "command_confirm=true") {
		t.Errorf("expected 'command_confirm=true' message, got: %s", result.ForLLM)
	}
}

// TestCronTool_CommandAllowedFromInternalChannel verifies command scheduling works from internal channels
func TestCronTool_CommandAllowedFromInternalChannel(t *testing.T) {
	tool := newTestCronTool(t)
	ctx := WithToolContext(context.Background(), "cli", "direct")
	result := tool.Execute(ctx, map[string]any{
		"action":          "add",
		"message":         "check disk",
		"command":         "df -h",
		"command_confirm": true,
		"at_seconds":      float64(60),
	})

	if result.IsError {
		t.Fatalf(
			"expected command scheduling to succeed from internal channel, got: %s",
			result.ForLLM,
		)
	}
	if !strings.Contains(result.ForLLM, "Cron job added") {
		t.Errorf("expected 'Cron job added', got: %s", result.ForLLM)
	}
}

// TestCronTool_AddJobRequiresSessionContext verifies fail-closed when channel/chatID missing
func TestCronTool_AddJobRequiresSessionContext(t *testing.T) {
	tool := newTestCronTool(t)
	result := tool.Execute(context.Background(), map[string]any{
		"action":     "add",
		"message":    "reminder",
		"at_seconds": float64(60),
	})

	if !result.IsError {
		t.Fatal("expected error when session context is missing")
	}
	if !strings.Contains(result.ForLLM, "no session context") {
		t.Errorf("expected 'no session context' message, got: %s", result.ForLLM)
	}
}

// TestCronTool_NonCommandJobAllowedFromRemoteChannel verifies regular reminders work from any channel
func TestCronTool_NonCommandJobAllowedFromRemoteChannel(t *testing.T) {
	tool := newTestCronTool(t)
	ctx := WithToolContext(context.Background(), "telegram", "chat-1")
	result := tool.Execute(ctx, map[string]any{
		"action":     "add",
		"message":    "time to stretch",
		"at_seconds": float64(600),
	})

	if result.IsError {
		t.Fatalf(
			"expected non-command reminder to succeed from remote channel, got: %s",
			result.ForLLM,
		)
	}
}

type stubJobExecutor struct {
	called     bool
	sessionKey string
	channel    string
	chatID     string
	msg        providers.Message
}

func (s *stubJobExecutor) ProcessDirectWithChannel(
	ctx context.Context,
	content, sessionKey, channel, chatID string,
) (string, error) {
	return "", nil
}

func (s *stubJobExecutor) PublishOutboundWithHistory(
	ctx context.Context,
	sessionKey, channel, chatID string,
	msg providers.Message,
) error {
	s.called = true
	s.sessionKey = sessionKey
	s.channel = channel
	s.chatID = chatID
	s.msg = msg
	return nil
}

func TestCronTool_ExecuteJobDeliverTruePublishesWithHistory(t *testing.T) {
	tool := newTestCronTool(t)
	exec := &stubJobExecutor{}
	tool.executor = exec
	job := &cron.CronJob{
		ID:   "job1",
		Name: "reminder",
		Payload: cron.CronPayload{
			Message: "hello from cron",
			Deliver: true,
			Channel: "telegram",
			To:      "-1003717341079/17",
		},
	}

	got := tool.ExecuteJob(context.Background(), job)
	if got != "ok" {
		t.Fatalf("ExecuteJob()=%q, want ok", got)
	}
	if !exec.called {
		t.Fatal("expected PublishOutboundWithHistory to be called")
	}
	if exec.channel != "telegram" || exec.chatID != "-1003717341079/17" {
		t.Fatalf("unexpected route: %s %s", exec.channel, exec.chatID)
	}
	if exec.msg.Role != "assistant" || exec.msg.Content != "hello from cron" {
		t.Fatalf("unexpected message: %+v", exec.msg)
	}
	wantSession := "agent:main:telegram:group:-1003717341079/17"
	if exec.sessionKey != wantSession {
		t.Fatalf("sessionKey=%q, want %q", exec.sessionKey, wantSession)
	}
}
