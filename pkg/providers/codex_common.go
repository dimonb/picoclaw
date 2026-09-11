package providers

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"strings"

	"github.com/openai/openai-go/v3/shared"

	"github.com/sipeed/picoclaw/pkg/config"
)

const (
	// codexDefaultModel is the model used when the configured one cannot be
	// sent to this transport. Keep it to a model the Codex backend actually
	// serves: gpt-5.3-codex was retired and every substitution onto it failed
	// with "not supported when using Codex with a ChatGPT account".
	codexDefaultModel        = "gpt-5.5"
	defaultCodexInstructions = "You are Codex, a coding assistant."
)

func codexUserAgent() string {
	osName := runtime.GOOS
	switch osName {
	case "darwin":
		osName = "macOS"
	case "linux":
		osName = "Linux"
	case "windows":
		osName = "Windows"
	}
	terminal := os.Getenv("TERM_PROGRAM")
	if terminal == "" {
		terminal = os.Getenv("TERM")
	}
	if terminal == "" {
		terminal = "unknown"
	}
	return fmt.Sprintf("codex_cli_rs/%s (%s; %s) %s", config.Version, osName, runtime.GOARCH, terminal)
}

// codexModelAliases maps the short names people actually type onto the
// identifiers the Codex backend expects. Without an entry here a bare "astra"
// falls through to the default model, which is the worst outcome: the turn runs
// on something else entirely.
var codexModelAliases = map[string]string{
	"astra": "gpt-6-astra",
}

func resolveCodexModel(model string) (string, string) {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return codexDefaultModel, "empty model"
	}

	if after, ok := strings.CutPrefix(m, "openai/"); ok {
		m = after
	} else if strings.Contains(m, "/") {
		return codexDefaultModel, "non-openai model namespace"
	}

	if canonical, ok := codexModelAliases[m]; ok {
		return canonical, ""
	}

	unsupportedPrefixes := []string{
		"glm",
		"claude",
		"anthropic",
		"gemini",
		"google",
		"moonshot",
		"kimi",
		"qwen",
		"deepseek",
		"llama",
		"meta-llama",
		"mistral",
		"grok",
		"xai",
		"zhipu",
	}
	for _, prefix := range unsupportedPrefixes {
		if strings.HasPrefix(m, prefix) {
			return codexDefaultModel, "unsupported model prefix"
		}
	}

	if strings.HasPrefix(m, "gpt-") || strings.HasPrefix(m, "o3") || strings.HasPrefix(m, "o4") {
		return m, ""
	}

	return codexDefaultModel, "unsupported model family"
}

func codexReasoningEffort(level string) (shared.ReasoningEffort, bool) {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "none":
		return shared.ReasoningEffortNone, true
	case "minimal":
		return shared.ReasoningEffortMinimal, true
	case "low":
		return shared.ReasoningEffortLow, true
	case "medium":
		return shared.ReasoningEffortMedium, true
	case "high":
		return shared.ReasoningEffortHigh, true
	case "xhigh":
		return shared.ReasoningEffortXhigh, true
	case "max":
		// Not in the openai-go enum yet; gpt-6-astra accepts it.
		return shared.ReasoningEffort("max"), true
	default:
		return "", false
	}
}

// codexEffortLadder orders reasoning efforts from cheapest to most expensive.
// Models come and go from both ends: gpt-6-astra dropped "none" and "minimal"
// and added "max", so the effort a caller asks for is not always one the model
// accepts.
var codexEffortLadder = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// codexUnsupportedEffortRe matches the server's rejection of a reasoning
// effort, e.g.
//
//	Unsupported value: 'none' is not supported with the 'gpt-6-astra' model.
//	Supported values are: 'low', 'medium', 'high', 'xhigh', and 'max'.
//
// The supported list is authoritative and model-specific, so it beats any table
// we could hardcode here — it stays correct for models that do not exist yet.
var codexUnsupportedEffortRe = regexp.MustCompile(
	`Unsupported value: '([^']+)' is not supported with the '[^']*' model\.\s*Supported values are: (.+)`,
)

// parseCodexUnsupportedEffort reports the rejected effort and the efforts the
// model does accept, or ok=false when msg is a different error.
func parseCodexUnsupportedEffort(msg string) (rejected string, supported []string, ok bool) {
	m := codexUnsupportedEffortRe.FindStringSubmatch(msg)
	if m == nil {
		return "", nil, false
	}
	for _, raw := range strings.Split(m[2], ",") {
		value := strings.ToLower(strings.Trim(strings.TrimSpace(raw), "'.\""))
		value = strings.TrimSpace(strings.TrimPrefix(value, "and "))
		value = strings.Trim(value, "'.")
		if value != "" {
			supported = append(supported, value)
		}
	}
	if len(supported) == 0 {
		return "", nil, false
	}
	return strings.ToLower(m[1]), supported, true
}

// nearestCodexEffort picks the supported effort closest to want on the ladder,
// preferring a stronger one on a tie so a capped request is never silently
// downgraded further than necessary.
func nearestCodexEffort(want string, supported []string) (string, bool) {
	wantIdx := indexOfCodexEffort(want)
	if wantIdx < 0 {
		return "", false
	}

	best, bestDist := "", 0
	for _, candidate := range supported {
		idx := indexOfCodexEffort(candidate)
		if idx < 0 {
			continue
		}
		dist := idx - wantIdx
		if dist < 0 {
			dist = -dist
		}
		if best == "" || dist < bestDist || (dist == bestDist && idx > indexOfCodexEffort(best)) {
			best, bestDist = candidate, dist
		}
	}
	return best, best != ""
}

func indexOfCodexEffort(effort string) int {
	effort = strings.ToLower(strings.TrimSpace(effort))
	for i, known := range codexEffortLadder {
		if known == effort {
			return i
		}
	}
	return -1
}

func resolveCodexToolCall(tc ToolCall) (name string, arguments string, ok bool) {
	name = tc.Name
	if name == "" && tc.Function != nil {
		name = tc.Function.Name
	}
	if name == "" {
		return "", "", false
	}

	if len(tc.Arguments) > 0 {
		argsJSON, err := json.Marshal(tc.Arguments)
		if err != nil {
			return "", "", false
		}
		return name, string(argsJSON), true
	}

	if tc.Function != nil && tc.Function.Arguments != "" {
		return name, tc.Function.Arguments, true
	}

	return name, "{}", true
}
