package providers

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

// rawUsageLimitEvent is the exact top-level error event captured from codex-ws
// production logs when the Codex "plus" plan limit was reached.
const rawUsageLimitEvent = `{"type":"error","error":{"type":"usage_limit_reached","message":"The usage limit has been reached","plan_type":"plus","resets_at":1782926326,"eligible_promo":null,"resets_in_seconds":5238},"status_code":429}`

func TestWSErrorToGoError_UsageLimitFromRawEvent(t *testing.T) {
	t.Parallel()

	var evt wsEvent
	if err := json.Unmarshal([]byte(rawUsageLimitEvent), &evt); err != nil {
		t.Fatalf("unmarshal raw event: %v", err)
	}
	if evt.Type != "error" || evt.StatusCode != 429 {
		t.Fatalf("parsed evt = %+v, want type=error status=429", evt)
	}

	err := wsErrorToGoError(evt.Error, evt.StatusCode)

	var ule *UsageLimitError
	if !errors.As(err, &ule) {
		t.Fatalf("wsErrorToGoError returned %T (%v), want *UsageLimitError", err, err)
	}
	if got := ule.ResetsAt.Unix(); got != 1782926326 {
		t.Errorf("ResetsAt = %d, want 1782926326", got)
	}
	if ule.Message != "The usage limit has been reached" {
		t.Errorf("Message = %q", ule.Message)
	}
}

func TestUsageLimitResetsAt_PrefersAbsoluteThenRelative(t *testing.T) {
	t.Parallel()

	if got := usageLimitResetsAt(&wsErrorObj{ResetsAt: 1782926326}); got.Unix() != 1782926326 {
		t.Errorf("absolute resets_at: got %d", got.Unix())
	}
	rel := usageLimitResetsAt(&wsErrorObj{ResetsInSeconds: 5238})
	if d := time.Until(rel); d < 5230*time.Second || d > 5238*time.Second {
		t.Errorf("relative resets_in_seconds: remaining %v out of range", d)
	}
	if got := usageLimitResetsAt(&wsErrorObj{}); !got.IsZero() {
		t.Errorf("no reset info should yield zero time, got %v", got)
	}
}

func TestWSErrorToGoError_NonUsageLimitIsServerError(t *testing.T) {
	t.Parallel()

	err := wsErrorToGoError(&wsErrorObj{Type: "internal_error", Message: "boom"}, 500)
	var ule *UsageLimitError
	if errors.As(err, &ule) {
		t.Fatalf("500 error misclassified as usage limit")
	}
	var wse *wsServerError
	if !errors.As(err, &wse) {
		t.Fatalf("got %T, want *wsServerError", err)
	}
}

func TestResponseFailedToGoError(t *testing.T) {
	t.Parallel()

	// No error payload → nil (preserve prior non-erroring behavior).
	if err := responseFailedToGoError(&wsResponseObj{Status: "failed"}); err != nil {
		t.Errorf("failed response w/o error payload should be nil, got %v", err)
	}
	// Error payload with a usage limit → typed UsageLimitError.
	r := &wsResponseObj{Status: "failed", Error: &wsErrorObj{Type: "usage_limit_reached", ResetsAt: 111}}
	var ule *UsageLimitError
	if err := responseFailedToGoError(r); !errors.As(err, &ule) {
		t.Errorf("got %T, want *UsageLimitError", err)
	}
}

func TestIsTerminalServerError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"usage limit", &UsageLimitError{Provider: "codex-ws"}, true},
		{"wrapped usage limit", fmt.Errorf("codex ws: %w", &UsageLimitError{}), true},
		{"server error", &wsServerError{StatusCode: 500, Msg: "x"}, true},
		{"generic connection drop", errors.New("ws read: i/o timeout"), false},
	}
	for _, tc := range cases {
		if got := isTerminalServerError(tc.err); got != tc.want {
			t.Errorf("%s: isTerminalServerError = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestClassifyError_UsageLimitCarriesResetsAt(t *testing.T) {
	t.Parallel()

	reset := time.Unix(1782926326, 0)
	fe := ClassifyError(fmt.Errorf("codex ws: %w",
		&UsageLimitError{Provider: "codex-ws", ResetsAt: reset}), "codex-ws", "gpt-5.5")
	if fe == nil {
		t.Fatal("ClassifyError returned nil for usage limit")
	}
	if fe.Reason != FailoverRateLimit {
		t.Errorf("Reason = %s, want rate_limit", fe.Reason)
	}
	if !fe.IsRetriable() {
		t.Error("usage limit should be retriable")
	}
	if !fe.ResetsAt.Equal(reset) {
		t.Errorf("ResetsAt = %v, want %v", fe.ResetsAt, reset)
	}
}

func TestCooldown_MarkUnavailableUntil(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_000_000, 0)
	ct, current := newTestTracker(now)

	// Explicit near-future reset is honored exactly.
	reset := now.Add(87 * time.Minute)
	ct.MarkUnavailableUntil("codex-ws", reset, FailoverRateLimit)
	if ct.IsAvailable("codex-ws") {
		t.Fatal("provider should be unavailable during cooldown")
	}
	if rem := ct.CooldownRemaining("codex-ws"); rem != 87*time.Minute {
		t.Errorf("CooldownRemaining = %v, want 87m", rem)
	}

	// After the reset time it becomes available again.
	*current = reset.Add(time.Second)
	if !ct.IsAvailable("codex-ws") {
		t.Error("provider should be available after reset time")
	}
}

func TestCooldown_MarkUnavailableUntil_PastAndCapped(t *testing.T) {
	t.Parallel()

	now := time.Unix(2_000_000, 0)

	// A reset time in the past falls back to the standard curve (>0).
	ct, _ := newTestTracker(now)
	ct.MarkUnavailableUntil("p", now.Add(-time.Hour), FailoverRateLimit)
	if ct.IsAvailable("p") {
		t.Error("past reset should still apply a standard cooldown")
	}
	if rem := ct.CooldownRemaining("p"); rem <= 0 || rem > time.Hour {
		t.Errorf("standard fallback cooldown = %v, want (0, 1h]", rem)
	}

	// A far-future reset is capped at maxExplicitCooldown.
	ct2, _ := newTestTracker(now)
	ct2.MarkUnavailableUntil("q", now.Add(48*time.Hour), FailoverRateLimit)
	if rem := ct2.CooldownRemaining("q"); rem != maxExplicitCooldown {
		t.Errorf("capped cooldown = %v, want %v", rem, maxExplicitCooldown)
	}
}
