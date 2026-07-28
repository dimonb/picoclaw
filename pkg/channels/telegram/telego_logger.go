package telegram

import (
	"os"
	"strings"

	"github.com/sipeed/picoclaw/pkg/logger"
)

// noisyTelegramMethods are Bot API methods that telego logs on every call and
// every response, carry no diagnostic value, and fire on a timer rather than in
// reaction to anything:
//
//   - getUpdates    — the long-polling loop, one pair of lines per poll cycle
//   - sendChatAction — the typing indicator, refreshed every 4s for the whole turn
//
// At debug level they bury the lines that explain what a turn is actually doing
// (context assembly, compaction, provider calls), which is exactly when someone
// is reading the log. Set PICOCLAW_TELEGRAM_API_DEBUG=1 to get them back.
var noisyTelegramMethods = []string{"getUpdates", "sendChatAction"}

// telegoLogger adapts picoclaw's logger to telego's Logger interface, dropping
// the high-frequency API chatter listed in noisyTelegramMethods.
//
// Errorf is never filtered — a failing getUpdates or sendChatAction is a real
// signal, unlike the successful ones.
type telegoLogger struct {
	inner   *logger.Logger
	verbose bool
}

func newTelegoLogger(component string) *telegoLogger {
	return &telegoLogger{
		inner:   logger.NewLogger(component),
		verbose: isTruthyEnv(os.Getenv("PICOCLAW_TELEGRAM_API_DEBUG")),
	}
}

func (l *telegoLogger) Debugf(format string, args ...any) {
	if !l.verbose && mentionsNoisyMethod(format, args) {
		return
	}
	l.inner.Debugf(format, args...)
}

func (l *telegoLogger) Errorf(format string, args ...any) {
	l.inner.Errorf(format, args...)
}

// mentionsNoisyMethod reports whether a telego debug line belongs to one of the
// muted methods. telego emits two shapes (bot.go), and the method name lands in
// the arguments rather than the format string in both:
//
//	"API response %s: %s"            → args[0] is the method name
//	"API call to: %q, with data: %s"  → args[0] is a URL ending in /<method>
//
// Matching the args instead of the rendered string keeps this off the hot path
// for the common case and avoids scanning large response bodies.
func mentionsNoisyMethod(format string, args []any) bool {
	if len(args) == 0 || !strings.HasPrefix(format, "API ") {
		return false
	}
	first, ok := args[0].(string)
	if !ok {
		return false
	}
	if idx := strings.LastIndexByte(first, '/'); idx >= 0 {
		first = first[idx+1:]
	}
	for _, method := range noisyTelegramMethods {
		if first == method {
			return true
		}
	}
	return false
}

func isTruthyEnv(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
