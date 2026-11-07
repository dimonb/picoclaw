package openai_responses_common

import (
	"encoding/json"
	"github.com/openai/openai-go/v3/responses"
	"testing"
)

func TestResponsesEffortSerializationAndIsolation(t *testing.T) {
	for _, tt := range []struct{ model, level, want string }{
		{"gpt-5.2", "max", "xhigh"}, {"gpt-5.1", "off", "none"}, {"o3", "off", "low"},
		{"gpt-5.2", "", ""}, {"gpt-5.2", "adaptive", ""}, {"gpt-4o", "high", ""}, {"gpt-6.1-sol", "max", ""},
	} {
		opts := map[string]any{"thinking_level": tt.level}
		params := responses.ResponseNewParams{Model: tt.model, Reasoning: ReasoningForOptions(tt.model, opts)}
		data, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		var wire map[string]any
		if err = json.Unmarshal(data, &wire); err != nil {
			t.Fatal(err)
		}
		r, ok := wire["reasoning"]
		if ok != (tt.want != "") {
			t.Fatalf("%+v: %s", tt, data)
		}
		if ok && r.(map[string]any)["effort"] != tt.want {
			t.Fatalf("%+v: %s", tt, data)
		}
		if _, ok := wire["reasoning_effort"]; ok {
			t.Fatal("Chat Completions shape in Responses")
		}
		if opts["thinking_level"] != tt.level {
			t.Fatal("mutated options")
		}
	}
}
