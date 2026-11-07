package anthropicprovider

import (
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

func TestEffortModelSafety(t *testing.T) {
	for _, model := range []string{"claude-3-haiku-20240307", "unknown", "claude-sonnet-4-5", "claude-sonnet-4-6"} {
		for _, level := range []string{"adaptive", "low", "max"} {
			p, err := buildParams(
				[]Message{{Role: "user", Content: "hi"}},
				nil,
				model,
				map[string]any{"thinking_level": level, "max_tokens": 200000, "effort_override": true},
			)
			if err != nil {
				t.Fatal(err)
			}
			want := anthropicThinkingModel(model, level)
			if (p.Thinking.OfEnabled != nil || p.Thinking.OfAdaptive != nil) != want {
				t.Fatalf("%s/%s invalid mapping", model, level)
			}
		}
	}
	for _, max := range []int64{0, 1, 1023, 1024, 1025, 4096} {
		p := anthropic.MessageNewParams{MaxTokens: max, Temperature: anthropic.Float(.7)}
		applyThinkingConfig(&p, "high")
		if max <= 1024 {
			if p.Thinking.OfEnabled != nil || !p.Temperature.Valid() {
				t.Fatalf("tiny budget %d", max)
			}
		} else if p.Thinking.OfEnabled.BudgetTokens < 1024 || p.Thinking.OfEnabled.BudgetTokens >= max {
			t.Fatalf("illegal budget %+v", p)
		}
	}
}

func TestEffortAnthropicInheritedAliasUnchanged(t *testing.T) {
	p, err := buildParams(
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		"custom-claude-alias",
		map[string]any{"thinking_level": "high", "max_tokens": 64000},
	)
	if err != nil {
		t.Fatal(err)
	}
	if p.Thinking.OfEnabled == nil || p.Thinking.OfEnabled.BudgetTokens != 32000 {
		t.Fatalf("inherited alias changed: %+v", p.Thinking)
	}
}
