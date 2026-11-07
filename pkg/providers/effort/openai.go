package effort

import "strings"

// OpenAI maps canonical controls only for explicitly known reasoning families.
// Empty/adaptive inherit the backend default. Off uses none when supported;
// otherwise it selects the minimum legal effort, NOT disabled reasoning.
// Source: openai-go v3.22.0 shared.ReasoningParam and model contracts.
// Unknown names (including arbitrary Azure deployment aliases) are omitted.
func OpenAI(model, raw string) (string, bool) {
	level, err := Normalize(raw)
	if err != nil || level == "" || level == "adaptive" {
		return "", false
	}
	model = strings.ToLower(strings.TrimSpace(model))
	var levels []string
	switch {
	case knownSnapshot(model, "gpt-5.2"):
		levels = []string{"none", "low", "medium", "high", "xhigh"}
	case model == "gpt-5.1-codex-max":
		levels = []string{"low", "medium", "high", "xhigh"}
	case knownSnapshot(model, "gpt-5.1"):
		levels = []string{"none", "low", "medium", "high"}
	case knownSnapshot(model, "gpt-5-pro"):
		levels = []string{"high"}
	case knownSnapshot(model, "gpt-5"), knownSnapshot(model, "gpt-5-mini"), knownSnapshot(model, "gpt-5-nano"):
		levels = []string{"minimal", "low", "medium", "high"}
	case knownSnapshot(model, "o1"),
		knownSnapshot(model, "o3"),
		knownSnapshot(model, "o3-mini"),
		knownSnapshot(model, "o4-mini"):
		levels = []string{"low", "medium", "high"}
	default:
		return "", false
	}
	if level == "off" {
		return levels[0], true
	}
	if level == "max" || level == "xhigh" {
		return levels[len(levels)-1], true
	}
	for _, v := range levels {
		if v == level {
			return v, true
		}
	}
	// E.g. gpt-5-pro has only high; never send an illegal low/medium.
	return levels[0], true
}

// Recognize only the base name or an ISO-date snapshot, not similarly prefixed
// chat/pro/codex models with potentially different contracts.
func knownSnapshot(model, base string) bool {
	if model == base {
		return true
	}
	if !strings.HasPrefix(model, base+"-") {
		return false
	}
	date := strings.TrimPrefix(model, base+"-")
	if len(date) != 10 || date[4] != '-' || date[7] != '-' {
		return false
	}
	for i, c := range date {
		if i != 4 && i != 7 && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}
