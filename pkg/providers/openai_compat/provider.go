package openai_compat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/providers/protocoltypes"
)

type (
	ToolCall               = protocoltypes.ToolCall
	FunctionCall           = protocoltypes.FunctionCall
	LLMResponse            = protocoltypes.LLMResponse
	UsageInfo              = protocoltypes.UsageInfo
	Message                = protocoltypes.Message
	ToolDefinition         = protocoltypes.ToolDefinition
	ToolFunctionDefinition = protocoltypes.ToolFunctionDefinition
	ExtraContent           = protocoltypes.ExtraContent
	GoogleExtra            = protocoltypes.GoogleExtra
	ReasoningDetail        = protocoltypes.ReasoningDetail
)

type Provider struct {
	apiKey          string
	apiBase         string
	maxTokensField  string // Field name for max tokens (e.g., "max_completion_tokens" for o1/glm models)
	httpClient      *http.Client
	extraBody       map[string]any // Additional fields to inject into request body
	preferResponses bool           // Prefer /responses for OpenAI-native models selected via the factory.
}

type Option func(*Provider)

const defaultRequestTimeout = 120 * time.Second

func WithMaxTokensField(maxTokensField string) Option {
	return func(p *Provider) {
		p.maxTokensField = maxTokensField
	}
}

func WithRequestTimeout(timeout time.Duration) Option {
	return func(p *Provider) {
		if timeout > 0 {
			p.httpClient.Timeout = timeout
		}
	}
}

func WithExtraBody(extraBody map[string]any) Option {
	return func(p *Provider) {
		p.extraBody = extraBody
	}
}

// WithResponsesPreferred marks this provider instance as OpenAI-native so it
// prefers /responses even after the factory strips the outer "openai/" prefix.
func WithResponsesPreferred() Option {
	return func(p *Provider) {
		p.preferResponses = true
	}
}

func NewProvider(apiKey, apiBase, proxy string, opts ...Option) *Provider {
	client := &http.Client{
		Timeout: defaultRequestTimeout,
	}

	if proxy != "" {
		parsed, err := url.Parse(proxy)
		if err == nil {
			client.Transport = &http.Transport{
				Proxy: http.ProxyURL(parsed),
			}
		} else {
			log.Printf("openai_compat: invalid proxy URL %q: %v", proxy, err)
		}
	}

	p := &Provider{
		apiKey:     apiKey,
		apiBase:    strings.TrimRight(apiBase, "/"),
		httpClient: client,
	}

	for _, opt := range opts {
		if opt != nil {
			opt(p)
		}
	}

	return p
}

func NewProviderWithMaxTokensField(apiKey, apiBase, proxy, maxTokensField string) *Provider {
	return NewProvider(apiKey, apiBase, proxy, WithMaxTokensField(maxTokensField))
}

func NewProviderWithMaxTokensFieldAndTimeout(
	apiKey, apiBase, proxy, maxTokensField string,
	requestTimeoutSeconds int,
) *Provider {
	return NewProvider(
		apiKey,
		apiBase,
		proxy,
		WithMaxTokensField(maxTokensField),
		WithRequestTimeout(time.Duration(requestTimeoutSeconds)*time.Second),
	)
}

func (p *Provider) Chat(
	ctx context.Context,
	messages []Message,
	tools []ToolDefinition,
	model string,
	options map[string]any,
) (*LLMResponse, error) {
	if p.apiBase == "" {
		return nil, fmt.Errorf("API base not configured")
	}

	normalizedModel := normalizeModel(model, p.apiBase)
	// Keep the legacy chat/completions path for histories that already depend on
	// reasoning_content, because Responses represents reasoning state differently.
	if shouldPreferResponses(model, normalizedModel, p.preferResponses) && !hasReasoningContentHistory(messages) {
		out, err := p.chatResponses(ctx, messages, tools, normalizedModel, options)
		if err == nil {
			return out, nil
		}
		if ctx.Err() != nil {
			return nil, err
		}
		log.Printf("openai_compat: /responses failed for %q, falling back to /chat/completions: %v", normalizedModel, err)

		fallbackOut, fallbackErr := p.chatCompletions(ctx, messages, tools, normalizedModel, options)
		if fallbackErr != nil {
			return nil, fmt.Errorf("responses request failed: %w; fallback chat/completions failed: %v", err, fallbackErr)
		}
		return fallbackOut, nil
	}

	return p.chatCompletions(ctx, messages, tools, normalizedModel, options)
}

// ChatStream implements streaming via OpenAI-compatible SSE (stream: true).
// onChunk receives the accumulated text so far on each text delta.
func (p *Provider) ChatStream(
	ctx context.Context,
	messages []Message,
	tools []ToolDefinition,
	model string,
	options map[string]any,
	onChunk func(accumulated string),
) (*LLMResponse, error) {
	if p.apiBase == "" {
		return nil, fmt.Errorf("API base not configured")
	}

	requestBody := buildChatCompletionsRequestBody(messages, tools, normalizeModel(model, p.apiBase), options, p.maxTokensField, p.apiBase)
	for k, v := range p.extraBody {
		requestBody[k] = v
	}
	requestBody["stream"] = true

	jsonData, err := json.Marshal(requestBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", p.apiBase+"/chat/completions", bytes.NewReader(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	streamClient := &http.Client{Transport: p.httpClient.Transport}
	resp, err := streamClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API request failed:\n  Status: %d\n  Body:   %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return parseStreamResponse(ctx, resp.Body, onChunk)
}

func (p *Provider) chatCompletions(
	ctx context.Context,
	messages []Message,
	tools []ToolDefinition,
	model string,
	options map[string]any,
) (*LLMResponse, error) {
	requestBody := buildChatCompletionsRequestBody(messages, tools, model, options, p.maxTokensField, p.apiBase)
	for k, v := range p.extraBody {
		requestBody[k] = v
	}
	return p.doRequest(ctx, "/chat/completions", requestBody, parseResponse)
}

func (p *Provider) chatResponses(
	ctx context.Context,
	messages []Message,
	tools []ToolDefinition,
	model string,
	options map[string]any,
) (*LLMResponse, error) {
	requestBody, err := buildResponsesRequestBody(messages, tools, model, options, p.apiBase, p.extraBody)
	if err != nil {
		return nil, err
	}
	return p.doRequest(ctx, "/responses", requestBody, parseResponsesResponse)
}

func buildChatCompletionsRequestBody(
	messages []Message,
	tools []ToolDefinition,
	model string,
	options map[string]any,
	maxTokensField string,
	apiBase string,
) map[string]any {
	requestBody := map[string]any{
		"model":    model,
		"messages": serializeMessages(messages),
	}

	// When fallback uses a different provider (e.g. DeepSeek), that provider must not inject web_search_preview.
	nativeSearch, _ := options["native_search"].(bool)
	nativeSearch = nativeSearch && isNativeSearchHost(apiBase)
	if len(tools) > 0 || nativeSearch {
		requestBody["tools"] = buildToolsList(tools, nativeSearch)
		requestBody["tool_choice"] = "auto"
	}

	if maxTokens, ok := asInt(options["max_tokens"]); ok {
		// Use configured maxTokensField if specified, otherwise fallback to model-based detection.
		fieldName := maxTokensField
		if fieldName == "" {
			// Fallback: detect from model name for backward compatibility.
			lowerModel := strings.ToLower(model)
			if strings.Contains(lowerModel, "glm") || strings.Contains(lowerModel, "o1") ||
				strings.Contains(lowerModel, "gpt-5") {
				fieldName = "max_completion_tokens"
			} else {
				fieldName = "max_tokens"
			}
		}
		requestBody[fieldName] = maxTokens
	}

	if temperature, ok := requestTemperature(model, options); ok {
		requestBody["temperature"] = temperature
	}

	// Prompt caching: pass a stable cache key so OpenAI can bucket requests
	// with the same key and reuse prefix KV cache across calls.
	// The key is typically the agent ID - stable per agent, shared across requests.
	// See: https://platform.openai.com/docs/guides/prompt-caching
	// Prompt caching is only supported by OpenAI-native endpoints.
	// Gemini and other providers reject unknown fields, so skip for non-OpenAI APIs.
	if cacheKey, ok := options["prompt_cache_key"].(string); ok && cacheKey != "" {
		if supportsPromptCacheKey(apiBase) {
			requestBody["prompt_cache_key"] = cacheKey
		}
	}

	return requestBody
}

// buildResponsesRequestBody keeps the option handling close to the legacy
// chat/completions path so the new route can reuse the existing compatibility
// knobs with minimal behavioral drift.
func buildResponsesRequestBody(
	messages []Message,
	tools []ToolDefinition,
	model string,
	options map[string]any,
	apiBase string,
	extraBody map[string]any,
) (map[string]any, error) {
	input, err := buildResponsesInput(messages)
	if err != nil {
		return nil, err
	}

	requestBody := map[string]any{
		"model": model,
		"input": input,
	}

	nativeSearch, _ := options["native_search"].(bool)
	nativeSearch = nativeSearch && isNativeSearchHost(apiBase)
	if len(tools) > 0 || nativeSearch {
		requestBody["tools"] = serializeResponseTools(tools, nativeSearch)
		requestBody["tool_choice"] = "auto"
	}

	if maxTokens, ok := asInt(options["max_tokens"]); ok {
		requestBody["max_output_tokens"] = maxTokens
	}

	if temperature, ok := requestTemperature(model, options); ok {
		requestBody["temperature"] = temperature
	}

	// Prompt caching follows the same compatibility rule as chat/completions:
	// send the key only to endpoints that are expected to understand it.
	if cacheKey, ok := options["prompt_cache_key"].(string); ok && cacheKey != "" {
		if supportsPromptCacheKey(apiBase) {
			requestBody["prompt_cache_key"] = cacheKey
		}
	}

	for k, v := range extraBody {
		requestBody[k] = v
	}

	return requestBody, nil
}

// buildResponsesInput translates the existing conversation format into the
// item-based Responses input shape while preserving tool call history.
func buildResponsesInput(messages []Message) ([]any, error) {
	input := make([]any, 0, len(messages))

	for _, m := range messages {
		switch m.Role {
		case "system", "user":
			input = append(input, map[string]any{
				"type":    "message",
				"role":    m.Role,
				"content": serializeResponsesMessageContent(m),
			})
		case "assistant":
			if strings.TrimSpace(m.Content) != "" || strings.TrimSpace(m.ReasoningContent) != "" || len(m.Media) > 0 || len(m.ToolCalls) == 0 {
				input = append(input, map[string]any{
					"type":    "message",
					"role":    m.Role,
					"content": serializeResponsesMessageContent(m),
				})
			}

			for _, tc := range m.ToolCalls {
				name, args, ok := resolveResponseToolCall(tc)
				if !ok {
					log.Printf("openai_compat: skipping invalid assistant tool call in responses history: id=%q", tc.ID)
					continue
				}
				input = append(input, map[string]any{
					"type":      "function_call",
					"call_id":   tc.ID,
					"name":      name,
					"arguments": args,
				})
			}
		case "tool":
			if strings.TrimSpace(m.ToolCallID) == "" {
				return nil, fmt.Errorf("tool message missing tool_call_id")
			}
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": m.ToolCallID,
				"output":  m.Content,
			})
		default:
			return nil, fmt.Errorf("unsupported message role: %s", m.Role)
		}
	}

	return input, nil
}

// serializeResponsesMessageContent converts plain text and inline image data
// into the content format expected by the Responses API.
func serializeResponsesMessageContent(m Message) any {
	effectiveText := m.Content
	if effectiveText == "" {
		effectiveText = m.ReasoningContent
	}

	if len(m.Media) == 0 {
		return effectiveText
	}

	parts := make([]map[string]any, 0, 1+len(m.Media))
	if effectiveText != "" {
		parts = append(parts, map[string]any{
			"type": "input_text",
			"text": effectiveText,
		})
	}

	for _, mediaURL := range m.Media {
		if strings.HasPrefix(mediaURL, "data:image/") {
			parts = append(parts, map[string]any{
				"type":      "input_image",
				"image_url": mediaURL,
			})
		}
	}

	if len(parts) == 0 {
		return effectiveText
	}

	return parts
}

// serializeResponseTools maps the existing OpenAI-compatible tool schema to the
// smaller function-tool shape accepted by the Responses API.
func serializeResponseTools(tools []ToolDefinition, nativeSearch bool) []map[string]any {
	result := make([]map[string]any, 0, len(tools)+1)
	for _, tool := range tools {
		if nativeSearch && strings.EqualFold(tool.Function.Name, "web_search") {
			continue
		}
		if tool.Type != "" && tool.Type != "function" {
			continue
		}

		entry := map[string]any{
			"type":       "function",
			"name":       tool.Function.Name,
			"parameters": tool.Function.Parameters,
		}
		if entry["parameters"] == nil {
			entry["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		if tool.Function.Description != "" {
			entry["description"] = tool.Function.Description
		}

		result = append(result, entry)
	}
	if nativeSearch {
		result = append(result, map[string]any{"type": "web_search_preview"})
	}
	return result
}

// resolveResponseToolCall rebuilds the assistant-side tool call record into the
// stringified argument form required by Responses conversation history.
func resolveResponseToolCall(tc ToolCall) (name string, arguments string, ok bool) {
	name = tc.Name
	if name == "" && tc.Function != nil {
		name = tc.Function.Name
	}
	if name == "" {
		return "", "", false
	}

	if len(tc.Arguments) > 0 {
		argsJSON, err := json.Marshal(tc.Arguments)
		if err != nil {
			return "", "", false
		}
		return name, string(argsJSON), true
	}

	if tc.Function != nil && tc.Function.Arguments != "" {
		return name, tc.Function.Arguments, true
	}

	return name, "{}", true
}

// parseStreamResponse parses an OpenAI-compatible SSE stream.
func parseStreamResponse(
	ctx context.Context,
	reader io.Reader,
	onChunk func(accumulated string),
) (*LLMResponse, error) {
	var textContent strings.Builder
	var finishReason string
	var usage *UsageInfo

	type toolAccum struct {
		id       string
		name     string
		argsJSON strings.Builder
	}
	activeTools := map[int]*toolAccum{}

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function *struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *UsageInfo `json:"usage"`
		}

		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		if len(chunk.Choices) == 0 {
			continue
		}

		choice := chunk.Choices[0]
		if choice.Delta.Content != "" {
			textContent.WriteString(choice.Delta.Content)
			if onChunk != nil {
				onChunk(textContent.String())
			}
		}
		for _, tc := range choice.Delta.ToolCalls {
			acc, ok := activeTools[tc.Index]
			if !ok {
				acc = &toolAccum{}
				activeTools[tc.Index] = acc
			}
			if tc.ID != "" {
				acc.id = tc.ID
			}
			if tc.Function != nil {
				if tc.Function.Name != "" {
					acc.name = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					acc.argsJSON.WriteString(tc.Function.Arguments)
				}
			}
		}
		if choice.FinishReason != nil {
			finishReason = *choice.FinishReason
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("streaming read error: %w", err)
	}

	var toolCalls []ToolCall
	for i := 0; i < len(activeTools); i++ {
		acc, ok := activeTools[i]
		if !ok {
			continue
		}
		args := make(map[string]any)
		raw := acc.argsJSON.String()
		if raw != "" {
			if err := json.Unmarshal([]byte(raw), &args); err != nil {
				log.Printf("openai_compat stream: failed to decode tool call arguments for %q: %v", acc.name, err)
				args["raw"] = raw
			}
		}
		toolCalls = append(toolCalls, ToolCall{ID: acc.id, Name: acc.name, Arguments: args})
	}

	if finishReason == "" {
		finishReason = "stop"
	}

	return &LLMResponse{
		Content:      textContent.String(),
		ToolCalls:    toolCalls,
		FinishReason: finishReason,
		Usage:        usage,
	}, nil
}

func requestTemperature(model string, options map[string]any) (float64, bool) {
	temperature, ok := asFloat(options["temperature"])
	if !ok {
		return 0, false
	}

	lowerModel := strings.ToLower(model)
	if strings.Contains(lowerModel, "kimi") && strings.Contains(lowerModel, "k2") {
		return 1.0, true
	}
	return temperature, true
}

// shouldPreferResponses centralizes the opt-in rule so OpenAI-native configs
// and gpt-5 models can try /responses first while other compat backends keep
// their existing chat/completions behavior even when model IDs are namespaced.
func shouldPreferResponses(rawModel, normalizedModel string, preferOpenAIModels bool) bool {
	rawModel = strings.ToLower(strings.TrimSpace(rawModel))
	normalizedModel = strings.ToLower(strings.TrimSpace(normalizedModel))

	if preferOpenAIModels && isOpenAINativeResponsesModel(normalizedModel) {
		return true
	}
	return strings.HasPrefix(rawModel, "gpt-5") ||
		strings.HasPrefix(normalizedModel, "gpt-5")
}

func isOpenAINativeResponsesModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	return strings.HasPrefix(model, "gpt-") ||
		strings.HasPrefix(model, "o1") ||
		strings.HasPrefix(model, "o3") ||
		strings.HasPrefix(model, "o4")
}

// hasReasoningContentHistory detects histories that already rely on the legacy
// reasoning_content field so they can stay on the older wire format.
func hasReasoningContentHistory(messages []Message) bool {
	for _, message := range messages {
		if strings.TrimSpace(message.ReasoningContent) != "" {
			return true
		}
	}
	return false
}

func (p *Provider) doRequest(
	ctx context.Context,
	path string,
	requestBody map[string]any,
	parse func(io.Reader) (*LLMResponse, error),
) (*LLMResponse, error) {
	jsonData, err := json.Marshal(requestBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", p.apiBase+path, bytes.NewReader(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	contentType := resp.Header.Get("Content-Type")
	// Non-200: read a prefix to tell HTML error page apart from JSON error body.
	if resp.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 256))
		if readErr != nil {
			return nil, fmt.Errorf("failed to read response: %w", readErr)
		}
		if looksLikeHTML(body, contentType) {
			return nil, wrapHTMLResponseError(resp.StatusCode, body, contentType, p.apiBase)
		}
		return nil, fmt.Errorf(
			"API request failed:\n  Status: %d\n  Body:   %s",
			resp.StatusCode,
			responsePreview(body, 128),
		)
	}

	// Peek without consuming so the full stream reaches the JSON decoder.
	reader := bufio.NewReader(resp.Body)
	prefix, err := reader.Peek(256) // io.EOF/ErrBufferFull are normal; only real errors abort
	if err != nil && err != io.EOF && err != bufio.ErrBufferFull {
		return nil, fmt.Errorf("failed to inspect response: %w", err)
	}
	if looksLikeHTML(prefix, contentType) {
		return nil, wrapHTMLResponseError(resp.StatusCode, prefix, contentType, p.apiBase)
	}

	out, err := parse(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to parse JSON response: %w", err)
	}

	return out, nil
}

func wrapHTMLResponseError(statusCode int, body []byte, contentType, apiBase string) error {
	respPreview := responsePreview(body, 128)
	return fmt.Errorf(
		"API request failed: %s returned HTML instead of JSON (content-type: %s); check api_base or proxy configuration.\n  Status: %d\n  Body:   %s",
		apiBase,
		contentType,
		statusCode,
		respPreview,
	)
}

func looksLikeHTML(body []byte, contentType string) bool {
	contentType = strings.ToLower(strings.TrimSpace(contentType))
	if strings.Contains(contentType, "text/html") || strings.Contains(contentType, "application/xhtml+xml") {
		return true
	}
	prefix := bytes.ToLower(leadingTrimmedPrefix(body, 128))
	return bytes.HasPrefix(prefix, []byte("<!doctype html")) ||
		bytes.HasPrefix(prefix, []byte("<html")) ||
		bytes.HasPrefix(prefix, []byte("<head")) ||
		bytes.HasPrefix(prefix, []byte("<body"))
}

func leadingTrimmedPrefix(body []byte, maxLen int) []byte {
	i := 0
	for i < len(body) {
		switch body[i] {
		case ' ', '\t', '\n', '\r', '\f', '\v':
			i++
		default:
			end := i + maxLen
			if end > len(body) {
				end = len(body)
			}
			return body[i:end]
		}
	}
	return nil
}

func responsePreview(body []byte, maxLen int) string {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return "<empty>"
	}
	if len(trimmed) <= maxLen {
		return string(trimmed)
	}
	return string(trimmed[:maxLen]) + "..."
}

func parseResponse(body io.Reader) (*LLMResponse, error) {
	var apiResponse struct {
		Choices []struct {
			Message struct {
				Content          string            `json:"content"`
				ReasoningContent string            `json:"reasoning_content"`
				Reasoning        string            `json:"reasoning"`
				ReasoningDetails []ReasoningDetail `json:"reasoning_details"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function *struct {
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					} `json:"function"`
					ExtraContent *struct {
						Google *struct {
							ThoughtSignature string `json:"thought_signature"`
						} `json:"google"`
					} `json:"extra_content"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *UsageInfo `json:"usage"`
	}

	if err := json.NewDecoder(body).Decode(&apiResponse); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	if len(apiResponse.Choices) == 0 {
		return &LLMResponse{
			Content:      "",
			FinishReason: "stop",
		}, nil
	}

	choice := apiResponse.Choices[0]
	toolCalls := make([]ToolCall, 0, len(choice.Message.ToolCalls))
	for _, tc := range choice.Message.ToolCalls {
		arguments := make(map[string]any)
		name := ""

		// Extract thought_signature from Gemini/Google-specific extra content
		thoughtSignature := ""
		if tc.ExtraContent != nil && tc.ExtraContent.Google != nil {
			thoughtSignature = tc.ExtraContent.Google.ThoughtSignature
		}

		if tc.Function != nil {
			name = tc.Function.Name
			if len(tc.Function.Arguments) > 0 {
				raw := bytes.TrimSpace(tc.Function.Arguments)
				if len(raw) > 0 {
					if raw[0] == '{' || raw[0] == '[' {
						if err := json.Unmarshal(raw, &arguments); err != nil {
							log.Printf("openai_compat: failed to decode tool call arguments for %q: %v", name, err)
							arguments["raw"] = string(raw)
						}
					} else {
						var argString string
						if err := json.Unmarshal(raw, &argString); err != nil {
							log.Printf("openai_compat: failed to decode tool call arguments for %q: %v", name, err)
							arguments["raw"] = string(raw)
						} else if argString != "" {
							if err := json.Unmarshal([]byte(argString), &arguments); err != nil {
								log.Printf("openai_compat: failed to decode tool call arguments for %q: %v", name, err)
								arguments["raw"] = argString
							}
						}
					}
				}
			}
		}

		// Build ToolCall with ExtraContent for Gemini 3 thought_signature persistence
		toolCall := ToolCall{
			ID:               tc.ID,
			Name:             name,
			Arguments:        arguments,
			ThoughtSignature: thoughtSignature,
		}

		if thoughtSignature != "" {
			toolCall.ExtraContent = &ExtraContent{
				Google: &GoogleExtra{
					ThoughtSignature: thoughtSignature,
				},
			}
		}

		toolCalls = append(toolCalls, toolCall)
	}

	return &LLMResponse{
		Content:          choice.Message.Content,
		ReasoningContent: choice.Message.ReasoningContent,
		Reasoning:        choice.Message.Reasoning,
		ReasoningDetails: choice.Message.ReasoningDetails,
		ToolCalls:        toolCalls,
		FinishReason:     choice.FinishReason,
		Usage:            apiResponse.Usage,
	}, nil
}

// parseResponsesResponse maps the Responses API envelope back to the legacy
// provider response shape used by the rest of the codebase.
func parseResponsesResponse(body io.Reader) (*LLMResponse, error) {
	var apiResponse struct {
		Status string `json:"status"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
		Output []struct {
			ID        string `json:"id"`
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Summary   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"summary"`
			Content []struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Refusal string `json:"refusal"`
			} `json:"content"`
		} `json:"output"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}

	if err := json.NewDecoder(body).Decode(&apiResponse); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	status := strings.TrimSpace(apiResponse.Status)
	switch status {
	case "completed", "incomplete":
		if len(apiResponse.Output) == 0 {
			return nil, errors.New("openai responses returned terminal status with empty output")
		}
	case "failed":
		if apiResponse.Error != nil {
			if msg := strings.TrimSpace(apiResponse.Error.Message); msg != "" {
				return nil, errors.New(msg)
			}
		}
		return nil, errors.New("openai responses request failed")
	default:
		return nil, fmt.Errorf("openai responses returned unexpected or non-terminal status: %q", status)
	}

	var content strings.Builder
	var reasoning strings.Builder
	var reasoningContent strings.Builder
	reasoningDetails := make([]ReasoningDetail, 0)
	toolCalls := make([]ToolCall, 0)
	for _, item := range apiResponse.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				if part.Text != "" {
					content.WriteString(part.Text)
					continue
				}
				if part.Refusal != "" {
					content.WriteString(part.Refusal)
				}
			}
		case "reasoning":
			for _, part := range item.Summary {
				if part.Text == "" {
					continue
				}
				if reasoning.Len() > 0 {
					reasoning.WriteString("\n")
				}
				reasoning.WriteString(part.Text)
				reasoningDetails = append(reasoningDetails, ReasoningDetail{
					Format: "text",
					Index:  len(reasoningDetails),
					Type:   part.Type,
					Text:   part.Text,
				})
			}
			for _, part := range item.Content {
				if part.Text == "" {
					continue
				}
				if reasoningContent.Len() > 0 {
					reasoningContent.WriteString("\n")
				}
				reasoningContent.WriteString(part.Text)
				reasoningDetails = append(reasoningDetails, ReasoningDetail{
					Format: "text",
					Index:  len(reasoningDetails),
					Type:   part.Type,
					Text:   part.Text,
				})
			}
		case "function_call":
			arguments := make(map[string]any)
			if item.Arguments != "" {
				if err := json.Unmarshal([]byte(item.Arguments), &arguments); err != nil {
					log.Printf("openai_compat: failed to decode responses tool call arguments for %q: %v", item.Name, err)
					arguments["raw"] = item.Arguments
				}
			}

			toolCalls = append(toolCalls, ToolCall{
				ID:        firstNonEmpty(item.CallID, item.ID),
				Name:      item.Name,
				Arguments: arguments,
			})
		}
	}

	finishReason := "stop"
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	} else if status == "incomplete" {
		finishReason = "length"
		if apiResponse.IncompleteDetails != nil && apiResponse.IncompleteDetails.Reason != "" && apiResponse.IncompleteDetails.Reason != "max_output_tokens" {
			finishReason = apiResponse.IncompleteDetails.Reason
		}
	} else if status == "failed" {
		finishReason = "error"
	}

	var usage *UsageInfo
	if apiResponse.Usage != nil {
		usage = &UsageInfo{
			PromptTokens:     apiResponse.Usage.InputTokens,
			CompletionTokens: apiResponse.Usage.OutputTokens,
			TotalTokens:      apiResponse.Usage.TotalTokens,
		}
	}

	return &LLMResponse{
		Content:          content.String(),
		ReasoningContent: reasoningContent.String(),
		Reasoning:        reasoning.String(),
		ReasoningDetails: reasoningDetails,
		ToolCalls:        toolCalls,
		FinishReason:     finishReason,
		Usage:            usage,
	}, nil
}

// firstNonEmpty prefers call_id but falls back to the raw item id when the
// response item omits it.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// openaiMessage is the wire-format message for OpenAI-compatible APIs.
// It mirrors protocoltypes.Message but omits SystemParts, which is an
// internal field that would be unknown to third-party endpoints.
type openaiMessage struct {
	Role             string     `json:"role"`
	Content          string     `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
}

// serializeMessages converts internal Message structs to the OpenAI wire format.
// - Strips SystemParts (unknown to third-party endpoints)
// - Converts messages with Media to multipart content format (text + image_url parts)
// - Preserves ToolCallID, ToolCalls, and ReasoningContent for all messages
func serializeMessages(messages []Message) []any {
	out := make([]any, 0, len(messages))
	for _, m := range messages {
		if len(m.Media) == 0 {
			out = append(out, openaiMessage{
				Role:             m.Role,
				Content:          m.Content,
				ReasoningContent: m.ReasoningContent,
				ToolCalls:        m.ToolCalls,
				ToolCallID:       m.ToolCallID,
			})
			continue
		}

		// Multipart content format for messages with media
		parts := make([]map[string]any, 0, 1+len(m.Media))
		if m.Content != "" {
			parts = append(parts, map[string]any{
				"type": "text",
				"text": m.Content,
			})
		}
		for _, mediaURL := range m.Media {
			if strings.HasPrefix(mediaURL, "data:image/") {
				parts = append(parts, map[string]any{
					"type": "image_url",
					"image_url": map[string]any{
						"url": mediaURL,
					},
				})
			}
		}

		msg := map[string]any{
			"role":    m.Role,
			"content": parts,
		}
		if m.ToolCallID != "" {
			msg["tool_call_id"] = m.ToolCallID
		}
		if len(m.ToolCalls) > 0 {
			msg["tool_calls"] = m.ToolCalls
		}
		if m.ReasoningContent != "" {
			msg["reasoning_content"] = m.ReasoningContent
		}
		out = append(out, msg)
	}
	return out
}

func normalizeModel(model, apiBase string) string {
	before, after, ok := strings.Cut(model, "/")
	if !ok {
		return model
	}

	if strings.Contains(strings.ToLower(apiBase), "openrouter.ai") {
		return model
	}

	prefix := strings.ToLower(before)
	switch prefix {
	case "litellm", "moonshot", "nvidia", "groq", "ollama", "deepseek", "google",
		"openrouter", "zhipu", "mistral", "vivgrid", "minimax", "novita":
		return after
	default:
		return model
	}
}

func buildToolsList(tools []ToolDefinition, nativeSearch bool) []any {
	result := make([]any, 0, len(tools)+1)
	for _, t := range tools {
		if nativeSearch && strings.EqualFold(t.Function.Name, "web_search") {
			continue
		}
		result = append(result, t)
	}
	if nativeSearch {
		result = append(result, map[string]any{"type": "web_search_preview"})
	}
	return result
}

func (p *Provider) SupportsNativeSearch() bool {
	return isNativeSearchHost(p.apiBase)
}

func isNativeSearchHost(apiBase string) bool {
	u, err := url.Parse(apiBase)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "api.openai.com" || strings.HasSuffix(host, ".openai.azure.com")
}

func supportsPromptCacheKey(apiBase string) bool {
	u, err := url.Parse(apiBase)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "api.openai.com" || strings.HasSuffix(host, ".openai.azure.com")
}

func asInt(v any) (int, bool) {
	switch val := v.(type) {
	case int:
		return val, true
	case int64:
		return int(val), true
	case float64:
		return int(val), true
	case float32:
		return int(val), true
	default:
		return 0, false
	}
}

func asFloat(v any) (float64, bool) {
	switch val := v.(type) {
	case float64:
		return val, true
	case float32:
		return float64(val), true
	case int:
		return float64(val), true
	case int64:
		return float64(val), true
	default:
		return 0, false
	}
}
