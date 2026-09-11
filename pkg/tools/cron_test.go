package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/cron"
)

func newTestCronToolWithConfig(t *testing.T, cfg *config.Config) *CronTool {
	t.Helper()
	storePath := filepath.Join(t.TempDir(), "cron.json")
	cronService := cron.NewCronService(storePath, nil)
	msgBus := bus.NewMessageBus()
	tool, err := NewCronTool(cronService, msgBus, t.TempDir(), true, 0, cfg)
	if err != nil {
		t.Fatalf("NewCronTool() error: %v", err)
	}
	return tool
}

func newTestCronTool(t *testing.T) *CronTool {
	t.Helper()
	return newTestCronToolWithConfig(t, config.DefaultConfig())
}

func parseCronJobResult(t *testing.T, result *ToolResult) cron.CronJob {
	t.Helper()
	text := result.ForLLM
	if idx := strings.Index(text, "{"); idx >= 0 {
		text = text[idx:]
	}
	var job cron.CronJob
	if err := json.Unmarshal([]byte(text), &job); err != nil {
		t.Fatalf("failed to parse cron job JSON %q: %v", result.ForLLM, err)
	}
	return job
}

func addTestCronJob(t *testing.T, tool *CronTool, name, channel, chatID, command string) *cron.CronJob {
	t.Helper()
	everyMS := int64(60_000)
	job, err := tool.cronService.AddJob(cron.AddJobInput{
		Name:     name,
		Schedule: cron.CronSchedule{Kind: "every", EveryMS: &everyMS},
		Payload: cron.CronPayload{
			Message: name + " message",
			Command: command,
			Channel: channel,
			To:      chatID,
		},
	})
	if err != nil {
		t.Fatalf("AddJob() error: %v", err)
	}
	return job
}

// waitForInboundTrigger reads the cron trigger the tool published to the bus.
func waitForInboundTrigger(t *testing.T, tool *CronTool) bus.InboundMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	select {
	case msg := <-tool.msgBus.InboundChan():
		return msg
	case <-ctx.Done():
		t.Fatal("timeout waiting for inbound cron trigger")
		return bus.InboundMessage{}
	}
}

func waitForOutboundMessage(t *testing.T, tool *CronTool) bus.OutboundMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	select {
	case msg := <-tool.msgBus.OutboundChan():
		return msg
	case <-ctx.Done():
		t.Fatal("timeout waiting for outbound message")
		return bus.OutboundMessage{}
	}
}

func executeTestCronJob(t *testing.T, tool *CronTool, job *cron.CronJob) string {
	t.Helper()
	status, err := tool.ExecuteJob(context.Background(), job)
	if err != nil {
		t.Fatalf("ExecuteJob() error: %v", err)
	}
	return status
}

// TestCronTool_CommandBlockedFromRemoteChannel verifies command scheduling is restricted by default.
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
	if !strings.Contains(result.ForLLM, "restricted to internal channels or configured remote channels") {
		t.Errorf("expected remote restriction message, got: %s", result.ForLLM)
	}
}

func TestCronTool_CommandAllowedFromRemoteChannelAllowlist(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Tools.Cron.CommandAllowedRemotes = []string{"telegram"}

	tool := newTestCronToolWithConfig(t, cfg)
	ctx := WithToolContext(context.Background(), "telegram", "chat-1")
	result := tool.Execute(ctx, map[string]any{
		"action":     "add",
		"message":    "check disk",
		"command":    "df -h",
		"at_seconds": float64(60),
	})

	if result.IsError {
		t.Fatalf("expected command scheduling from allowed remote channel to succeed, got: %s", result.ForLLM)
	}
}

func TestCronTool_CommandAllowedFromRemoteChatIDAllowlist(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Tools.Cron.CommandAllowedRemotes = []string{" telegram:1234567890 "}

	tool := newTestCronToolWithConfig(t, cfg)
	ctx := WithToolContext(context.Background(), "telegram", "1234567890")
	result := tool.Execute(ctx, map[string]any{
		"action":     "add",
		"message":    "check disk",
		"command":    "df -h",
		"at_seconds": float64(60),
	})

	if result.IsError {
		t.Fatalf("expected command scheduling from allowed remote chat to succeed, got: %s", result.ForLLM)
	}
}

func TestCronTool_CommandAllowedFromRemoteWildcardAllowlist(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Tools.Cron.CommandAllowedRemotes = []string{"*"}

	tool := newTestCronToolWithConfig(t, cfg)
	ctx := WithToolContext(context.Background(), "telegram", "chat-1")
	result := tool.Execute(ctx, map[string]any{
		"action":     "add",
		"message":    "check disk",
		"command":    "df -h",
		"at_seconds": float64(60),
	})

	if result.IsError {
		t.Fatalf("expected wildcard allowlist to allow remote command scheduling, got: %s", result.ForLLM)
	}
}

func TestCronTool_CommandAllowedRemoteWildcardRequiresNonEmptyChannel(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Tools.Cron.CommandAllowedRemotes = []string{"*"}

	tool := newTestCronToolWithConfig(t, cfg)
	ctx := WithToolContext(context.Background(), "", "chat-1")
	result := tool.Execute(ctx, map[string]any{
		"action":     "add",
		"message":    "check disk",
		"command":    "df -h",
		"at_seconds": float64(60),
	})

	if !result.IsError {
		t.Fatal("expected missing channel to remain blocked even with wildcard allowlist")
	}
	if !strings.Contains(result.ForLLM, "no session context") {
		t.Errorf("expected session context error, got: %s", result.ForLLM)
	}
}

func TestCronTool_CommandBlockedFromDifferentRemoteChatID(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Tools.Cron.CommandAllowedRemotes = []string{"telegram:1234567890"}

	tool := newTestCronToolWithConfig(t, cfg)
	ctx := WithToolContext(context.Background(), "telegram", "other-chat")
	result := tool.Execute(ctx, map[string]any{
		"action":          "add",
		"message":         "check disk",
		"command":         "df -h",
		"command_confirm": true,
		"at_seconds":      float64(60),
	})

	if !result.IsError {
		t.Fatal("expected command scheduling from non-allowlisted remote chat to fail")
	}
	if !strings.Contains(result.ForLLM, "restricted to internal channels or configured remote channels") {
		t.Errorf("expected remote restriction message, got: %s", result.ForLLM)
	}
}

func TestCronTool_CommandAllowedRemoteRequiresConfirmWhenAllowCommandDisabled(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Tools.Cron.AllowCommand = false
	cfg.Tools.Cron.CommandAllowedRemotes = []string{"telegram"}

	tool := newTestCronToolWithConfig(t, cfg)
	ctx := WithToolContext(context.Background(), "telegram", "chat-1")
	result := tool.Execute(ctx, map[string]any{
		"action":     "add",
		"message":    "check disk",
		"command":    "df -h",
		"at_seconds": float64(60),
	})

	if !result.IsError {
		t.Fatal("expected allowlisted remote command scheduling to require confirm when allow_command is disabled")
	}
	if !strings.Contains(result.ForLLM, "command_confirm=true") {
		t.Errorf("expected command_confirm requirement message, got: %s", result.ForLLM)
	}
}

func TestCronTool_AllowCommandDoesNotBypassRemoteAllowlist(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Tools.Cron.AllowCommand = true

	tool := newTestCronToolWithConfig(t, cfg)
	ctx := WithToolContext(context.Background(), "telegram", "chat-1")
	result := tool.Execute(ctx, map[string]any{
		"action":     "add",
		"message":    "check disk",
		"command":    "df -h",
		"at_seconds": float64(60),
	})

	if !result.IsError {
		t.Fatal("expected allow_command=true not to bypass remote allowlist")
	}
	if !strings.Contains(result.ForLLM, "restricted to internal channels or configured remote channels") {
		t.Errorf("expected remote restriction message, got: %s", result.ForLLM)
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
		t.Fatalf("expected command scheduling to succeed from internal channel, got: %s", result.ForLLM)
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
		t.Fatalf("expected non-command reminder to succeed from remote channel, got: %s", result.ForLLM)
	}
}

func TestCronTool_GetReturnsFullJobPayload(t *testing.T) {
	tool := newTestCronTool(t)
	ctx := WithToolContext(context.Background(), "telegram", "chat-1")
	everyMS := int64(60_000)
	message := strings.Repeat("daily briefing details ", 8)
	job, err := tool.cronService.AddJob(cron.AddJobInput{
		Name:     "daily",
		Schedule: cron.CronSchedule{Kind: "every", EveryMS: &everyMS},
		Payload:  cron.CronPayload{Message: message, Channel: "telegram", To: "chat-1"},
	})
	if err != nil {
		t.Fatalf("AddJob() error: %v", err)
	}

	result := tool.Execute(ctx, map[string]any{
		"action": "get",
		"job_id": job.ID,
	})

	if result.IsError {
		t.Fatalf("get failed: %s", result.ForLLM)
	}
	got := parseCronJobResult(t, result)
	if got.ID != job.ID || got.Payload.Message != message || got.Payload.Channel != "telegram" ||
		got.Payload.To != "chat-1" {
		t.Fatalf("get returned wrong payload: %+v", got)
	}
	if got.Schedule.Kind != "every" || got.Schedule.EveryMS == nil || *got.Schedule.EveryMS != everyMS {
		t.Fatalf("get returned wrong schedule: %+v", got.Schedule)
	}
	if got.State.NextRunAtMS == nil {
		t.Fatal("get should include next run state")
	}
}

func TestCronTool_UpdateSchedulePreservesPayload(t *testing.T) {
	tool := newTestCronTool(t)
	ctx := WithToolContext(context.Background(), "cli", "direct")
	original, err := tool.cronService.AddJob(cron.AddJobInput{
		Name:     "AI daily",
		Schedule: cron.CronSchedule{Kind: "cron", Expr: "0 8 * * *"},
		Payload: cron.CronPayload{
			Message: "fetch RSS, include source links",
			Channel: "weixin",
			To:      "chat-1",
		},
	})
	if err != nil {
		t.Fatalf("AddJob() error: %v", err)
	}

	result := tool.Execute(ctx, map[string]any{
		"action":    "update",
		"job_id":    original.ID,
		"cron_expr": "30 10 * * *",
	})

	if result.IsError {
		t.Fatalf("update failed: %s", result.ForLLM)
	}
	updated, ok := tool.cronService.GetJob(original.ID)
	if !ok {
		t.Fatal("updated job not found")
	}
	if updated.ID != original.ID || updated.CreatedAtMS != original.CreatedAtMS {
		t.Fatalf("identity changed after update: before=%+v after=%+v", original, updated)
	}
	if updated.Payload.Message != original.Payload.Message || updated.Payload.Channel != original.Payload.Channel ||
		updated.Payload.To != original.Payload.To {
		t.Fatalf("payload was not preserved: %+v", updated.Payload)
	}
	if updated.Schedule.Kind != "cron" || updated.Schedule.Expr != "30 10 * * *" {
		t.Fatalf("schedule not updated: %+v", updated.Schedule)
	}
	if updated.DeleteAfterRun {
		t.Fatal("cron schedule should not delete after run")
	}
}

func TestCronTool_UpdateMessagePreservesScheduleAndNextRun(t *testing.T) {
	tool := newTestCronTool(t)
	ctx := WithToolContext(context.Background(), "telegram", "chat-1")
	everyMS := int64(120_000)
	original, err := tool.cronService.AddJob(cron.AddJobInput{
		Name:     "reminder",
		Schedule: cron.CronSchedule{Kind: "every", EveryMS: &everyMS},
		Payload:  cron.CronPayload{Message: "old message", Channel: "telegram", To: "chat-1"},
	})
	if err != nil {
		t.Fatalf("AddJob() error: %v", err)
	}
	if original.State.NextRunAtMS == nil {
		t.Fatal("expected original next run")
	}
	nextRunBefore := *original.State.NextRunAtMS

	result := tool.Execute(ctx, map[string]any{
		"action":  "update",
		"job_id":  original.ID,
		"message": "new message",
	})

	if result.IsError {
		t.Fatalf("update failed: %s", result.ForLLM)
	}
	updated, _ := tool.cronService.GetJob(original.ID)
	if updated.Payload.Message != "new message" {
		t.Fatalf("message not updated: %+v", updated.Payload)
	}
	if updated.Name != "reminder" {
		t.Fatalf("name should be preserved, got %q", updated.Name)
	}
	if updated.Schedule.Kind != "every" || updated.Schedule.EveryMS == nil || *updated.Schedule.EveryMS != everyMS {
		t.Fatalf("schedule should be preserved: %+v", updated.Schedule)
	}
	if updated.State.NextRunAtMS == nil || *updated.State.NextRunAtMS != nextRunBefore {
		t.Fatalf("next run should be preserved: before=%d after=%v", nextRunBefore, updated.State.NextRunAtMS)
	}
}

func TestCronTool_UpdateValidationErrors(t *testing.T) {
	tool := newTestCronTool(t)
	ctx := WithToolContext(context.Background(), "cli", "direct")
	job, err := tool.cronService.AddJob(cron.AddJobInput{
		Name:     "job",
		Schedule: cron.CronSchedule{Kind: "cron", Expr: "0 8 * * *"},
		Payload:  cron.CronPayload{Message: "message", Channel: "cli", To: "direct"},
	})
	if err != nil {
		t.Fatalf("AddJob() error: %v", err)
	}

	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{
			name: "invalid job id",
			args: map[string]any{"action": "update", "job_id": "missing", "message": "new"},
			want: "not found",
		},
		{
			name: "missing patch",
			args: map[string]any{"action": "update", "job_id": job.ID},
			want: "at least one update field",
		},
		{
			name: "multiple schedule fields",
			args: map[string]any{
				"action":        "update",
				"job_id":        job.ID,
				"every_seconds": float64(60),
				"cron_expr":     "0 9 * * *",
			},
			want: "only one of",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tool.Execute(ctx, tt.args)
			if !result.IsError {
				t.Fatalf("expected error, got: %s", result.ForLLM)
			}
			if !strings.Contains(result.ForLLM, tt.want) {
				t.Fatalf("error = %q, want substring %q", result.ForLLM, tt.want)
			}
		})
	}
}

func TestCronTool_ListFiltersJobsForRemoteChannel(t *testing.T) {
	tool := newTestCronTool(t)
	ctx := WithToolContext(context.Background(), "telegram", "chat-1")
	everyMS := int64(60_000)

	ownJob, err := tool.cronService.AddJob(cron.AddJobInput{
		Name:     "own",
		Schedule: cron.CronSchedule{Kind: "every", EveryMS: &everyMS},
		Payload:  cron.CronPayload{Message: "visible", Channel: "telegram", To: "chat-1"},
	})
	if err != nil {
		t.Fatalf("AddJob() error: %v", err)
	}
	otherChatJob, err := tool.cronService.AddJob(cron.AddJobInput{
		Name:     "other-chat",
		Schedule: cron.CronSchedule{Kind: "every", EveryMS: &everyMS},
		Payload:  cron.CronPayload{Message: "hidden", Channel: "telegram", To: "chat-2"},
	})
	if err != nil {
		t.Fatalf("AddJob() error: %v", err)
	}
	otherChannelJob, err := tool.cronService.AddJob(cron.AddJobInput{
		Name:     "other-channel",
		Schedule: cron.CronSchedule{Kind: "every", EveryMS: &everyMS},
		Payload:  cron.CronPayload{Message: "hidden", Channel: "feishu", To: "chat-1"},
	})
	if err != nil {
		t.Fatalf("AddJob() error: %v", err)
	}
	commandJob, err := tool.cronService.AddJob(cron.AddJobInput{
		Name:     "command",
		Schedule: cron.CronSchedule{Kind: "every", EveryMS: &everyMS},
		Payload:  cron.CronPayload{Message: "hidden command", Channel: "telegram", To: "chat-1"},
	})
	if err != nil {
		t.Fatalf("AddJob() error: %v", err)
	}
	commandJob.Payload.Command = "df -h"
	if err := tool.cronService.UpdateJob(commandJob); err != nil {
		t.Fatalf("UpdateJob() error: %v", err)
	}

	result := tool.Execute(ctx, map[string]any{"action": "list"})

	if result.IsError {
		t.Fatalf("list failed: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, ownJob.ID) {
		t.Fatalf("list should include own job %s, got: %s", ownJob.ID, result.ForLLM)
	}
	for _, hiddenID := range []string{otherChatJob.ID, otherChannelJob.ID, commandJob.ID} {
		if strings.Contains(result.ForLLM, hiddenID) {
			t.Fatalf("list should not include hidden job %s, got: %s", hiddenID, result.ForLLM)
		}
	}
}

func TestCronTool_RemoteCannotAccessOtherChatJob(t *testing.T) {
	tool := newTestCronTool(t)
	job, err := tool.cronService.AddJob(cron.AddJobInput{
		Name:     "private",
		Schedule: cron.CronSchedule{Kind: "cron", Expr: "0 8 * * *"},
		Payload:  cron.CronPayload{Message: "secret", Channel: "telegram", To: "chat-1"},
	})
	if err != nil {
		t.Fatalf("AddJob() error: %v", err)
	}
	ctx := WithToolContext(context.Background(), "telegram", "chat-2")

	getResult := tool.Execute(ctx, map[string]any{"action": "get", "job_id": job.ID})
	if !getResult.IsError || !strings.Contains(getResult.ForLLM, "not accessible") {
		t.Fatalf("expected inaccessible get, got: %+v", getResult)
	}

	updateResult := tool.Execute(ctx, map[string]any{"action": "update", "job_id": job.ID, "message": "changed"})
	if !updateResult.IsError || !strings.Contains(updateResult.ForLLM, "not accessible") {
		t.Fatalf("expected inaccessible update, got: %+v", updateResult)
	}
	unchanged, ok := tool.cronService.GetJob(job.ID)
	if !ok {
		t.Fatal("job should still exist")
	}
	if unchanged.Payload.Message != "secret" {
		t.Fatalf("unauthorized update mutated job: %+v", unchanged.Payload)
	}
}

func TestCronTool_RemoteCannotAccessCommandJob(t *testing.T) {
	tool := newTestCronTool(t)
	job, err := tool.cronService.AddJob(cron.AddJobInput{
		Name:     "command",
		Schedule: cron.CronSchedule{Kind: "cron", Expr: "0 8 * * *"},
		Payload:  cron.CronPayload{Message: "run command", Channel: "telegram", To: "chat-1"},
	})
	if err != nil {
		t.Fatalf("AddJob() error: %v", err)
	}
	job.Payload.Command = "df -h"
	if err := tool.cronService.UpdateJob(job); err != nil {
		t.Fatalf("UpdateJob() error: %v", err)
	}
	ctx := WithToolContext(context.Background(), "telegram", "chat-1")

	getResult := tool.Execute(ctx, map[string]any{"action": "get", "job_id": job.ID})
	if !getResult.IsError || !strings.Contains(getResult.ForLLM, "not accessible") {
		t.Fatalf("expected inaccessible get, got: %+v", getResult)
	}

	updateResult := tool.Execute(ctx, map[string]any{"action": "update", "job_id": job.ID, "message": "changed"})
	if !updateResult.IsError || !strings.Contains(updateResult.ForLLM, "not accessible") {
		t.Fatalf("expected inaccessible update, got: %+v", updateResult)
	}
	unchanged, ok := tool.cronService.GetJob(job.ID)
	if !ok {
		t.Fatal("job should still exist")
	}
	if unchanged.Payload.Message != "run command" || unchanged.Payload.Command != "df -h" {
		t.Fatalf("unauthorized update mutated command job: %+v", unchanged.Payload)
	}
}

func TestCronTool_AllowlistedRemoteCanAccessOwnCommandJob(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Tools.Cron.CommandAllowedRemotes = []string{"telegram:chat-1"}
	tool := newTestCronToolWithConfig(t, cfg)
	job := addTestCronJob(t, tool, "command", "telegram", "chat-1", "df -h")
	ctx := WithToolContext(context.Background(), "telegram", "chat-1")

	listResult := tool.Execute(ctx, map[string]any{"action": "list"})
	if listResult.IsError || !strings.Contains(listResult.ForLLM, job.ID) {
		t.Fatalf("expected list to include own command job, got: %+v", listResult)
	}

	getResult := tool.Execute(ctx, map[string]any{"action": "get", "job_id": job.ID})
	if getResult.IsError {
		t.Fatalf("expected get to access own command job, got: %s", getResult.ForLLM)
	}
	got := parseCronJobResult(t, getResult)
	if got.ID != job.ID || got.Payload.Command != "df -h" {
		t.Fatalf("get returned wrong command job: %+v", got)
	}

	updateResult := tool.Execute(ctx, map[string]any{
		"action":  "update",
		"job_id":  job.ID,
		"message": "updated command description",
	})
	if updateResult.IsError {
		t.Fatalf("expected update to access own command job, got: %s", updateResult.ForLLM)
	}
	updated, _ := tool.cronService.GetJob(job.ID)
	if updated.Payload.Message != "updated command description" || updated.Payload.Command != "df -h" {
		t.Fatalf("update returned wrong command payload: %+v", updated.Payload)
	}
}

func TestCronTool_AllowlistedRemoteCannotAccessOtherChatCommandJob(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Tools.Cron.CommandAllowedRemotes = []string{"telegram"}
	tool := newTestCronToolWithConfig(t, cfg)
	job := addTestCronJob(t, tool, "command", "telegram", "chat-2", "df -h")
	ctx := WithToolContext(context.Background(), "telegram", "chat-1")

	listResult := tool.Execute(ctx, map[string]any{"action": "list"})
	if listResult.IsError || strings.Contains(listResult.ForLLM, job.ID) {
		t.Fatalf("expected list to hide other chat command job, got: %+v", listResult)
	}

	for _, action := range []string{"get", "update"} {
		args := map[string]any{"action": action, "job_id": job.ID}
		if action == "update" {
			args["message"] = "changed"
		}
		result := tool.Execute(ctx, args)
		if !result.IsError || !strings.Contains(result.ForLLM, "not accessible") {
			t.Fatalf("expected %s to reject other chat command job, got: %+v", action, result)
		}
	}
}

func TestCronTool_NonAllowlistedRemoteCannotAccessOwnCommandJob(t *testing.T) {
	tool := newTestCronTool(t)
	job := addTestCronJob(t, tool, "command", "telegram", "chat-1", "df -h")
	ctx := WithToolContext(context.Background(), "telegram", "chat-1")

	listResult := tool.Execute(ctx, map[string]any{"action": "list"})
	if listResult.IsError || strings.Contains(listResult.ForLLM, job.ID) {
		t.Fatalf("expected list to hide non-allowlisted command job, got: %+v", listResult)
	}

	for _, action := range []string{"get", "update"} {
		args := map[string]any{"action": action, "job_id": job.ID}
		if action == "update" {
			args["message"] = "changed"
		}
		result := tool.Execute(ctx, args)
		if !result.IsError || !strings.Contains(result.ForLLM, "not accessible") {
			t.Fatalf("expected %s to reject non-allowlisted command job, got: %+v", action, result)
		}
	}
}

func TestCronTool_WildcardRemoteCanAccessOwnCommandJob(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Tools.Cron.CommandAllowedRemotes = []string{"*"}
	tool := newTestCronToolWithConfig(t, cfg)
	job := addTestCronJob(t, tool, "command", "telegram", "chat-1", "df -h")
	other := addTestCronJob(t, tool, "other", "telegram", "chat-2", "uptime")
	ctx := WithToolContext(context.Background(), "telegram", "chat-1")

	listResult := tool.Execute(ctx, map[string]any{"action": "list"})
	if listResult.IsError || !strings.Contains(listResult.ForLLM, job.ID) {
		t.Fatalf("expected wildcard list to include own command job, got: %+v", listResult)
	}
	if strings.Contains(listResult.ForLLM, other.ID) {
		t.Fatalf("wildcard list should still hide other chat job, got: %s", listResult.ForLLM)
	}

	getResult := tool.Execute(ctx, map[string]any{"action": "get", "job_id": job.ID})
	if getResult.IsError {
		t.Fatalf("expected wildcard get to access own command job, got: %s", getResult.ForLLM)
	}
}

func TestCronTool_InternalChannelCanAccessAllCommandJobs(t *testing.T) {
	tool := newTestCronTool(t)
	job := addTestCronJob(t, tool, "command", "telegram", "chat-1", "df -h")
	ctx := WithToolContext(context.Background(), "cli", "direct")

	listResult := tool.Execute(ctx, map[string]any{"action": "list"})
	if listResult.IsError || !strings.Contains(listResult.ForLLM, job.ID) {
		t.Fatalf("expected internal list to include command job, got: %+v", listResult)
	}

	getResult := tool.Execute(ctx, map[string]any{"action": "get", "job_id": job.ID})
	if getResult.IsError {
		t.Fatalf("expected internal get to access command job, got: %s", getResult.ForLLM)
	}

	updateResult := tool.Execute(ctx, map[string]any{
		"action":  "update",
		"job_id":  job.ID,
		"message": "internal update",
	})
	if updateResult.IsError {
		t.Fatalf("expected internal update to access command job, got: %s", updateResult.ForLLM)
	}
}

func TestCronTool_AllowlistedRemoteCanManageOwnCommandJob(t *testing.T) {
	for _, action := range []string{"remove", "enable", "disable"} {
		t.Run(action, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.Tools.Cron.CommandAllowedRemotes = []string{"telegram:chat-1"}
			tool := newTestCronToolWithConfig(t, cfg)
			job := addTestCronJob(t, tool, "command", "telegram", "chat-1", "df -h")
			if action == "enable" {
				tool.cronService.EnableJob(job.ID, false)
			}
			ctx := WithToolContext(context.Background(), "telegram", "chat-1")

			result := tool.Execute(ctx, map[string]any{"action": action, "job_id": job.ID})
			if result.IsError {
				t.Fatalf("expected %s to access own command job, got: %s", action, result.ForLLM)
			}

			saved, ok := tool.cronService.GetJob(job.ID)
			switch action {
			case "remove":
				if ok {
					t.Fatalf("remove should delete own command job: %+v", saved)
				}
			case "enable":
				if !ok || !saved.Enabled {
					t.Fatalf("enable should enable own command job: %+v", saved)
				}
			case "disable":
				if !ok || saved.Enabled {
					t.Fatalf("disable should disable own command job: %+v", saved)
				}
			}
		})
	}
}

func TestCronTool_RemoteCannotManageOtherChatJob(t *testing.T) {
	for _, action := range []string{"remove", "enable", "disable"} {
		t.Run(action, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.Tools.Cron.CommandAllowedRemotes = []string{"telegram"}
			tool := newTestCronToolWithConfig(t, cfg)
			job := addTestCronJob(t, tool, "command", "telegram", "chat-2", "df -h")
			ctx := WithToolContext(context.Background(), "telegram", "chat-1")

			result := tool.Execute(ctx, map[string]any{"action": action, "job_id": job.ID})
			if !result.IsError || !strings.Contains(result.ForLLM, "not accessible") {
				t.Fatalf("expected %s to reject other chat job, got: %+v", action, result)
			}

			saved, ok := tool.cronService.GetJob(job.ID)
			if !ok {
				t.Fatalf("%s should not remove other chat job", action)
			}
			if !saved.Enabled {
				t.Fatalf("%s should not disable other chat job: %+v", action, saved)
			}
		})
	}
}

func TestCronTool_RemoteCannotManageCommandJobUnlessAllowlisted(t *testing.T) {
	for _, action := range []string{"remove", "enable", "disable"} {
		t.Run(action, func(t *testing.T) {
			tool := newTestCronTool(t)
			job := addTestCronJob(t, tool, "command", "telegram", "chat-1", "df -h")
			ctx := WithToolContext(context.Background(), "telegram", "chat-1")

			result := tool.Execute(ctx, map[string]any{"action": action, "job_id": job.ID})
			if !result.IsError || !strings.Contains(result.ForLLM, "not accessible") {
				t.Fatalf("expected %s to reject non-allowlisted command job, got: %+v", action, result)
			}

			saved, ok := tool.cronService.GetJob(job.ID)
			if !ok {
				t.Fatalf("%s should not remove non-allowlisted command job", action)
			}
			if !saved.Enabled {
				t.Fatalf("%s should not disable non-allowlisted command job: %+v", action, saved)
			}
		})
	}
}

func TestCronTool_InternalChannelCanManageAllJobs(t *testing.T) {
	for _, action := range []string{"remove", "enable", "disable"} {
		t.Run(action, func(t *testing.T) {
			tool := newTestCronTool(t)
			job := addTestCronJob(t, tool, "command", "telegram", "chat-1", "df -h")
			if action == "enable" {
				tool.cronService.EnableJob(job.ID, false)
			}
			ctx := WithToolContext(context.Background(), "cli", "direct")

			result := tool.Execute(ctx, map[string]any{"action": action, "job_id": job.ID})
			if result.IsError {
				t.Fatalf("expected internal %s to access command job, got: %s", action, result.ForLLM)
			}

			saved, ok := tool.cronService.GetJob(job.ID)
			switch action {
			case "remove":
				if ok {
					t.Fatalf("internal remove should delete command job: %+v", saved)
				}
			case "enable":
				if !ok || !saved.Enabled {
					t.Fatalf("internal enable should enable command job: %+v", saved)
				}
			case "disable":
				if !ok || saved.Enabled {
					t.Fatalf("internal disable should disable command job: %+v", saved)
				}
			}
		})
	}
}

func TestCronTool_RemoteCanManageOwnNonCommandJob(t *testing.T) {
	for _, action := range []string{"remove", "enable", "disable"} {
		t.Run(action, func(t *testing.T) {
			tool := newTestCronTool(t)
			job := addTestCronJob(t, tool, "reminder", "telegram", "chat-1", "")
			if action == "enable" {
				tool.cronService.EnableJob(job.ID, false)
			}
			ctx := WithToolContext(context.Background(), "telegram", "chat-1")

			result := tool.Execute(ctx, map[string]any{"action": action, "job_id": job.ID})
			if result.IsError {
				t.Fatalf("expected %s to access own non-command job, got: %s", action, result.ForLLM)
			}
		})
	}
}

func TestCronTool_WildcardRemoteCanManageOwnCommandJob(t *testing.T) {
	for _, action := range []string{"remove", "enable", "disable"} {
		t.Run(action, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.Tools.Cron.CommandAllowedRemotes = []string{"*"}
			tool := newTestCronToolWithConfig(t, cfg)
			job := addTestCronJob(t, tool, "command", "telegram", "chat-1", "df -h")
			if action == "enable" {
				tool.cronService.EnableJob(job.ID, false)
			}
			other := addTestCronJob(t, tool, "other", "telegram", "chat-2", "uptime")
			ctx := WithToolContext(context.Background(), "telegram", "chat-1")

			result := tool.Execute(ctx, map[string]any{"action": action, "job_id": job.ID})
			if result.IsError {
				t.Fatalf("expected wildcard %s to access own command job, got: %s", action, result.ForLLM)
			}

			otherResult := tool.Execute(ctx, map[string]any{"action": action, "job_id": other.ID})
			if !otherResult.IsError || !strings.Contains(otherResult.ForLLM, "not accessible") {
				t.Fatalf("wildcard %s should still reject other chat job, got: %+v", action, otherResult)
			}
		})
	}
}

func TestCronTool_CommandUpdateSafetyGates(t *testing.T) {
	t.Run("exec disabled", func(t *testing.T) {
		cfg := config.DefaultConfig()
		cfg.Tools.Exec.Enabled = false
		tool := newTestCronToolWithConfig(t, cfg)
		ctx := WithToolContext(context.Background(), "cli", "direct")
		job, err := tool.cronService.AddJob(cron.AddJobInput{
			Name:     "job",
			Schedule: cron.CronSchedule{Kind: "cron", Expr: "0 8 * * *"},
			Payload:  cron.CronPayload{Message: "message", Channel: "cli", To: "direct"},
		})
		if err != nil {
			t.Fatalf("AddJob() error: %v", err)
		}

		result := tool.Execute(ctx, map[string]any{
			"action":          "update",
			"job_id":          job.ID,
			"command":         "df -h",
			"command_confirm": true,
		})

		if !result.IsError || !strings.Contains(result.ForLLM, "command execution is disabled") {
			t.Fatalf("expected exec disabled error, got: %+v", result)
		}
	})

	t.Run("confirm required", func(t *testing.T) {
		cfg := config.DefaultConfig()
		cfg.Tools.Cron.AllowCommand = false
		tool := newTestCronToolWithConfig(t, cfg)
		ctx := WithToolContext(context.Background(), "cli", "direct")
		job, err := tool.cronService.AddJob(cron.AddJobInput{
			Name:     "job",
			Schedule: cron.CronSchedule{Kind: "cron", Expr: "0 8 * * *"},
			Payload:  cron.CronPayload{Message: "message", Channel: "cli", To: "direct"},
		})
		if err != nil {
			t.Fatalf("AddJob() error: %v", err)
		}

		result := tool.Execute(ctx, map[string]any{
			"action":  "update",
			"job_id":  job.ID,
			"command": "df -h",
		})

		if !result.IsError || !strings.Contains(result.ForLLM, "command_confirm=true") {
			t.Fatalf("expected confirm error, got: %+v", result)
		}

		result = tool.Execute(ctx, map[string]any{
			"action":          "update",
			"job_id":          job.ID,
			"command":         "df -h",
			"command_confirm": true,
		})

		if result.IsError {
			t.Fatalf("expected confirmed command update to succeed, got: %s", result.ForLLM)
		}
		updated, _ := tool.cronService.GetJob(job.ID)
		if updated.Payload.Command != "df -h" {
			t.Fatalf("command not updated: %+v", updated.Payload)
		}

		result = tool.Execute(ctx, map[string]any{
			"action":          "update",
			"job_id":          job.ID,
			"command":         "",
			"command_confirm": true,
		})

		if result.IsError {
			t.Fatalf("expected empty command update to clear command, got: %s", result.ForLLM)
		}
		updated, _ = tool.cronService.GetJob(job.ID)
		if updated.Payload.Command != "" {
			t.Fatalf("command not cleared: %+v", updated.Payload)
		}
	})
}

func TestCronTool_InternalCanAccessCommandJobFromAnyChannel(t *testing.T) {
	tool := newTestCronTool(t)
	ctx := WithToolContext(context.Background(), "cli", "direct")
	job, err := tool.cronService.AddJob(cron.AddJobInput{
		Name:     "command",
		Schedule: cron.CronSchedule{Kind: "cron", Expr: "0 8 * * *"},
		Payload:  cron.CronPayload{Message: "run command", Channel: "telegram", To: "chat-1"},
	})
	if err != nil {
		t.Fatalf("AddJob() error: %v", err)
	}
	job.Payload.Command = "df -h"
	if err := tool.cronService.UpdateJob(job); err != nil {
		t.Fatalf("UpdateJob() error: %v", err)
	}

	getResult := tool.Execute(ctx, map[string]any{"action": "get", "job_id": job.ID})
	if getResult.IsError {
		t.Fatalf("get failed: %s", getResult.ForLLM)
	}
	got := parseCronJobResult(t, getResult)
	if got.Payload.Command != "df -h" || got.Payload.Channel != "telegram" || got.Payload.To != "chat-1" {
		t.Fatalf("get returned wrong command job: %+v", got.Payload)
	}

	updateResult := tool.Execute(ctx, map[string]any{
		"action":    "update",
		"job_id":    job.ID,
		"cron_expr": "30 10 * * *",
	})
	if updateResult.IsError {
		t.Fatalf("update failed: %s", updateResult.ForLLM)
	}
	updated, _ := tool.cronService.GetJob(job.ID)
	if updated.Payload.Command != "df -h" {
		t.Fatalf("command should be preserved: %+v", updated.Payload)
	}
	if updated.Schedule.Kind != "cron" || updated.Schedule.Expr != "30 10 * * *" {
		t.Fatalf("schedule not updated: %+v", updated.Schedule)
	}
}

func TestCronTool_AddJobCapturesSchedulingSession(t *testing.T) {
	tool := newTestCronTool(t)
	ctx := WithToolContext(context.Background(), "telegram", "-100123/5629")
	ctx = WithToolSessionContext(ctx, "main", "sk_v1_abc", nil)
	ctx = WithToolOriginContext(ctx, &bus.InboundContext{
		Channel:  "telegram",
		ChatID:   "-100123/5629",
		ChatType: "group",
		TopicID:  "5629",
		SenderID: "35243507",
		Account:  "bot-a",
	})

	result := tool.Execute(ctx, map[string]any{
		"action":     "add",
		"message":    "ping me",
		"at_seconds": float64(60),
	})
	if result.IsError {
		t.Fatalf("add failed: %s", result.ForLLM)
	}

	jobs := tool.cronService.ListJobs(true)
	if len(jobs) != 1 {
		t.Fatalf("want 1 job, got %d", len(jobs))
	}
	payload := jobs[0].Payload
	if payload.SessionKey != "sk_v1_abc" || payload.AgentID != "main" {
		t.Fatalf("scheduling session not captured: %+v", payload)
	}
	if payload.Origin == nil {
		t.Fatal("origin context not captured")
	}
	if payload.Origin.ChatType != "group" || payload.Origin.TopicID != "5629" {
		t.Fatalf("origin scope not captured: %+v", payload.Origin)
	}
	if payload.Origin.SenderID != "35243507" || payload.Origin.Account != "bot-a" {
		t.Fatalf("origin identity not captured: %+v", payload.Origin)
	}
}

func TestCronTool_AddJobSessionModeArgument(t *testing.T) {
	tool := newTestCronTool(t)
	ctx := WithToolContext(context.Background(), "cli", "direct")

	result := tool.Execute(ctx, map[string]any{
		"action":     "add",
		"message":    "monitor",
		"at_seconds": float64(60),
		"session":    "isolated",
	})
	if result.IsError {
		t.Fatalf("add failed: %s", result.ForLLM)
	}
	jobs := tool.cronService.ListJobs(true)
	if len(jobs) != 1 || jobs[0].Payload.SessionMode != config.CronSessionModeIsolated {
		t.Fatalf("session mode not stored: %+v", jobs)
	}

	bad := tool.Execute(ctx, map[string]any{
		"action":     "add",
		"message":    "monitor",
		"at_seconds": float64(60),
		"session":    "nope",
	})
	if !bad.IsError {
		t.Fatal("expected invalid session mode to be rejected")
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

	if got := executeTestCronJob(t, tool, job); got != "ok" {
		t.Fatalf("ExecuteJob() = %q, want ok", got)
	}

	msg := waitForOutboundMessage(t, tool)
	if !strings.Contains(msg.Content, "command execution is disabled") {
		t.Fatalf("expected exec disabled message, got: %s", msg.Content)
	}
}

func TestCronTool_ExecuteJobInjectsTriggerIntoSchedulingSession(t *testing.T) {
	tool := newTestCronTool(t)

	job := &cron.CronJob{ID: "job-1", Name: "poem"}
	job.Schedule = cron.CronSchedule{Kind: "cron", Expr: "0 9 * * *", TZ: "Asia/Jerusalem"}
	job.Payload.Channel = "telegram"
	job.Payload.To = "-100123/5629"
	job.Payload.Message = "send me a poem"
	job.Payload.SessionKey = "sk_v1_deadbeef"
	job.Payload.AgentID = "main"
	job.Payload.Origin = &cron.CronOrigin{
		ChatType: "group",
		TopicID:  "5629",
		SenderID: "35243507",
		Account:  "bot-a",
	}

	if got := executeTestCronJob(t, tool, job); got != "dispatched" {
		t.Fatalf("ExecuteJob() = %q, want dispatched", got)
	}

	msg := waitForInboundTrigger(t, tool)
	if msg.SessionKey != "sk_v1_deadbeef" {
		t.Fatalf("session key = %q, want the scheduling session", msg.SessionKey)
	}
	if msg.Channel != "telegram" || msg.ChatID != "-100123/5629" {
		t.Fatalf("target = %s/%s, want telegram/-100123/5629", msg.Channel, msg.ChatID)
	}
	if msg.Context.ChatType != "group" || msg.Context.TopicID != "5629" {
		t.Fatalf("origin scope lost: %+v", msg.Context)
	}
	if msg.Context.SenderID != "35243507" || msg.Context.Account != "bot-a" {
		t.Fatalf("origin identity lost: %+v", msg.Context)
	}
	if msg.Sender.DisplayName != cronSenderName {
		t.Fatalf("sender display name = %q, want %q", msg.Sender.DisplayName, cronSenderName)
	}
	if !strings.HasPrefix(msg.Content, "[cron] Scheduled job \"poem\" (id: job-1, 0 9 * * * Asia/Jerusalem)") {
		t.Fatalf("missing cron header, got: %s", msg.Content)
	}
	if !strings.Contains(msg.Content, "send me a poem") {
		t.Fatalf("scheduled instruction missing, got: %s", msg.Content)
	}
}

func TestCronTool_ExecuteJobIsolatedSessionMode(t *testing.T) {
	tool := newTestCronTool(t)

	job := &cron.CronJob{ID: "job-iso"}
	job.Payload.Channel = "telegram"
	job.Payload.To = "chat-1"
	job.Payload.Message = "standalone monitor"
	job.Payload.SessionKey = "sk_v1_deadbeef"
	job.Payload.SessionMode = config.CronSessionModeIsolated

	if got := executeTestCronJob(t, tool, job); got != "dispatched" {
		t.Fatalf("ExecuteJob() = %q, want dispatched", got)
	}

	msg := waitForInboundTrigger(t, tool)
	if !strings.HasPrefix(msg.SessionKey, "agent:cron-job-iso-") {
		t.Fatalf("session key = %q, want agent:cron-job-iso-{uuid}", msg.SessionKey)
	}
}

func TestCronTool_ExecuteJobWithoutSessionKeyFallsBackToRouting(t *testing.T) {
	tool := newTestCronTool(t)

	// Jobs written before the session key was recorded (and CLI-created jobs)
	// must not get a synthetic key — routing picks the channel's own session.
	job := &cron.CronJob{ID: "job-legacy"}
	job.Payload.Channel = "telegram"
	job.Payload.To = "chat-1"
	job.Payload.Message = "legacy reminder"

	if got := executeTestCronJob(t, tool, job); got != "dispatched" {
		t.Fatalf("ExecuteJob() = %q, want dispatched", got)
	}

	msg := waitForInboundTrigger(t, tool)
	if msg.SessionKey != "" {
		t.Fatalf("session key = %q, want empty so routing decides", msg.SessionKey)
	}
}

func TestCronTool_ExecuteJobInjectsCommandOutput(t *testing.T) {
	tool := newTestCronToolWithConfig(t, config.DefaultConfig())

	job := &cron.CronJob{ID: "job-cmd", Name: "watchdog"}
	job.Payload.Channel = "cli"
	job.Payload.To = "direct"
	job.Payload.Message = "watch the queue"
	job.Payload.Command = "echo cron-test-ok"
	job.Payload.SessionKey = "sk_v1_deadbeef"

	if got := executeTestCronJob(t, tool, job); got != "dispatched" {
		t.Fatalf("ExecuteJob() = %q, want dispatched", got)
	}

	msg := waitForInboundTrigger(t, tool)
	if msg.SessionKey != "sk_v1_deadbeef" {
		t.Fatalf("session key = %q, want the scheduling session", msg.SessionKey)
	}
	if !strings.Contains(msg.Content, "cron-test-ok") {
		t.Fatalf("expected command output in trigger, got: %s", msg.Content)
	}
	if !strings.Contains(msg.Content, "watch the queue") {
		t.Fatalf("expected job purpose in trigger, got: %s", msg.Content)
	}
}

func TestCronTool_ExecuteJobRunsCommandWithRawDelivery(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Tools.Cron.CommandDelivery = config.CronCommandDeliveryRaw

	tool := newTestCronToolWithConfig(t, cfg)
	job := &cron.CronJob{}
	job.Payload.Channel = "cli"
	job.Payload.To = "direct"
	job.Payload.Command = "echo cron-test-ok"

	if got := executeTestCronJob(t, tool, job); got != "ok" {
		t.Fatalf("ExecuteJob() = %q, want ok", got)
	}

	msg := waitForOutboundMessage(t, tool)
	if !strings.Contains(msg.Content, "cron-test-ok") {
		t.Fatalf("expected command output containing 'cron-test-ok', got: %s", msg.Content)
	}
}
