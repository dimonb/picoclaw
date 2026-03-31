package agent

import "strings"

// ThinkingLevel controls how the provider sends thinking parameters.
//
//   - "none"/"minimal"/"low"/"medium"/"high"/"xhigh": sent as reasoning effort
//     for OpenAI reasoning models (including Codex-backed Responses API models)
//   - "adaptive": sends {thinking: {type: "adaptive"}} + output_config.effort
//     for Anthropic Claude 4.6+
//   - "off": disables provider-specific thinking/reasoning controls
type ThinkingLevel string

const (
	ThinkingOff      ThinkingLevel = "off"
	ThinkingNone     ThinkingLevel = "none"
	ThinkingMinimal  ThinkingLevel = "minimal"
	ThinkingLow      ThinkingLevel = "low"
	ThinkingMedium   ThinkingLevel = "medium"
	ThinkingHigh     ThinkingLevel = "high"
	ThinkingXHigh    ThinkingLevel = "xhigh"
	ThinkingAdaptive ThinkingLevel = "adaptive"
)

// parseThinkingLevel normalizes a config string to a ThinkingLevel.
// Case-insensitive and whitespace-tolerant for user-facing config values.
// Returns ThinkingOff for unknown or empty values.
func parseThinkingLevel(level string) ThinkingLevel {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "none":
		return ThinkingNone
	case "minimal":
		return ThinkingMinimal
	case "adaptive":
		return ThinkingAdaptive
	case "low":
		return ThinkingLow
	case "medium":
		return ThinkingMedium
	case "high":
		return ThinkingHigh
	case "xhigh":
		return ThinkingXHigh
	default:
		return ThinkingOff
	}
}
