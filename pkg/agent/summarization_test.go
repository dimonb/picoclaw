package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
)

type compactionSummaryMockProvider struct {
	prompts []string
}

func (m *compactionSummaryMockProvider) Chat(
	_ context.Context,
	messages []providers.Message,
	_ []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	prompt := messages[0].Content
	m.prompts = append(m.prompts, prompt)

	switch {
	case strings.Contains(prompt, "Update the running conversation summary for future context injection."):
		return &providers.LLMResponse{Content: `## Key Context
- user is working on compaction summaries
## Decisions
- store detailed compaction notes in timestamped files
## Open Loops
- none
## Preferences / Constraints
- prefer separate files over prompt inflation`}, nil
	case strings.Contains(prompt, "Write a detailed compaction memory note for this conversation segment."):
		return &providers.LLMResponse{Content: `## Session
- focus: compaction flow
## What Happened
- reviewed the summary flow and decided to persist a detailed note
## Decisions
- use timestamped compaction files
## Action Items
- none
## Open Questions
- none
## Artifacts Mentioned
- memory/YYYYMM/YYYYMMDD-HHMMSS.compactions.md
## Preferences / Working Style
- keep the context summary short`}, nil
	case strings.Contains(prompt, "Merge the existing running summary and the new partial summaries into one updated running summary."):
		return &providers.LLMResponse{Content: `## Key Context
- merged
## Decisions
- merged
## Open Loops
- none
## Preferences / Constraints
- none`}, nil
	case strings.Contains(prompt, "Merge these detailed compaction notes into one cohesive daily memory entry."):
		return &providers.LLMResponse{Content: `## Session
- merged
## What Happened
- merged
## Decisions
- merged
## Action Items
- none
## Open Questions
- none
## Artifacts Mentioned
- none
## Preferences / Working Style
- none`}, nil
	default:
		return &providers.LLMResponse{Content: "unexpected prompt"}, nil
	}
}

func (m *compactionSummaryMockProvider) GetDefaultModel() string {
	return "mock-model"
}

func TestSummarizeSessionWritesDetailedCompactionFile(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         workspace,
				Model:             "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
			},
		},
	}

	provider := &compactionSummaryMockProvider{}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	agent := al.registry.GetDefaultAgent()
	if agent == nil {
		t.Fatal("expected default agent")
	}

	sessionKey := "session-1"
	history := []providers.Message{
		{Role: "user", Content: "Need a better compaction summary flow."},
		{Role: "assistant", Content: "Current prompt is too generic."},
		{Role: "user", Content: "Write the detailed summary into a separate file."},
		{Role: "assistant", Content: "We should keep the running summary short."},
		{Role: "user", Content: "Add timestamps to the compaction filename."},
		{Role: "assistant", Content: "Use hours, minutes, and seconds."},
	}
	for _, msg := range history {
		agent.Sessions.AddFullMessage(sessionKey, msg)
	}

	al.summarizeSession(agent, sessionKey, "telegram", "chat-42")

	summary := agent.Sessions.GetSummary(sessionKey)
	if !strings.Contains(summary, "## Key Context") {
		t.Fatalf("summary missing structured heading: %q", summary)
	}

	finalHistory := agent.Sessions.GetHistory(sessionKey)
	if len(finalHistory) != 4 {
		t.Fatalf("history len = %d, want 4", len(finalHistory))
	}

	files, err := filepath.Glob(
		filepath.Join(workspace, "memory", "*", "*.compactions.md"),
	)
	if err != nil {
		t.Fatalf("Glob failed: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("expected one compaction file, got %d", len(files))
	}

	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "Session: `session-1`") {
		t.Fatalf("compaction file missing session metadata: %q", content)
	}
	if !strings.Contains(content, "Channel: `telegram`") {
		t.Fatalf("compaction file missing channel metadata: %q", content)
	}
	if !strings.Contains(content, "## What Happened") {
		t.Fatalf("compaction file missing detailed note body: %q", content)
	}

	if len(provider.prompts) != 2 {
		t.Fatalf("expected 2 provider calls, got %d", len(provider.prompts))
	}
	if !strings.Contains(provider.prompts[0], "Keep the result under 180 words.") {
		t.Fatalf("running summary prompt missing compactness rule: %q", provider.prompts[0])
	}
	if !strings.Contains(provider.prompts[1], "Target 250-500 words.") {
		t.Fatalf("detailed compaction prompt missing target length: %q", provider.prompts[1])
	}
	if !strings.Contains(provider.prompts[1], "<channel>telegram</channel>") {
		t.Fatalf("detailed compaction prompt missing channel metadata: %q", provider.prompts[1])
	}
}

func TestSummarizeSession_UsesConfiguredKeepMessages(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:             workspace,
				Model:                 "test-model",
				MaxTokens:             4096,
				MaxToolIterations:     10,
				SummarizeKeepMessages: 6,
			},
		},
	}

	provider := &compactionSummaryMockProvider{}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	agent := al.registry.GetDefaultAgent()
	if agent == nil {
		t.Fatal("expected default agent")
	}

	sessionKey := "session-keep-6"
	history := []providers.Message{
		{Role: "user", Content: "m1"},
		{Role: "assistant", Content: "m2"},
		{Role: "user", Content: "m3"},
		{Role: "assistant", Content: "m4"},
		{Role: "user", Content: "m5"},
		{Role: "assistant", Content: "m6"},
		{Role: "user", Content: "m7"},
		{Role: "assistant", Content: "m8"},
	}
	for _, msg := range history {
		agent.Sessions.AddFullMessage(sessionKey, msg)
	}

	al.summarizeSession(agent, sessionKey, "telegram", "chat-42")

	finalHistory := agent.Sessions.GetHistory(sessionKey)
	if len(finalHistory) != 6 {
		t.Fatalf("history len = %d, want 6", len(finalHistory))
	}
	if finalHistory[0].Content != "m3" {
		t.Fatalf("expected kept window to start at m3, got %q", finalHistory[0].Content)
	}
}

func TestBuildRunningSummaryMergePromptIncludesConflictRules(t *testing.T) {
	t.Parallel()

	prompt := buildRunningSummaryMergePrompt(
		"## Key Context\n- prior summary",
		[]string{"## Key Context\n- first", "## Key Context\n- second"},
	)

	if !strings.Contains(prompt, "Prefer newer information when facts conflict.") {
		t.Fatalf("prompt missing conflict rule: %q", prompt)
	}
	if !strings.Contains(prompt, "<existing_summary>") {
		t.Fatalf("prompt missing existing summary block: %q", prompt)
	}
	if !strings.Contains(prompt, "<summary index=\"2\">") {
		t.Fatalf("prompt missing indexed summary block: %q", prompt)
	}
}

func TestThreadAwareKeepCount_NoThreads(t *testing.T) {
	t.Parallel()
	history := []providers.Message{
		{Role: "user", Content: "a", MessageIDs: []string{"1"}},
		{Role: "assistant", Content: "b", MessageIDs: []string{"2"}},
		{Role: "user", Content: "c", MessageIDs: []string{"3"}},
		{Role: "assistant", Content: "d", MessageIDs: []string{"4"}},
		{Role: "user", Content: "e", MessageIDs: []string{"5"}},
		{Role: "assistant", Content: "f", MessageIDs: []string{"6"}},
	}
	// No reply threads — should keep exactly minKeep.
	if got := threadAwareKeepCount(history, 4); got != 4 {
		t.Fatalf("expected 4, got %d", got)
	}
}

func TestThreadAwareKeepCount_ActiveThreadExtendsWindow(t *testing.T) {
	t.Parallel()
	// History: root at index 4 (#5), reply thread continues into kept window.
	history := []providers.Message{
		{Role: "user", Content: "old-1", MessageIDs: []string{"1"}},
		{Role: "assistant", Content: "old-2", MessageIDs: []string{"2"}},
		{Role: "user", Content: "old-3", MessageIDs: []string{"3"}},
		{Role: "assistant", Content: "old-4", MessageIDs: []string{"4"}},
		{Role: "user", Content: "thread root", MessageIDs: []string{"5"}},
		{Role: "assistant", Content: "context", MessageIDs: []string{"6"}},
		// Kept window starts here (last 4):
		{Role: "user", Content: "follow-up on root", MessageIDs: []string{"7"}, ReplyToMessageID: "5"},
		{Role: "assistant", Content: "ok", MessageIDs: []string{"8"}},
		{Role: "user", Content: "more", MessageIDs: []string{"9"}},
		{Role: "assistant", Content: "done", MessageIDs: []string{"10"}},
	}
	// msg #7 replies to #5 which sits just outside the keep window, so keep should
	// extend by one without exceeding the half-history cap.
	got := threadAwareKeepCount(history, 4)
	if got != 5 {
		t.Fatalf("expected 5, got %d", got)
	}
}

func TestSummarizeSession_ThreadRootOlderThanHalfStillSummarizes(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:             workspace,
				Model:                 "test-model",
				MaxTokens:             4096,
				MaxToolIterations:     10,
				SummarizeKeepMessages: 6,
			},
		},
	}

	provider := &compactionSummaryMockProvider{}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	agent := al.registry.GetDefaultAgent()
	if agent == nil {
		t.Fatal("expected default agent")
	}

	sessionKey := "session-thread-root-old"
	history := []providers.Message{
		{Role: "user", Content: "root question", MessageIDs: []string{"1"}},
		{Role: "assistant", Content: "answer", MessageIDs: []string{"2"}},
		{Role: "user", Content: "follow-up 1", MessageIDs: []string{"3"}, ReplyToMessageID: "1"},
		{Role: "assistant", Content: "reply 1", MessageIDs: []string{"4"}, ReplyToMessageID: "1"},
		{Role: "user", Content: "follow-up 2", MessageIDs: []string{"5"}, ReplyToMessageID: "1"},
		{Role: "assistant", Content: "reply 2", MessageIDs: []string{"6"}, ReplyToMessageID: "1"},
		{Role: "user", Content: "follow-up 3", MessageIDs: []string{"7"}, ReplyToMessageID: "1"},
		{Role: "assistant", Content: "reply 3", MessageIDs: []string{"8"}, ReplyToMessageID: "1"},
		{Role: "user", Content: "follow-up 4", MessageIDs: []string{"9"}, ReplyToMessageID: "1"},
		{Role: "assistant", Content: "reply 4", MessageIDs: []string{"10"}, ReplyToMessageID: "1"},
		{Role: "user", Content: "follow-up 5", MessageIDs: []string{"11"}, ReplyToMessageID: "1"},
		{Role: "assistant", Content: "reply 5", MessageIDs: []string{"12"}, ReplyToMessageID: "1"},
	}
	for _, msg := range history {
		agent.Sessions.AddFullMessage(sessionKey, msg)
	}

	al.summarizeSession(agent, sessionKey, "telegram", "chat-42")

	summary := agent.Sessions.GetSummary(sessionKey)
	if !strings.Contains(summary, "## Key Context") {
		t.Fatalf("summary missing structured heading: %q", summary)
	}

	finalHistory := agent.Sessions.GetHistory(sessionKey)
	if len(finalHistory) != 6 {
		t.Fatalf("history len = %d, want 6", len(finalHistory))
	}
}

func TestThreadAwareKeepCount_ThreadRootInKeepWindow(t *testing.T) {
	t.Parallel()
	history := []providers.Message{
		{Role: "user", Content: "old stuff", MessageIDs: []string{"1"}},
		{Role: "assistant", Content: "old reply", MessageIDs: []string{"2"}},
		{Role: "user", Content: "old stuff", MessageIDs: []string{"3"}},
		{Role: "assistant", Content: "old reply", MessageIDs: []string{"4"}},
		// Root is already inside the keep window:
		{Role: "user", Content: "root", MessageIDs: []string{"5"}},
		{Role: "assistant", Content: "ok", MessageIDs: []string{"6"}},
		{Role: "user", Content: "reply to root", MessageIDs: []string{"7"}, ReplyToMessageID: "5"},
		{Role: "assistant", Content: "done", MessageIDs: []string{"8"}},
	}
	// Parent #5 is already kept — no extension needed.
	if got := threadAwareKeepCount(history, 4); got != 4 {
		t.Fatalf("expected 4, got %d", got)
	}
}

func TestFormatConversationMessages_ThreadAnnotations(t *testing.T) {
	t.Parallel()
	batch := []providers.Message{
		{Role: "user", Content: "hello", MessageIDs: []string{"10"}},
		{Role: "assistant", Content: "hi", MessageIDs: []string{"11"}},
		{Role: "user", Content: "follow-up", MessageIDs: []string{"12"}, ReplyToMessageID: "10"},
		{Role: "assistant", Content: "sure", ReplyToMessageID: "10"},
		{Role: "user", Content: "plain"},
	}
	out := formatConversationMessages(batch)
	if !strings.Contains(out, "[[msg:#10]]") {
		t.Fatalf("missing msg annotation: %q", out)
	}
	if !strings.Contains(out, "[[msg:#12, reply_to:#10]]") {
		t.Fatalf("missing combined annotation: %q", out)
	}
	if !strings.Contains(out, "[[reply_to:#10]]") {
		t.Fatalf("missing reply_to-only annotation: %q", out)
	}
	if !strings.Contains(out, "user: plain") {
		t.Fatalf("plain message lost: %q", out)
	}
}
