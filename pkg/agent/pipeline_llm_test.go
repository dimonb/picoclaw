package agent

import (
	"errors"
	"testing"
)

// A retired model is not transient: retrying the turn after a backoff asks the
// backend for the same model and fails the same way, only later.
func TestTransientLLMRetryReason_ModelUnavailable(t *testing.T) {
	// Shaped like the fallback chain reports it: the per-candidate text used to
	// say failover(timeout), which matched the timeout pattern here.
	err := errors.New("fallback: all 2 candidates failed:\n  [1] codex-ws/gpt-5.5: failover(timeout): " +
		"codex ws connect: prewarm drain: codex ws: server error: " +
		"The 'gpt-5.5' model is not supported when using Codex with a ChatGPT account. (reason=timeout, 569ms)")
	if reason, ok := transientLLMRetryReason(err); ok {
		t.Fatalf("transientLLMRetryReason = %q, true; want not transient", reason)
	}
}
