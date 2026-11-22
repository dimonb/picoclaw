package cliprovider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/isolation"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers/effort"
)

// ClaudeCliProvider implements LLMProvider using the claude CLI as a subprocess.
//
// Prompt caching: the CLI places its cache breakpoints on the latest messages
// only when it continues a stored session. A fresh run that receives the whole
// history as one stdin prompt can cache only the static system part, so the
// provider remembers which CLI session already holds each conversation
// (options["session_key"]) and, while picoclaw's history still extends what
// that session has seen, resumes it with only the new messages. Any divergence
// (summarization, restart, edited history, failure) falls back to a fresh
// session that receives the full history.
type ClaudeCliProvider struct {
	command   string
	workspace string

	mu       sync.Mutex
	sessions map[string]*claudeCliSession

	newSessionID func() string
}

// claudeCliSession records the conversation prefix a CLI session holds.
type claudeCliSession struct {
	id     string
	count  int      // conversation (non-system) messages the session received
	digest [32]byte // fingerprint of those messages
}

// claudeCliMaxResumeContext bounds the average per-call prompt size of a
// resumed session. The CLI session also accumulates its own tool traffic that
// picoclaw's summarization cannot see; past this size the next call restarts
// from picoclaw's (possibly summarized) history instead.
const claudeCliMaxResumeContext = 120_000

// NewClaudeCliProvider creates a new Claude CLI provider.
func NewClaudeCliProvider(workspace string) *ClaudeCliProvider {
	return &ClaudeCliProvider{
		command:      "claude",
		workspace:    workspace,
		sessions:     make(map[string]*claudeCliSession),
		newSessionID: uuid.NewString,
	}
}

// SupportsThinking implements providers.ThinkingCapable: thinking_level is
// forwarded as the CLI --effort flag.
func (p *ClaudeCliProvider) SupportsThinking() bool { return true }

// Chat implements LLMProvider.Chat by executing the claude CLI.
func (p *ClaudeCliProvider) Chat(
	ctx context.Context, messages []Message, tools []ToolDefinition, model string, options map[string]any,
) (*LLMResponse, error) {
	systemPrompt := p.buildSystemPrompt(messages, tools)
	conv := conversationMessages(messages)
	sessionKey, _ := options["session_key"].(string)

	if prev := p.takeSession(sessionKey); prev != nil {
		if delta, ok := resumeDelta(prev, conv); ok {
			resp, raw, err := p.run(ctx, systemPrompt, p.messagesToPrompt(delta), model, options,
				"--resume", prev.id)
			if err == nil {
				logger.DebugCF("claude-cli", "resumed session",
					map[string]any{"session_key": sessionKey, "session_id": prev.id, "delta": len(delta)})
				p.storeSession(sessionKey, prev.id, conv, raw)
				return resp, nil
			}
			if !isMissingSessionError(err) {
				return nil, err
			}
			logger.WarnCF("claude-cli", "stored session unavailable, starting a fresh one",
				map[string]any{"session_key": sessionKey, "error": err.Error()})
		}
	}

	if sessionKey != "" {
		logger.DebugCF("claude-cli", "starting fresh session",
			map[string]any{"session_key": sessionKey, "messages": len(conv)})
	}
	var extra []string
	if sessionKey != "" {
		extra = []string{"--session-id", p.newSessionID()}
	} else {
		extra = []string{"--no-session-persistence"}
	}
	resp, raw, err := p.run(ctx, systemPrompt, p.messagesToPrompt(conv), model, options, extra...)
	if err != nil {
		return nil, err
	}
	if sessionKey != "" {
		id := raw.SessionID
		if id == "" {
			id = extra[1]
		}
		p.storeSession(sessionKey, id, conv, raw)
	}
	return resp, nil
}

// run executes one CLI invocation.
func (p *ClaudeCliProvider) run(
	ctx context.Context, systemPrompt, prompt, model string, options map[string]any, extra ...string,
) (*LLMResponse, *claudeCliJSONResponse, error) {
	args := []string{"-p", "--output-format", "json", "--dangerously-skip-permissions", "--no-chrome"}
	if systemPrompt != "" {
		args = append(args, "--system-prompt", systemPrompt)
	}
	if model != "" && model != "claude-code" {
		args = append(args, "--model", model)
	}
	if raw, _ := options["thinking_level"].(string); raw != "" {
		if level, ok := effort.ClaudeCLI(raw); ok {
			args = append(args, "--effort", level)
		}
	}
	args = append(args, extra...)
	args = append(args, "-") // read from stdin

	cmd := exec.CommandContext(ctx, p.command, args...)
	if p.workspace != "" {
		cmd.Dir = p.workspace
	}
	cmd.Stdin = bytes.NewReader([]byte(prompt))

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Execute the CLI through the shared isolation wrapper so external provider
	// processes honor the configured isolation policy.
	if err := isolation.Run(cmd); err != nil {
		stderrStr := strings.TrimSpace(stderr.String())
		stdoutStr := strings.TrimSpace(stdout.String())
		switch {
		case stderrStr != "" && stdoutStr != "":
			return nil, nil, fmt.Errorf("claude cli error: %w\nstderr: %s\nstdout: %s", err, stderrStr, stdoutStr)
		case stderrStr != "":
			return nil, nil, fmt.Errorf("claude cli error: %s", stderrStr)
		case stdoutStr != "":
			return nil, nil, fmt.Errorf("claude cli error: %w\noutput: %s", err, stdoutStr)
		default:
			return nil, nil, fmt.Errorf("claude cli error: %w", err)
		}
	}

	var raw claudeCliJSONResponse
	_ = json.Unmarshal(stdout.Bytes(), &raw)
	resp, err := p.parseClaudeCliResponse(stdout.String())
	if err != nil {
		return nil, nil, err
	}
	return resp, &raw, nil
}

// conversationMessages drops system messages, which travel via --system-prompt.
func conversationMessages(messages []Message) []Message {
	conv := make([]Message, 0, len(messages))
	for _, msg := range messages {
		if msg.Role != "system" {
			conv = append(conv, msg)
		}
	}
	return conv
}

func digestMessages(messages []Message) [32]byte {
	h := sha256.New()
	for _, msg := range messages {
		for _, field := range []string{msg.Role, msg.Content, msg.ToolCallID} {
			fmt.Fprintf(h, "%d:%s", len(field), field)
		}
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// resumeDelta returns the messages the stored session has not seen yet, or
// false when conv does not extend the stored prefix. Assistant messages right
// after the prefix are the session's own replies, which it already holds.
func resumeDelta(prev *claudeCliSession, conv []Message) ([]Message, bool) {
	if prev.count == 0 || len(conv) <= prev.count || digestMessages(conv[:prev.count]) != prev.digest {
		return nil, false
	}
	delta := conv[prev.count:]
	for len(delta) > 0 && delta[0].Role == "assistant" {
		delta = delta[1:]
	}
	return delta, len(delta) > 0
}

// takeSession removes and returns the stored session, so a concurrent call for
// the same key starts fresh instead of forking the same CLI session.
func (p *ClaudeCliProvider) takeSession(key string) *claudeCliSession {
	if key == "" {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.sessions[key]
	delete(p.sessions, key)
	return s
}

func (p *ClaudeCliProvider) storeSession(key, id string, conv []Message, raw *claudeCliJSONResponse) {
	if key == "" || id == "" || len(conv) == 0 {
		return
	}
	if raw != nil && raw.NumTurns > 0 {
		u := raw.Usage
		if (u.InputTokens+u.CacheCreationInputTokens+u.CacheReadInputTokens)/raw.NumTurns > claudeCliMaxResumeContext {
			return
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sessions == nil {
		p.sessions = make(map[string]*claudeCliSession)
	}
	p.sessions[key] = &claudeCliSession{id: id, count: len(conv), digest: digestMessages(conv)}
}

func isMissingSessionError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "No conversation found")
}

// GetDefaultModel returns the default model identifier.
func (p *ClaudeCliProvider) GetDefaultModel() string {
	return "claude-code"
}

// messagesToPrompt converts messages to a CLI-compatible prompt string.
func (p *ClaudeCliProvider) messagesToPrompt(messages []Message) string {
	var parts []string

	for _, msg := range messages {
		switch msg.Role {
		case "system":
			// handled via --system-prompt flag
		case "user":
			parts = append(parts, "User: "+msg.Content)
		case "assistant":
			parts = append(parts, "Assistant: "+msg.Content)
		case "tool":
			parts = append(parts, fmt.Sprintf("[Tool Result for %s]: %s", msg.ToolCallID, msg.Content))
		}
	}

	// Simplify single user message
	if len(parts) == 1 && strings.HasPrefix(parts[0], "User: ") {
		return strings.TrimPrefix(parts[0], "User: ")
	}

	return strings.Join(parts, "\n")
}

// buildSystemPrompt combines system messages and tool definitions.
func (p *ClaudeCliProvider) buildSystemPrompt(messages []Message, tools []ToolDefinition) string {
	var parts []string

	for _, msg := range messages {
		if msg.Role == "system" {
			parts = append(parts, msg.Content)
		}
	}

	if len(tools) > 0 {
		parts = append(parts, buildCLIToolsPrompt(tools))
	}

	return strings.Join(parts, "\n\n")
}

// parseClaudeCliResponse parses the JSON output from the claude CLI.
func (p *ClaudeCliProvider) parseClaudeCliResponse(output string) (*LLMResponse, error) {
	var resp claudeCliJSONResponse
	if err := json.Unmarshal([]byte(output), &resp); err != nil {
		return nil, fmt.Errorf("failed to parse claude cli response: %w", err)
	}

	if resp.IsError {
		return nil, fmt.Errorf("claude cli returned error: %s", resp.Result)
	}

	toolCalls := p.extractToolCalls(resp.Result)

	finishReason := "stop"
	content := resp.Result
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
		content = p.stripToolCallsJSON(resp.Result)
	}

	var usage *UsageInfo
	if resp.Usage.InputTokens > 0 || resp.Usage.OutputTokens > 0 {
		usage = &UsageInfo{
			PromptTokens:       resp.Usage.InputTokens + resp.Usage.CacheCreationInputTokens + resp.Usage.CacheReadInputTokens,
			CompletionTokens:   resp.Usage.OutputTokens,
			TotalTokens:        resp.Usage.InputTokens + resp.Usage.CacheCreationInputTokens + resp.Usage.CacheReadInputTokens + resp.Usage.OutputTokens,
			CachedPromptTokens: resp.Usage.CacheReadInputTokens,
		}
	}

	return &LLMResponse{
		Content:      strings.TrimSpace(content),
		ToolCalls:    toolCalls,
		FinishReason: finishReason,
		Usage:        usage,
	}, nil
}

// extractToolCalls delegates to the shared extractToolCallsFromText function.
func (p *ClaudeCliProvider) extractToolCalls(text string) []ToolCall {
	return extractToolCallsFromText(text)
}

// stripToolCallsJSON delegates to the shared stripToolCallsFromText function.
func (p *ClaudeCliProvider) stripToolCallsJSON(text string) string {
	return stripToolCallsFromText(text)
}

// findMatchingBrace finds the index after the closing brace matching the opening brace at pos.
func findMatchingBrace(text string, pos int) int {
	depth := 0
	for i := pos; i < len(text); i++ {
		if text[i] == '{' {
			depth++
		} else if text[i] == '}' {
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return pos
}

// claudeCliJSONResponse represents the JSON output from the claude CLI.
// Matches the real claude CLI v2.x output format.
type claudeCliJSONResponse struct {
	Type         string             `json:"type"`
	Subtype      string             `json:"subtype"`
	IsError      bool               `json:"is_error"`
	Result       string             `json:"result"`
	SessionID    string             `json:"session_id"`
	TotalCostUSD float64            `json:"total_cost_usd"`
	DurationMS   int                `json:"duration_ms"`
	DurationAPI  int                `json:"duration_api_ms"`
	NumTurns     int                `json:"num_turns"`
	Usage        claudeCliUsageInfo `json:"usage"`
}

// claudeCliUsageInfo represents token usage from the claude CLI response.
type claudeCliUsageInfo struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}
