package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/sipeed/picoclaw/pkg/logger"
)

const (
	codexWSEndpoint       = "wss://chatgpt.com/backend-api/responses"
	codexWSBetaHeader     = "responses_websockets=2026-02-06"
	codexWSEndpointEnvVar = "CODEX_WS_URL"
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
	Effort string `json:"effort"`
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
	Strict      bool           `json:"strict"`
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

// CodexWSProvider connects to the Codex backend via persistent WebSocket
// using the same protocol as the official Codex CLI.
type CodexWSProvider struct {
	tokenSource     func() (string, string, error)
	accountID       string
	enableWebSearch bool
	baseURL         string

	mu                 sync.Mutex
	conn               *websocket.Conn
	previousResponseID string
	// sentMsgCount tracks how many non-system messages we've already sent
	// in this session so we only transmit new ones each turn.
	sentMsgCount int
	sessionID    string
}

func NewCodexWSProvider(token, accountID string) *CodexWSProvider {
	baseURL := os.Getenv(codexWSEndpointEnvVar)
	if baseURL == "" {
		baseURL = codexWSEndpoint
	}
	_ = token // token fetched fresh via tokenSource
	return &CodexWSProvider{
		tokenSource:     createCodexTokenSource(),
		accountID:       accountID,
		enableWebSearch: true,
		baseURL:         baseURL,
		sessionID:       uuid.New().String(),
	}
}

func (p *CodexWSProvider) GetDefaultModel() string { return codexDefaultModel }
func (p *CodexWSProvider) SupportsThinking() bool  { return true }
func (p *CodexWSProvider) SupportsNativeSearch() bool {
	return p.enableWebSearch
}

// Close tears down the WebSocket connection.
func (p *CodexWSProvider) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closeConn()
}

func (p *CodexWSProvider) closeConn() {
	if p.conn != nil {
		_ = p.conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		_ = p.conn.Close()
		p.conn = nil
	}
}

// connect dials the WebSocket and runs the prewarm turn.
// Must be called with p.mu held.
func (p *CodexWSProvider) connect(instructions string, tools []wsToolDef, model string, options map[string]any) error {
	tok, accID, err := p.tokenSource()
	if err != nil {
		return fmt.Errorf("token: %w", err)
	}
	if accID != "" {
		p.accountID = accID
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: wsHandshakeTimeout,
	}
	hdrs := http.Header{}
	hdrs.Set("Authorization", "Bearer "+tok)
	hdrs.Set("originator", "codex_cli_rs")
	hdrs.Set("User-Agent", codexUserAgent())
	hdrs.Set("openai-beta", codexWSBetaHeader)
	if p.accountID != "" {
		hdrs.Set("Chatgpt-Account-Id", p.accountID)
	}

	wsURL := p.baseURL
	conn, _, err := dialer.Dial(wsURL, hdrs)
	if err != nil {
		return fmt.Errorf("ws dial: %w", err)
	}
	p.conn = conn
	p.previousResponseID = ""
	p.sentMsgCount = 0

	// Prewarm: send generate=false to let the server load context.
	boolFalse := false
	req := p.buildRequest(instructions, "", nil, tools, model, options)
	req.Generate = &boolFalse

	if err := p.sendRequest(req); err != nil {
		p.closeConn()
		return fmt.Errorf("prewarm send: %w", err)
	}
	respID, _, err := p.drainStream(nil, nil)
	if err != nil {
		p.closeConn()
		return fmt.Errorf("prewarm drain: %w", err)
	}
	p.previousResponseID = respID
	return nil
}

func (p *CodexWSProvider) sendRequest(req wsRequest) error {
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	return p.conn.WriteMessage(websocket.TextMessage, data)
}

// drainStream reads events until response.completed / response.failed.
// onText is called with accumulated text on each delta; onItem is called
// for each completed output item.
func (p *CodexWSProvider) drainStream(
	onText func(string),
	onItem func(wsOutputItem),
) (responseID string, usage wsUsage, err error) {
	textByIndex := map[int]strings.Builder{}
	argsByItem := map[string]strings.Builder{}
	itemsByID := map[string]wsOutputItem{}

	for {
		_, msg, readErr := p.conn.ReadMessage()
		if readErr != nil {
			err = fmt.Errorf("ws read: %w", readErr)
			return
		}

		var evt wsEvent
		if jsonErr := json.Unmarshal(msg, &evt); jsonErr != nil {
			continue
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
	instructions, prevRespID string,
	input []wsInputItem,
	tools []wsToolDef,
	model string,
	options map[string]any,
) wsRequest {
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
		PromptCacheKey:     p.sessionID,
		Text:               &wsText{Verbosity: "low"},
		ClientMetadata: map[string]any{
			"x-codex-turn-metadata": fmt.Sprintf(
				`{"session_id":%q,"turn_id":%q,"sandbox":"none"}`,
				p.sessionID, uuid.New().String(),
			),
		},
	}

	if level, ok := options["thinking_level"].(string); ok && level != "" && level != "off" {
		if effort, effortOK := codexReasoningEffort(level); effortOK {
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

	p.mu.Lock()
	defer p.mu.Unlock()

	// (Re)connect if needed.
	if p.conn == nil {
		if err := p.connect(instructions, wsTools, resolvedModel, options); err != nil {
			return wsUsage{}, fmt.Errorf("codex ws connect: %w", err)
		}
	}

	// Build the input slice with only NEW messages since the last turn.
	newMsgs := convMsgs[p.sentMsgCount:]
	input := buildWSInput(newMsgs)
	p.sentMsgCount = len(convMsgs)

	req := p.buildRequest(instructions, p.previousResponseID, input, wsTools, resolvedModel, options)

	if err := p.sendRequest(req); err != nil {
		// Connection broken — reset and retry once.
		p.closeConn()
		logger.WarnCF("provider.codex_ws", "Send failed, reconnecting", map[string]any{"error": err.Error()})
		if err2 := p.connect(instructions, wsTools, resolvedModel, options); err2 != nil {
			return wsUsage{}, fmt.Errorf("codex ws reconnect: %w", err2)
		}
		// After reconnect sentMsgCount was reset to 0, rebuild full input.
		newMsgs = convMsgs
		p.sentMsgCount = len(convMsgs)
		req = p.buildRequest(instructions, p.previousResponseID, buildWSInput(newMsgs), wsTools, resolvedModel, options)
		if err3 := p.sendRequest(req); err3 != nil {
			p.closeConn()
			return wsUsage{}, fmt.Errorf("codex ws send after reconnect: %w", err3)
		}
	}

	respID, usage, err := p.drainStream(onText, onItem)
	if err != nil {
		p.closeConn()
		return wsUsage{}, fmt.Errorf("codex ws stream: %w", err)
	}
	p.previousResponseID = respID
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
			Strict:      false,
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
