package providers

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestCodexWSLive talks to the real Codex backend. It is skipped unless
// PICOCLAW_CODEX_WS_LIVE=1, because it needs working OpenAI credentials in the
// picoclaw auth store and burns real quota.
//
//	PICOCLAW_HOME=<dir with auth.json> PICOCLAW_CODEX_WS_LIVE=1 \
//	  CODEX_WS_MODEL=gpt-6-astra go test -tags goolm,stdjson ./pkg/providers/ \
//	  -run TestCodexWSLive -v
func TestCodexWSLive(t *testing.T) {
	if os.Getenv("PICOCLAW_CODEX_WS_LIVE") != "1" {
		t.Skip("set PICOCLAW_CODEX_WS_LIVE=1 to run against the real Codex backend")
	}

	model := os.Getenv("CODEX_WS_MODEL")
	if model == "" {
		model = codexDefaultModel
	}
	resolved, reason := resolveCodexModel(model)
	t.Logf("model %q -> %q (reason: %q)", model, resolved, reason)

	p := NewCodexWSProvider("", "")
	defer p.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	options := map[string]any{"session_key": "live-test"}
	if level := os.Getenv("CODEX_WS_THINKING"); level != "" {
		options["thinking_level"] = level
	}

	prompt := os.Getenv("CODEX_WS_PROMPT")
	if prompt == "" {
		prompt = "Reply with exactly: pong"
	}
	messages := []Message{
		{Role: "system", Content: "You are a terse assistant."},
		{Role: "user", Content: prompt},
	}

	resp, err := p.ChatStream(ctx, messages, nil, resolved, options, nil)
	if err != nil {
		t.Fatalf("ChatStream(%q) error: %v", resolved, err)
	}
	t.Logf("content=%q tool_calls=%d usage=%+v", resp.Content, len(resp.ToolCalls), resp.Usage)
	if strings.TrimSpace(resp.Content) == "" && len(resp.ToolCalls) == 0 {
		t.Fatalf("empty response from %q", resolved)
	}
}

// TestCodexWSLiveToolCall mirrors a real agent turn: instructions, a tool
// catalog, a reasoning level, then a second turn feeding the tool result back.
func TestCodexWSLiveToolCall(t *testing.T) {
	if os.Getenv("PICOCLAW_CODEX_WS_LIVE") != "1" {
		t.Skip("set PICOCLAW_CODEX_WS_LIVE=1 to run against the real Codex backend")
	}

	model := os.Getenv("CODEX_WS_MODEL")
	if model == "" {
		model = codexDefaultModel
	}
	resolved, _ := resolveCodexModel(model)

	p := NewCodexWSProvider("", "")
	defer p.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	level := os.Getenv("CODEX_WS_THINKING")
	if level == "" {
		level = "medium"
	}
	options := map[string]any{"session_key": "live-tool-test", "thinking_level": level}

	tools := []ToolDefinition{{
		Type: "function",
		Function: ToolFunctionDefinition{
			Name:        "get_weather",
			Description: "Get the current weather for a city.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{"city": map[string]any{"type": "string"}},
				"required":   []string{"city"},
			},
		},
	}}

	messages := make([]Message, 0, 4)
	messages = append(messages,
		Message{Role: "system", Content: "You are a terse assistant. Use tools when they apply."},
		Message{Role: "user", Content: "What is the weather in Lisbon? Call the tool."},
	)

	resp, err := p.ChatStream(ctx, messages, tools, resolved, options, nil)
	if err != nil {
		t.Fatalf("turn 1 ChatStream(%q) error: %v", resolved, err)
	}
	t.Logf("turn 1: content=%q tool_calls=%d", resp.Content, len(resp.ToolCalls))
	if len(resp.ToolCalls) == 0 {
		t.Fatalf("turn 1: expected a tool call from %q, got content %q", resolved, resp.Content)
	}
	call := resp.ToolCalls[0]
	t.Logf("turn 1 call: id=%q name=%q args=%v fn=%+v", call.ID, call.Name, call.Arguments, call.Function)

	messages = append(messages,
		Message{Role: "assistant", ToolCalls: resp.ToolCalls},
		Message{Role: "tool", ToolCallID: call.ID, Content: `{"temp_c": 19, "sky": "clear"}`},
	)

	resp2, err := p.ChatStream(ctx, messages, tools, resolved, options, nil)
	if err != nil {
		t.Fatalf("turn 2 ChatStream(%q) error: %v", resolved, err)
	}
	t.Logf("turn 2: content=%q tool_calls=%d", resp2.Content, len(resp2.ToolCalls))
	if strings.TrimSpace(resp2.Content) == "" && len(resp2.ToolCalls) == 0 {
		t.Fatalf("turn 2: empty response from %q", resolved)
	}
}

// TestCodexWSLiveStreaming checks that text actually arrives as deltas (the
// chat UI depends on it) and that native web_search is accepted by the model.
func TestCodexWSLiveStreaming(t *testing.T) {
	if os.Getenv("PICOCLAW_CODEX_WS_LIVE") != "1" {
		t.Skip("set PICOCLAW_CODEX_WS_LIVE=1 to run against the real Codex backend")
	}

	model := os.Getenv("CODEX_WS_MODEL")
	if model == "" {
		model = codexDefaultModel
	}
	resolved, _ := resolveCodexModel(model)

	p := NewCodexWSProvider("", "")
	defer p.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	chunks := 0
	onChunk := func(string) { chunks++ }

	messages := []Message{
		{Role: "system", Content: "You are a terse assistant."},
		{Role: "user", Content: "Count from 1 to 10, space separated, nothing else."},
	}

	resp, err := p.ChatStream(ctx, messages, nil, resolved,
		map[string]any{"session_key": "live-stream-test"}, onChunk)
	if err != nil {
		t.Fatalf("ChatStream(%q) error: %v", resolved, err)
	}
	t.Logf("chunks=%d content=%q usage=%+v", chunks, resp.Content, resp.Usage)
	if chunks == 0 {
		t.Errorf("no streaming chunks from %q: the chat UI would show nothing until the turn ends", resolved)
	}
	if resp.Usage == nil {
		t.Errorf("usage missing from %q response: context-usage reporting is blind", resolved)
	}
}

// TestCodexWSLiveNativeSearch exercises the server-side web_search tool, which
// is injected as a bare {"type":"web_search"} entry alongside the functions.
func TestCodexWSLiveNativeSearch(t *testing.T) {
	if os.Getenv("PICOCLAW_CODEX_WS_LIVE") != "1" {
		t.Skip("set PICOCLAW_CODEX_WS_LIVE=1 to run against the real Codex backend")
	}

	model := os.Getenv("CODEX_WS_MODEL")
	if model == "" {
		model = codexDefaultModel
	}
	resolved, _ := resolveCodexModel(model)

	p := NewCodexWSProvider("", "")
	defer p.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	tools := []ToolDefinition{{
		Type: "function",
		Function: ToolFunctionDefinition{
			Name:        "noop",
			Description: "Does nothing.",
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
		},
	}}

	resp, err := p.ChatStream(ctx, messages2(), tools, resolved, map[string]any{
		"session_key":   "live-search-test",
		"native_search": true,
	}, nil)
	if err != nil {
		t.Fatalf("ChatStream(%q) with native search error: %v", resolved, err)
	}
	t.Logf("content=%q tool_calls=%d", resp.Content, len(resp.ToolCalls))
}

func messages2() []Message {
	return []Message{
		{Role: "system", Content: "You are a terse assistant."},
		{Role: "user", Content: "Reply with exactly: ok"},
	}
}
