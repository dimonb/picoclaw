package tools

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/cron"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func newTestCronToolWithConfig(t *testing.T, cfg *config.Config) *CronTool {
	t.Helper()
	storePath := filepath.Join(t.TempDir(), "cron.json")
	cronService := cron.NewCronService(storePath, nil)
	msgBus := bus.NewMessageBus()
	tool, err := NewCronTool(cronService, nil, msgBus, t.TempDir(), true, 0, cfg)
	if err != nil {
		t.Fatalf("NewCronTool() error: %v", err)
	}
	return tool
}

func newTestCronTool(t *testing.T, mutateCfg ...func(*config.Config)) *CronTool {
	t.Helper()
	cfg := config.DefaultConfig()
	for _, mutate := range mutateCfg {
		if mutate != nil {
			mutate(cfg)
		}
	}
	return newTestCronToolWithConfig(t, cfg)
}

// TestCronTool_CommandBlockedFromRemoteChannel verifies command scheduling is restricted to internal channels
func TestCronTool_CommandBlockedFromRemoteChannel(t *testing.T) {
	tool := newTestCronTool(t, func(cfg *config.Config) {
		cfg.Tools.Exec.AllowRemote = false
	})
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

func TestCronTool_CommandDoesNotRequireConfirmByDefault(t *testing.T) {
	tool := newTestCronTool(t)
	ctx := WithToolContext(context.Background(), "cli", "direct")
	result := tool.Execute(ctx, map[string]any{
		"action":     "add",
		"message":    "check disk",
		"command":    "df -h",
		"at_seconds": float64(60),
	})

	if result.IsError {
		t.Fatalf("expected command scheduling without confirm to succeed by default, got: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "Cron job added") {
		t.Errorf("expected 'Cron job added', got: %s", result.ForLLM)
	}
}


func TestCronTool_CommandRequiresConfirmWhenAllowCommandDisabled(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Tools.Cron.AllowCommand = false

	tool := newTestCronToolWithConfig(t, cfg)
	ctx := WithToolContext(context.Background(), "cli", "direct")
	result := tool.Execute(ctx, map[string]any{
		"action":     "add",
		"message":    "check disk",
		"command":    "df -h",
		"at_seconds": float64(60),
	})

	if !result.IsError {
		t.Fatal("expected command scheduling to require confirm when allow_command is disabled")
	}
	if !strings.Contains(result.ForLLM, "command_confirm=true") {
		t.Errorf("expected command_confirm requirement message, got: %s", result.ForLLM)
	}
}

func TestCronTool_CommandAllowedWithConfirmWhenAllowCommandDisabled(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Tools.Cron.AllowCommand = false

	tool := newTestCronToolWithConfig(t, cfg)
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
			"expected command scheduling with confirm to succeed when allow_command is disabled, got: %s",
			result.ForLLM,
		)
	}
	if !strings.Contains(result.ForLLM, "Cron job added") {
		t.Errorf("expected 'Cron job added', got: %s", result.ForLLM)
	}
}

func TestCronTool_CommandBlockedWhenExecDisabled(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Tools.Exec.Enabled = false

	tool := newTestCronToolWithConfig(t, cfg)
	ctx := WithToolContext(context.Background(), "cli", "direct")
	result := tool.Execute(ctx, map[string]any{
		"action":          "add",
		"message":         "check disk",
		"command":         "df -h",
		"command_confirm": true,
		"at_seconds":      float64(60),
	})

	if !result.IsError {
		t.Fatal("expected command scheduling to be blocked when exec is disabled")
	}
	if !strings.Contains(result.ForLLM, "command execution is disabled") {
		t.Errorf("expected exec disabled message, got: %s", result.ForLLM)
	}
}

// TestCronTool_CommandAllowedFromInternalChannel verifies command scheduling works from internal channels
func TestCronTool_CommandAllowedFromInternalChannel(t *testing.T) {
	tool := newTestCronTool(t, func(cfg *config.Config) {
		cfg.Tools.Exec.AllowRemote = false
	})
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

func TestCronTool_CommandAllowedFromRemoteChannelWhenAllowRemoteEnabled(t *testing.T) {
	tool := newTestCronTool(t, func(cfg *config.Config) {
		cfg.Tools.Exec.AllowRemote = true
	})
	ctx := WithToolContext(context.Background(), "telegram", "chat-1")
	result := tool.Execute(ctx, map[string]any{
		"action":          "add",
		"message":         "check disk",
		"command":         "df -h",
		"command_confirm": true,
		"at_seconds":      float64(60),
	})

	if result.IsError {
		t.Fatalf("expected command scheduling to succeed when allowRemote=true, got: %s", result.ForLLM)
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

func TestCronTool_AddJobDefaultsToAgentModeAndBindsSession(t *testing.T) {
	tool := newTestCronTool(t)
	ctx := WithToolSessionKey(
		WithToolContext(context.Background(), "telegram", "-1003717341079/17"),
		"agent:main:telegram:group:-1003717341079/17",
	)

	result := tool.Execute(ctx, map[string]any{
		"action":     "add",
		"message":    "check actions",
		"at_seconds": float64(60),
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}

	jobs := tool.cronService.ListJobs(false)
	if len(jobs) != 1 {
		t.Fatalf("jobs len=%d, want 1", len(jobs))
	}
	if jobs[0].Payload.Mode != cron.ModeAgent {
		t.Fatalf("mode=%q, want %q", jobs[0].Payload.Mode, cron.ModeAgent)
	}
	if jobs[0].Payload.SessionKey != "agent:main:telegram:group:-1003717341079/17" {
		t.Fatalf("session_key=%q", jobs[0].Payload.SessionKey)
	}
}

type stubJobExecutor struct {
	published      bool
	processed      bool
	sessionKey     string
	channel        string
	chatID         string
	msg            providers.Message
	inboundMessage bus.InboundMessage
}

func (s *stubJobExecutor) ProcessDirectWithMessage(
	ctx context.Context,
	msg bus.InboundMessage,
) (string, error) {
	s.processed = true
	s.inboundMessage = msg
	return "processed", nil
}

func (s *stubJobExecutor) PublishOutboundWithHistory(
	ctx context.Context,
	sessionKey, channel, chatID string,
	msg providers.Message,
) error {
	s.published = true
	s.sessionKey = sessionKey
	s.channel = channel
	s.chatID = chatID
	s.msg = msg
	return nil
}

func TestCronTool_ExecuteJobDirectModePublishesWithHistory(t *testing.T) {
	tool := newTestCronTool(t)
	exec := &stubJobExecutor{}
	tool.executor = exec
	job := &cron.CronJob{
		ID:   "job1",
		Name: "reminder",
		Payload: cron.CronPayload{
			Message:    "hello from cron",
			Mode:       cron.ModeDirect,
			Channel:    "telegram",
			To:         "-1003717341079/17",
			SessionKey: "agent:main:telegram:group:-1003717341079/17",
		},
	}

	got := tool.ExecuteJob(context.Background(), job)
	if got != "ok" {
		t.Fatalf("ExecuteJob()=%q, want ok", got)
	}
	if !exec.published {
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
	if exec.msg.Metadata[providers.MessageMetaSourceKind] != providers.MessageSourceCron {
		t.Fatalf("source kind=%q", exec.msg.Metadata[providers.MessageMetaSourceKind])
	}
	if exec.msg.Metadata[providers.MessageMetaDispatch] != cron.ModeDirect {
		t.Fatalf("dispatch mode=%q", exec.msg.Metadata[providers.MessageMetaDispatch])
	}
}

func TestCronTool_ExecuteJobAgentModeUsesStoredSessionKeyAndMetadata(t *testing.T) {
	tool := newTestCronTool(t)
	exec := &stubJobExecutor{}
	tool.executor = exec
	job := &cron.CronJob{
		ID:   "job2",
		Name: "check actions",
		Payload: cron.CronPayload{
			Message:    "check the actions and report",
			Mode:       cron.ModeAgent,
			Channel:    "telegram",
			To:         "-1003717341079/17",
			SessionKey: "agent:main:telegram:group:-1003717341079/17",
		},
	}

	got := tool.ExecuteJob(context.Background(), job)
	if got != "ok" {
		t.Fatalf("ExecuteJob()=%q, want ok", got)
	}
	if !exec.processed {
		t.Fatal("expected ProcessDirectWithMessage to be called")
	}
	if exec.inboundMessage.SessionKey != "agent:main:telegram:group:-1003717341079/17" {
		t.Fatalf("sessionKey=%q", exec.inboundMessage.SessionKey)
	}
	if exec.inboundMessage.Metadata[providers.MessageMetaSourceKind] != providers.MessageSourceCron {
		t.Fatalf("source kind=%q", exec.inboundMessage.Metadata[providers.MessageMetaSourceKind])
	}
	if exec.inboundMessage.Metadata[providers.MessageMetaTriggerKind] != providers.MessageTriggerCron {
		t.Fatalf("trigger kind=%q", exec.inboundMessage.Metadata[providers.MessageMetaTriggerKind])
	}
	if exec.inboundMessage.Metadata[providers.MessageMetaTriggerID] != "job2" {
		t.Fatalf("trigger id=%q", exec.inboundMessage.Metadata[providers.MessageMetaTriggerID])
	}
	if exec.inboundMessage.Metadata[providers.MessageMetaDispatch] != cron.ModeAgent {
		t.Fatalf("dispatch mode=%q", exec.inboundMessage.Metadata[providers.MessageMetaDispatch])
	}
}

func TestCronTool_ExecuteJobLegacyDeliverTrueMapsToDirectMode(t *testing.T) {
	tool := newTestCronTool(t)
	exec := &stubJobExecutor{}
	tool.executor = exec
	job := &cron.CronJob{
		ID:   "job3",
		Name: "legacy reminder",
		Payload: cron.CronPayload{
			Message:    "legacy direct reminder",
			Deliver:    boolPtr(true),
			Channel:    "telegram",
			To:         "-1003717341079",
			SessionKey: "agent:main:telegram:group:-1003717341079",
		},
	}

	got := tool.ExecuteJob(context.Background(), job)
	if got != "ok" {
		t.Fatalf("ExecuteJob()=%q, want ok", got)
	}
	if !exec.published {
		t.Fatal("expected PublishOutboundWithHistory to be called for legacy deliver=true")
	}
	if exec.msg.Metadata[providers.MessageMetaDispatch] != cron.ModeDirect {
		t.Fatalf("dispatch mode=%q", exec.msg.Metadata[providers.MessageMetaDispatch])
	}
}

func TestCronTool_ExecuteJobPublishesErrorWhenExecDisabled(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Tools.Exec.Enabled = false

	tool := newTestCronToolWithConfig(t, cfg)
	job := &cron.CronJob{}
	job.Payload.Channel = "cli"
	job.Payload.To = "direct"
	job.Payload.Command = "df -h"

	if got := tool.ExecuteJob(context.Background(), job); got != "ok" {
		t.Fatalf("ExecuteJob() = %q, want ok", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	msg, ok := tool.msgBus.SubscribeOutbound(ctx)
	if !ok {
		t.Fatal("expected outbound message")
	}
	if !strings.Contains(msg.Content, "command execution is disabled") {
		t.Fatalf("expected exec disabled message, got: %s", msg.Content)
	}
}

func boolPtr(v bool) *bool { return &v }
