package cron

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestCronEffortPersistenceUpdateClearAndInvalidAtomicity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	cs := NewCronService(path, nil)
	input := AddJobInput{
		Name:     "effort",
		Schedule: CronSchedule{Kind: "every", EveryMS: int64Ptr(60000)},
		Payload:  CronPayload{Message: "hello", Effort: " HIGH "},
	}
	job, err := cs.AddJob(input)
	if err != nil {
		t.Fatal(err)
	}
	read := func() []byte {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	check := func(want string) {
		t.Helper()
		for _, service := range []*CronService{cs, NewCronService(path, nil)} {
			got, ok := service.GetJob(job.ID)
			if !ok || got.Payload.Effort != want {
				t.Fatalf("persisted effort: job=%+v exists=%v want=%q", got, ok, want)
			}
		}
	}
	check("high")
	before := read()
	input.Payload.Effort = "invalid"
	if _, err := cs.AddJob(input); err == nil {
		t.Fatal("invalid add accepted")
	}
	if len(cs.ListJobs(true)) != 1 || !bytes.Equal(before, read()) {
		t.Fatal("invalid add mutated memory or disk")
	}
	invalid, _ := cs.GetJob(job.ID)
	invalid.Name = "must not persist"
	invalid.Payload.Effort = "invalid"
	if err := cs.UpdateJob(invalid); err == nil {
		t.Fatal("invalid update accepted")
	}
	if !bytes.Equal(before, read()) {
		t.Fatal("invalid update mutated disk")
	}
	current, _ := cs.GetJob(job.ID)
	if current.Name != "effort" {
		t.Fatal("invalid update mutated memory")
	}
	check("high")
	current.Payload.Effort = " LoW "
	if err := cs.UpdateJob(current); err != nil {
		t.Fatal(err)
	}
	check("low")
	current.Payload.Effort = ""
	if err := cs.UpdateJob(current); err != nil {
		t.Fatal(err)
	}
	check("")
	if bytes.Contains(read(), []byte(`"effort":`)) {
		t.Fatal("cleared effort must be omitted for backwards compatibility")
	}
}

func TestCronEffortLegacyPayloadInherits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	legacy := []byte(
		`{"version":1,"jobs":[{"id":"legacy","name":"old","enabled":true,"schedule":{"kind":"every","everyMs":60000},"payload":{"kind":"agent_turn","message":"old"},"state":{},"createdAtMs":1,"updatedAtMs":1}]}`,
	)
	if err := os.WriteFile(path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	cs := NewCronService(path, nil)
	job, ok := cs.GetJob("legacy")
	if !ok || job.Payload.Effort != "" {
		t.Fatalf("legacy job did not inherit: %+v, %v", job, ok)
	}
	if err := cs.UpdateJob(job); err != nil {
		t.Fatal(err)
	}
	reloaded, ok := NewCronService(path, nil).GetJob("legacy")
	if !ok || reloaded.Payload.Effort != "" {
		t.Fatal("legacy roundtrip changed effort")
	}
}
