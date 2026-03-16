package memory

import (
	"context"

	"github.com/sipeed/picoclaw/pkg/providers"
)

type ContextSnapshot struct {
	History []providers.Message
	Summary string
}

type SummaryCompactionResult struct {
	Applied                 bool
	HistoryCountBefore      int
	HistoryCountAfter       int
	SummarizedCount         int
	NewMessagesWhileRunning int
}

// Store defines an interface for persistent session storage.
// Each method is an atomic operation — there is no separate Save() call.
type Store interface {
	// AddMessage appends a simple text message to a session.
	AddMessage(ctx context.Context, sessionKey, role, content string) error

	// AddFullMessage appends a complete message (with tool calls, etc.) to a session.
	AddFullMessage(ctx context.Context, sessionKey string, msg providers.Message) error

	// GetHistory returns all messages for a session in insertion order.
	// Returns an empty slice (not nil) if the session does not exist.
	GetHistory(ctx context.Context, sessionKey string) ([]providers.Message, error)

	// GetSummary returns the conversation summary for a session.
	// Returns an empty string if no summary exists.
	GetSummary(ctx context.Context, sessionKey string) (string, error)

	// GetContextSnapshot returns history and summary from a single consistent snapshot.
	GetContextSnapshot(ctx context.Context, sessionKey string) (ContextSnapshot, error)

	// SetSummary updates the conversation summary for a session.
	SetSummary(ctx context.Context, sessionKey, summary string) error

	// TruncateHistory removes all but the last keepLast messages from a session.
	// If keepLast <= 0, all messages are removed.
	TruncateHistory(ctx context.Context, sessionKey string, keepLast int) error

	// SetHistory replaces all messages in a session with the provided history.
	SetHistory(ctx context.Context, sessionKey string, history []providers.Message) error

	// ApplySummaryCompaction atomically updates the summary and truncates only
	// the portion of history covered by the completed summarization snapshot.
	ApplySummaryCompaction(
		ctx context.Context,
		sessionKey, summary string,
		expectedHistoryCount, keepLast int,
	) (SummaryCompactionResult, error)

	// Compact reclaims storage by physically removing logically truncated
	// data. Backends that do not accumulate dead data may return nil.
	Compact(ctx context.Context, sessionKey string) error

	// Close releases any resources held by the store.
	Close() error
}
