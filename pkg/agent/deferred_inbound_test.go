package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	runtimeevents "github.com/sipeed/picoclaw/pkg/events"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// gatedProvider answers every call directly (no tool calls). Each call
// announces itself on started and then waits on release, so a test can act
// while the model is "thinking" — exactly the window a steering message or a
// cron firing lands in. Messages seen by each call are recorded.
type gatedProvider struct {
	mu       sync.Mutex
	calls    int
	started  chan int
	release  chan struct{}
	seen     [][]providers.Message
	response func(call int) string
}

func newGatedProvider(response func(call int) string) *gatedProvider {
	return &gatedProvider{
		started:  make(chan int, 16),
		release:  make(chan struct{}, 16),
		response: response,
	}
}

func (p *gatedProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	_ []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.seen = append(p.seen, append([]providers.Message(nil), messages...))
	p.mu.Unlock()

	p.started <- call
	select {
	case <-p.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &providers.LLMResponse{Content: p.response(call), ToolCalls: []providers.ToolCall{}}, nil
}

func (p *gatedProvider) GetDefaultModel() string { return "gated-mock" }

func (p *gatedProvider) messagesForCall(call int) []providers.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	if call < 1 || call > len(p.seen) {
		return nil
	}
	return p.seen[call-1]
}

func waitForCall(t *testing.T, p *gatedProvider, want int) {
	t.Helper()
	select {
	case got := <-p.started:
		if got != want {
			t.Fatalf("provider call %d started, want %d", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timeout waiting for provider call %d", want)
	}
}

func newDeferredTestConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         t.TempDir(),
				ModelName:         "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
			},
		},
	}
}

func countUserMessages(msgs []providers.Message, content string) int {
	n := 0
	for _, m := range msgs {
		if m.Role == "user" && strings.Contains(m.Content, content) {
			n++
		}
	}
	return n
}

// A steering message that arrives while the model is composing a direct answer
// is injected into the continued turn — once. The coordinator refreshed its
// local pending slice right after CallLLM and then appended the same slice
// again at the top of the next iteration, so every such message went in twice.
func TestRunTurn_SteeringAfterDirectAnswerIsInjectedOnce(t *testing.T) {
	provider := newGatedProvider(func(call int) string {
		if call == 1 {
			return "answer to the first question"
		}
		return "answer after steering"
	})
	al := NewAgentLoop(newDeferredTestConfig(t), bus.NewMessageBus(), provider)

	runtimeCh, closeRuntimeEvents := subscribeRuntimeEventsForTest(
		t, al, 16, runtimeevents.KindAgentSteeringInjected,
	)
	defer closeRuntimeEvents()

	resultCh := make(chan string, 1)
	go func() {
		resp, _ := al.ProcessDirectWithChannel(context.Background(), "first question", "test-session", "test", "chat1")
		resultCh <- resp
	}()

	// The model is mid-answer; steer now so the direct answer sees the queue.
	waitForCall(t, provider, 1)
	if err := al.Steer(providers.Message{Role: "user", Content: "change course"}); err != nil {
		t.Fatalf("Steer failed: %v", err)
	}
	provider.release <- struct{}{}

	waitForCall(t, provider, 2)
	provider.release <- struct{}{}

	select {
	case resp := <-resultCh:
		if resp != "answer after steering" {
			t.Fatalf("final response = %q, want the steered answer", resp)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for the turn")
	}

	if got := countUserMessages(provider.messagesForCall(2), "change course"); got != 1 {
		t.Fatalf("steering message appeared %d times in the continued turn, want 1", got)
	}

	events := collectRuntimeEventStream(runtimeCh)
	evt, ok := findRuntimeEvent(events, runtimeevents.KindAgentSteeringInjected)
	if !ok {
		t.Fatal("expected a steering injected event")
	}
	if payload := evt.Payload.(SteeringInjectedPayload); payload.Count != 1 {
		t.Fatalf("steering injected count = %d, want 1", payload.Count)
	}
}

// A DeferWhileBusy message (a cron firing) that lands on a busy session must
// not be steered into the running turn: that drops the answer the user is
// waiting for and hands the model a second task mid-turn. It waits for the
// session to go idle and then runs as its own turn, so both answers are
// delivered.
func TestRun_DeferWhileBusyWaitsForIdleSession(t *testing.T) {
	provider := newGatedProvider(func(call int) string {
		if call == 1 {
			return "answer to the user"
		}
		return "answer to the cron trigger"
	})
	msgBus := bus.NewMessageBus()
	al := NewAgentLoop(newDeferredTestConfig(t), msgBus, provider)

	runtimeCh, closeRuntimeEvents := subscribeRuntimeEventsForTest(
		t, al, 16, runtimeevents.KindAgentSteeringInjected, runtimeevents.KindAgentInterruptReceived,
	)
	defer closeRuntimeEvents()

	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()
	runDone := make(chan error, 1)
	go func() { runDone <- al.Run(runCtx) }()

	inbound := func(content string, deferWhileBusy bool) bus.InboundMessage {
		return bus.InboundMessage{
			Context: bus.InboundContext{
				Channel:  "pico",
				ChatID:   "session-1",
				ChatType: "direct",
				SenderID: "user-1",
			},
			Content:        content,
			DeferWhileBusy: deferWhileBusy,
		}
	}

	if err := msgBus.PublishInbound(context.Background(), inbound("user question", false)); err != nil {
		t.Fatalf("PublishInbound(user) error = %v", err)
	}
	waitForCall(t, provider, 1)

	// The user's turn is live. A cron firing for the same session arrives.
	if err := msgBus.PublishInbound(context.Background(), inbound("[cron] scheduled job fired", true)); err != nil {
		t.Fatalf("PublishInbound(cron) error = %v", err)
	}

	// Give the inbound loop time to route it; it must be parked, not steered.
	time.Sleep(200 * time.Millisecond)
	if n := al.steering.len(); n != 0 {
		t.Fatalf("deferred message was steered: %d queued", n)
	}
	if got := countUserMessages(provider.messagesForCall(1), "[cron]"); got != 0 {
		t.Fatalf("cron trigger reached the user's turn")
	}

	// Let the user's turn finish; the cron trigger should then run on its own.
	provider.release <- struct{}{}
	waitForCall(t, provider, 2)
	if got := countUserMessages(provider.messagesForCall(2), "[cron]"); got != 1 {
		t.Fatalf("cron trigger missing from its own turn: %+v", provider.messagesForCall(2))
	}
	provider.release <- struct{}{}

	// Both answers reach the chat, in order.
	var outputs []string
	deadline := time.After(5 * time.Second)
	for len(outputs) < 2 {
		select {
		case out := <-msgBus.OutboundChan():
			if out.Content != "" {
				outputs = append(outputs, out.Content)
			}
		case <-deadline:
			t.Fatalf("timed out waiting for both answers, got %v", outputs)
		}
	}
	if outputs[0] != "answer to the user" || outputs[1] != "answer to the cron trigger" {
		t.Fatalf("outputs = %v", outputs)
	}

	events := collectRuntimeEventStream(runtimeCh)
	if _, steered := findRuntimeEvent(events, runtimeevents.KindAgentSteeringInjected); steered {
		t.Fatal("deferred message must never be injected as steering")
	}

	runCancel()
	select {
	case <-runDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Run() did not stop")
	}
}
