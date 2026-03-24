package session

import (
	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// ContextSnapshot is a consistent snapshot of history + summary.
type ContextSnapshot = memory.ContextSnapshot

// SummaryCompactionResult holds the result of an atomic compaction operation.
type SummaryCompactionResult = memory.SummaryCompactionResult

// SessionStore defines the persistence operations used by the agent loop.
// Both SessionManager (legacy JSON backend) and JSONLBackend satisfy this
// interface, allowing the storage layer to be swapped without touching the
// agent loop code.
//
// Write methods (Add*, Set*, Truncate*) are fire-and-forget: they do not
// return errors. Implementations should log failures internally. This
// matches the original SessionManager contract that the agent loop relies on.
type SessionStore interface {
	// AddMessage appends a simple role/content message to the session.
	AddMessage(sessionKey, role, content string)
	// AddFullMessage appends a complete message including tool calls.
	AddFullMessage(sessionKey string, msg providers.Message)
	// GetHistory returns the full message history for the session.
	GetHistory(key string) []providers.Message
	// GetSummary returns the conversation summary, or "" if none.
	GetSummary(key string) string
	// GetContextSnapshot returns history and summary from a single consistent snapshot.
	GetContextSnapshot(key string) ContextSnapshot
	// SetSummary replaces the conversation summary.
	SetSummary(key, summary string)
	// SetHistory replaces the full message history.
	SetHistory(key string, history []providers.Message)
	// TruncateHistory keeps only the last keepLast messages.
	TruncateHistory(key string, keepLast int)
	// ApplySummaryCompaction atomically updates summary and truncates only the
	// portion of history that was actually summarized, preserving messages that
	// arrived while summarization was running.
	ApplySummaryCompaction(
		key, summary string,
		expectedHistoryCount, keepLast int,
	) SummaryCompactionResult
	// GetThinkingLevel returns the thinking level for the session.
	GetThinkingLevel(key string) string
	// SetThinkingLevel sets the thinking level for the session.
	SetThinkingLevel(key, level string)
	// Save persists any pending state to durable storage.
	Save(key string) error
	// Close releases resources held by the store.
	Close() error
}
