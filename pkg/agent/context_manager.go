package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// ContextManager manages conversation context via a pluggable strategy.
// Exactly ONE ContextManager is active per AgentLoop, selected by config.
// The default ("legacy") preserves current summarization behavior.
type ContextManager interface {
	// Assemble builds budget-aware context from the ContextManager's own storage.
	// Called before BuildMessages. Returns assembled messages ready for LLM.
	Assemble(ctx context.Context, req *AssembleRequest) (*AssembleResponse, error)

	// Compact compresses conversation history.
	// Called after turn completes (may be async internally) and on context overflow (sync).
	Compact(ctx context.Context, req *CompactRequest) error

	// Ingest records a message into the ContextManager's own storage.
	// Called after each message is persisted to session JSONL.
	// The returned response carries inserted row IDs so callers can later
	// stamp delivered channel refs onto the row via UpdateChannelMessageID
	// when delivery happens out-of-band (e.g. via the steering loop's
	// deferred PublishResponseIfNeeded path). Implementations that do not
	// persist messages return an empty response.
	Ingest(ctx context.Context, req *IngestRequest) (*IngestResponse, error)

	// UpdateChannelMessageID stamps the delivered channel-native ref onto
	// a previously ingested message. Called by deferred-delivery paths
	// (processMessageSync, runTurnWithSteering) after the upstream
	// PublishResponseIfNeeded captures the channel ID, so the persisted
	// row becomes addressable by its delivered ref. Implementations that
	// do not persist messages return nil without error.
	UpdateChannelMessageID(ctx context.Context, sessionKey string, messageID int64, channelMessageID string) error

	// Clear removes all stored context for a session (messages, summaries, etc.).
	// Called when the user issues /clear or /reset.
	Clear(ctx context.Context, sessionKey string) error
}

// AssembleRequest is the input to Assemble.
type AssembleRequest struct {
	SessionKey string // session identifier

	// HistoryBudget is the token budget for everything the ContextManager
	// returns — assembled history messages *and* the summary it embeds into
	// the system prompt. The caller has already subtracted the rest of the
	// request (static system prompt, tool definitions, output reserve and a
	// safety margin) from the model's context window, so a manager that fills
	// this budget exactly still produces a request that fits.
	// See historyTokenBudget.
	HistoryBudget int
}

// AssembleResponse is the output of Assemble.
type AssembleResponse struct {
	History []providers.Message // assembled conversation history for BuildMessages
	Summary string              // conversation summary embedded into system prompt by BuildMessages
}

// CompactRequest is the input to Compact.
type CompactRequest struct {
	SessionKey string                // session identifier
	Reason     ContextCompressReason // proactive_budget | llm_retry | summarize

	// HistoryBudget carries the same meaning as AssembleRequest.HistoryBudget:
	// the token budget the stored context must fit into so the next Assemble
	// can return everything without dropping messages on the floor.
	HistoryBudget int
}

// IngestRequest is the input to Ingest.
type IngestRequest struct {
	SessionKey string            // session identifier
	Message    providers.Message // the message just persisted
}

// IngestResponse is the result of Ingest. Carries the ContextManager-internal
// IDs of the just-inserted messages. Empty for managers that do not
// persist messages.
type IngestResponse struct {
	MessageIDs []int64
}

// ContextManagerFactory constructs a ContextManager from config.
// al provides access to the AgentLoop's runtime resources (provider, model, workspace, etc.)
// cfg is the raw JSON configuration from config.json (may be nil).
type ContextManagerFactory func(cfg json.RawMessage, al *AgentLoop) (ContextManager, error)

var (
	cmRegistryMu sync.RWMutex
	cmRegistry   = map[string]ContextManagerFactory{}
)

// RegisterContextManager registers a named ContextManager factory.
func RegisterContextManager(name string, factory ContextManagerFactory) error {
	if name == "" {
		return fmt.Errorf("context manager name is required")
	}
	if factory == nil {
		return fmt.Errorf("context manager %q factory is nil", name)
	}

	cmRegistryMu.Lock()
	defer cmRegistryMu.Unlock()

	if _, exists := cmRegistry[name]; exists {
		return fmt.Errorf("context manager %q is already registered", name)
	}
	cmRegistry[name] = factory
	return nil
}

func lookupContextManager(name string) (ContextManagerFactory, bool) {
	cmRegistryMu.RLock()
	defer cmRegistryMu.RUnlock()

	f, ok := cmRegistry[name]
	return f, ok
}
