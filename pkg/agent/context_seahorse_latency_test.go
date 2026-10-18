//go:build !mipsle && !netbsd && !(freebsd && arm)

package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/seahorse"
)

func TestSeahorseProactiveCooperativeDeadline(t *testing.T) {
	var calls atomic.Int32
	engine, err := seahorse.NewEngine(
		seahorse.Config{DBPath: t.TempDir() + "/test.db"},
		func(ctx context.Context, _ string, _ seahorse.CompleteOptions) (string, error) {
			calls.Add(1)
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > proactiveCompactTimeout {
				t.Error("missing proactive deadline")
			}
			<-ctx.Done()
			return "", ctx.Err()
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	mgr := &seahorseContextManager{engine: engine}
	ingestBulk(t, mgr, "deadline", 40)
	err = mgr.Compact(
		context.Background(),
		&CompactRequest{SessionKey: "deadline", Reason: ContextCompressReasonProactive, HistoryBudget: 1000},
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected cooperative cancellation, got %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("unexpected provider calls: %d", calls.Load())
	}
}

func TestSeahorseProactiveOnePassRetryRetainsBudget(t *testing.T) {
	mgr, engine, calls := newBudgetTestManager(t)
	ctx := context.Background()
	ingestBulk(t, mgr, "iterations", 100)
	req := &CompactRequest{SessionKey: "iterations", Reason: ContextCompressReasonProactive, HistoryBudget: 1000}
	if err := mgr.Compact(ctx, req); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 {
		t.Fatalf("proactive did not stop after one pass: %d", len(*calls))
	}
	before, err := engine.Assemble(ctx, "iterations", seahorse.AssembleInput{Budget: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	req.Reason = ContextCompressReasonRetry
	if err = mgr.Compact(ctx, req); err != nil {
		t.Fatal(err)
	}
	if len(*calls) <= 2 {
		t.Fatalf("retry inherited one-pass cap: %d", len(*calls))
	}
	after, err := engine.Assemble(ctx, "iterations", seahorse.AssembleInput{Budget: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if assembleTokens(after) >= assembleTokens(before) {
		t.Fatal("retry made no progress")
	}
}
