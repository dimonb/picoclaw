//go:build !mipsle && !netbsd && !(freebsd && arm)

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/providers/protocoltypes"
	"github.com/sipeed/picoclaw/pkg/seahorse"
	"github.com/sipeed/picoclaw/pkg/session"
	"github.com/sipeed/picoclaw/pkg/tokenizer"
)

// seahorseContextManager adapts seahorse.Engine to agent.ContextManager.
type seahorseContextManager struct {
	engine   *seahorse.Engine
	sessions session.SessionStore // for startup bootstrap
}

// seahorseManagerConfig is the optional context_manager_config block for the
// seahorse backend (PICOCLAW_AGENTS_DEFAULTS_CONTEXT_MANAGER_CONFIG).
type seahorseManagerConfig struct {
	// LeafSummaryCompression: "relaxed" (default) or "strict". See seahorse.Config.
	LeafSummaryCompression string `json:"leafSummaryCompression,omitempty"`
}

// newSeahorseContextManager creates a seahorse-backed ContextManager.
func newSeahorseContextManager(cfg json.RawMessage, al *AgentLoop) (ContextManager, error) {
	if al == nil {
		return nil, fmt.Errorf("seahorse: AgentLoop is required")
	}

	// Parse optional manager config (leafSummaryCompression, ...)
	var mgrCfg seahorseManagerConfig
	if len(cfg) > 0 {
		if err := json.Unmarshal(cfg, &mgrCfg); err != nil {
			return nil, fmt.Errorf("seahorse: parse context_manager_config: %w", err)
		}
	}

	// Resolve workspace for DB path
	// DB stores session data, so it goes in sessions/ directory
	agent := al.registry.GetDefaultAgent()
	dbPath := agent.Workspace + "/sessions/seahorse.db"

	// Create CompleteFn from provider
	completeFn := providerToCompleteFn(agent.Provider, agent.Model)

	// Create engine
	engine, err := seahorse.NewEngine(seahorse.Config{
		DBPath:                 dbPath,
		LeafSummaryCompression: mgrCfg.LeafSummaryCompression,
	}, completeFn)
	if err != nil {
		return nil, fmt.Errorf("seahorse: create engine: %w", err)
	}

	mgr := &seahorseContextManager{
		engine:   engine,
		sessions: agent.Sessions,
	}

	// Register seahorse tools with the agent's tool registry
	retrieval := mgr.engine.GetRetrieval()
	al.RegisterTool(seahorse.NewGrepTool(retrieval))
	al.RegisterTool(seahorse.NewExpandTool(retrieval))
	al.RegisterTool(seahorse.NewFetchMessageTool(retrieval))

	// Bootstrap all existing sessions at startup
	if agent.Sessions != nil {
		ctx := context.Background()
		for _, sessionKey := range agent.Sessions.ListSessions() {
			mgr.bootstrapSession(ctx, sessionKey)
		}
	}

	return mgr, nil
}

// providerToCompleteFn wraps providers.LLMProvider as a seahorse.CompleteFn.
func providerToCompleteFn(provider providers.LLMProvider, model string) seahorse.CompleteFn {
	return func(ctx context.Context, prompt string, opts seahorse.CompleteOptions) (string, error) {
		sessionKey := opts.SessionKey
		if sessionKey == "" {
			sessionKey = "seahorse"
		}
		resp, err := provider.Chat(
			ctx,
			[]providers.Message{{Role: "user", Content: prompt}},
			nil, // no tools for summarization
			model,
			map[string]any{
				"max_tokens":       opts.MaxTokens,
				"temperature":      opts.Temperature,
				"prompt_cache_key": "seahorse",
				// Summarization is a self-contained prompt, not a turn in any
				// conversation: it must not land in a chat's provider session.
				// "stateless" additionally tells session-based providers
				// (codex-ws) to use a throwaway connection instead of chaining
				// these one-shot prompts onto each other.
				"session_key": sessionKey,
				"stateless":   true,
			},
		)
		if err != nil {
			return "", err
		}
		return resp.Content, nil
	}
}

// Assemble builds budget-aware context from seahorse SQLite.
func (m *seahorseContextManager) Assemble(ctx context.Context, req *AssembleRequest) (*AssembleResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("seahorse assemble: nil request")
	}

	// HistoryBudget already excludes the system prompt, tool definitions and
	// the output reserve — it is exactly what the assembled messages plus the
	// summary may cost.
	budget := req.HistoryBudget
	if budget <= 0 {
		budget = 100000
	}

	result, err := m.engine.Assemble(ctx, req.SessionKey, seahorse.AssembleInput{
		Budget: budget,
	})
	if err != nil {
		return nil, fmt.Errorf("seahorse assemble: %w", err)
	}

	history := seahorseToProviderMessages(result)
	logger.DebugCF("agent", "Seahorse assemble result", map[string]any{
		"session_key":    req.SessionKey,
		"history_budget": budget,
		"history_msgs":   len(history),
		"summary_chars":  len(result.Summary),
		"summary_tokens": tokenizer.EstimateMessageTokens(providers.Message{Content: result.Summary}),
		"evicted":        result.Evicted,
	})

	// Summary is already formatted as XML with system prompt addition by assembler
	return &AssembleResponse{
		History: history,
		Summary: result.Summary,
		Evicted: result.Evicted,
	}, nil
}

// proactiveCompactIterations caps how much summarization work a single
// over-budget turn performs inline. Each iteration is an LLM call the user is
// waiting on, so a badly overgrown conversation converges across a few turns
// (the caller's trim fallback keeps the current request valid) instead of
// stalling one turn for minutes.
const proactiveCompactIterations = 6

// Compact compresses conversation history via seahorse summarization.
func (m *seahorseContextManager) Compact(ctx context.Context, req *CompactRequest) error {
	if req == nil {
		return nil
	}

	switch req.Reason {
	case ContextCompressReasonProactive, ContextCompressReasonRetry:
		// Both mean "the request does not fit". Leaf compaction alone compresses
		// one chunk per call, which cannot catch up with an already-overgrown
		// conversation, so drive compaction until the stored context is actually
		// under budget.
		//
		// The target sits below the budget on purpose: compacting to exactly the
		// budget puts the conversation back over it after a single message, and
		// every re-compaction rewrites the prompt prefix (dropping provider-side
		// prefix cache and forcing a codex-ws session replay).
		if req.HistoryBudget > 0 {
			target := int(float64(req.HistoryBudget) * seahorse.ContextThreshold)
			iterations := proactiveCompactIterations
			if req.Reason == ContextCompressReasonRetry {
				// The provider already rejected the request; there is no valid
				// request to fall back to, so run to completion.
				iterations = seahorse.MaxCompactIterations
			}
			_, err := m.engine.CompactUntilUnder(ctx, req.SessionKey, target, iterations)
			return err
		}
	}

	budget := req.HistoryBudget
	_, err := m.engine.Compact(ctx, req.SessionKey, seahorse.CompactInput{
		Budget: &budget,
	})
	return err
}

// Ingest records a message into seahorse SQLite.
// All existing sessions are bootstrapped at startup, so this only ingests new messages.
func (m *seahorseContextManager) Ingest(ctx context.Context, req *IngestRequest) (*IngestResponse, error) {
	if req == nil {
		return &IngestResponse{}, nil
	}

	msg := providerToSeahorseMessage(req.Message)
	result, err := m.engine.Ingest(ctx, req.SessionKey, []seahorse.Message{msg})
	if err != nil {
		return nil, err
	}
	if result == nil {
		return &IngestResponse{}, nil
	}
	return &IngestResponse{MessageIDs: append([]int64(nil), result.MessageIDs...)}, nil
}

// UpdateChannelMessageID stamps a delivered channel-native ref onto a
// previously ingested message in seahorse SQLite.
func (m *seahorseContextManager) UpdateChannelMessageID(
	ctx context.Context,
	sessionKey string,
	messageID int64,
	channelMessageID string,
) error {
	if m.engine == nil {
		return nil
	}
	return m.engine.UpdateMessageChannelMessageID(ctx, sessionKey, messageID, channelMessageID)
}

// Clear removes all stored context for a session (seahorse DB + JSONL).
func (m *seahorseContextManager) Clear(ctx context.Context, sessionKey string) error {
	if err := m.engine.ClearSession(ctx, sessionKey); err != nil {
		return err
	}
	if m.sessions != nil {
		m.sessions.SetHistory(sessionKey, []providers.Message{})
		m.sessions.SetSummary(sessionKey, "")
		return m.sessions.Save(sessionKey)
	}
	return nil
}

// bootstrapSession reconciles JSONL session history into seahorse SQLite.
func (m *seahorseContextManager) bootstrapSession(ctx context.Context, sessionKey string) {
	if m.sessions == nil {
		return
	}

	history := m.sessions.GetHistory(sessionKey)
	if len(history) == 0 {
		return
	}

	// Convert provider messages to seahorse messages
	msgs := make([]seahorse.Message, len(history))
	for i, h := range history {
		msgs[i] = providerToSeahorseMessage(h)
	}

	if err := m.engine.Bootstrap(ctx, sessionKey, msgs); err != nil {
		logger.WarnCF("seahorse", "bootstrap", map[string]any{
			"session": sessionKey,
			"error":   err.Error(),
		})
	}
}

// providerToSeahorseMessage converts a providers.Message to a seahorse.Message.
func providerToSeahorseMessage(msg protocoltypes.Message) seahorse.Message {
	result := seahorse.Message{
		Role:             msg.Role,
		Content:          msg.Content,
		ModelName:        msg.ModelName,
		ReasoningContent: msg.ReasoningContent,
		ChannelMessageID: msg.MessageID,
		Metadata:         msg.Metadata,
		Attachments:      append([]protocoltypes.Attachment(nil), msg.Attachments...),
		TokenCount:       tokenizer.EstimateMessageTokens(msg),
		CreatedAt:        normalizeSeahorseMessageCreatedAt(msg.CreatedAt),
	}

	hasStructured := len(msg.ToolCalls) > 0 || msg.ToolCallID != "" || len(msg.Media) > 0

	// When a structured message also carries raw text content (e.g. an
	// assistant reply with both narrative and tool_calls), preserve it as a
	// dedicated text part so the original content survives the
	// parts-only INSERT path (store.AddMessageWithPartsAndReasoning derives
	// the messages.content column from parts, ignoring msg.Content).
	// Tool results are excluded because their text is stored inside the
	// tool_result part below.
	if hasStructured && msg.Content != "" && msg.ToolCallID == "" {
		result.Parts = append(result.Parts, seahorse.MessagePart{
			Type: "text",
			Text: msg.Content,
		})
	}

	// Convert ToolCalls → MessageParts
	for _, tc := range msg.ToolCalls {
		part := seahorse.MessagePart{
			Type:       "tool_use",
			Name:       tc.Function.Name,
			Arguments:  tc.Function.Arguments,
			ToolCallID: tc.ID,
		}
		result.Parts = append(result.Parts, part)
	}

	// Convert tool result
	if msg.ToolCallID != "" {
		part := seahorse.MessagePart{
			Type:       "tool_result",
			ToolCallID: msg.ToolCallID,
			Text:       msg.Content,
		}
		result.Parts = append(result.Parts, part)
	}

	// Convert media attachments
	for _, mediaURI := range msg.Media {
		part := seahorse.MessagePart{
			Type:     "media",
			MediaURI: mediaURI,
		}
		result.Parts = append(result.Parts, part)
	}

	return result
}

func normalizeSeahorseMessageCreatedAt(createdAt *time.Time) time.Time {
	if createdAt == nil || createdAt.IsZero() {
		return time.Time{}
	}
	return createdAt.UTC().Truncate(time.Second)
}

// seahorseToProviderMessages converts a seahorse.AssembleResult to []providers.Message.
func seahorseToProviderMessages(result *seahorse.AssembleResult) []protocoltypes.Message {
	messages := make([]protocoltypes.Message, 0, len(result.Messages))

	// Convert assembled messages (which already include summary XML messages)
	for _, msg := range result.Messages {
		pm := protocoltypes.Message{
			Role:             msg.Role,
			ModelName:        msg.ModelName,
			ReasoningContent: msg.ReasoningContent,
			MessageID:        msg.ChannelMessageID,
			Metadata:         msg.Metadata,
			Attachments:      append([]protocoltypes.Attachment(nil), msg.Attachments...),
		}

		// When parts exist, msg.Content is the synthetic readable form
		// (partsToReadableContent) derived for FTS5/summary use. Rebuild
		// pm.Content from real text/tool_result parts instead to avoid
		// leaking "[tool_use: …]" strings back into the LLM context.
		hasParts := len(msg.Parts) > 0
		if !hasParts {
			pm.Content = msg.Content
		}

		var textParts []string
		for _, part := range msg.Parts {
			switch part.Type {
			case "text":
				if part.Text != "" {
					textParts = append(textParts, part.Text)
				}
			case "tool_use":
				pm.ToolCalls = append(pm.ToolCalls, protocoltypes.ToolCall{
					ID:   part.ToolCallID,
					Type: "function", // Required by OpenAI-compatible APIs (GLM, etc.)
					Function: &protocoltypes.FunctionCall{
						Name:      part.Name,
						Arguments: part.Arguments,
					},
				})
			case "tool_result":
				pm.ToolCallID = part.ToolCallID
				if pm.Content == "" && part.Text != "" {
					pm.Content = part.Text
				}
			case "media":
				if part.MediaURI != "" {
					pm.Media = append(pm.Media, part.MediaURI)
				}
			}
		}
		if hasParts && pm.Content == "" && len(textParts) > 0 {
			pm.Content = strings.Join(textParts, "\n")
		}

		messages = append(messages, pm)
	}

	return messages
}

func init() {
	if err := RegisterContextManager("seahorse", newSeahorseContextManager); err != nil {
		panic(fmt.Sprintf("register seahorse context manager: %v", err))
	}
}
