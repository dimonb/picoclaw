package openai_compat

import (
	"encoding/json"
	"testing"
)

func TestNativeOpenAIEffortSerializationAndIsolation(t *testing.T) {
	for _, base := range []string{"https://api.openai.com/v1", "https://example.openai.azure.com/openai/v1", "https://compat.example/v1", "https://api.openai.com.evil.test/v1"} {
		t.Run(base, func(t *testing.T) {
			p := NewProvider("unused", base, "")
			native := base == "https://api.openai.com/v1" || base == "https://example.openai.azure.com/openai/v1"
			if p.SupportsThinking() != native {
				t.Fatal("capability mismatch")
			}
			for _, level := range []string{"max", "off", "", "adaptive", "high", ""} {
				opts := map[string]any{}
				if level != "" {
					opts["thinking_level"] = level
				}
				body := p.buildRequestBody(nil, nil, "gpt-5.2", opts)
				data, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				var wire map[string]any
				if err = json.Unmarshal(data, &wire); err != nil {
					t.Fatal(err)
				}
				want := ""
				if native {
					switch level {
					case "max":
						want = "xhigh"
					case "off":
						want = "none"
					case "high":
						want = "high"
					}
				}
				got, exists := wire["reasoning_effort"]
				if (want != "") != exists || exists && got != want {
					t.Fatalf("%s got %v want %q: %s", level, got, want, data)
				}
				if _, ok := wire["reasoning"]; ok {
					t.Fatal("Responses shape in Chat Completions")
				}
				if len(opts) > 1 || level == "" && len(opts) != 0 {
					t.Fatal("options mutated")
				}
			}
			body := p.buildRequestBody(nil, nil, "gpt-4o", map[string]any{"thinking_level": "high"})
			if _, ok := body["reasoning_effort"]; ok {
				t.Fatal("unsupported model")
			}
		})
	}
}

func TestEffortStaticPrecedence(t *testing.T) {
	p := NewProvider("unused", "https://api.openai.com/v1", "", WithExtraBody(map[string]any{"reasoning_effort": "low"}))
	for _, override := range []bool{false, true, false} {
		b := p.buildRequestBody(nil, nil, "gpt-5.2", map[string]any{"thinking_level": "high", "effort_override": override})
		want := "low"
		if override {
			want = "high"
		}
		if b["reasoning_effort"] != want {
			t.Fatalf("%v: %v", override, b)
		}
	}
}
