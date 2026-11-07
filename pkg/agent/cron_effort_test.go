package agent

import (
	"context"
	"fmt"
	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/providers"
	"sync"
	"testing"
)

type effortCall struct {
	text     string
	level    any
	override any
}
type effortCapture struct {
	mu    sync.Mutex
	calls []effortCall
}

func (p *effortCapture) SupportsThinking() bool  { return true }
func (p *effortCapture) GetDefaultModel() string { return "test-model" }
func (p *effortCapture) Chat(ctx context.Context, msgs []providers.Message, tools []providers.ToolDefinition, model string, opts map[string]any) (*providers.LLMResponse, error) {
	text := ""
	for _, m := range msgs {
		if m.Role == "user" {
			text = m.Content
		}
	}
	p.mu.Lock()
	p.calls = append(p.calls, effortCall{text, opts["thinking_level"], opts["effort_override"]})
	p.mu.Unlock()
	return &providers.LLMResponse{Content: "ok", FinishReason: "stop"}, nil
}

type effortReplaceHook struct{ modelRewriteHook }

func (h effortReplaceHook) BeforeLLM(ctx context.Context, req *LLMHookRequest) (*LLMHookRequest, HookDecision, error) {
	n := req.Clone()
	n.Options = map[string]any{"temperature": .2}
	return n, HookDecision{Action: HookActionModify}, nil
}

func TestCronEffortAgentIsolationAndHookReplacement(t *testing.T) {
	p := &effortCapture{}
	al, _, cleanup := newTurnCoordTestLoop(t, p)
	defer cleanup()
	if err := al.MountHook(NamedHook("replace-options", effortReplaceHook{})); err != nil {
		t.Fatal(err)
	}
	run := func(chat, level, text string) {
		_, err := al.processMessage(context.Background(), testInboundMessage(bus.InboundMessage{Channel: "pico", ChatID: chat, SessionKey: chat, Content: text, Effort: level}))
		if err != nil {
			t.Error(err)
		}
	}
	run("same", "max", "override")
	run("same", "", "normal")
	var wg sync.WaitGroup
	for i, level := range []string{"off", "low", "medium", "high", "xhigh", "max", "adaptive", ""} {
		wg.Add(1)
		go func(i int, level string) {
			defer wg.Done()
			run(fmt.Sprintf("session-%d", i), level, fmt.Sprintf("job-%s", level))
		}(i, level)
	}
	wg.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.calls) != 10 {
		t.Fatalf("calls: %v", p.calls)
	}
	for _, c := range p.calls {
		want := ""
		switch c.text {
		case "override":
			want = "max"
		case "normal":
		default:
			if len(c.text) >= 4 {
				want = c.text[4:]
			}
		}
		if want == "" {
			if c.level != nil || c.override != nil {
				t.Fatalf("leak: %+v", c)
			}
		} else if c.level != want || c.override != true {
			t.Fatalf("lost override: %+v want %s", c, want)
		}
	}
}
