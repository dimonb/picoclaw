package providers

import "testing"

func TestResolveCodexModelAliasesAndDefault(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		input        string
		wantModel    string
		wantFallback bool
	}{
		{name: "astra alias", input: "astra", wantModel: "gpt-6-astra"},
		{name: "astra alias with openai prefix", input: "openai/astra", wantModel: "gpt-6-astra"},
		{name: "astra alias is case insensitive", input: "Astra", wantModel: "gpt-6-astra"},
		{name: "canonical astra passes through", input: "gpt-6-astra", wantModel: "gpt-6-astra"},
		{name: "empty falls back", input: "", wantModel: codexDefaultModel, wantFallback: true},
		{name: "foreign model falls back", input: "claude-opus-5", wantModel: codexDefaultModel, wantFallback: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotModel, reason := resolveCodexModel(tt.input)
			if gotModel != tt.wantModel {
				t.Fatalf("resolveCodexModel(%q) = %q, want %q", tt.input, gotModel, tt.wantModel)
			}
			if tt.wantFallback != (reason != "") {
				t.Fatalf("resolveCodexModel(%q) reason = %q, wantFallback %t", tt.input, reason, tt.wantFallback)
			}
		})
	}
}

// The default is what every unresolvable model is rewritten to, so a retired
// model here fails every such turn.
func TestCodexDefaultModelIsServed(t *testing.T) {
	t.Parallel()

	if codexDefaultModel == "gpt-5.3-codex" {
		t.Fatalf("codexDefaultModel is retired: the backend answers "+
			"%q with \"not supported when using Codex with a ChatGPT account\"", codexDefaultModel)
	}
}

func TestCodexReasoningEffortMax(t *testing.T) {
	t.Parallel()

	effort, ok := codexReasoningEffort("max")
	if !ok || string(effort) != "max" {
		t.Fatalf("codexReasoningEffort(\"max\") = (%q, %t), want (\"max\", true)", effort, ok)
	}
	if _, ok := codexReasoningEffort("nonsense"); ok {
		t.Fatalf("codexReasoningEffort(\"nonsense\") should not resolve")
	}
}

func TestParseCodexUnsupportedEffort(t *testing.T) {
	t.Parallel()

	// Verbatim message returned by gpt-6-astra for thinking_level=none.
	msg := "Unsupported value: 'none' is not supported with the 'gpt-6-astra' model. " +
		"Supported values are: 'low', 'medium', 'high', 'xhigh', and 'max'."

	rejected, supported, ok := parseCodexUnsupportedEffort(msg)
	if !ok {
		t.Fatalf("parseCodexUnsupportedEffort did not match the server message")
	}
	if rejected != "none" {
		t.Fatalf("rejected = %q, want none", rejected)
	}
	want := []string{"low", "medium", "high", "xhigh", "max"}
	if len(supported) != len(want) {
		t.Fatalf("supported = %v, want %v", supported, want)
	}
	for i := range want {
		if supported[i] != want[i] {
			t.Fatalf("supported = %v, want %v", supported, want)
		}
	}

	if _, _, ok := parseCodexUnsupportedEffort("The usage limit has been reached"); ok {
		t.Fatalf("an unrelated server error must not be read as an effort rejection")
	}
}

func TestNearestCodexEffort(t *testing.T) {
	t.Parallel()

	astra := []string{"low", "medium", "high", "xhigh", "max"}

	tests := []struct {
		want      string
		supported []string
		expect    string
		expectOK  bool
	}{
		{want: "none", supported: astra, expect: "low", expectOK: true},
		{want: "minimal", supported: astra, expect: "low", expectOK: true},
		{want: "medium", supported: astra, expect: "medium", expectOK: true},
		{want: "max", supported: []string{"low", "medium", "high"}, expect: "high", expectOK: true},
		// Ties resolve upward so a request is never downgraded further than needed.
		{want: "medium", supported: []string{"low", "high"}, expect: "high", expectOK: true},
		{want: "unknown-level", supported: astra},
		{want: "none", supported: []string{"bogus"}},
	}

	for _, tt := range tests {
		got, ok := nearestCodexEffort(tt.want, tt.supported)
		if ok != tt.expectOK || got != tt.expect {
			t.Fatalf("nearestCodexEffort(%q, %v) = (%q, %t), want (%q, %t)",
				tt.want, tt.supported, got, ok, tt.expect, tt.expectOK)
		}
	}
}

func TestParseWSResponseUsage(t *testing.T) {
	t.Parallel()

	items := []wsOutputItem{{
		Type:    "message",
		Content: []wsContentPart{{Type: "output_text", Text: "hi"}},
	}}

	resp := parseWSResponse(items, wsUsage{InputTokens: 21, OutputTokens: 5, TotalTokens: 26})
	if resp.Usage == nil {
		t.Fatalf("usage dropped: context-usage reporting depends on it")
	}
	if resp.Usage.PromptTokens != 21 || resp.Usage.CompletionTokens != 5 || resp.Usage.TotalTokens != 26 {
		t.Fatalf("usage = %+v", resp.Usage)
	}

	// A total the backend omitted is derived, not reported as zero.
	resp = parseWSResponse(items, wsUsage{InputTokens: 7, OutputTokens: 3})
	if resp.Usage == nil || resp.Usage.TotalTokens != 10 {
		t.Fatalf("usage = %+v, want derived total 10", resp.Usage)
	}

	// No usage at all stays nil: zeros would read as a real measurement.
	if resp := parseWSResponse(items, wsUsage{}); resp.Usage != nil {
		t.Fatalf("usage = %+v, want nil when the backend reported none", resp.Usage)
	}
}

func TestCodexEffortFallbackFromError(t *testing.T) {
	t.Parallel()

	p := &CodexWSProvider{effortFallbacks: make(map[string]string)}
	err := &wsServerError{Msg: "Unsupported value: 'none' is not supported with the 'gpt-6-astra' model. " +
		"Supported values are: 'low', 'medium', 'high', 'xhigh', and 'max'."}

	effort, ok := p.effortFallbackFromError("gpt-6-astra", "none", err)
	if !ok || effort != "low" {
		t.Fatalf("effortFallbackFromError = (%q, %t), want (low, true)", effort, ok)
	}
	// The resolution is cached so later turns skip the rejection round-trip.
	if got := p.cachedEffortFallback("gpt-6-astra", "none"); got != "low" {
		t.Fatalf("cachedEffortFallback = %q, want low", got)
	}
	if got := p.cachedEffortFallback("gpt-6-astra", "high"); got != "" {
		t.Fatalf("cachedEffortFallback for an unrejected level = %q, want empty", got)
	}

	if _, ok := p.effortFallbackFromError("gpt-6-astra", "none", &wsServerError{Msg: "boom"}); ok {
		t.Fatalf("an unrelated server error must not trigger effort renegotiation")
	}
	if _, ok := p.effortFallbackFromError("gpt-6-astra", "", err); ok {
		t.Fatalf("no thinking level was requested, nothing to renegotiate")
	}
}
