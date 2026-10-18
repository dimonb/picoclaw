package seahorse

import (
	"context"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func TestCompactionCrossEntryPointSerialization(t *testing.T) {
	for _, foregroundFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(foregroundFirst), func(t *testing.T) {
			ce, s, id := newTestCompactionEngine(t)
			ctx := context.Background()
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
			done := make(chan error, 1)
			var calls atomic.Int32
			ce.complete = func(context.Context, string, CompleteOptions) (string, error) {
				if calls.Add(1) == 1 {
					close(entered)
				}
				<-release
				return "Short summary.", nil
			}
			run := func(foreground bool) error {
				if foreground {
					_, err := ce.CompactUntilUnder(ctx, id, 1, 1)
					return err
				}
				budget := 100000
				_, err := ce.Compact(ctx, id, CompactInput{Budget: &budget, Force: true})
				return err
			}
			go func() { done <- run(foregroundFirst) }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("provider not entered")
			}
			// The other public entry point must neither call provider nor rewrite ordinals.
			if err := run(!foregroundFirst); err != nil {
				t.Fatal(err)
			}
			// Condensed selection must use the same guard too.
			if result, err := ce.compactCondensed(ctx, id); err != nil || result != nil {
				t.Fatalf("busy condensed result %v %v", result, err)
			}
			during, err := s.GetContextItems(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, during) || calls.Load() != 1 {
				t.Fatal("overlapping pass mutated source or called provider")
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if !waitForCondensed(ce, id, time.Second) {
				t.Fatal("async rollup did not finish")
			}
			after, err := s.GetContextItems(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			summaries := 0
			for _, item := range after {
				if item.ItemType == "summary" {
					summaries++
				}
			}
			if summaries != 1 {
				t.Fatalf("expected exactly one summary, got %d", summaries)
			}
			// Summarized messages must still be available for history recall.
			var count int
			if err = s.db.QueryRowContext(ctx, "SELECT count(*) FROM messages WHERE conversation_id = ?", id).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 80 {
				t.Fatalf("raw history lost: %d", count)
			}
			// Guard released: later maintenance still progresses.
			for i := 0; i < 40; i++ {
				m, err := s.AddMessage(ctx, id, "user", "later source", 100)
				if err != nil {
					t.Fatal(err)
				}
				if err = s.AppendContextMessage(ctx, id, m.ID); err != nil {
					t.Fatal(err)
				}
			}
			after, _ = s.GetContextItems(ctx, id)
			ce.complete = mockCompleteFn
			if _, err = ce.CompactUntilUnder(ctx, id, 1, 1); err != nil {
				t.Fatal(err)
			}
			next, _ := s.GetContextItems(ctx, id)
			if reflect.DeepEqual(after, next) {
				t.Fatal("guard blocked subsequent progress")
			}
		})
	}
}
