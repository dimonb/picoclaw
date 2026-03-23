package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/constants"
	"github.com/sipeed/picoclaw/pkg/cron"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/routing"
	"github.com/sipeed/picoclaw/pkg/utils"
)

// JobExecutor is the interface for executing cron jobs through the agent
type JobExecutor interface {
	ProcessDirectWithMessage(ctx context.Context, msg bus.InboundMessage) (string, error)
	PublishOutboundWithHistory(ctx context.Context, sessionKey, channel, chatID string, msg providers.Message) error
}

// CronTool provides scheduling capabilities for the agent
type CronTool struct {
	cronService *cron.CronService
	executor    JobExecutor
	msgBus      *bus.MessageBus
	execTool    *ExecTool
	allowRemote bool
}

// NewCronTool creates a new CronTool
// execTimeout: 0 means no timeout, >0 sets the timeout duration
func NewCronTool(
	cronService *cron.CronService, executor JobExecutor, msgBus *bus.MessageBus, workspace string, restrict bool,
	execTimeout time.Duration, config *config.Config,
) (*CronTool, error) {
	execTool, err := NewExecToolWithConfig(workspace, restrict, config)
	if err != nil {
		return nil, fmt.Errorf("unable to configure exec tool: %w", err)
	}

	execTool.SetTimeout(execTimeout)
	allowRemote := false
	if config != nil {
		allowRemote = config.Tools.Exec.AllowRemote
	}
	return &CronTool{
		cronService: cronService,
		executor:    executor,
		msgBus:      msgBus,
		execTool:    execTool,
		allowRemote: allowRemote,
	}, nil
}

// Name returns the tool name
func (t *CronTool) Name() string {
	return "cron"
}

// Description returns the tool description
func (t *CronTool) Description() string {
	return "Schedule reminders, agent tasks, or system commands. IMPORTANT: When user asks to be reminded or scheduled, you MUST call this tool. Use mode='agent' (default) when the scheduled text should be processed by the agent with the bound session context. Use mode='direct' only for literal reminders that should be posted unchanged. Use 'at_seconds' for one-time reminders (e.g., 'remind me in 10 minutes' → at_seconds=600). Use 'every_seconds' ONLY for recurring tasks (e.g., 'every 2 hours' → every_seconds=7200). Use 'cron_expr' for complex recurring schedules. Use 'command' to execute shell commands directly."
}

// Parameters returns the tool parameters schema
func (t *CronTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"add", "list", "remove", "enable", "disable"},
				"description": "Action to perform. Use 'add' when user wants to schedule a reminder or task.",
			},
			"message": map[string]any{
				"type":        "string",
				"description": "Instruction for the scheduled job. In mode='agent', this is passed back to the agent as a synthetic user message when triggered. In mode='direct', this exact text is posted to the channel unchanged. If 'command' is used, this describes what the command does.",
			},
			"mode": map[string]any{
				"type":        "string",
				"enum":        []string{cron.ModeAgent, cron.ModeDirect},
				"description": "Execution mode. 'agent' (default) re-enters the agent with the bound session context. 'direct' posts the message as-is without running the agent.",
			},
			"command": map[string]any{
				"type":        "string",
				"description": "Optional: Shell command to execute directly (e.g., 'df -h'). If set, the command output is published without running the LLM, and mode='direct' is forced.",
			},
			"command_confirm": map[string]any{
				"type":        "boolean",
				"description": "Required when using command=true. Must be true to explicitly confirm scheduling a shell command.",
			},
			"at_seconds": map[string]any{
				"type":        "integer",
				"description": "One-time reminder: seconds from now when to trigger (e.g., 600 for 10 minutes later). Use this for one-time reminders like 'remind me in 10 minutes'.",
			},
			"every_seconds": map[string]any{
				"type":        "integer",
				"description": "Recurring interval in seconds (e.g., 3600 for every hour). Use this ONLY for recurring tasks like 'every 2 hours' or 'daily reminder'.",
			},
			"cron_expr": map[string]any{
				"type":        "string",
				"description": "Cron expression for complex recurring schedules (e.g., '0 9 * * *' for daily at 9am). Use this for complex recurring schedules.",
			},
			"job_id": map[string]any{
				"type":        "string",
				"description": "Job ID (for remove/enable/disable)",
			},
		},
		"required": []string{"action"},
	}
}

// Execute runs the tool with the given arguments
func (t *CronTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	action, ok := args["action"].(string)
	if !ok {
		return ErrorResult("action is required")
	}

	switch action {
	case "add":
		return t.addJob(ctx, args)
	case "list":
		return t.listJobs()
	case "remove":
		return t.removeJob(args)
	case "enable":
		return t.enableJob(args, true)
	case "disable":
		return t.enableJob(args, false)
	default:
		return ErrorResult(fmt.Sprintf("unknown action: %s", action))
	}
}

func (t *CronTool) addJob(ctx context.Context, args map[string]any) *ToolResult {
	channel := ToolChannel(ctx)
	chatID := ToolChatID(ctx)
	sessionKey := ToolSessionKey(ctx)

	if channel == "" || chatID == "" {
		return ErrorResult("no session context (channel/chat_id not set). Use this tool in an active conversation.")
	}

	message, ok := args["message"].(string)
	if !ok || message == "" {
		return ErrorResult("message is required for add")
	}

	var schedule cron.CronSchedule

	// Check for at_seconds (one-time), every_seconds (recurring), or cron_expr
	atSeconds, hasAt := args["at_seconds"].(float64)
	everySeconds, hasEvery := args["every_seconds"].(float64)
	cronExpr, hasCron := args["cron_expr"].(string)

	// Fix: type assertions return true for zero values, need additional validity checks
	// This prevents LLMs that fill unused optional parameters with defaults (0) from triggering wrong type
	hasAt = hasAt && atSeconds > 0
	hasEvery = hasEvery && everySeconds > 0
	hasCron = hasCron && cronExpr != ""

	// Priority: at_seconds > every_seconds > cron_expr
	if hasAt {
		atMS := time.Now().UnixMilli() + int64(atSeconds)*1000
		schedule = cron.CronSchedule{
			Kind: "at",
			AtMS: &atMS,
		}
	} else if hasEvery {
		everyMS := int64(everySeconds) * 1000
		schedule = cron.CronSchedule{
			Kind:    "every",
			EveryMS: &everyMS,
		}
	} else if hasCron {
		schedule = cron.CronSchedule{
			Kind: "cron",
			Expr: cronExpr,
		}
	} else {
		return ErrorResult("one of at_seconds, every_seconds, or cron_expr is required")
	}

	mode, err := resolveCronMode(args)
	if err != nil {
		return ErrorResult(err.Error())
	}

	// GHSA-pv8c-p6jf-3fpp: command scheduling requires internal channel + explicit confirm.
	// Non-command reminders (plain messages) remain open to all channels.
	command, _ := args["command"].(string)
	commandConfirm, _ := args["command_confirm"].(bool)
	if command != "" {
		if !t.allowRemote && !constants.IsInternalChannel(channel) {
			return ErrorResult("scheduling command execution is restricted to internal channels")
		}
		if !commandConfirm {
			return ErrorResult("command_confirm=true is required to schedule command execution")
		}
		mode = cron.ModeDirect
	}

	// Truncate message for job name (max 30 chars)
	messagePreview := utils.Truncate(message, 30)

	job, err := t.cronService.AddJob(
		messagePreview,
		schedule,
		message,
		mode,
		channel,
		chatID,
		sessionKey,
	)
	if err != nil {
		return ErrorResult(fmt.Sprintf("Error adding job: %v", err))
	}

	if command != "" {
		job.Payload.Command = command
		// Need to save the updated payload
		t.cronService.UpdateJob(job)
	}

	return SilentResult(fmt.Sprintf("Cron job added: %s (id: %s)", job.Name, job.ID))
}

func (t *CronTool) listJobs() *ToolResult {
	jobs := t.cronService.ListJobs(false)

	if len(jobs) == 0 {
		return SilentResult("No scheduled jobs")
	}

	var result strings.Builder
	result.WriteString("Scheduled jobs:\n")
	for _, j := range jobs {
		var scheduleInfo string
		if j.Schedule.Kind == "every" && j.Schedule.EveryMS != nil {
			scheduleInfo = fmt.Sprintf("every %ds", *j.Schedule.EveryMS/1000)
		} else if j.Schedule.Kind == "cron" {
			scheduleInfo = j.Schedule.Expr
		} else if j.Schedule.Kind == "at" {
			scheduleInfo = "one-time"
		} else {
			scheduleInfo = "unknown"
		}
		result.WriteString(fmt.Sprintf("- %s (id: %s, %s)\n", j.Name, j.ID, scheduleInfo))
	}

	return SilentResult(result.String())
}

func (t *CronTool) removeJob(args map[string]any) *ToolResult {
	jobID, ok := args["job_id"].(string)
	if !ok || jobID == "" {
		return ErrorResult("job_id is required for remove")
	}

	if t.cronService.RemoveJob(jobID) {
		return SilentResult(fmt.Sprintf("Cron job removed: %s", jobID))
	}
	return ErrorResult(fmt.Sprintf("Job %s not found", jobID))
}

func (t *CronTool) enableJob(args map[string]any, enable bool) *ToolResult {
	jobID, ok := args["job_id"].(string)
	if !ok || jobID == "" {
		return ErrorResult("job_id is required for enable/disable")
	}

	job := t.cronService.EnableJob(jobID, enable)
	if job == nil {
		return ErrorResult(fmt.Sprintf("Job %s not found", jobID))
	}

	status := "enabled"
	if !enable {
		status = "disabled"
	}
	return SilentResult(fmt.Sprintf("Cron job '%s' %s", job.Name, status))
}

// ExecuteJob executes a cron job through the agent
func (t *CronTool) ExecuteJob(ctx context.Context, job *cron.CronJob) string {
	// Get channel/chatID from job payload
	channel := job.Payload.Channel
	chatID := job.Payload.To

	// Default values if not set
	if channel == "" {
		channel = "cli"
	}
	if chatID == "" {
		chatID = "direct"
	}
	sessionKey := cronSessionKey(job, channel, chatID)
	mode := job.Payload.EffectiveMode()

	// Execute command if present
	if job.Payload.Command != "" {
		args := map[string]any{
			"command":   job.Payload.Command,
			"__channel": channel,
			"__chat_id": chatID,
		}

		result := t.execTool.Execute(ctx, args)
		var output string
		if result.IsError {
			output = fmt.Sprintf("Error executing scheduled command: %s", result.ForLLM)
		} else {
			output = fmt.Sprintf("Scheduled command '%s' executed:\n%s", job.Payload.Command, result.ForLLM)
		}

		msg := providers.Message{
			Role:     "assistant",
			Content:  output,
			Metadata: cronMessageMetadata(job, channel, chatID, cron.ModeDirect),
		}
		pubCtx, pubCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer pubCancel()
		if t.executor != nil {
			if err := t.executor.PublishOutboundWithHistory(pubCtx, sessionKey, channel, chatID, msg); err != nil {
				return fmt.Sprintf("Error: %v", err)
			}
			return "ok"
		}
		t.msgBus.PublishOutbound(pubCtx, bus.OutboundMessage{
			Channel: channel,
			ChatID:  chatID,
			Content: output,
		})
		return "ok"
	}

	// Direct mode posts the message unchanged without agent processing.
	if mode == cron.ModeDirect {
		if t.executor == nil {
			pubCtx, pubCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer pubCancel()
			t.msgBus.PublishOutbound(pubCtx, bus.OutboundMessage{
				Channel: channel,
				ChatID:  chatID,
				Content: job.Payload.Message,
			})
			return "ok"
		}

		msg := providers.Message{
			Role:     "assistant",
			Content:  job.Payload.Message,
			Metadata: cronMessageMetadata(job, channel, chatID, cron.ModeDirect),
		}

		pubCtx, pubCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer pubCancel()
		if err := t.executor.PublishOutboundWithHistory(pubCtx, sessionKey, channel, chatID, msg); err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		return "ok"
	}

	// Agent mode re-enters the agent with the original session binding.
	if t.executor == nil {
		return "Error: agent executor is not configured for cron agent mode"
	}
	response, err := t.executor.ProcessDirectWithMessage(ctx, bus.InboundMessage{
		Channel:    channel,
		SenderID:   "cron",
		ChatID:     chatID,
		Content:    job.Payload.Message,
		SessionKey: sessionKey,
		Metadata:   cronMessageMetadata(job, channel, chatID, cron.ModeAgent),
	})
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}

	// Response is automatically sent via MessageBus by AgentLoop
	_ = response // Will be sent by AgentLoop
	return "ok"
}

func resolveCronMode(args map[string]any) (string, error) {
	if raw, ok := args["mode"].(string); ok && strings.TrimSpace(raw) != "" {
		mode := cron.NormalizeMode(raw)
		if mode == "" {
			return "", fmt.Errorf("mode must be %q or %q", cron.ModeAgent, cron.ModeDirect)
		}
		return mode, nil
	}
	if deliver, ok := args["deliver"].(bool); ok {
		if deliver {
			return cron.ModeDirect, nil
		}
		return cron.ModeAgent, nil
	}
	return cron.ModeAgent, nil
}

func cronSessionKey(job *cron.CronJob, channel, chatID string) string {
	if strings.HasPrefix(strings.TrimSpace(job.Payload.SessionKey), "agent:") {
		return strings.TrimSpace(job.Payload.SessionKey)
	}
	if channel == "cli" || chatID == "direct" {
		return routing.BuildAgentMainSessionKey(routing.DefaultAgentID)
	}
	return routing.BuildAgentPeerSessionKey(routing.SessionKeyParams{
		AgentID: routing.DefaultAgentID,
		Channel: channel,
		Peer: &routing.RoutePeer{
			Kind: "group",
			ID:   chatID,
		},
	})
}

func cronMessageMetadata(job *cron.CronJob, channel, chatID, mode string) map[string]string {
	return providers.CloneMessageMetadata(map[string]string{
		providers.MessageMetaSourceKind:  providers.MessageSourceCron,
		providers.MessageMetaChannel:     channel,
		providers.MessageMetaChatID:      chatID,
		providers.MessageMetaTriggerKind: providers.MessageTriggerCron,
		providers.MessageMetaTriggerID:   job.ID,
		providers.MessageMetaDispatch:    mode,
	})
}
