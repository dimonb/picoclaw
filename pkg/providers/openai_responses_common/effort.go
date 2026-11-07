package openai_responses_common

import (
	"github.com/openai/openai-go/v3/shared"

	"github.com/sipeed/picoclaw/pkg/providers/effort"
)

// ReasoningForOptions is request-local. A zero value is omitted by the SDK.
func ReasoningForOptions(model string, options map[string]any) shared.ReasoningParam {
	raw, _ := options["thinking_level"].(string)
	if value, ok := effort.OpenAI(model, raw); ok {
		return shared.ReasoningParam{Effort: shared.ReasoningEffort(value)}
	}
	return shared.ReasoningParam{}
}
