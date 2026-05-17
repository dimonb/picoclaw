// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"regexp"
	"strings"

	"github.com/sipeed/picoclaw/pkg/logger"
)

var leadingMessageAnnotationPattern = regexp.MustCompile(`^\s*\[([^\]]+)\]\s*`)

// stripLeadingMessageAnnotations removes leaked `[from:…; msgs:#…, reply_to:#…]`
// envelope prefixes that the LLM sometimes echoes back into its final answer.
// Only brackets whose body contains one of the annotation markers are stripped;
// unrelated leading bracketed text (e.g. `[Черновик]`) is preserved.
func stripLeadingMessageAnnotations(text string) (string, string) {
	var stripped strings.Builder

	for {
		match := leadingMessageAnnotationPattern.FindStringSubmatchIndex(text)
		if match == nil || match[0] != 0 {
			return text, stripped.String()
		}

		body := strings.ToLower(text[match[2]:match[3]])
		if !strings.Contains(body, "msgs:#") &&
			!strings.Contains(body, "reply_to:#") &&
			!strings.HasPrefix(body, "from:") {
			return text, stripped.String()
		}

		stripped.WriteString(text[match[0]:match[1]])
		text = text[match[1]:]
	}
}

func logStrippedMessageAnnotations(stripped string) {
	if stripped == "" {
		return
	}

	logger.DebugCF("agent", "Stripped leading message annotations from final response",
		map[string]any{
			"stripped": stripped,
		})
}
