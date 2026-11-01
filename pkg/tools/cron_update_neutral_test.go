package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/cron"
)

// Model the all-fields payload emitted by strict-schema providers, including
// fields added in this fix. Decode JSON to exercise the real float64 path.
func strictCronUpdate(t *testing.T, id string) map[string]any {
	t.Helper()
	var args map[string]any
	err := json.Unmarshal(
		[]byte(
			fmt.Sprintf(
				`{"action":"update","job_id":%q,"message":"","name":"","command":"","command_confirm":false,"clear_command":false,"at_seconds":0,"every_seconds":0,"cron_expr":"","session":"","notify":""}`,
				id,
			),
		),
		&args,
	)
	if err != nil {
		t.Fatal(err)
	}
	return args
}

func TestCronTool_StrictUpdatePreservesUnchangedFields(t *testing.T) {
	for _, kind := range []string{"at", "every", "cron"} {
		t.Run(kind, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.Tools.Cron.CommandAllowedRemotes = []string{"telegram:chat-1"}
			cfg.Tools.Cron.AllowCommand = false // Retaining a command must not need confirmation.
			tool := newTestCronToolWithConfig(t, cfg)
			ctx := WithToolContext(context.Background(), "telegram", "chat-1")
			at, every := time.Now().Add(time.Hour).UnixMilli(), int64(60000)
			schedule := cron.CronSchedule{Kind: kind}
			switch kind {
			case "at":
				schedule.AtMS = &at
			case "every":
				schedule.EveryMS = &every
			case "cron":
				schedule.Expr = "0 8 * * *"
				schedule.TZ = "Asia/Jerusalem"
			}
			original, err := tool.cronService.AddJob(cron.AddJobInput{
				Name:     "preserve name",
				Schedule: schedule,
				Payload: cron.CronPayload{
					Kind:        "agent_turn",
					Message:     "old",
					Command:     "echo keep",
					Channel:     "telegram",
					To:          "chat-1",
					SessionKey:  "pinned",
					AgentID:     "agent-1",
					SessionMode: "isolated",
					Notify:      "always",
					Origin:      &cron.CronOrigin{TopicID: "42", SenderID: "owner"},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			args := strictCronUpdate(t, original.ID)
			args["message"] = "new"
			if result := tool.Execute(ctx, args); result.IsError {
				t.Fatal(result.ForLLM)
			}
			updated, _ := tool.cronService.GetJob(original.ID)
			expected := *original
			expected.Payload.Message = "new"
			expected.UpdatedAtMS = updated.UpdatedAtMS
			if !reflect.DeepEqual(expected, *updated) {
				t.Fatalf("unexpected mutation:\nbefore %+v\nafter %+v", expected, *updated)
			}
		})
	}
}

func TestCronTool_StrictUpdateRealSchedules(t *testing.T) {
	for _, key := range []string{"at_seconds", "every_seconds", "cron_expr"} {
		t.Run(key, func(t *testing.T) {
			tool := newTestCronTool(t)
			job := addTestCronJob(t, tool, "keep", "cli", "direct", "")
			args := strictCronUpdate(t, job.ID)
			if key == "cron_expr" {
				args[key] = "0 9 * * *"
			} else {
				args[key] = float64(120)
			}
			if result := tool.Execute(WithToolContext(context.Background(), "cli", "direct"), args); result.IsError {
				t.Fatal(result.ForLLM)
			}
			updated, _ := tool.cronService.GetJob(job.ID)
			switch key {
			case "at_seconds":
				if updated.Schedule.Kind != "at" || !updated.DeleteAfterRun || updated.Schedule.AtMS == nil {
					t.Fatal(updated.Schedule)
				}
			case "every_seconds":
				if updated.Schedule.Kind != "every" || *updated.Schedule.EveryMS != 120000 || updated.DeleteAfterRun {
					t.Fatal(updated.Schedule)
				}
			case "cron_expr":
				if updated.Schedule.Kind != "cron" || updated.Schedule.Expr != "0 9 * * *" || updated.DeleteAfterRun {
					t.Fatal(updated.Schedule)
				}
			}
			if updated.Payload.Message != job.Payload.Message || updated.Name != job.Name {
				t.Fatal("neutral strings changed payload/name")
			}
		})
	}
}

func TestCronTool_StrictUpdateInvalidIsAtomic(t *testing.T) {
	tests := []struct {
		key   string
		value any
	}{
		{"at_seconds", -1.0},
		{"every_seconds", -1},
		{"at_seconds", 1.5},
		{"every_seconds", "0"},
		{"at_seconds", nil},
		{"at_seconds", math.NaN()},
		{"at_seconds", math.Inf(1)},
		{"at_seconds", float64(math.MaxInt64)},
		{"every_seconds", int64(math.MaxInt64)},
		{"cron_expr", 123},
		{"cron_expr", " "},
		{"cron_expr", "invalid"},
		{"name", " "},
		{"name", 123},
		{"message", false},
		{"command", nil},
		{"session", "invalid"},
		{"notify", "invalid"},
		{"clear_command", "false"},
	}
	for i, tt := range tests {
		t.Run(fmt.Sprintf("%s-%d", tt.key, i), func(t *testing.T) {
			tool := newTestCronTool(t)
			job := addTestCronJob(t, tool, "keep", "cli", "direct", "")
			args := strictCronUpdate(t, job.ID)
			args["message"] = "new"
			args[tt.key] = tt.value
			if result := tool.Execute(WithToolContext(context.Background(), "cli", "direct"), args); !result.IsError {
				t.Fatalf("accepted invalid %s=%v", tt.key, tt.value)
			}
			updated, _ := tool.cronService.GetJob(job.ID)
			if !reflect.DeepEqual(job, updated) {
				t.Fatal("invalid update changed stored job")
			}
		})
	}
	tool := newTestCronTool(t)
	job := addTestCronJob(t, tool, "keep", "cli", "direct", "")
	ctx := WithToolContext(context.Background(), "cli", "direct")
	args := strictCronUpdate(t, job.ID)
	if result := tool.Execute(ctx, args); !result.IsError || !strings.Contains(result.ForLLM, "at least one") {
		t.Fatal(result)
	}
	args["at_seconds"] = 60.0
	args["every_seconds"] = 120.0
	if result := tool.Execute(ctx, args); !result.IsError || !strings.Contains(result.ForLLM, "only one") {
		t.Fatal(result)
	}
	args["every_seconds"] = 0.0
	args["cron_expr"] = "0 9 * * *"
	if result := tool.Execute(ctx, args); !result.IsError || !strings.Contains(result.ForLLM, "only one") {
		t.Fatal(result)
	}
}

func TestCronTool_StrictUpdateAccessAndCommandGates(t *testing.T) {
	for _, tc := range []struct {
		name, channel, chat, session  string
		allowed, exec, confirm, clear bool
		command, want                 string
	}{
		{"other chat", "telegram", "other", "", true, true, false, false, "", "not accessible"},
		{"other session", "telegram", "chat-1", "other-session", true, true, false, false, "", "not accessible"},
		{"unallowed command job", "telegram", "chat-1", "", false, true, false, false, "", "not accessible"},
		{"replace needs confirm", "cli", "direct", "", false, true, false, false, "echo new", "command_confirm"},
		{"clear needs confirm", "cli", "direct", "", false, true, false, true, "", "command_confirm"},
		{"clear exec disabled", "cli", "direct", "", false, false, true, true, "", "disabled"},
		{"replace exec disabled", "cli", "direct", "", false, false, true, false, "echo new", "disabled"},
		{"clear conflict", "cli", "direct", "", false, true, true, true, "echo new", "cannot be combined"},
		{"clear explicit", "cli", "direct", "", false, true, true, true, "", ""},
		{"replace explicit", "cli", "direct", "", false, true, true, false, "echo new", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.Tools.Exec.Enabled = tc.exec
			cfg.Tools.Cron.AllowCommand = false
			if tc.allowed {
				cfg.Tools.Cron.CommandAllowedRemotes = []string{"telegram:chat-1"}
			}
			tool := newTestCronToolWithConfig(t, cfg)
			job := addTestCronJob(t, tool, "keep", "telegram", "chat-1", "echo keep")
			job.Payload.SessionKey = "pinned"
			if err := tool.cronService.UpdateJob(job); err != nil {
				t.Fatal(err)
			}
			before, _ := tool.cronService.GetJob(job.ID)
			args := strictCronUpdate(t, job.ID)
			args["message"] = "new"
			args["clear_command"] = tc.clear
			args["command_confirm"] = tc.confirm
			args["command"] = tc.command
			ctx := WithToolContext(context.Background(), tc.channel, tc.chat)
			if tc.session != "" {
				ctx = WithToolSessionContext(ctx, "main", tc.session, nil)
			}
			result := tool.Execute(ctx, args)
			updated, _ := tool.cronService.GetJob(job.ID)
			if tc.want != "" {
				if !result.IsError || !strings.Contains(result.ForLLM, tc.want) {
					t.Fatalf("want %s, got %+v", tc.want, result)
				}
				if !reflect.DeepEqual(before, updated) {
					t.Fatal("rejected update mutated job")
				}
				return
			}
			if result.IsError {
				t.Fatal(result.ForLLM)
			}
			if updated.Payload.Command != tc.command {
				t.Fatalf("command = %q", updated.Payload.Command)
			}
		})
	}
}

func TestCronTool_StrictUpdateRealPayloadFields(t *testing.T) {
	tool := newTestCronTool(t)
	job := addTestCronJob(t, tool, "old", "telegram", "chat-1", "")
	args := strictCronUpdate(t, job.ID)
	args["name"] = "new name"
	args["session"] = "isolated"
	args["notify"] = "always"
	// Older schemas lack clear_command: absence must be just as neutral as false.
	delete(args, "clear_command")
	result := tool.Execute(WithToolContext(context.Background(), "telegram", "chat-1"), args)
	if result.IsError {
		t.Fatal(result.ForLLM)
	}
	updated, _ := tool.cronService.GetJob(job.ID)
	if updated.Name != "new name" || updated.Payload.SessionMode != "isolated" || updated.Payload.Notify != "always" {
		t.Fatalf("payload update lost: %+v", updated)
	}
	if updated.Payload.Message != job.Payload.Message || !reflect.DeepEqual(updated.Schedule, job.Schedule) ||
		!reflect.DeepEqual(updated.State, job.State) {
		t.Fatal("neutral message/schedule/state not retained")
	}
}

func TestCronTool_StrictUpdateNeutralIntegerTypes(t *testing.T) {
	for _, zero := range []any{int(0), int64(0), float64(0)} {
		schedule, has, errResult := schedulePatch(
			map[string]any{"at_seconds": zero, "every_seconds": zero, "cron_expr": ""},
		)
		if errResult != nil || has || !reflect.DeepEqual(schedule, cron.CronSchedule{}) {
			t.Fatalf("zero %T not neutral", zero)
		}
	}
}

func TestCronTool_StrictNeutralSchemaAndClearWithoutCommand(t *testing.T) {
	tool := newTestCronTool(t)
	props := tool.Parameters()["properties"].(map[string]any)
	for _, key := range []string{"session", "notify"} {
		schema := props[key].(map[string]any)
		neutral := false
		for _, value := range schema["enum"].([]string) {
			if value == "" {
				neutral = true
			}
		}
		if !neutral {
			t.Fatalf("%s schema excludes empty unchanged value", key)
		}
	}
	if props["clear_command"].(map[string]any)["type"] != "boolean" {
		t.Fatal("clear_command must have explicit boolean schema")
	}
	job := addTestCronJob(t, tool, "keep", "cli", "direct", "echo keep")
	args := strictCronUpdate(t, job.ID)
	delete(args, "command")
	args["clear_command"] = true
	args["command_confirm"] = true
	if result := tool.Execute(WithToolContext(context.Background(), "cli", "direct"), args); result.IsError {
		t.Fatal(result.ForLLM)
	}
	updated, _ := tool.cronService.GetJob(job.ID)
	if updated.Payload.Command != "" {
		t.Fatal("clear_command without command did not clear")
	}
}
