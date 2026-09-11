package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/constants"
	"github.com/sipeed/picoclaw/pkg/cron"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/utils"
)

// cronSenderID marks a firing as machine-generated in logs and routing.
const cronSenderID = "cron"

// cronSenderName is the display name the trigger carries, so the prompt
// annotation reads "[from:cron]" instead of impersonating the user.
const cronSenderName = "cron"

// cronCommandOutputLimit caps how much scheduled command output is injected
// into the session, so a chatty script cannot flood the context window.
const cronCommandOutputLimit = 4000

// cronIsolatedSessionPrefix marks the throwaway sessions minted for firings in
// isolated mode.
const cronIsolatedSessionPrefix = "agent:cron-"

// CronTool provides scheduling capabilities for the agent
type CronTool struct {
	cronService           *cron.CronService
	msgBus                *bus.MessageBus
	execTool              *ExecTool
	allowCommand          bool
	execEnabled           bool
	commandAllowedRemotes []string
	sessionMode           string
	commandDelivery       string
	notifyMode            string
}

// NewCronTool creates a new CronTool
// execTimeout: 0 means no timeout, >0 sets the timeout duration
func NewCronTool(
	cronService *cron.CronService, msgBus *bus.MessageBus, workspace string, restrict bool,
	execTimeout time.Duration, cfg *config.Config,
) (*CronTool, error) {
	allowCommand := true
	execEnabled := true
	var commandAllowedRemotes []string
	sessionMode := config.CronSessionModeOrigin
	commandDelivery := config.CronCommandDeliverySession
	notifyMode := config.CronNotifyOutput
	if cfg != nil {
		allowCommand = cfg.Tools.Cron.AllowCommand
		execEnabled = cfg.Tools.Exec.Enabled
		commandAllowedRemotes = cfg.Tools.Cron.CommandAllowedRemotes
		sessionMode = normalizeCronSessionMode(cfg.Tools.Cron.SessionMode, sessionMode)
		commandDelivery = normalizeCronCommandDelivery(cfg.Tools.Cron.CommandDelivery, commandDelivery)
		notifyMode = normalizeCronNotify(cfg.Tools.Cron.Notify, notifyMode)
	}

	var execTool *ExecTool
	if execEnabled {
		var err error
		execTool, err = NewExecToolWithConfig(workspace, restrict, cfg)
		if err != nil {
			return nil, fmt.Errorf("unable to configure exec tool: %w", err)
		}
	}

	if execTool != nil {
		execTool.SetTimeout(execTimeout)
	}
	return &CronTool{
		cronService:           cronService,
		msgBus:                msgBus,
		execTool:              execTool,
		allowCommand:          allowCommand,
		execEnabled:           execEnabled,
		commandAllowedRemotes: commandAllowedRemotes,
		sessionMode:           sessionMode,
		commandDelivery:       commandDelivery,
		notifyMode:            notifyMode,
	}, nil
}

func normalizeCronNotify(value, fallback string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case config.CronNotifyOutput:
		return config.CronNotifyOutput
	case config.CronNotifyAlways:
		return config.CronNotifyAlways
	default:
		return fallback
	}
}

func normalizeCronSessionMode(value, fallback string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case config.CronSessionModeOrigin:
		return config.CronSessionModeOrigin
	case config.CronSessionModeIsolated:
		return config.CronSessionModeIsolated
	default:
		return fallback
	}
}

func normalizeCronCommandDelivery(value, fallback string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case config.CronCommandDeliverySession:
		return config.CronCommandDeliverySession
	case config.CronCommandDeliveryRaw:
		return config.CronCommandDeliveryRaw
	default:
		return fallback
	}
}

// Name returns the tool name
func (t *CronTool) Name() string {
	return "cron"
}

// Description returns the tool description
func (t *CronTool) Description() string {
	return `Schedule, inspect, and update reminders, tasks, or system commands. 
IMPORTANT: When user asks to be reminded or scheduled, you MUST call this tool. 
Use 'at_seconds' for one-time reminders (e.g., 'remind me in 10 minutes' → at_seconds=600). 
Use 'every_seconds' ONLY for recurring tasks (e.g., 'every 2 hours' → every_seconds=7200). 
Use 'cron_expr' for complex recurring schedules. 
Use 'command' to execute shell commands directly.
A firing is injected back into this conversation, so scheduled work continues with the context it was created in.`
}

// Parameters returns the tool parameters schema
func (t *CronTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"add", "list", "get", "update", "remove", "enable", "disable"},
				"description": "Action to perform. Use 'get' before editing and 'update' to change existing jobs without losing their payload. Remote channels can only list/get/update jobs for the current channel/chat_id.",
			},
			"name": map[string]any{
				"type":        "string",
				"description": "Optional job display name for update or add.",
			},
			"message": map[string]any{
				"type":        "string",
				"description": "The reminder/task message to display when triggered. If 'command' is used, this describes what the command does.",
			},
			"command": map[string]any{
				"type":        "string",
				"description": "Optional: Shell command to execute directly (e.g., 'df -h'). If set, the agent will run this command and report output instead of just showing the message. For update, omit to preserve the command or pass an empty string to clear it.",
			},
			"command_confirm": map[string]any{
				"type":        "boolean",
				"description": "Optional explicit confirmation flag for scheduling a shell command. Command execution must also be enabled via tools.cron.allow_command.",
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
				"description": "Job ID (for get/update/remove/enable/disable)",
			},
			"notify": map[string]any{
				"type":        "string",
				"enum":        []string{"output", "always"},
				"description": "Command jobs only. 'output' (default) wakes you only when the command printed something or exited non-zero — a watchdog that finds nothing costs nothing. 'always' reports every run. Ignored for message jobs.",
			},
			"session": map[string]any{
				"type":        "string",
				"enum":        []string{"origin", "isolated"},
				"description": "Where the firing runs. 'origin' (default) injects it into this conversation so you keep the context that scheduled it. 'isolated' runs each firing in a fresh empty session — only for noisy standalone monitors.",
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
		return t.listJobs(ctx)
	case "get":
		return t.getJob(ctx, args)
	case "update":
		return t.updateJob(ctx, args)
	case "remove":
		return t.removeJob(ctx, args)
	case "enable":
		return t.enableJob(ctx, args, true)
	case "disable":
		return t.enableJob(ctx, args, false)
	default:
		return ErrorResult(fmt.Sprintf("unknown action: %s", action))
	}
}

func (t *CronTool) addJob(ctx context.Context, args map[string]any) *ToolResult {
	channel := ToolChannel(ctx)
	chatID := ToolChatID(ctx)

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

	// GHSA-pv8c-p6jf-3fpp: command scheduling requires internal channel. When
	// allow_command is disabled, explicit confirmation is required as an override.
	// Non-command reminders remain open to all channels.
	command, _ := args["command"].(string)
	commandConfirm, _ := args["command_confirm"].(bool)
	if command != "" {
		if !t.execEnabled {
			return ErrorResult("command execution is disabled")
		}
		if !constants.IsInternalChannel(channel) && !isCommandAllowedRemote(channel, chatID, t.commandAllowedRemotes) {
			return ErrorResult(
				"scheduling command execution is restricted to internal channels or configured remote channels",
			)
		}
		if !t.allowCommand && !commandConfirm {
			return ErrorResult("command_confirm=true is required when allow_command is disabled")
		}
	}

	sessionMode, errResult := cronSessionModeArg(args)
	if errResult != nil {
		return errResult
	}

	notifyMode, errResult := cronNotifyArg(args)
	if errResult != nil {
		return errResult
	}

	// Truncate message for job name (max 30 chars)
	messagePreview := utils.Truncate(message, 30)

	job, err := t.cronService.AddJob(cron.AddJobInput{
		Name:     messagePreview,
		Schedule: schedule,
		Payload: cron.CronPayload{
			Kind:    "agent_turn",
			Message: message,
			Command: command,
			Channel: channel,
			To:      chatID,
			// Pin the firing to the session that scheduled it. Channel+chat id
			// alone cannot reproduce the key: it is derived from the whole
			// scope (chat type, topic, account, sender).
			SessionKey:  cronSchedulingSessionKey(ctx),
			AgentID:     ToolAgentID(ctx),
			SessionMode: sessionMode,
			Notify:      notifyMode,
			Origin:      cronOriginFromContext(ctx),
		},
	})
	if err != nil {
		return ErrorResult(fmt.Sprintf("Error adding job: %v", err))
	}

	return SilentResult(fmt.Sprintf("Cron job added: %s (id: %s)", job.Name, job.ID))
}

// cronSchedulingSessionKey returns the session to pin a new job to. Keys minted
// for isolated firings are throwaway: a job scheduled from inside one must not
// inherit it, or every future firing would land in a session nobody can see.
func cronSchedulingSessionKey(ctx context.Context) string {
	sessionKey := ToolSessionKey(ctx)
	if strings.HasPrefix(sessionKey, cronIsolatedSessionPrefix) {
		return ""
	}
	return sessionKey
}

// cronSessionModeArg reads the optional per-job session mode.
func cronSessionModeArg(args map[string]any) (string, *ToolResult) {
	value, present, errResult := optionalString(args, "session")
	if errResult != nil {
		return "", errResult
	}
	if !present {
		return "", nil
	}
	mode := strings.ToLower(strings.TrimSpace(value))
	switch mode {
	case "":
		return "", nil
	case config.CronSessionModeOrigin, config.CronSessionModeIsolated:
		return mode, nil
	default:
		return "", ErrorResult("session must be 'origin' or 'isolated'")
	}
}

// cronNotifyArg reads the optional per-job notify mode.
func cronNotifyArg(args map[string]any) (string, *ToolResult) {
	value, present, errResult := optionalString(args, "notify")
	if errResult != nil {
		return "", errResult
	}
	if !present {
		return "", nil
	}
	mode := strings.ToLower(strings.TrimSpace(value))
	switch mode {
	case "":
		return "", nil
	case config.CronNotifyOutput, config.CronNotifyAlways:
		return mode, nil
	default:
		return "", ErrorResult("notify must be 'output' or 'always'")
	}
}

// cronOriginFromContext snapshots the scheduling turn's inbound context so the
// firing can be replayed with the same routing and scope.
func cronOriginFromContext(ctx context.Context) *cron.CronOrigin {
	inbound := ToolOriginContext(ctx)
	if inbound == nil {
		return nil
	}
	origin := cron.CronOrigin{
		Account:   inbound.Account,
		ChatType:  inbound.ChatType,
		TopicID:   inbound.TopicID,
		SpaceID:   inbound.SpaceID,
		SpaceType: inbound.SpaceType,
		SenderID:  inbound.SenderID,
	}
	if origin == (cron.CronOrigin{}) {
		return nil
	}
	return &origin
}

func (t *CronTool) listJobs(ctx context.Context) *ToolResult {
	jobs := t.cronService.ListJobs(false)

	var accessibleJobs []cron.CronJob
	for _, job := range jobs {
		if t.canAccessJob(ctx, &job) {
			accessibleJobs = append(accessibleJobs, job)
		}
	}
	jobs = accessibleJobs

	if len(jobs) == 0 {
		return SilentResult("No scheduled jobs")
	}

	var result strings.Builder
	result.WriteString("Scheduled jobs:\n")
	for _, j := range jobs {
		result.WriteString(fmt.Sprintf("- %s (id: %s, %s)\n", j.Name, j.ID, describeCronSchedule(&j.Schedule)))
	}

	return SilentResult(result.String())
}

func describeCronSchedule(schedule *cron.CronSchedule) string {
	switch {
	case schedule.Kind == "every" && schedule.EveryMS != nil:
		return fmt.Sprintf("every %ds", *schedule.EveryMS/1000)
	case schedule.Kind == "cron":
		if tz := strings.TrimSpace(schedule.TZ); tz != "" {
			return fmt.Sprintf("%s %s", schedule.Expr, tz)
		}
		return schedule.Expr
	case schedule.Kind == "at":
		return "one-time"
	default:
		return "unknown"
	}
}

func (t *CronTool) getJob(ctx context.Context, args map[string]any) *ToolResult {
	jobID, errResult := requiredCronJobID(args, "get")
	if errResult != nil {
		return errResult
	}

	job, ok := t.cronService.GetJob(jobID)
	if !ok {
		return ErrorResult(fmt.Sprintf("Job %s not found", jobID))
	}
	if !t.canAccessJob(ctx, job) {
		return ErrorResult(fmt.Sprintf("Job %s is not accessible from this channel", jobID))
	}

	return SilentResult(formatCronJobJSON(job))
}

func (t *CronTool) updateJob(ctx context.Context, args map[string]any) *ToolResult {
	jobID, errResult := requiredCronJobID(args, "update")
	if errResult != nil {
		return errResult
	}

	job, ok := t.cronService.GetJob(jobID)
	if !ok {
		return ErrorResult(fmt.Sprintf("Job %s not found", jobID))
	}
	if !t.canAccessJob(ctx, job) {
		return ErrorResult(fmt.Sprintf("Job %s is not accessible from this channel", jobID))
	}

	patches := 0

	name, namePresent, nameErr := optionalNonEmptyString(args, "name")
	if nameErr != nil {
		return nameErr
	}
	if namePresent {
		job.Name = name
		patches++
	}

	message, messagePresent, messageErr := optionalNonEmptyString(args, "message")
	if messageErr != nil {
		return messageErr
	}
	if messagePresent {
		job.Payload.Message = message
		patches++
	}

	schedule, hasSchedule, errResult := schedulePatch(args)
	if errResult != nil {
		return errResult
	}
	if hasSchedule {
		job.Schedule = schedule
		job.DeleteAfterRun = schedule.Kind == "at"
		patches++
	}

	command, commandPresent, errResult := optionalString(args, "command")
	if errResult != nil {
		return errResult
	}
	if commandPresent {
		if errResult := t.validateCommandMutation(ctx, args); errResult != nil {
			return errResult
		}
		job.Payload.Command = command
		patches++
	}

	if _, present := args["session"]; present {
		sessionMode, errResult := cronSessionModeArg(args)
		if errResult != nil {
			return errResult
		}
		job.Payload.SessionMode = sessionMode
		patches++
	}

	if _, present := args["notify"]; present {
		notifyMode, errResult := cronNotifyArg(args)
		if errResult != nil {
			return errResult
		}
		job.Payload.Notify = notifyMode
		patches++
	}

	if patches == 0 {
		return ErrorResult("at least one update field is required")
	}

	// Migration path for jobs that predate session pinning (and CLI-created
	// ones): the first update from a real session adopts it. Jobs that already
	// carry a session are only reachable from that session, so this cannot move
	// someone else's job.
	if job.Payload.SessionKey == "" {
		if sessionKey := cronSchedulingSessionKey(ctx); sessionKey != "" {
			job.Payload.SessionKey = sessionKey
			job.Payload.AgentID = ToolAgentID(ctx)
			job.Payload.Origin = cronOriginFromContext(ctx)
		}
	}

	if err := t.cronService.UpdateJob(job); err != nil {
		return ErrorResult(fmt.Sprintf("Error updating job: %v", err))
	}

	updated, _ := t.cronService.GetJob(jobID)
	return SilentResult(fmt.Sprintf("Cron job updated:\n%s", formatCronJobJSON(updated)))
}

func (t *CronTool) removeJob(ctx context.Context, args map[string]any) *ToolResult {
	jobID, ok := args["job_id"].(string)
	if !ok || jobID == "" {
		return ErrorResult("job_id is required for remove")
	}

	job, ok := t.cronService.GetJob(jobID)
	if !ok {
		return ErrorResult(fmt.Sprintf("Job %s not found", jobID))
	}
	if !t.canAccessJob(ctx, job) {
		return ErrorResult(fmt.Sprintf("Job %s is not accessible from this channel", jobID))
	}

	if t.cronService.RemoveJob(jobID) {
		return SilentResult(fmt.Sprintf("Cron job removed: %s", jobID))
	}
	return ErrorResult(fmt.Sprintf("Job %s not found", jobID))
}

func requiredCronJobID(args map[string]any, action string) (string, *ToolResult) {
	jobID, ok := args["job_id"].(string)
	if !ok || jobID == "" {
		return "", ErrorResult(fmt.Sprintf("job_id is required for %s", action))
	}
	return jobID, nil
}

func optionalNonEmptyString(args map[string]any, key string) (string, bool, *ToolResult) {
	_, present := args[key]
	if !present {
		return "", false, nil
	}
	text, _, errResult := optionalString(args, key)
	if errResult != nil {
		return "", false, errResult
	}
	if strings.TrimSpace(text) == "" {
		return "", false, ErrorResult(fmt.Sprintf("%s cannot be empty", key))
	}
	return text, true, nil
}

func optionalString(args map[string]any, key string) (string, bool, *ToolResult) {
	value, present := args[key]
	if !present {
		return "", false, nil
	}
	text, ok := value.(string)
	if !ok {
		return "", false, ErrorResult(fmt.Sprintf("%s must be a string", key))
	}
	return text, true, nil
}

func schedulePatch(args map[string]any) (cron.CronSchedule, bool, *ToolResult) {
	var schedule cron.CronSchedule
	patches := 0

	if _, present := args["at_seconds"]; present {
		seconds, errResult := positiveSeconds(args, "at_seconds")
		if errResult != nil {
			return cron.CronSchedule{}, false, errResult
		}
		atMS := time.Now().UnixMilli() + seconds*1000
		schedule = cron.CronSchedule{Kind: "at", AtMS: &atMS}
		patches++
	}

	if _, present := args["every_seconds"]; present {
		seconds, errResult := positiveSeconds(args, "every_seconds")
		if errResult != nil {
			return cron.CronSchedule{}, false, errResult
		}
		everyMS := seconds * 1000
		schedule = cron.CronSchedule{Kind: "every", EveryMS: &everyMS}
		patches++
	}

	if _, present := args["cron_expr"]; present {
		cronExpr, ok := args["cron_expr"].(string)
		if !ok {
			return cron.CronSchedule{}, false, ErrorResult("cron_expr must be a string")
		}
		if strings.TrimSpace(cronExpr) == "" {
			return cron.CronSchedule{}, false, ErrorResult("cron_expr cannot be empty")
		}
		schedule = cron.CronSchedule{Kind: "cron", Expr: cronExpr}
		patches++
	}

	if patches > 1 {
		return cron.CronSchedule{}, false, ErrorResult("only one of at_seconds, every_seconds, or cron_expr can be set")
	}
	return schedule, patches == 1, nil
}

func positiveSeconds(args map[string]any, key string) (int64, *ToolResult) {
	value := args[key]
	var seconds int64
	switch v := value.(type) {
	case float64:
		if v != float64(int64(v)) {
			return 0, ErrorResult(fmt.Sprintf("%s must be a positive integer", key))
		}
		seconds = int64(v)
	case int:
		seconds = int64(v)
	case int64:
		seconds = v
	default:
		return 0, ErrorResult(fmt.Sprintf("%s must be a positive integer", key))
	}
	if seconds <= 0 {
		return 0, ErrorResult(fmt.Sprintf("%s must be a positive integer", key))
	}
	return seconds, nil
}

func (t *CronTool) validateCommandMutation(ctx context.Context, args map[string]any) *ToolResult {
	channel := ToolChannel(ctx)
	chatID := ToolChatID(ctx)
	if !t.execEnabled {
		return ErrorResult("command execution is disabled")
	}
	if !constants.IsInternalChannel(channel) && !isCommandAllowedRemote(channel, chatID, t.commandAllowedRemotes) {
		return ErrorResult(
			"updating command execution is restricted to internal channels or configured remote channels",
		)
	}
	commandConfirm, _ := args["command_confirm"].(bool)
	if !t.allowCommand && !commandConfirm {
		return ErrorResult("command_confirm=true is required when allow_command is disabled")
	}
	return nil
}

func isCommandAllowedRemote(channel, chatID string, allowed []string) bool {
	if channel == "" {
		return false
	}

	target := channel
	if chatID != "" {
		target = channel + ":" + chatID
	}

	for _, entry := range allowed {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if entry == "*" || entry == channel || entry == target {
			return true
		}
	}

	return false
}

func (t *CronTool) canAccessJob(ctx context.Context, job *cron.CronJob) bool {
	channel := ToolChannel(ctx)
	if constants.IsInternalChannel(channel) {
		return true
	}

	chatID := ToolChatID(ctx)
	if channel == "" || chatID == "" {
		return false
	}
	if job.Payload.Channel != channel || job.Payload.To != chatID {
		return false
	}
	// Same chat can host several sessions (forum topics, per-sender scopes).
	// When both sides know their session, require a match.
	if jobKey := job.Payload.SessionKey; jobKey != "" {
		if currentKey := ToolSessionKey(ctx); currentKey != "" && currentKey != jobKey {
			return false
		}
	}
	if job.Payload.Command != "" {
		return isCommandAllowedRemote(channel, chatID, t.commandAllowedRemotes)
	}
	return true
}

func formatCronJobJSON(job *cron.CronJob) string {
	data, err := json.Marshal(job)
	if err != nil {
		return fmt.Sprintf("%+v", *job)
	}
	return string(data)
}

func (t *CronTool) enableJob(ctx context.Context, args map[string]any, enable bool) *ToolResult {
	jobID, ok := args["job_id"].(string)
	if !ok || jobID == "" {
		return ErrorResult("job_id is required for enable/disable")
	}

	job, ok := t.cronService.GetJob(jobID)
	if !ok {
		return ErrorResult(fmt.Sprintf("Job %s not found", jobID))
	}
	if !t.canAccessJob(ctx, job) {
		return ErrorResult(fmt.Sprintf("Job %s is not accessible from this channel", jobID))
	}

	updatedJob := t.cronService.EnableJob(jobID, enable)
	if updatedJob == nil {
		return ErrorResult(fmt.Sprintf("Job %s not found", jobID))
	}

	status := "enabled"
	if !enable {
		status = "disabled"
	}
	return SilentResult(fmt.Sprintf("Cron job '%s' %s", updatedJob.Name, status))
}

// ExecuteJob dispatches a cron firing.
//
// A firing is published as a normal inbound message carrying the session key
// captured when the job was scheduled. That puts it through the same path as a
// real user message: the session is claimed (or the trigger is queued as
// steering when a turn is already running), history and summaries load, and the
// response is delivered by the usual outbound path. Calling the agent directly
// would bypass all of that and race with live turns in the same chat.
func (t *CronTool) ExecuteJob(ctx context.Context, job *cron.CronJob) (string, error) {
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

	if job.Payload.Command != "" {
		return t.executeCommandJob(ctx, job, channel, chatID)
	}

	return t.dispatchTrigger(ctx, job, channel, chatID, cronTriggerPrompt(job))
}

// executeCommandJob runs a scheduled shell command. By default the result is
// injected into the session so the agent decides whether it is worth reporting;
// tools.cron.command_delivery=raw restores posting the output straight to chat.
func (t *CronTool) executeCommandJob(
	ctx context.Context,
	job *cron.CronJob,
	channel, chatID string,
) (string, error) {
	if !t.execEnabled || t.execTool == nil {
		t.publishToChat(ctx, channel, chatID, "Error executing scheduled command: command execution is disabled")
		return "ok", nil
	}

	args := map[string]any{
		"action":    "run",
		"command":   job.Payload.Command,
		"__channel": channel,
		"__chat_id": chatID,
	}

	result := t.execTool.Execute(ctx, args)

	// A watchdog that finds nothing should cost nothing: no agent turn, no
	// message. A non-zero exit still reports — the script itself breaking is an
	// event worth hearing about, even with no output.
	if !t.shouldReportCommandResult(job, result) {
		logger.DebugCF("cron", "Scheduled command produced nothing to report", map[string]any{
			"job_id":   job.ID,
			"job_name": job.Name,
			"notify":   t.notifyModeFor(job),
		})
		return "silent", nil
	}

	if t.commandDelivery == config.CronCommandDeliveryRaw {
		var output string
		if result.IsError {
			output = fmt.Sprintf("Error executing scheduled command: %s", result.ForLLM)
		} else {
			output = fmt.Sprintf("Scheduled command '%s' executed:\n%s", job.Payload.Command, result.ForLLM)
		}
		t.publishToChat(ctx, channel, chatID, output)
		return "ok", nil
	}

	return t.dispatchTrigger(ctx, job, channel, chatID, cronCommandTriggerPrompt(job, result))
}

// dispatchTrigger injects the firing into the agent through the inbound bus.
func (t *CronTool) dispatchTrigger(
	ctx context.Context,
	job *cron.CronJob,
	channel, chatID, content string,
) (string, error) {
	if t.msgBus == nil {
		return "", fmt.Errorf("cannot dispatch cron job %s: message bus is unavailable", job.ID)
	}

	sessionKey := t.triggerSessionKey(job)

	pubCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if err := t.msgBus.PublishInbound(pubCtx, bus.InboundMessage{
		Context:    cronTriggerInboundContext(job, channel, chatID),
		Sender:     bus.SenderInfo{DisplayName: cronSenderName},
		Content:    content,
		SessionKey: sessionKey,
	}); err != nil {
		return "", fmt.Errorf("failed to dispatch cron trigger: %w", err)
	}

	logger.InfoCF("cron", "Cron trigger dispatched", map[string]any{
		"job_id":       job.ID,
		"job_name":     job.Name,
		"channel":      channel,
		"chat_id":      chatID,
		"session_key":  sessionKey,
		"session_mode": t.sessionModeFor(job),
	})

	return "dispatched", nil
}

// triggerSessionKey resolves which session the firing runs in. An empty result
// is deliberate: it lets routing pick the channel's natural session for jobs
// scheduled before the session key was recorded (and by the CLI, which has no
// session at all).
func (t *CronTool) triggerSessionKey(job *cron.CronJob) string {
	if t.sessionModeFor(job) == config.CronSessionModeIsolated {
		return fmt.Sprintf("%s%s-%s", cronIsolatedSessionPrefix, job.ID, uuid.New().String())
	}
	return job.Payload.SessionKey
}

func (t *CronTool) sessionModeFor(job *cron.CronJob) string {
	return normalizeCronSessionMode(job.Payload.SessionMode, t.sessionMode)
}

func (t *CronTool) notifyModeFor(job *cron.CronJob) string {
	return normalizeCronNotify(job.Payload.Notify, t.notifyMode)
}

// shouldReportCommandResult decides whether a finished command is worth
// surfacing at all, before either delivery path spends anything on it.
func (t *CronTool) shouldReportCommandResult(job *cron.CronJob, result *ToolResult) bool {
	if t.notifyModeFor(job) == config.CronNotifyAlways {
		return true
	}
	output := strings.TrimSpace(result.ForLLM)
	return result.IsError || (output != "" && output != execNoOutputPlaceholder)
}

// cronTriggerInboundContext rebuilds the scheduling turn's inbound context.
// The sender id is preserved so routing resolves the same agent; the trigger is
// marked as machine-generated through the content header and display name.
func cronTriggerInboundContext(job *cron.CronJob, channel, chatID string) bus.InboundContext {
	inbound := bus.InboundContext{
		Channel:  channel,
		ChatID:   chatID,
		ChatType: "direct",
		SenderID: cronSenderID,
	}

	origin := job.Payload.Origin
	if origin == nil {
		return inbound
	}

	inbound.Account = origin.Account
	inbound.TopicID = origin.TopicID
	inbound.SpaceID = origin.SpaceID
	inbound.SpaceType = origin.SpaceType
	if origin.ChatType != "" {
		inbound.ChatType = origin.ChatType
	}
	if origin.SenderID != "" {
		inbound.SenderID = origin.SenderID
	}

	return inbound
}

// cronTriggerPrompt wraps the scheduled instruction so the agent can tell a
// firing apart from a fresh user message.
func cronTriggerPrompt(job *cron.CronJob) string {
	var sb strings.Builder
	sb.WriteString(cronTriggerHeader(job))
	sb.WriteString("This is an automated trigger, not a new message from the user. ")
	sb.WriteString("The instruction below was scheduled earlier in this conversation:\n\n")
	sb.WriteString(job.Payload.Message)
	return sb.String()
}

// cronCommandTriggerPrompt reports a scheduled command's result into the
// session and leaves the escalation decision to the agent.
func cronCommandTriggerPrompt(job *cron.CronJob, result *ToolResult) string {
	var sb strings.Builder
	sb.WriteString(cronTriggerHeader(job))
	sb.WriteString("This is an automated trigger, not a new message from the user. ")
	sb.WriteString(fmt.Sprintf("The scheduled command %q ", job.Payload.Command))
	if result.IsError {
		sb.WriteString("failed")
	} else {
		sb.WriteString("finished")
	}
	if message := strings.TrimSpace(job.Payload.Message); message != "" {
		sb.WriteString(fmt.Sprintf(" (purpose: %s)", message))
	}
	sb.WriteString(". Output:\n\n")
	sb.WriteString(utils.Truncate(strings.TrimSpace(result.ForLLM), cronCommandOutputLimit))
	sb.WriteString("\n\nDecide whether this needs reporting. Stay silent if there is nothing worth saying.")
	return sb.String()
}

func cronTriggerHeader(job *cron.CronJob) string {
	firedAt := time.Now()
	if tz := strings.TrimSpace(job.Schedule.TZ); tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			firedAt = firedAt.In(loc)
		}
	}
	return fmt.Sprintf(
		"[cron] Scheduled job %q (id: %s, %s) fired at %s.\n",
		job.Name,
		job.ID,
		describeCronSchedule(&job.Schedule),
		firedAt.Format("2006-01-02 15:04:05 MST"),
	)
}

// publishToChat delivers text straight to the chat, bypassing the agent. Used
// for cron failures the agent cannot be asked about and for raw command output.
func (t *CronTool) publishToChat(ctx context.Context, channel, chatID, content string) {
	if t.msgBus == nil {
		return
	}

	pubCtx, pubCancel := context.WithTimeout(ctx, 10*time.Second)
	defer pubCancel()

	feedback := make(chan bus.DeliveryResult, 1)
	_ = t.msgBus.PublishOutbound(pubCtx, bus.OutboundMessage{
		Context:  bus.NewOutboundContext(channel, chatID, ""),
		Content:  content,
		Feedback: feedback,
	})

	select {
	case res := <-feedback:
		if res.Err != nil {
			logger.WarnCF("cron", "Scheduled job delivery failed", map[string]any{"error": res.Err.Error()})
		}
	case <-pubCtx.Done():
		logger.WarnCF("cron", "Scheduled job delivery timeout", map[string]any{})
	}
}
