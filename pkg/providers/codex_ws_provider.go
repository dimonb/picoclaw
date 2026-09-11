package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/sipeed/picoclaw/pkg/logger"
)

const (
	codexWSEndpoint          = "wss://chatgpt.com/backend-api/codex/responses"
	codexWSBetaHeader        = "responses_websockets=2026-02-06"
	codexWSEndpointEnvVar    = "CODEX_WS_URL"
	wsSessionIdleTimeout     = 10 * time.Minute
	wsSessionCleanupInterval = 2 * time.Minute
	wsRetryTimeout           = 30 * time.Second
	// wsWriteTimeout bounds a single WriteMessage. Without it a wedged
	// connection (peer stopped reading, TCP send buffer full) blocks the
	// write — and the whole agent turn — forever: socket I/O does not observe
	// ctx, and the only other deadline (the read deadline below) never applies
	// because we never reach the read phase.
	wsWriteTimeout = 30 * time.Second
)

// ---------- request structs ----------

type wsRequest struct {
	Type               string         `json:"type"`
	Model              string         `json:"model"`
	Instructions       string         `json:"instructions,omitempty"`
	PreviousResponseID string         `json:"previous_response_id,omitempty"`
	Input              []wsInputItem  `json:"input"`
	Tools              []wsToolDef    `json:"tools,omitempty"`
	ToolChoice         string         `json:"tool_choice"`
	ParallelToolCalls  bool           `json:"parallel_tool_calls"`
	Reasoning          *wsReasoning   `json:"reasoning,omitempty"`
	Store              bool           `json:"store"`
	Stream             bool           `json:"stream"`
	Include            []string       `json:"include,omitempty"`
	PromptCacheKey     string         `json:"prompt_cache_key,omitempty"`
	Text               *wsText        `json:"text,omitempty"`
	Generate           *bool          `json:"generate,omitempty"`
	ClientMetadata     map[string]any `json:"client_metadata,omitempty"`
}

type wsReasoning struct {
	Effort string `json:"effort,omitempty"`
}

type wsText struct {
	Verbosity string `json:"verbosity,omitempty"`
}

// wsInputItem is a discriminated union. We marshal it ourselves via RawMessage.
type wsInputItem struct {
	raw json.RawMessage
}

func (w wsInputItem) MarshalJSON() ([]byte, error) {
	return w.raw, nil
}

func wsMessageItem(role, content string) wsInputItem {
	b, _ := json.Marshal(map[string]any{
		"type":    "message",
		"role":    role,
		"content": content,
	})
	return wsInputItem{raw: b}
}

func wsMessageItemWithParts(role string, parts []map[string]any) wsInputItem {
	b, _ := json.Marshal(map[string]any{
		"type":    "message",
		"role":    role,
		"content": parts,
	})
	return wsInputItem{raw: b}
}

func wsFunctionCallItem(callID, name, arguments string) wsInputItem {
	b, _ := json.Marshal(map[string]any{
		"type":      "function_call",
		"call_id":   callID,
		"name":      name,
		"arguments": arguments,
	})
	return wsInputItem{raw: b}
}

func wsFunctionCallOutputItem(callID, output string) wsInputItem {
	b, _ := json.Marshal(map[string]any{
		"type":    "function_call_output",
		"call_id": callID,
		"output":  output,
	})
	return wsInputItem{raw: b}
}

type wsToolDef struct {
	Type        string         `json:"type"`
	Name        string         `json:"name,omitempty"`
	Description string         `json:"description,omitempty"`
	Strict      *bool          `json:"strict,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// ---------- response/event structs ----------

type wsEvent struct {
	Type        string         `json:"type"`
	Response    *wsResponseObj `json:"response,omitempty"`
	Item        *wsOutputItem  `json:"item,omitempty"`
	OutputIndex int            `json:"output_index"`
	ItemID      string         `json:"item_id,omitempty"`
	Delta       string         `json:"delta,omitempty"`
	Error       *wsErrorObj    `json:"error,omitempty"`
	StatusCode  int            `json:"status_code,omitempty"`
}

type wsResponseObj struct {
	ID     string         `json:"id"`
	Status string         `json:"status"`
	Output []wsOutputItem `json:"output"`
	Usage  wsUsage        `json:"usage"`
	Error  *wsErrorObj    `json:"error,omitempty"`
}

// wsErrorObj is the server's error payload, delivered either as a top-level
// {"type":"error", ...} event or nested inside a failed response. Example:
//
//	{"type":"error","status_code":429,
//	 "error":{"type":"usage_limit_reached","message":"The usage limit has been reached",
//	          "plan_type":"plus","resets_at":1782926326,"resets_in_seconds":5238}}
type wsErrorObj struct {
	Type            string `json:"type"`
	Message         string `json:"message"`
	PlanType        string `json:"plan_type,omitempty"`
	ResetsAt        int64  `json:"resets_at,omitempty"`         // epoch seconds
	ResetsInSeconds int64  `json:"resets_in_seconds,omitempty"` // relative fallback
}

// wsServerError marks an application-level rejection from the Codex server (as
// opposed to a transport/connection failure). Reconnecting does not help, so
// chatStream must fail fast to the fallback chain rather than retry.
type wsServerError struct {
	StatusCode int
	Msg        string
}

func (e *wsServerError) Error() string {
	if e.StatusCode > 0 {
		return fmt.Sprintf("codex ws: server error (status %d): %s", e.StatusCode, e.Msg)
	}
	return fmt.Sprintf("codex ws: server error: %s", e.Msg)
}

// isTerminalServerError reports whether err is a server-side rejection that a
// reconnect+retry cannot fix (usage limit or a failed response).
func isTerminalServerError(err error) bool {
	var ule *UsageLimitError
	if errors.As(err, &ule) {
		return true
	}
	var wse *wsServerError
	return errors.As(err, &wse)
}

// isUsageLimit reports whether an error payload / status is a 429 usage limit.
func isUsageLimit(e *wsErrorObj, statusCode int) bool {
	if statusCode == 429 {
		return true
	}
	if e == nil {
		return false
	}
	t := strings.ToLower(e.Type)
	return strings.Contains(t, "usage_limit") || strings.Contains(t, "rate_limit")
}

// usageLimitResetsAt derives an absolute reset time from the payload, preferring
// the absolute resets_at epoch, then the relative resets_in_seconds.
func usageLimitResetsAt(e *wsErrorObj) time.Time {
	if e == nil {
		return time.Time{}
	}
	if e.ResetsAt > 0 {
		return time.Unix(e.ResetsAt, 0)
	}
	if e.ResetsInSeconds > 0 {
		return time.Now().Add(time.Duration(e.ResetsInSeconds) * time.Second)
	}
	return time.Time{}
}

// wsErrorToGoError converts a top-level {"type":"error"} event into a typed Go
// error: *UsageLimitError for 429/usage limits, *wsServerError otherwise.
func wsErrorToGoError(e *wsErrorObj, statusCode int) error {
	if isUsageLimit(e, statusCode) {
		ule := &UsageLimitError{Provider: "codex-ws", ResetsAt: usageLimitResetsAt(e)}
		if e != nil {
			ule.Message = e.Message
		}
		return ule
	}
	msg := "unknown"
	if e != nil {
		if e.Message != "" {
			msg = e.Message
		} else if e.Type != "" {
			msg = e.Type
		}
	}
	return &wsServerError{StatusCode: statusCode, Msg: msg}
}

// responseFailedToGoError converts a failed response carrying an error object
// into a typed Go error. Returns nil when there is no error payload, so a plain
// failed response keeps its previous (non-erroring) behavior.
func responseFailedToGoError(r *wsResponseObj) error {
	if r == nil || r.Error == nil {
		return nil
	}
	return wsErrorToGoError(r.Error, 0)
}

type wsOutputItem struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Role      string          `json:"role,omitempty"`
	Content   []wsContentPart `json:"content,omitempty"`
	Name      string          `json:"name,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
}

type wsContentPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type wsUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// ---------- provider ----------

// wsSessionState holds per-conversation WebSocket state.
// Each session has its own mutex so concurrent sessions run in parallel.
type wsSessionState struct {
	mu                 sync.Mutex
	conn               *websocket.Conn
	previousResponseID string
	// sentMsgCount tracks how many non-system messages we've already sent
	// in this WS session so we only transmit new ones each turn.
	sentMsgCount int
	// sentDigest fingerprints the messages already sent. Sending only the tail
	// past sentMsgCount is correct exactly while the conversation stays
	// append-only; when the prefix changes underneath us (compaction rewriting
	// history, or an unrelated one-shot prompt landing on the same session key)
	// the count alone cannot tell, and the server would answer against a
	// conversation we no longer have. See historyPrefixDigest.
	sentDigest string
	sessionID  string
	// lastUsed is stored as Unix nanoseconds for atomic access — the cleanup
	// goroutine reads it under p.mu (not sess.mu), so plain time.Time would race.
	lastUsedNs atomic.Int64
}

// CodexWSProvider connects to the Codex backend via persistent WebSocket
// using the same protocol as the official Codex CLI.
// Each logical conversation (identified by session_key in options) gets its
// own WebSocket connection to avoid cross-session state contamination.
// Concurrent sessions run in parallel — the global mu only guards the map.
type CodexWSProvider struct {
	tokenSource func() (string, string, error)
	// accountID is written by concurrent connectSession calls (each under their
	// own sess.mu), so use atomic to avoid data races.
	accountID       atomic.Pointer[string]
	enableWebSearch bool
	baseURL         string

	mu       sync.Mutex
	sessions map[string]*wsSessionState
	done     chan struct{}

	// effortMu guards effortFallbacks, which remembers the reasoning effort a
	// model actually accepted for a requested thinking level. Without it every
	// turn would pay the rejection round-trip again.
	effortMu        sync.Mutex
	effortFallbacks map[string]string
}

func NewCodexWSProvider(token, accountID string) *CodexWSProvider {
	baseURL := os.Getenv(codexWSEndpointEnvVar)
	if baseURL == "" {
		baseURL = codexWSEndpoint
	}
	_ = token // token fetched fresh via tokenSource
	p := &CodexWSProvider{
		tokenSource:     createCodexTokenSource(),
		enableWebSearch: true,
		baseURL:         baseURL,
		sessions:        make(map[string]*wsSessionState),
		done:            make(chan struct{}),
		effortFallbacks: make(map[string]string),
	}
	if accountID != "" {
		p.accountID.Store(&accountID)
	}
	go p.cleanupIdleSessions()
	return p
}

func (p *CodexWSProvider) GetDefaultModel() string { return codexDefaultModel }
func (p *CodexWSProvider) SupportsThinking() bool  { return true }
func (p *CodexWSProvider) SupportsNativeSearch() bool {
	return p.enableWebSearch
}

// Close tears down all WebSocket connections and stops the cleanup goroutine.
func (p *CodexWSProvider) Close() {
	select {
	case <-p.done:
	default:
		close(p.done)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, sess := range p.sessions {
		sess.mu.Lock()
		p.closeSession(sess)
		sess.mu.Unlock()
		delete(p.sessions, key)
	}
}

// cleanupIdleSessions runs in the background and closes WS connections that
// have been idle for longer than wsSessionIdleTimeout.
func (p *CodexWSProvider) cleanupIdleSessions() {
	ticker := time.NewTicker(wsSessionCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-ticker.C:
		}
		nowNs := time.Now().UnixNano()
		p.mu.Lock()
		for key, sess := range p.sessions {
			lastNs := sess.lastUsedNs.Load()
			if lastNs == 0 || time.Duration(nowNs-lastNs) < wsSessionIdleTimeout {
				continue
			}
			// Skip sessions currently in use.
			if !sess.mu.TryLock() {
				continue
			}
			p.closeSession(sess)
			sess.mu.Unlock()
			delete(p.sessions, key)
			logger.DebugCF("provider.codex_ws", "Idle session closed",
				map[string]any{"session_key": key})
		}
		p.mu.Unlock()
	}
}

func (p *CodexWSProvider) closeSession(sess *wsSessionState) {
	if sess != nil && sess.conn != nil {
		// Best-effort close frame, bounded so a wedged connection can't block
		// teardown forever.
		_ = sess.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_ = sess.conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		_ = sess.conn.Close()
		sess.conn = nil
	}
}

// getSession returns the session for sessionKey, creating a new (disconnected)
// one if it doesn't exist. Only the map is touched under the global lock;
// the caller is responsible for connecting and locking the session itself.
func (p *CodexWSProvider) getSession(sessionKey string) *wsSessionState {
	p.mu.Lock()
	sess, ok := p.sessions[sessionKey]
	if !ok {
		sess = &wsSessionState{sessionID: uuid.New().String()}
		p.sessions[sessionKey] = sess
	}
	p.mu.Unlock()
	return sess
}

// deleteSession removes the session from the map under the global lock.
func (p *CodexWSProvider) deleteSession(sessionKey string) {
	p.mu.Lock()
	delete(p.sessions, sessionKey)
	p.mu.Unlock()
}

// connectSession dials a new WebSocket and runs the prewarm turn for the given session.
// Called with sess.mu held.
func (p *CodexWSProvider) connectSession(
	sess *wsSessionState,
	instructions string,
	tools []wsToolDef,
	model string,
	options map[string]any,
) error {
	tok, accID, err := p.tokenSource()
	if err != nil {
		return fmt.Errorf("token: %w", err)
	}
	if accID != "" {
		p.accountID.Store(&accID)
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: wsHandshakeTimeout,
	}
	hdrs := http.Header{}
	hdrs.Set("Authorization", "Bearer "+tok)
	hdrs.Set("Originator", "codex_cli_rs")
	hdrs.Set("User-Agent", codexUserAgent())
	hdrs.Set("Openai-Beta", codexWSBetaHeader)
	if ptr := p.accountID.Load(); ptr != nil && *ptr != "" {
		hdrs.Set("Chatgpt-Account-Id", *ptr)
	}

	wsURL := p.baseURL
	conn, resp, err := dialer.Dial(wsURL, hdrs)
	if err != nil {
		if resp != nil {
			logger.ErrorCF("provider.codex_ws", "WebSocket handshake failed",
				map[string]any{
					"status": resp.Status,
					"url":    wsURL,
					"error":  err.Error(),
				})
		}
		return fmt.Errorf("ws dial: %w", err)
	}
	sess.conn = conn
	sess.previousResponseID = ""
	sess.sentMsgCount = 0
	sess.sentDigest = ""

	logger.DebugCF("provider.codex_ws", "WebSocket connected, sending prewarm", map[string]any{"url": wsURL})
	// Prewarm: send generate=false to let the server load context.
	// Use a minimal request — extra fields (text, prompt_cache_key, client_metadata)
	// can cause the server to close with 1000 before emitting response.completed.
	prewarmInstructions := instructions
	if prewarmInstructions == "" {
		prewarmInstructions = "You are a helpful assistant."
	}
	boolFalse := false
	req := wsRequest{
		Type:              "response.create",
		Model:             model,
		Instructions:      prewarmInstructions,
		Input:             []wsInputItem{},
		Tools:             []wsToolDef{},
		ToolChoice:        "auto",
		ParallelToolCalls: true,
		Store:             false,
		Stream:            true,
		Generate:          &boolFalse,
	}

	if err := p.sendToSession(sess, req); err != nil {
		p.closeSession(sess)
		return fmt.Errorf("prewarm send: %w", err)
	}
	respID, _, err := p.drainStream(sess, nil, nil)
	if err != nil {
		p.closeSession(sess)
		return fmt.Errorf("prewarm drain: %w", err)
	}
	sess.previousResponseID = respID
	logger.DebugCF("provider.codex_ws", "Prewarm done", map[string]any{"response_id": respID})
	return nil
}

func (p *CodexWSProvider) sendToSession(sess *wsSessionState, req wsRequest) error {
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	// Bound the write so a wedged connection can't hang the turn indefinitely.
	// On timeout the connection is unusable; the caller's retry loop closes it
	// and reconnects.
	_ = sess.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	defer sess.conn.SetWriteDeadline(time.Time{})
	return sess.conn.WriteMessage(websocket.TextMessage, data)
}

// drainStream reads events until response.completed / response.failed.
// onText is called with accumulated text on each delta; onItem is called
// for each completed output item.
func (p *CodexWSProvider) drainStream(
	sess *wsSessionState,
	onText func(string),
	onItem func(wsOutputItem),
) (responseID string, usage wsUsage, err error) {
	textByIndex := map[int]strings.Builder{}
	argsByItem := map[string]strings.Builder{}
	itemsByID := map[string]wsOutputItem{}

	// 120s read deadline so we never hang indefinitely.
	_ = sess.conn.SetReadDeadline(time.Now().Add(120 * time.Second))
	defer sess.conn.SetReadDeadline(time.Time{})

	for {
		_, msg, readErr := sess.conn.ReadMessage()
		if readErr != nil {
			err = fmt.Errorf("ws read: %w", readErr)
			return responseID, usage, err
		}

		var evt wsEvent
		if jsonErr := json.Unmarshal(msg, &evt); jsonErr != nil {
			logger.DebugCF(
				"provider.codex_ws",
				"Failed to parse event",
				map[string]any{"raw": string(msg[:min(len(msg), 200)])},
			)
			continue
		}
		if evt.Type != "response.output_text.delta" && evt.Type != "response.function_call_arguments.delta" {
			logger.DebugCF(
				"provider.codex_ws",
				"WS event",
				map[string]any{"type": evt.Type, "raw": string(msg[:min(len(msg), 300)])},
			)
		}

		switch evt.Type {
		case "response.created":
			if evt.Response != nil {
				responseID = evt.Response.ID
			}

		case "response.output_item.added":
			if evt.Item != nil {
				itemsByID[evt.Item.ID] = *evt.Item
			}

		case "response.output_text.delta":
			sb := textByIndex[evt.OutputIndex]
			sb.WriteString(evt.Delta)
			textByIndex[evt.OutputIndex] = sb
			if onText != nil {
				// Build accumulated text across all output_text parts.
				var total strings.Builder
				for _, b := range textByIndex {
					total.WriteString(b.String())
				}
				onText(total.String())
			}

		case "response.function_call_arguments.delta":
			sb := argsByItem[evt.ItemID]
			sb.WriteString(evt.Delta)
			argsByItem[evt.ItemID] = sb

		case "response.output_item.done":
			if evt.Item == nil {
				continue
			}
			item := *evt.Item
			// Patch in accumulated text / arguments from deltas.
			if item.Type == "message" {
				for i, part := range item.Content {
					if part.Type == "output_text" {
						if sb, ok := textByIndex[evt.OutputIndex]; ok {
							item.Content[i].Text = sb.String()
						}
					}
				}
				// If content is empty but we have text for this index, synthesize.
				if len(item.Content) == 0 {
					if sb, ok := textByIndex[evt.OutputIndex]; ok && sb.Len() > 0 {
						item.Content = []wsContentPart{{Type: "output_text", Text: sb.String()}}
					}
				}
			} else if item.Type == "function_call" {
				if sb, ok := argsByItem[item.ID]; ok {
					item.Arguments = sb.String()
				}
			}
			itemsByID[item.ID] = item
			if onItem != nil {
				onItem(item)
			}

		case "error":
			// Top-level server error (e.g. 429 usage_limit_reached). Surface it
			// immediately instead of blocking on the read deadline.
			err = wsErrorToGoError(evt.Error, evt.StatusCode)
			return responseID, usage, err

		case "response.completed", "response.incomplete":
			if evt.Response != nil {
				if evt.Response.ID != "" {
					responseID = evt.Response.ID
				}
				usage = evt.Response.Usage
			}
			return responseID, usage, err

		case "response.failed":
			if evt.Response != nil {
				if evt.Response.ID != "" {
					responseID = evt.Response.ID
				}
				usage = evt.Response.Usage
			}
			// Only turn a failed response into an error when it carries an error
			// payload (e.g. a usage limit delivered this way); otherwise keep the
			// prior behavior of returning whatever was already streamed.
			err = responseFailedToGoError(evt.Response)
			return responseID, usage, err
		}
	}
}

// buildRequest constructs the wsRequest. input must already be the slice of
// only NEW items for this turn (empty for prewarm).
func (p *CodexWSProvider) buildRequest(
	sess *wsSessionState,
	instructions, prevRespID string,
	input []wsInputItem,
	tools []wsToolDef,
	model string,
	options map[string]any,
	effortOverride string,
) wsRequest {
	if input == nil {
		input = []wsInputItem{}
	}
	if tools == nil {
		tools = []wsToolDef{}
	}
	req := wsRequest{
		Type:               "response.create",
		Model:              model,
		Instructions:       instructions,
		PreviousResponseID: prevRespID,
		Input:              input,
		Tools:              tools,
		ToolChoice:         "auto",
		ParallelToolCalls:  true,
		Store:              false,
		Stream:             true,
		PromptCacheKey:     sess.sessionID,
		Text:               &wsText{Verbosity: "low"},
		ClientMetadata: map[string]any{
			"x-codex-turn-metadata": fmt.Sprintf(
				`{"session_id":%q,"turn_id":%q,"sandbox":"none"}`,
				sess.sessionID, uuid.New().String(),
			),
		},
	}

	if level, ok := options["thinking_level"].(string); ok && level != "" && level != "off" {
		switch {
		case effortOverride != "":
			// The model rejected the requested effort and told us what it takes.
			req.Reasoning = &wsReasoning{Effort: effortOverride}
			req.Include = []string{"reasoning.encrypted_content"}
		case level == "auto":
			// Let server choose effort (send reasoning block with no explicit effort).
			req.Reasoning = &wsReasoning{}
			req.Include = []string{"reasoning.encrypted_content"}
		default:
			if effort, effortOK := codexReasoningEffort(level); effortOK {
				req.Reasoning = &wsReasoning{Effort: string(effort)}
				req.Include = []string{"reasoning.encrypted_content"}
			}
		}
	}

	return req
}

func codexEffortCacheKey(model, level string) string {
	return model + "\x00" + strings.ToLower(strings.TrimSpace(level))
}

// cachedEffortFallback returns the effort previously found to work for this
// model/level pair, or "" when the requested level has never been rejected.
func (p *CodexWSProvider) cachedEffortFallback(model, level string) string {
	if level == "" {
		return ""
	}
	p.effortMu.Lock()
	defer p.effortMu.Unlock()
	return p.effortFallbacks[codexEffortCacheKey(model, level)]
}

func (p *CodexWSProvider) rememberEffortFallback(model, level, effort string) {
	p.effortMu.Lock()
	defer p.effortMu.Unlock()
	p.effortFallbacks[codexEffortCacheKey(model, level)] = effort
}

// Chat implements LLMProvider.
func (p *CodexWSProvider) Chat(
	ctx context.Context,
	messages []Message,
	tools []ToolDefinition,
	model string,
	options map[string]any,
) (*LLMResponse, error) {
	var outputItems []wsOutputItem
	usage, err := p.chatStream(ctx, messages, tools, model, options, nil, func(item wsOutputItem) {
		outputItems = append(outputItems, item)
	})
	if err != nil {
		return nil, err
	}
	return parseWSResponse(outputItems, usage), nil
}

// ChatStream implements StreamingProvider.
func (p *CodexWSProvider) ChatStream(
	ctx context.Context,
	messages []Message,
	tools []ToolDefinition,
	model string,
	options map[string]any,
	onChunk func(string),
) (*LLMResponse, error) {
	var outputItems []wsOutputItem
	usage, err := p.chatStream(ctx, messages, tools, model, options, onChunk, func(item wsOutputItem) {
		outputItems = append(outputItems, item)
	})
	if err != nil {
		return nil, err
	}
	return parseWSResponse(outputItems, usage), nil
}

func (p *CodexWSProvider) chatStream(
	ctx context.Context,
	messages []Message,
	tools []ToolDefinition,
	model string,
	options map[string]any,
	onText func(string),
	onItem func(wsOutputItem),
) (wsUsage, error) {
	resolvedModel, substitutionReason := resolveCodexModel(model)
	if substitutionReason != "" {
		// Silent substitution is how "my model does nothing" bugs are born:
		// the turn succeeds, just not on the model that was configured.
		logger.WarnCF("provider.codex_ws", "Requested model is not usable on this transport, substituting",
			map[string]any{
				"requested": model,
				"using":     resolvedModel,
				"reason":    substitutionReason,
			})
	}
	useNativeSearch := p.enableWebSearch && (options["native_search"] == true)
	wsTools := translateToolsForWS(tools, useNativeSearch)

	// Separate system prompt from conversation messages.
	var instructions string
	var convMsgs []Message
	for _, m := range messages {
		if m.Role == "system" {
			instructions = m.Content
		} else {
			convMsgs = append(convMsgs, m)
		}
	}
	if instructions == "" {
		instructions = defaultCodexInstructions
	}

	// Each logical session (Telegram chat, DM, etc.) gets its own WS connection.
	sessionKey, _ := options["session_key"].(string)
	if sessionKey == "" {
		sessionKey = "default"
	}
	stateless, _ := options["stateless"].(bool)

	// Phase A: look up (or allocate) the session struct — global lock, map only.
	//
	// Stateless callers (context compaction and other one-shot prompts) get a
	// private, throwaway connection instead: their prompts are unrelated to each
	// other and to any chat, so joining a shared session would chain them onto a
	// foreign previous_response_id, serialize them behind that chat's turns, and
	// leave the conversation the chat resumes from polluted.
	var sess *wsSessionState
	if stateless {
		sess = &wsSessionState{sessionID: uuid.New().String()}
		defer func() {
			sess.mu.Lock()
			p.closeSession(sess)
			sess.mu.Unlock()
		}()
	} else {
		sess = p.getSession(sessionKey)
	}
	// Dropping a shared session from the map on failure forces the next turn to
	// start clean; a throwaway session is not in the map to begin with, and
	// deleting by key here would evict an unrelated live chat session.
	dropSession := func() {
		if !stateless {
			p.deleteSession(sessionKey)
		}
	}

	// Phase B: all WS I/O under the per-session lock so different sessions
	// proceed in parallel while turns within the same session are serialized.
	sess.mu.Lock()
	defer sess.mu.Unlock()

	sess.lastUsedNs.Store(time.Now().UnixNano())

	// Connect if first use or the connection was closed by the cleanup routine.
	if sess.conn == nil {
		if err := p.connectSession(sess, instructions, wsTools, resolvedModel, options); err != nil {
			dropSession()
			return wsUsage{}, fmt.Errorf("codex ws connect: %w", err)
		}
	}

	// Replaying only the tail past sentMsgCount is valid while the conversation
	// grows append-only. History compaction shrinks it, and a rewritten prefix
	// can even keep the same length, so verify the prefix we already sent still
	// matches before trusting the cursor. On any mismatch, reconnect and replay
	// the full (compressed) history.
	if reason := codexWSReplayResetReason(sess.sentMsgCount, sess.sentDigest, convMsgs); reason != "" {
		// Warn, not debug: every occurrence throws away the server-side prefix
		// cache and replays the whole conversation, so this is the signal to
		// look at when latency climbs. reason says which invariant broke and
		// replayed_msgs says what it cost.
		logger.WarnCF("provider.codex_ws", "History changed under the session, restarting and replaying",
			map[string]any{
				"session_key":   sessionKey,
				"reason":        reason,
				"old_sent":      sess.sentMsgCount,
				"new_len":       len(convMsgs),
				"dropped_msgs":  max(0, sess.sentMsgCount-len(convMsgs)),
				"replayed_msgs": len(convMsgs),
			})
		p.closeSession(sess)
		if err := p.connectSession(sess, instructions, wsTools, resolvedModel, options); err != nil {
			dropSession()
			return wsUsage{}, fmt.Errorf("codex ws reconnect after compaction: %w", err)
		}
	}
	if normalizedSentCount, reset := normalizeCodexWSReplayCursor(sess.sentMsgCount, len(convMsgs)); reset {
		logger.WarnCF(
			"provider.codex_ws",
			"Replay cursor exceeded history after reconnect, resetting session replay state",
			map[string]any{"session_key": sessionKey, "sent_count": sess.sentMsgCount, "history_len": len(convMsgs)},
		)
		sess.sentMsgCount = normalizedSentCount
		sess.previousResponseID = ""
	}

	// Reasoning efforts are model-specific and change between model
	// generations, so reuse whatever this model accepted for this level before.
	requestedLevel, _ := options["thinking_level"].(string)
	effortOverride := p.cachedEffortFallback(resolvedModel, requestedLevel)
	effortRetries := 0

	// Build the initial request with only NEW messages since the last turn.
	newMsgs := convMsgs[sess.sentMsgCount:]
	req := p.buildRequest(
		sess,
		instructions,
		sess.previousResponseID,
		buildWSInput(ensureToolOutputs(newMsgs)),
		wsTools,
		resolvedModel,
		options,
		effortOverride,
	)
	sess.sentMsgCount = len(convMsgs)
	sess.sentDigest = historyPrefixDigest(convMsgs)

	// Send + drain with reconnect backoff.
	// On any connection error (send failure or mid-stream drop) we reconnect
	// and replay the full history, retrying until ~30 seconds have elapsed.
	var (
		respID    string
		usage     wsUsage
		lastErr   error
		backoff   = 2 * time.Second
		deadline  = time.Now().Add(wsRetryTimeout)
		firstSend = true
	)
	for {
		if firstSend {
			firstSend = false
		} else {
			// Reconnect before retry: close broken conn, wait, dial again.
			p.closeSession(sess)
			remaining := time.Until(deadline)
			if remaining <= 0 {
				break
			}
			sleep := backoff
			if sleep > remaining {
				sleep = remaining
			}
			logger.WarnCF("provider.codex_ws", "Reconnecting after error",
				map[string]any{"error": lastErr.Error(), "backoff": sleep.String(), "session_key": sessionKey})
			select {
			case <-ctx.Done():
				dropSession()
				return wsUsage{}, ctx.Err()
			case <-time.After(sleep):
			}
			backoff = min(backoff*2, 15*time.Second)
			if err := p.connectSession(sess, instructions, wsTools, resolvedModel, options); err != nil {
				lastErr = err
				continue
			}
			// Replay full history after reconnect.
			sess.sentMsgCount = len(convMsgs)
			sess.sentDigest = historyPrefixDigest(convMsgs)
			req = p.buildRequest(
				sess,
				instructions,
				sess.previousResponseID,
				buildWSInput(ensureToolOutputs(convMsgs)),
				wsTools,
				resolvedModel,
				options,
				effortOverride,
			)
		}

		if err := p.sendToSession(sess, req); err != nil {
			lastErr = err
			continue
		}
		respID, usage, lastErr = p.drainStream(sess, onText, onItem)
		if lastErr == nil {
			break
		}
		// The model may reject the reasoning effort outright (gpt-6-astra
		// dropped "none"/"minimal"). The rejection names the efforts it does
		// take, so retry on the nearest one instead of failing the turn over a
		// knob the caller does not care that precisely about.
		if effortRetries < maxCodexEffortRetries {
			if effort, ok := p.effortFallbackFromError(
				resolvedModel,
				requestedLevel,
				lastErr,
			); ok &&
				effort != effortOverride {
				effortOverride = effort
				effortRetries++
				continue
			}
		}
		// Server-side rejection (usage limit / failed response): reconnecting
		// would just hit the same error. Fail fast so the fallback chain can
		// switch providers immediately instead of burning the retry deadline.
		if isTerminalServerError(lastErr) {
			break
		}
	}

	if lastErr != nil {
		p.closeSession(sess)
		dropSession()
		return wsUsage{}, fmt.Errorf("codex ws: %w", lastErr)
	}
	sess.previousResponseID = respID
	return usage, nil
}

// maxCodexEffortRetries bounds the effort renegotiation so a server that keeps
// rejecting our choice cannot spin the retry loop.
const maxCodexEffortRetries = 2

// effortFallbackFromError inspects a failed turn for an "unsupported reasoning
// effort" rejection and returns the nearest effort the model accepts, caching
// it so later turns skip the round-trip.
func (p *CodexWSProvider) effortFallbackFromError(model, level string, err error) (string, bool) {
	if err == nil || level == "" {
		return "", false
	}
	var serverErr *wsServerError
	if !errors.As(err, &serverErr) {
		return "", false
	}
	rejected, supported, ok := parseCodexUnsupportedEffort(serverErr.Msg)
	if !ok {
		return "", false
	}
	effort, ok := nearestCodexEffort(rejected, supported)
	if !ok {
		return "", false
	}

	p.rememberEffortFallback(model, level, effort)
	logger.WarnCF("provider.codex_ws", "Model rejected the reasoning effort, retrying with the nearest supported one",
		map[string]any{
			"model":          model,
			"thinking_level": level,
			"rejected":       rejected,
			"using":          effort,
			"supported":      strings.Join(supported, ","),
		})
	return effort, true
}

func normalizeCodexWSReplayCursor(sentMsgCount, historyLen int) (int, bool) {
	if sentMsgCount < 0 || sentMsgCount > historyLen {
		return 0, true
	}
	return sentMsgCount, false
}

// historyPrefixDigest fingerprints a conversation slice. It covers everything
// that identifies a message to the server (role, text, tool calls and results)
// so any rewrite of already-sent history is detected.
func historyPrefixDigest(msgs []Message) string {
	h := fnv.New64a()
	sep := []byte{0}
	for _, m := range msgs {
		_, _ = h.Write([]byte(m.Role))
		_, _ = h.Write(sep)
		_, _ = h.Write([]byte(m.Content))
		_, _ = h.Write(sep)
		_, _ = h.Write([]byte(m.ToolCallID))
		for _, tc := range m.ToolCalls {
			_, _ = h.Write([]byte(tc.ID))
			if tc.Function != nil {
				_, _ = h.Write([]byte(tc.Function.Name))
				_, _ = h.Write([]byte(tc.Function.Arguments))
			}
		}
		_, _ = h.Write(sep)
	}
	return strconv.FormatUint(h.Sum64(), 16)
}

// codexWSReplayResetReason reports why the incremental replay cursor can no
// longer be trusted, or "" when appending the tail is still correct.
//
// Sending only convMsgs[sentMsgCount:] assumes the conversation is the one the
// server already holds, extended at the end. Two things break that assumption:
// history compaction (which shrinks or rewrites the prefix) and unrelated
// prompts reusing the same session key (a one-shot summarization request looks
// like "1 message" turn after turn, so the count matches while the content is
// entirely different — the server would then receive an empty delta and answer
// the *previous* prompt again).
func codexWSReplayResetReason(sentMsgCount int, sentDigest string, convMsgs []Message) string {
	if sentMsgCount <= 0 {
		return ""
	}
	if len(convMsgs) < sentMsgCount {
		return "history_shrank"
	}
	if sentDigest == "" {
		// No fingerprint recorded for this connection (older state): fall back
		// to the length check alone.
		return ""
	}
	if historyPrefixDigest(convMsgs[:sentMsgCount]) != sentDigest {
		return "prefix_rewritten"
	}
	if len(convMsgs) == sentMsgCount {
		// Nothing new to say: replaying is the only way to get a fresh answer
		// instead of an empty-input continuation of the previous response.
		return "no_new_messages"
	}
	return ""
}

// ensureToolOutputs guarantees that every assistant function_call in msgs has a
// matching tool result. Context compaction (seahorse leaf-summaries) can drop a
// tool result while keeping its assistant tool_call, which makes the Codex
// server reject the whole turn with "No tool output found for function call
// <id>". For each such orphaned call we synthesize a placeholder tool result so
// the request stays well-formed. It is a no-op (returning the input slice
// unchanged) when every tool_call is already paired, so healthy turns pay no
// cost and are not mutated.
func ensureToolOutputs(msgs []Message) []Message {
	haveOutput := make(map[string]bool)
	for _, m := range msgs {
		// Tool results arrive as role "tool", or role "user" carrying a
		// ToolCallID (see buildWSInput) — both become function_call_output.
		if m.ToolCallID != "" && (m.Role == "tool" || m.Role == "user") {
			haveOutput[m.ToolCallID] = true
		}
	}

	orphaned := false
	for _, m := range msgs {
		if m.Role != "assistant" {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.ID != "" && !haveOutput[tc.ID] {
				orphaned = true
				break
			}
		}
		if orphaned {
			break
		}
	}
	if !orphaned {
		return msgs
	}

	out := make([]Message, 0, len(msgs)+2)
	for _, m := range msgs {
		out = append(out, m)
		if m.Role != "assistant" {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.ID == "" || haveOutput[tc.ID] {
				continue
			}
			out = append(out, Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    "[tool output unavailable: omitted during context compaction]",
			})
			haveOutput[tc.ID] = true
		}
	}
	return out
}

// buildWSInput converts picoclaw Messages to wsInputItems.
func buildWSInput(msgs []Message) []wsInputItem {
	var items []wsInputItem
	for _, msg := range msgs {
		switch msg.Role {
		case "user":
			if msg.ToolCallID != "" {
				items = append(items, wsFunctionCallOutputItem(msg.ToolCallID, msg.Content))
			} else if len(msg.Media) > 0 {
				parts := buildWSContentParts(msg)
				items = append(items, wsMessageItemWithParts("user", parts))
			} else {
				items = append(items, wsMessageItem("user", msg.Content))
			}
		case "assistant":
			if len(msg.ToolCalls) > 0 {
				if msg.Content != "" {
					items = append(items, wsMessageItem("assistant", msg.Content))
				}
				for _, tc := range msg.ToolCalls {
					name, args, ok := resolveCodexToolCall(tc)
					if !ok {
						continue
					}
					items = append(items, wsFunctionCallItem(tc.ID, name, args))
				}
			} else {
				items = append(items, wsMessageItem("assistant", msg.Content))
			}
		case "tool":
			items = append(items, wsFunctionCallOutputItem(msg.ToolCallID, msg.Content))
		}
	}
	return items
}

func buildWSContentParts(msg Message) []map[string]any {
	var parts []map[string]any
	if msg.Content != "" {
		parts = append(parts, map[string]any{"type": "input_text", "text": msg.Content})
	}
	for _, mediaURL := range msg.Media {
		if strings.HasPrefix(mediaURL, "data:image/") {
			parts = append(parts, map[string]any{
				"type":      "input_image",
				"image_url": mediaURL,
				"detail":    "auto",
			})
		}
	}
	return parts
}

func translateToolsForWS(tools []ToolDefinition, enableWebSearch bool) []wsToolDef {
	var result []wsToolDef
	for _, t := range tools {
		if t.Type != "function" {
			continue
		}
		if enableWebSearch && strings.EqualFold(t.Function.Name, "web_search") {
			continue
		}
		params, _ := json.Marshal(t.Function.Parameters)
		var paramsMap map[string]any
		_ = json.Unmarshal(params, &paramsMap)
		td := wsToolDef{
			Type:        "function",
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  paramsMap,
		}
		result = append(result, td)
	}
	if enableWebSearch {
		result = append(result, wsToolDef{Type: "web_search"})
	}
	return result
}

func parseWSResponse(items []wsOutputItem, usage wsUsage) *LLMResponse {
	var content strings.Builder
	var toolCalls []ToolCall

	for _, item := range items {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				if part.Type == "output_text" {
					content.WriteString(part.Text)
				}
			}
		case "function_call":
			var args map[string]any
			if err := json.Unmarshal([]byte(item.Arguments), &args); err != nil {
				args = map[string]any{"raw": item.Arguments}
			}
			toolCalls = append(toolCalls, ToolCall{
				ID:        item.CallID,
				Name:      item.Name,
				Arguments: args,
			})
		}
	}

	finishReason := "stop"
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	}

	resp := &LLMResponse{
		Content:      content.String(),
		ToolCalls:    toolCalls,
		FinishReason: finishReason,
	}
	// The backend omits usage on some turns; reporting zeros as a real
	// measurement would make context-usage look empty rather than unknown.
	if usage.TotalTokens > 0 || usage.InputTokens > 0 || usage.OutputTokens > 0 {
		total := usage.TotalTokens
		if total == 0 {
			total = usage.InputTokens + usage.OutputTokens
		}
		resp.Usage = &UsageInfo{
			PromptTokens:     usage.InputTokens,
			CompletionTokens: usage.OutputTokens,
			TotalTokens:      total,
		}
	}
	return resp
}

// wsHandshakeTimeout is the timeout for the WebSocket handshake.
const wsHandshakeTimeout = 30e9 // 30s as time.Duration
