package tools

import (
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/cron"
)

func TestCronEffortBusOriginAndIsolated(t *testing.T) {
	for _, mode := range []string{config.CronSessionModeOrigin, config.CronSessionModeIsolated} {
		tool := newTestCronTool(t)
		job := &cron.CronJob{ID: "effort-job"}
		job.Payload.Channel = "telegram"
		job.Payload.To = "chat"
		job.Payload.Message = "hi"
		job.Payload.SessionKey = "original"
		job.Payload.SessionMode = mode
		job.Payload.Effort = "max"
		if got := executeTestCronJob(t, tool, job); got != "dispatched" {
			t.Fatal(got)
		}
		msg := waitForInboundTrigger(t, tool)
		if msg.Effort != "max" || !msg.DeferWhileBusy {
			t.Fatalf("lost effort/defer: %+v", msg)
		}
		if (msg.SessionKey == "original") != (mode == config.CronSessionModeOrigin) {
			t.Fatal(msg.SessionKey)
		}
	}
	for _, args := range []map[string]any{{"effort": "", "clear_effort": false}, {"effort": " HIGH "}, {"effort": "", "clear_effort": true}} {
		if _, _, err := cronEffortArgs(args); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range []map[string]any{{"effort": "bogus"}, {"effort": "high", "clear_effort": true}, {"clear_effort": "true"}} {
		if _, _, err := cronEffortArgs(args); err == nil {
			t.Fatal(args)
		}
	}
}
