//go:build !mipsle && !netbsd && !(freebsd && arm)

package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers/protocoltypes"
	"github.com/sipeed/picoclaw/pkg/seahorse"
)

// newBudgetTestManager builds a seahorse manager whose summarizer is a local
// stub, so compaction can be exercised without an LLM.
func newBudgetTestManager(t *testing.T) (*seahorseContextManager, *seahorse.Engine, *[]string) {
	t.Helper()

	var sessionKeys []string
	complete := func(_ context.Context, prompt string, opts seahorse.CompleteOptions) (string, error) {
		sessionKeys = append(sessionKeys, opts.SessionKey)
		return "compressed summary of " + fmt.Sprint(len(prompt)) + " chars", nil
	}

	engine, err := seahorse.NewEngine(seahorse.Config{DBPath: t.TempDir() + "/test.db"}, complete)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(func() { engine.Close() })

	return &seahorseContextManager{engine: engine}, engine, &sessionKeys
}

func ingestBulk(t *testing.T, mgr *seahorseContextManager, sessionKey string, turns int) {
	t.Helper()
	ctx := context.Background()
	body := strings.Repeat("some reasonably long conversational content ", 40)
	for i := 0; i < turns; i++ {
		if _, err := mgr.Ingest(ctx, &IngestRequest{
			SessionKey: sessionKey,
			Message:    protocoltypes.Message{Role: "user", Content: fmt.Sprintf("q%d %s", i, body)},
		}); err != nil {
			t.Fatalf("Ingest: %v", err)
		}
		if _, err := mgr.Ingest(ctx, &IngestRequest{
			SessionKey: sessionKey,
			Message:    protocoltypes.Message{Role: "assistant", Content: fmt.Sprintf("a%d %s", i, body)},
		}); err != nil {
			t.Fatalf("Ingest: %v", err)
		}
	}
}

// A proactive compact used to be a near no-op: it ran one leaf compaction and
// then compared the stored size against the full context window, so an
// over-budget conversation stayed over budget and the runtime re-trimmed it on
// every single turn. It must now actually bring the stored context under the
// budget it was handed.
func TestSeahorseProactiveCompactReducesBelowBudget(t *testing.T) {
	mgr, engine, _ := newBudgetTestManager(t)
	ctx := context.Background()
	const sessionKey = "proactive-budget"

	ingestBulk(t, mgr, sessionKey, 60)

	const budget = 6000
	before, err := engine.Assemble(ctx, sessionKey, seahorse.AssembleInput{Budget: 1 << 30})
	if err != nil {
		t.Fatalf("Assemble before: %v", err)
	}
	beforeTokens := assembleTokens(before)
	if beforeTokens <= budget {
		t.Fatalf("test setup produced %d tokens, need more than the %d budget", beforeTokens, budget)
	}

	if err := mgr.Compact(ctx, &CompactRequest{
		SessionKey:    sessionKey,
		Reason:        ContextCompressReasonProactive,
		HistoryBudget: budget,
	}); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	after, err := engine.Assemble(ctx, sessionKey, seahorse.AssembleInput{Budget: 1 << 30})
	if err != nil {
		t.Fatalf("Assemble after: %v", err)
	}
	afterTokens := assembleTokens(after)
	if afterTokens >= beforeTokens {
		t.Fatalf("stored context did not shrink: %d -> %d tokens", beforeTokens, afterTokens)
	}
}

// Summarization prompts must never be attributed to the chat being summarized:
// on a session-based provider that would splice them into the user's own
// conversation.
func TestSeahorseCompactionUsesItsOwnProviderSession(t *testing.T) {
	mgr, _, sessionKeys := newBudgetTestManager(t)
	ctx := context.Background()
	const sessionKey = "agent:main:telegram:group:-100/5798"

	ingestBulk(t, mgr, sessionKey, 40)

	if err := mgr.Compact(ctx, &CompactRequest{
		SessionKey:    sessionKey,
		Reason:        ContextCompressReasonProactive,
		HistoryBudget: 4000,
	}); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	if len(*sessionKeys) == 0 {
		t.Fatal("no summarization calls were made")
	}
	for _, key := range *sessionKeys {
		if key == "" {
			t.Fatal("summarization ran without a session key, so it lands on the shared default session")
		}
		if key == sessionKey {
			t.Fatalf("summarization reused the chat's session key %q", key)
		}
		if !strings.HasPrefix(key, "seahorse:compact:") {
			t.Fatalf("unexpected summarization session key %q", key)
		}
	}
}

func assembleTokens(result *seahorse.AssembleResult) int {
	if result == nil {
		return 0
	}
	total := len(result.Summary) * 2 / 5
	for _, m := range result.Messages {
		total += seahorse.EstimateMessageTokens(m)
	}
	return total
}
