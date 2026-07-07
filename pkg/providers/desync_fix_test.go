package providers

import (
	"fmt"
	"testing"
)

// A codex-ws application-level server rejection (e.g. the stateful-protocol
// desync "No tool output found for function call ...") must be classified as a
// retriable FailoverError so the fallback chain switches to another provider
// instead of surfacing a hard error to the user.
func TestClassifyError_WSServerError_TriggersFailover(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
	}{
		{"no tool output desync", &wsServerError{StatusCode: 0, Msg: "No tool output found for function call call_x"}},
		{"wrapped as chatStream returns it", fmt.Errorf("codex ws: %w", &wsServerError{Msg: "No tool output found for function call call_x"})},
		{"400-ish must not be non-retriable Format", &wsServerError{StatusCode: 400, Msg: "bad request"}},
		{"500 transient", &wsServerError{StatusCode: 500, Msg: "internal"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fe := ClassifyError(tc.err, "codex-ws", "gpt-5.5")
			if fe == nil {
				t.Fatalf("ClassifyError returned nil; want a FailoverError so the chain fails over")
			}
			if !fe.IsRetriable() {
				t.Errorf("Reason=%s is not retriable; the turn would be stranded instead of failing over", fe.Reason)
			}
		})
	}
}

func TestClassifyError_WSServerError_AuthPreserved(t *testing.T) {
	t.Parallel()

	fe := ClassifyError(&wsServerError{StatusCode: 401, Msg: "unauthorized"}, "codex-ws", "gpt-5.5")
	if fe == nil || fe.Reason != FailoverAuth {
		t.Fatalf("401 server error => %v, want FailoverAuth", fe)
	}
}

func TestEnsureToolOutputs_PairedUnchanged(t *testing.T) {
	t.Parallel()

	in := []Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_x", Name: "exec"}}},
		{Role: "tool", ToolCallID: "call_x", Content: "ok"},
	}
	out := ensureToolOutputs(in)
	if len(out) != len(in) {
		t.Fatalf("paired history should be returned unchanged, len=%d want %d", len(out), len(in))
	}
}

func TestEnsureToolOutputs_SynthesizesForOrphan(t *testing.T) {
	t.Parallel()

	// Compaction dropped the tool result for call_x but kept its assistant call.
	in := []Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_x", Name: "exec"}}},
		{Role: "assistant", Content: "done"},
	}
	out := ensureToolOutputs(in)

	foundAt := -1
	for i, m := range out {
		if m.Role == "tool" && m.ToolCallID == "call_x" {
			foundAt = i
		}
	}
	if foundAt == -1 {
		t.Fatalf("no synthetic tool output inserted for orphaned call_x; out=%+v", out)
	}
	if foundAt == 0 || out[foundAt-1].Role != "assistant" {
		t.Errorf("synthetic output must sit right after its assistant message (idx %d)", foundAt)
	}
	// Idempotent: a second pass over the now-paired history is a no-op.
	if o := ensureToolOutputs(out); len(o) != len(out) {
		t.Errorf("second pass mutated an already-paired history: %d -> %d", len(out), len(o))
	}
}

func TestEnsureToolOutputs_UserRoleCountsAsOutput(t *testing.T) {
	t.Parallel()

	// A tool result delivered as role "user" + ToolCallID (buildWSInput maps
	// this to function_call_output) must count as paired.
	in := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_y"}}},
		{Role: "user", ToolCallID: "call_y", Content: "result"},
	}
	out := ensureToolOutputs(in)
	if len(out) != len(in) {
		t.Fatalf("user-role tool result should count as paired; len=%d want %d", len(out), len(in))
	}
}
