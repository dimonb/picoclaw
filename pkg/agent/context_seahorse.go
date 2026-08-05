//go:build !mipsle && !netbsd && !(freebsd && arm)

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
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

	// bootstrapped latches reconciliation per session key. The startup sweep
	// runs in the background, so a session may get a turn before the sweep
	// reaches it: whoever gets there first reconciles it, everyone else waits
	// on the same sync.Once. See ensureBootstrapped.
	bootstrapped sync.Map // sessionKey → *sync.Once
}

// seahorseManagerConfig is the optional context_manager_config block for the
// seahorse backend (PICOCLAW_AGENTS_DEFAULTS_CONTEXT_MANAGER_CONFIG).
type seahorseManagerConfig struct {
	// LeafSummaryCompression: "relaxed" (default) or "strict". See seahorse.Config.
	LeafSummaryCompression string `json:"leafSummaryCompression,omitempty"`
	// IgnoreSessionPatterns keeps whole classes of session out of seahorse.
	// Glob syntax, ':'-segmented: "*" matches within a segment, "**" across.
	// A deployment that runs a lot of one-shot automation (cron jobs are the
	// usual case) otherwise accumulates a conversation per run forever, and
	// pays for all of them on every startup sweep.
	IgnoreSessionPatterns []string `json:"ignoreSessionPatterns,omitempty"`
	// StatelessSessionPatterns match sessions that are stored but never
	// compacted or assembled from. Same glob syntax.
	StatelessSessionPatterns []string `json:"statelessSessionPatterns,omitempty"`
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
		DBPath:                   dbPath,
		LeafSummaryCompression:   mgrCfg.LeafSummaryCompression,
		IgnoreSessionPatterns:    mgrCfg.IgnoreSessionPatterns,
		StatelessSessionPatterns: mgrCfg.StatelessSessionPatterns,
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

	// Reconcile the stored sessions with their JSONL history in the background.
	//
	// This sweep used to run inline, right here — which is inside NewAgentLoop,
	// which the gateway calls before it starts a single channel or HTTP
	// listener. A workspace with a few hundred sessions therefore kept the bot
	// completely offline for minutes on every restart (8.5 minutes on the beta
	// bot, longer than the interval between deploys, so it never finished
	// serving anything before the next roll killed it). Nothing in the sweep
	// needs to precede serving: a session that gets a turn before the sweep
	// reaches it reconciles itself first, via ensureBootstrapped.
	if agent.Sessions != nil {
		go mgr.bootstrapAllSessions(context.Background())
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
	m.ensureBootstrapped(ctx, req.SessionKey)

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
	m.ensureBootstrapped(ctx, req.SessionKey)

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
	m.ensureBootstrapped(ctx, req.SessionKey)

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
	// Both sides are about to be wiped, so there is nothing to reconcile — but
	// a sweep already walking this session must finish before the wipe, and no
	// later sweep may resurrect the history from a JSONL we are about to empty.
	m.skipBootstrap(sessionKey)

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

// bootstrapAllSessions reconciles every known session, one at a time.
//
// Sequential on purpose: the work is a single SQLite file behind one write
// lock, so fanning it out buys nothing and would only compete with live turns
// for the same lock. What matters is that it no longer blocks startup.
func (m *seahorseContextManager) bootstrapAllSessions(ctx context.Context) {
	if m == nil || m.sessions == nil || m.engine == nil {
		return
	}
	startedAt := time.Now()
	keys := m.sessions.ListSessions()
	for _, sessionKey := range keys {
		m.ensureBootstrapped(ctx, sessionKey)
	}
	logger.InfoCF("seahorse", "bootstrap: startup sweep finished", map[string]any{
		"sessions":    len(keys),
		"duration_ms": time.Since(startedAt).Milliseconds(),
	})
}

// ensureBootstrapped reconciles one session unless that already happened.
//
// Every ContextManager entry point calls this before touching the engine, so a
// turn never reads or writes a conversation the background sweep has not
// reconciled yet — it reconciles that one session inline instead, and the sweep
// later skips it. Concurrent callers for the same session block on the same
// sync.Once rather than racing to rebuild it.
//
// Ordering note: Assemble runs before a turn writes anything to the JSONL, so
// by the time Ingest lands the latch is already closed. That matters — a
// bootstrap that ran *between* the JSONL append and the matching Ingest would
// see the new message in the history, ingest it, and then Ingest would store it
// a second time.
func (m *seahorseContextManager) ensureBootstrapped(ctx context.Context, sessionKey string) {
	if m == nil || m.sessions == nil || m.engine == nil || sessionKey == "" {
		return
	}
	once, _ := m.bootstrapped.LoadOrStore(sessionKey, &sync.Once{})
	once.(*sync.Once).Do(func() {
		m.bootstrapSession(ctx, sessionKey)
	})
}

// skipBootstrap closes the latch without reconciling, waiting out a sweep that
// is already inside this session.
func (m *seahorseContextManager) skipBootstrap(sessionKey string) {
	if m == nil || sessionKey == "" {
		return
	}
	once, _ := m.bootstrapped.LoadOrStore(sessionKey, &sync.Once{})
	once.(*sync.Once).Do(func() {})
}

// bootstrapReconcileVersion invalidates every recorded history revision when
// the reconcile itself changes. Bump it whenever Bootstrap starts fixing
// something it used to leave alone (a new backfill, a changed match rule),
// otherwise sessions that have not moved since would never be re-examined.
const bootstrapReconcileVersion = "v1"

// bootstrapSession reconciles JSONL session history into seahorse SQLite.
func (m *seahorseContextManager) bootstrapSession(ctx context.Context, sessionKey string) {
	if m.sessions == nil {
		return
	}
	if !m.engine.StoresSession(sessionKey) {
		// Nothing to reconcile, and reading the history back would be the
		// expensive half of the sweep — this is what makes ignoring a noisy
		// class of session (cron runs, say) actually cheap.
		return
	}

	// Sample the revision BEFORE reading the history. The other order can
	// record a revision newer than what was actually reconciled, which would
	// make the next startup skip a real delta. This way the worst case is a
	// redundant reconcile.
	revision := m.historyRevision(sessionKey)
	if revision != "" {
		stored, err := m.engine.HistoryRevision(ctx, sessionKey)
		if err != nil {
			logger.WarnCF("seahorse", "bootstrap: read history revision", map[string]any{
				"session": sessionKey,
				"error":   err.Error(),
			})
		} else if stored == revision {
			// The history has not moved since it was last reconciled, so the
			// DB already mirrors it — no need to parse the JSONL at all.
			return
		}
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
		return
	}

	if revision != "" {
		if err := m.engine.SetHistoryRevision(ctx, sessionKey, revision); err != nil {
			logger.WarnCF("seahorse", "bootstrap: record history revision", map[string]any{
				"session": sessionKey,
				"error":   err.Error(),
			})
		}
	}
}

// historyRevision returns the current revision of a session's stored history,
// or "" when the session store cannot tell — which callers treat as "changed".
func (m *seahorseContextManager) historyRevision(sessionKey string) string {
	revStore, ok := m.sessions.(session.HistoryRevisionStore)
	if !ok {
		return ""
	}
	revision := revStore.HistoryRevision(sessionKey)
	if revision == "" {
		return ""
	}
	return bootstrapReconcileVersion + ":" + revision
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
