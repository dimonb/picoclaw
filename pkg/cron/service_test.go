package cron

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestSaveStore_FilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file permission bits are not enforced on Windows")
	}

	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "cron", "jobs.json")

	cs := NewCronService(storePath, nil)

	_, err := cs.AddJob("test", CronSchedule{Kind: "every", EveryMS: int64Ptr(60000)}, "hello", false, "cli", "direct")
	if err != nil {
		t.Fatalf("AddJob failed: %v", err)
	}

	info, err := os.Stat(storePath)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}

	perm := info.Mode().Perm()
	if perm != 0o600 {
		t.Errorf("cron store has permission %04o, want 0600", perm)
	}
}

func TestStart_OverdueOneShotJobRunsImmediatelyAfterRestart(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "cron", "jobs.json")

	overdueAt := time.Now().Add(-2 * time.Minute).UnixMilli()
	store := CronStore{
		Version: 1,
		Jobs: []CronJob{{
			ID:      "job-overdue-at",
			Name:    "overdue one-shot",
			Enabled: true,
			Schedule: CronSchedule{
				Kind: "at",
				AtMS: &overdueAt,
			},
			Payload: CronPayload{
				Kind:    "agent_turn",
				Message: "run me",
			},
			State: CronJobState{},
		}},
	}

	if err := os.MkdirAll(filepath.Dir(storePath), 0o755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	data, err := json.Marshal(store)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	if err := os.WriteFile(storePath, data, 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	executed := make(chan *CronJob, 1)
	cs := NewCronService(storePath, func(job *CronJob) (string, error) {
		executed <- job
		return "ok", nil
	})

	if err := cs.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer cs.Stop()

	select {
	case job := <-executed:
		if job.ID != "job-overdue-at" {
			t.Fatalf("executed unexpected job id %q", job.ID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("expected overdue one-shot job to execute after startup")
	}

	if err := cs.Load(); err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	jobs := cs.ListJobs(false)
	if len(jobs) != 0 {
		t.Fatalf("expected no enabled jobs after one-shot execution, got %d", len(jobs))
	}
	allJobs := cs.ListJobs(true)
	if len(allJobs) != 1 || allJobs[0].Enabled {
		t.Fatalf("expected executed overdue one-shot job to remain only as disabled history entry, got %+v", allJobs)
	}
}

func TestStart_ExecutedOneShotJobIsNotReplayed(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "cron", "jobs.json")

	overdueAt := time.Now().Add(-2 * time.Minute).UnixMilli()
	lastRun := time.Now().Add(-90 * time.Second).UnixMilli()
	store := CronStore{
		Version: 1,
		Jobs: []CronJob{{
			ID:      "job-already-ran",
			Name:    "already ran one-shot",
			Enabled: true,
			Schedule: CronSchedule{
				Kind: "at",
				AtMS: &overdueAt,
			},
			Payload: CronPayload{
				Kind:    "agent_turn",
				Message: "do not replay",
			},
			State: CronJobState{
				LastRunAtMS: &lastRun,
			},
		}},
	}

	if err := os.MkdirAll(filepath.Dir(storePath), 0o755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	data, err := json.Marshal(store)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	if err := os.WriteFile(storePath, data, 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	executed := make(chan *CronJob, 1)
	cs := NewCronService(storePath, func(job *CronJob) (string, error) {
		executed <- job
		return "ok", nil
	})

	if err := cs.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer cs.Stop()

	select {
	case job := <-executed:
		t.Fatalf("did not expect already executed one-shot job to replay, got %q", job.ID)
	case <-time.After(1500 * time.Millisecond):
	}

	if err := cs.Load(); err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	jobs := cs.ListJobs(true)
	if len(jobs) != 1 {
		t.Fatalf("expected job to remain present, got %d jobs", len(jobs))
	}
	if jobs[0].State.NextRunAtMS != nil {
		t.Fatal("expected already executed overdue one-shot job to have no next run")
	}
}

func int64Ptr(v int64) *int64 {
	return &v
}
