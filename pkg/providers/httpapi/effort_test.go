package httpapi

import "testing"

func TestEffortGeminiModelSafetyAndPrecedence(t *testing.T) {
	for _, tc := range []struct {
		model, level, key string
		want              any
	}{
		{"gemini-3-pro-preview", "medium", "thinkingLevel", "high"},
		{"gemini-3-pro-preview", "off", "thinkingLevel", "low"},
		{"gemini-3-flash-preview", "off", "thinkingLevel", "minimal"},
		{"gemini-2.5-pro", "off", "thinkingBudget", 128},
		{"gemini-2.5-flash", "off", "thinkingBudget", 0},
		{"gemini-2.5-flash-lite", "off", "thinkingBudget", 512},
	} {
		if got := buildGeminiThinkingConfig(tc.model, map[string]any{"thinking_level": tc.level, "effort_override": true})[tc.key]; got != tc.want {
			t.Fatalf("%+v got %v", tc, got)
		}
	}
	if got := buildGeminiThinkingConfig("gemini-300-unknown", map[string]any{"thinking_level": "high", "effort_override": true}); got != nil {
		t.Fatal(got)
	}
	static := map[string]any{"temperature": .3, "thinkingConfig": map[string]any{"thinkingLevel": "low"}}
	p := NewGeminiProvider("unused", "https://example.test", "", "", 0, map[string]any{"generationConfig": static}, nil)
	for _, override := range []bool{false, true, false} {
		body := p.buildRequestBody(nil, nil, "gemini-3-flash-preview", map[string]any{"thinking_level": "high", "effort_override": override})
		gen := body["generationConfig"].(map[string]any)
		got := gen["thinkingConfig"].(map[string]any)["thinkingLevel"]
		want := "low"
		if override {
			want = "high"
		}
		if got != want || gen["temperature"] != .3 {
			t.Fatalf("%v: %v", override, gen)
		}
	}
	if static["thinkingConfig"].(map[string]any)["thinkingLevel"] != "low" {
		t.Fatal("static map mutated")
	}
}

func TestEffortGeminiOrdinaryDefaultsUnchanged(t *testing.T) {
	for _, tc := range []struct {
		model, level, key string
		want              any
	}{
		{"gemini-2.5-flash-lite", "off", "thinkingBudget", 0},
		{"gemini-2.5-pro", "off", "thinkingBudget", nil},
		{"gemini-3-pro-preview", "off", "thinkingLevel", nil},
		{"gemini-3-pro-preview", "medium", "thinkingLevel", "medium"},
	} {
		if got := buildGeminiThinkingConfig(tc.model, map[string]any{"thinking_level": tc.level})[tc.key]; got != tc.want {
			t.Fatalf("ordinary default changed %+v: %v", tc, got)
		}
	}
}
