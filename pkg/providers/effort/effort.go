// Package effort defines the canonical, transport-independent thinking levels.
package effort

import (
	"fmt"
	"strings"
)

// Normalize validates an optional effort. Empty means inherit the model default.
// These are the same levels accepted by model_list.thinking_level.
func Normalize(raw string) (string, error) {
	level := strings.ToLower(strings.TrimSpace(raw))
	switch level {
	case "", "off", "low", "medium", "high", "xhigh", "max", "adaptive":
		return level, nil
	default:
		return "", fmt.Errorf("effort must be off, low, medium, high, xhigh, max, or adaptive (empty inherits the model default)")
	}
}
