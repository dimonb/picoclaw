package seahorse

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

// Both source kinds are eligible: without the shared guard every pairing
// below reaches a second provider call, rather than vacuously finding no work.
func TestEligibleCompactionSerialization(t *testing.T) {
	for _, first := range []string{"leaf", "condensed"} {
		for _, second := range []string{"leaf", "condensed"} {
			t.Run(first+"_then_"+second, func(t *testing.T) {
				ce, s, id := newTestCompactionEngine(t)
				ctx := context.Background()
				seedLeafSummaries(t, s, id, CondensedMinFanout, 1000)
				for i := 0; i < 80; i++ {
					m, err := s.AddMessage(ctx, id, "user", fmt.Sprintf("source %d", i), 100)
					if err != nil {
						t.Fatal(err)
					}
					if err = s.AppendContextMessage(ctx, id, m.ID); err != nil {
						t.Fatal(err)
					}
				}
				before, err := s.GetContextItems(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				entered := make(chan struct{})
				release := make(chan struct{})
				var calls atomic.Int32
				ce.complete = func(context.Context, string, CompleteOptions) (string, error) {
					if calls.Add(1) == 1 {
						close(entered)
						<-release
					}
					return "A compact summary.", nil
				}
				run := func(kind string) (*string, error) {
					if kind == "leaf" {
						return ce.compactLeaf(ctx, id, true)
					}
					return ce.compactCondensed(ctx, id)
				}
				type result struct {
					id  *string
					err error
				}
				done := make(chan result, 1)
				go func() { v, e := run(first); done <- result{v, e} }()
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("eligible provider not entered")
				}
				busy, busyErr := run(second)
				during, readErr := s.GetContextItems(ctx, id)
				// Always release the provider before assertions, including on regression.
				close(release)
				finished := <-done
				if busy != nil || busyErr != nil || calls.Load() != 1 || readErr != nil || !reflect.DeepEqual(before, during) {
					t.Fatalf("overlapping pass: id=%v err=%v calls=%d read=%v", busy, busyErr, calls.Load(), readErr)
				}
				if finished.err != nil || finished.id == nil {
					t.Fatalf("first pass: %+v", finished)
				}
				after, err := s.GetContextItems(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, item := range after {
					if item.SummaryID == *finished.id {
						found = true
					}
				}
				if !found {
					t.Fatal("published summary absent from context")
				}
				if first == "condensed" {
					parents, e := s.GetSummaryParents(ctx, *finished.id)
					if e != nil || len(parents) != CondensedMinFanout {
						t.Fatalf("lost parent links: %v %v", parents, e)
					}
					for _, parent := range parents {
						if _, e = s.GetSummary(ctx, parent.SummaryID); e != nil {
							t.Fatal(e)
						}
					}
				} else {
					sources, e := s.GetSummarySourceMessages(ctx, *finished.id)
					if e != nil || len(sources) == 0 {
						t.Fatalf("lost source links: %d %v", len(sources), e)
					}
				}
				var count int
				if err = s.db.QueryRowContext(ctx, "SELECT count(*) FROM messages WHERE conversation_id = ?", id).Scan(&count); err != nil || count != 80 {
					t.Fatalf("history count=%d err=%v", count, err)
				}
				// Seed a fresh eligible group even if the first pass consumed this kind.
				for i := 0; i < CondensedMinFanout; i++ {
					sum, e := s.CreateSummary(ctx, CreateSummaryInput{ConversationID: id, Kind: SummaryKindLeaf, Content: "later leaf", TokenCount: 1000})
					if e != nil {
						t.Fatal(e)
					}
					if e = s.AppendContextSummary(ctx, id, sum.SummaryID); e != nil {
						t.Fatal(e)
					}
				}
				for i := 0; i < 80; i++ {
					m, e := s.AddMessage(ctx, id, "user", "later raw source", 100)
					if e != nil {
						t.Fatal(e)
					}
					if e = s.AppendContextMessage(ctx, id, m.ID); e != nil {
						t.Fatal(e)
					}
				}
				later, e := run(second)
				if e != nil || later == nil || calls.Load() < 2 {
					t.Fatalf("guard failed to release: %v %v calls=%d", later, e, calls.Load())
				}
			})
		}
	}
}

func TestCompactionMutationGuardReleasedOnFailure(t *testing.T) {
	for _, kind := range []string{"leaf", "condensed"} {
		for _, cancel := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/cancel=%v", kind, cancel), func(t *testing.T) {
				ce, s, id := newTestCompactionEngine(t)
				ctx := context.Background()
				seedLeafSummaries(t, s, id, CondensedMinFanout, 1000)
				for i := 0; i < 80; i++ {
					m, e := s.AddMessage(ctx, id, "user", "source", 100)
					if e != nil {
						t.Fatal(e)
					}
					if e = s.AppendContextMessage(ctx, id, m.ID); e != nil {
						t.Fatal(e)
					}
				}
				before, _ := s.GetContextItems(ctx, id)
				failureCtx, cancelFn := context.WithCancel(ctx)
				defer cancelFn()
				ce.complete = func(c context.Context, _ string, _ CompleteOptions) (string, error) {
					if cancel {
						cancelFn()
						return "", c.Err()
					}
					return "", errors.New("provider unavailable")
				}
				run := func(c context.Context) (*string, error) {
					if kind == "leaf" {
						return ce.compactLeaf(c, id, true)
					}
					return ce.compactCondensed(c, id)
				}
				if _, e := run(failureCtx); e == nil {
					t.Fatal("expected provider failure")
				}
				after, _ := s.GetContextItems(ctx, id)
				if !reflect.DeepEqual(before, after) {
					t.Fatal("failed pass mutated context")
				}
				ce.complete = func(context.Context, string, CompleteOptions) (string, error) { return "A compact summary.", nil }
				if v, e := run(ctx); e != nil || v == nil {
					t.Fatalf("guard retained after failure: %v %v", v, e)
				}
			})
		}
	}
}
