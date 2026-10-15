package providers

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

const retiredCodexModelMsg = "The 'gpt-5.5' model is not supported when using Codex with a ChatGPT account."

func TestClassifyError_ModelUnavailable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
	}{
		{"codex-ws server error", &wsServerError{StatusCode: 400, Msg: retiredCodexModelMsg}},
		{"wrapped as the prewarm returns it", fmt.Errorf("codex ws connect: prewarm drain: %w",
			&wsServerError{StatusCode: 400, Msg: retiredCodexModelMsg})},
		{"codex http", errors.New(
			"API request failed:\n  Status: 400\n  Body: {\"detail\":\"" + retiredCodexModelMsg + "\"}",
		)},
		{
			"openai api",
			errors.New(`status 404: {"error":{"code":"model_not_found","message":"The model gpt-9 does not exist"}}`),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fe := ClassifyError(tc.err, "codex-ws", "gpt-5.5")
			if fe == nil || fe.Reason != FailoverModelUnavailable {
				t.Fatalf("ClassifyError = %v, want model_unavailable", fe)
			}
			if !fe.IsRetriable() {
				t.Error("model_unavailable must let the chain move on to the next candidate")
			}
		})
	}
}

// A retired primary is skipped straight to the fallback: no in-place retry,
// which is only for timeouts and would ask for the same dead model again.
func TestFallback_ModelUnavailable_AdvancesWithoutInPlaceRetry(t *testing.T) {
	ct := NewCooldownTracker()
	fc := NewFallbackChain(ct, nil)
	fc.SetPrimaryTimeoutRetries(2)

	candidates := []FallbackCandidate{
		makeCandidate("codex-ws", "gpt-5.5"),
		makeCandidate("codex-ws", "gpt-6.1-sol"),
	}

	calls := map[string]int{}
	run := func(ctx context.Context, provider, model string) (*LLMResponse, error) {
		calls[model]++
		if model == "gpt-5.5" {
			return nil, fmt.Errorf("codex ws connect: prewarm drain: %w",
				&wsServerError{StatusCode: 400, Msg: retiredCodexModelMsg})
		}
		return &LLMResponse{Content: "ok", FinishReason: "stop"}, nil
	}

	result, err := fc.Execute(context.Background(), candidates, run)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Model != "gpt-6.1-sol" {
		t.Errorf("model = %q, want gpt-6.1-sol", result.Model)
	}
	if calls["gpt-5.5"] != 1 {
		t.Errorf("retired model called %d times, want 1", calls["gpt-5.5"])
	}
}
