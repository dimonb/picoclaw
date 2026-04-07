package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
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
)

// ---------- request structs ----------

type wsRequest struct {
	Type               string          `json:"type"`
	Model              string          `json:"model"`
	Instructions       string          `json:"instructions,omitempty"`
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
	Input              []wsInputItem   `json:"input"`
	Tools              []wsToolDef     `json:"tools,omitempty"`
	ToolChoice         string          `json:"tool_choice"`
	ParallelToolCalls  bool            `json:"parallel_tool_calls"`
	Reasoning          *wsReasoning    `json:"reasoning,omitempty"`
	Store              bool            `json:"store"`
	Stream             bool            `json:"stream"`
	Include            []string        `json:"include,omitempty"`
	PromptCacheKey     string          `json:"prompt_cache_key,omitempty"`
	Text               *wsText         `json:"text,omitempty"`
	Generate           *bool           `json:"generate,omitempty"`
	ClientMetadata     map[string]any  `json:"client_metadata,omitempty"`
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
}

type wsResponseObj struct {
	ID     string        `json:"id"`
	Status string        `json:"status"`
	Output []wsOutputItem `json:"output"`
	Usage  wsUsage       `json:"usage"`
}

type wsOutputItem struct {
	ID        string            `json:"id"`
	Type      string            `json:"type"`
	Role      string            `json:"role,omitempty"`
	Content   []wsContentPart   `json:"content,omitempty"`
	Name      string            `json:"name,omitempty"`
	CallID    string            `json:"call_id,omitempty"`
	Arguments string            `json:"arguments,omitempty"`
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
	sessionID    string
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
	tokenSource     func() (string, string, error)
	// accountID is written by concurrent connectSession calls (each under their
	// own sess.mu), so use atomic to avoid data races.
	accountID       atomic.Pointer[string]
	enableWebSearch bool
	baseURL         string

	mu       sync.Mutex
	sessions map[string]*wsSessionState
	done     chan struct{}
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
func (p *CodexWSProvider) connectSession(sess *wsSessionState, instructions string, tools []wsToolDef, model string, options map[string]any) error {
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
	hdrs.Set("originator", "codex_cli_rs")
	hdrs.Set("User-Agent", codexUserAgent())
	hdrs.Set("openai-beta", codexWSBetaHeader)
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
			return
		}

		var evt wsEvent
		if jsonErr := json.Unmarshal(msg, &evt); jsonErr != nil {
			logger.DebugCF("provider.codex_ws", "Failed to parse event", map[string]any{"raw": string(msg[:min(len(msg), 200)])})
			continue
		}
		if evt.Type != "response.output_text.delta" && evt.Type != "response.function_call_arguments.delta" {
			logger.DebugCF("provider.codex_ws", "WS event", map[string]any{"type": evt.Type, "raw": string(msg[:min(len(msg), 300)])})
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

		case "response.completed", "response.failed", "response.incomplete":
			if evt.Response != nil {
				if evt.Response.ID != "" {
					responseID = evt.Response.ID
				}
				usage = evt.Response.Usage
			}
			return
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
		if level == "auto" {
			// Let server choose effort (send reasoning block with no explicit effort).
			req.Reasoning = &wsReasoning{}
			req.Include = []string{"reasoning.encrypted_content"}
		} else if effort, effortOK := codexReasoningEffort(level); effortOK {
			req.Reasoning = &wsReasoning{Effort: string(effort)}
			req.Include = []string{"reasoning.encrypted_content"}
		}
	}

	return req
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
	_, err := p.chatStream(ctx, messages, tools, model, options, nil, func(item wsOutputItem) {
		outputItems = append(outputItems, item)
	})
	if err != nil {
		return nil, err
	}
	return parseWSResponse(outputItems), nil
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
	_, err := p.chatStream(ctx, messages, tools, model, options, onChunk, func(item wsOutputItem) {
		outputItems = append(outputItems, item)
	})
	if err != nil {
		return nil, err
	}
	return parseWSResponse(outputItems), nil
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
	resolvedModel, _ := resolveCodexModel(model)
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

	// Phase A: look up (or allocate) the session struct — global lock, map only.
	sess := p.getSession(sessionKey)

	// Phase B: all WS I/O under the per-session lock so different sessions
	// proceed in parallel while turns within the same session are serialized.
	sess.mu.Lock()
	defer sess.mu.Unlock()

	sess.lastUsedNs.Store(time.Now().UnixNano())

	// Connect if first use or the connection was closed by the cleanup routine.
	if sess.conn == nil {
		if err := p.connectSession(sess, instructions, wsTools, resolvedModel, options); err != nil {
			p.deleteSession(sessionKey)
			return wsUsage{}, fmt.Errorf("codex ws connect: %w", err)
		}
	}

	// If history was compacted (summarized), the message array shrinks below
	// sentMsgCount. Reconnect and replay the full (compressed) history.
	if len(convMsgs) < sess.sentMsgCount {
		logger.DebugCF("provider.codex_ws", "History compacted, reconnecting session",
			map[string]any{"session_key": sessionKey, "old_sent": sess.sentMsgCount, "new_len": len(convMsgs)})
		p.closeSession(sess)
		if err := p.connectSession(sess, instructions, wsTools, resolvedModel, options); err != nil {
			p.deleteSession(sessionKey)
			return wsUsage{}, fmt.Errorf("codex ws reconnect after compaction: %w", err)
		}
	}

	// Build the input slice with only NEW messages since the last turn.
	newMsgs := convMsgs[sess.sentMsgCount:]
	input := buildWSInput(newMsgs)
	sess.sentMsgCount = len(convMsgs)

	req := p.buildRequest(sess, instructions, sess.previousResponseID, input, wsTools, resolvedModel, options)

	if err := p.sendToSession(sess, req); err != nil {
		// Connection broken — reconnect once and replay full history.
		p.closeSession(sess)
		logger.WarnCF("provider.codex_ws", "Send failed, reconnecting",
			map[string]any{"error": err.Error(), "session_key": sessionKey})
		if err2 := p.connectSession(sess, instructions, wsTools, resolvedModel, options); err2 != nil {
			p.deleteSession(sessionKey)
			return wsUsage{}, fmt.Errorf("codex ws reconnect: %w", err2)
		}
		sess.sentMsgCount = len(convMsgs)
		req = p.buildRequest(sess, instructions, sess.previousResponseID, buildWSInput(convMsgs), wsTools, resolvedModel, options)
		if err3 := p.sendToSession(sess, req); err3 != nil {
			p.closeSession(sess)
			p.deleteSession(sessionKey)
			return wsUsage{}, fmt.Errorf("codex ws send after reconnect: %w", err3)
		}
	}

	respID, usage, streamErr := p.drainStream(sess, onText, onItem)
	if streamErr != nil {
		// Connection dropped mid-stream (e.g. keepalive ping timeout, network reset).
		// Reconnect once and replay the full message history.
		p.closeSession(sess)
		logger.WarnCF("provider.codex_ws", "Stream read failed, reconnecting and retrying",
			map[string]any{"error": streamErr.Error(), "session_key": sessionKey})
		if err := p.connectSession(sess, instructions, wsTools, resolvedModel, options); err != nil {
			p.deleteSession(sessionKey)
			return wsUsage{}, fmt.Errorf("codex ws stream: %w", streamErr)
		}
		sess.sentMsgCount = len(convMsgs)
		retryReq := p.buildRequest(sess, instructions, sess.previousResponseID, buildWSInput(convMsgs), wsTools, resolvedModel, options)
		if err := p.sendToSession(sess, retryReq); err != nil {
			p.closeSession(sess)
			p.deleteSession(sessionKey)
			return wsUsage{}, fmt.Errorf("codex ws stream retry send: %w", err)
		}
		respID, usage, streamErr = p.drainStream(sess, onText, onItem)
		if streamErr != nil {
			p.closeSession(sess)
			p.deleteSession(sessionKey)
			return wsUsage{}, fmt.Errorf("codex ws stream: %w", streamErr)
		}
	}
	sess.previousResponseID = respID
	return usage, nil
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

func parseWSResponse(items []wsOutputItem) *LLMResponse {
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

	return &LLMResponse{
		Content:      content.String(),
		ToolCalls:    toolCalls,
		FinishReason: finishReason,
	}
}

// wsHandshakeTimeout is the timeout for the WebSocket handshake.
const wsHandshakeTimeout = 30e9 // 30s as time.Duration
