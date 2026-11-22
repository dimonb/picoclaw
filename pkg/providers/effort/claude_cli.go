package effort

// ClaudeCLI maps a canonical control to the claude CLI --effort flag
// (low, medium, high, xhigh, max). Empty/adaptive inherit the CLI default.
// Off selects the minimum legal effort, NOT disabled reasoning; the CLI has
// no flag to disable thinking.
func ClaudeCLI(raw string) (string, bool) {
	level, err := Normalize(raw)
	if err != nil {
		return "", false
	}
	switch level {
	case "", "adaptive":
		return "", false
	case "off":
		return "low", true
	default:
		return level, true
	}
}
