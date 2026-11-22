package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func newSilentTestAgentLoop(t *testing.T, reply string) (*AgentLoop, *bus.MessageBus) {
	t.Helper()
	cfg := &config.Config{Agents: config.AgentsConfig{Defaults: config.AgentDefaults{
		Workspace: t.TempDir(), ModelName: "test-model", MaxTokens: 4096, MaxToolIterations: 10,
	}}}
	msgBus := bus.NewMessageBus()
	provider := &seahorseTestProvider{chatFn: func(
		context.Context, []providers.Message, []providers.ToolDefinition, string, map[string]any,
	) (*providers.LLMResponse, error) {
		return &providers.LLMResponse{Content: reply}, nil
	}}
	return NewAgentLoop(cfg, msgBus, provider), msgBus
}

func drainOutbound(msgBus *bus.MessageBus, wait time.Duration) []bus.OutboundMessage {
	var out []bus.OutboundMessage
	deadline := time.After(wait)
	for {
		select {
		case msg := <-msgBus.OutboundChan():
			out = append(out, msg)
		case <-deadline:
			return out
		}
	}
}

// NO_REPLY: nothing is delivered (only the silent cleanup signal is
// published) and the exchange stays in session history.
func TestSilentReply_NotDeliveredButKeptInHistory(t *testing.T) {
	for _, reply := range []string{"NO_REPLY", " NO_REPLY.\n"} {
		al, msgBus := newSilentTestAgentLoop(t, reply)
		inbound := bus.NormalizeInboundMessage(bus.InboundMessage{
			Context: bus.InboundContext{
				Channel: "telegram", ChatID: "-100123", ChatType: "group", SenderID: "telegram:42",
				MessageID: "-100123:7",
			},
			Content: "lunch at 12?",
		})

		al.runTurnWithSteering(context.Background(), inbound)

		out := drainOutbound(msgBus, 300*time.Millisecond)
		if len(out) != 1 {
			t.Fatalf("reply %q: outbound = %+v, want exactly the silent signal", reply, out)
		}
		if out[0].Content != "" || out[0].Context.Raw[metadataKeyMessageKind] != bus.MessageKindSilent ||
			out[0].Context.Raw[metadataKeyOutboundKind] != outboundKindFinal || out[0].ChatID != "-100123" {
			t.Errorf("silent signal = %+v", out[0])
		}

		route, agent, err := al.resolveMessageRoute(inbound)
		if err != nil {
			t.Fatal(err)
		}
		history := agent.Sessions.GetHistory(al.allocateRouteSession(route, inbound).SessionKey)
		if len(history) < 2 || history[len(history)-1].Role != "assistant" ||
			!bus.IsSilentReply(history[len(history)-1].Content) {
			t.Errorf("history should end with the assistant NO_REPLY, got %+v", history)
		}
	}
}

// A silent answer is not replaced by DefaultResponse on the SendResponse path.
func TestSilentReply_DefaultResponseNotApplied(t *testing.T) {
	al, msgBus := newSilentTestAgentLoop(t, "NO_REPLY")
	if _, err := al.processSystemMessage(context.Background(), bus.InboundMessage{
		Channel: "system", ChatID: "telegram:chat1", SenderID: "cron", Content: "check",
	}); err != nil {
		t.Fatal(err)
	}
	for _, msg := range drainOutbound(msgBus, 300*time.Millisecond) {
		if msg.Content != "" {
			t.Errorf("delivered %q, want nothing (no DefaultResponse for silence)", msg.Content)
		}
	}
}

// Streaming never shows a (prefix of) NO_REPLY and cancels the draft when
// the final answer is silent; normal text still streams.
func TestStreamingPublisher_HoldsBackSilentReply(t *testing.T) {
	ctx := context.Background()
	st := &recordingStreamer{}
	p := &streamingChunkPublisher{streamer: st}
	for _, partial := range []string{"N", "NO_", "NO_REPL", "NO_REPLY"} {
		p.Update(ctx, partial)
	}
	if err := p.Finalize(ctx, "NO_REPLY", nil); err != nil {
		t.Fatal(err)
	}
	if len(st.updates) != 0 || len(st.finalized) != 0 || st.canceled != 1 || p.Published() {
		t.Errorf("updates=%v finalized=%v canceled=%d published=%v", st.updates, st.finalized, st.canceled,
			p.Published())
	}

	st2 := &recordingStreamer{}
	p2 := &streamingChunkPublisher{streamer: st2}
	p2.Update(ctx, "No")
	p2.Update(ctx, "No problem")
	if err := p2.Finalize(ctx, "No problem, done.", nil); err != nil {
		t.Fatal(err)
	}
	if len(st2.updates) != 2 || len(st2.finalized) != 1 || st2.canceled != 0 {
		t.Errorf("normal stream: updates=%v finalized=%v canceled=%d", st2.updates, st2.finalized, st2.canceled)
	}
}

func TestGroupChatNote_OnlyForGroups(t *testing.T) {
	workspace := t.TempDir()
	cb := NewContextBuilder(workspace)
	system := func(req PromptBuildRequest) string {
		req.CurrentMessage = "hi"
		msgs := cb.BuildMessagesFromPrompt(req)
		if len(msgs) == 0 || msgs[0].Role != "system" {
			t.Fatalf("no system message: %+v", msgs)
		}
		return msgs[0].Content
	}

	direct := system(PromptBuildRequest{Channel: "telegram", ChatID: "1", ChatType: "direct"})
	if strings.Contains(direct, "## Group Chat") || strings.Contains(direct, "NO_REPLY") {
		t.Error("direct chat must not get the group note")
	}
	group := system(PromptBuildRequest{Channel: "telegram", ChatID: "-1", ChatType: "group"})
	if !strings.Contains(group, "## Group Chat") || !strings.Contains(group, "exactly NO_REPLY") ||
		!strings.Contains(group, "addressed to you (mention or reply to you): no") {
		t.Errorf("group note missing:\n%s", group)
	}
	mentioned := system(PromptBuildRequest{Channel: "mattermost", ChatID: "c", ChatType: "channel", Mentioned: true})
	if !strings.Contains(mentioned, "addressed to you (mention or reply to you): yes") {
		t.Error("mentioned flag not rendered")
	}

	// The wording can be tuned per workspace.
	if err := os.WriteFile(filepath.Join(workspace, groupChatGuidanceFile),
		[]byte("## Group Chat\nCustom etiquette. Use NO_REPLY to stay quiet."), 0o600); err != nil {
		t.Fatal(err)
	}
	custom := system(PromptBuildRequest{Channel: "telegram", ChatID: "-1", ChatType: "group"})
	if !strings.Contains(custom, "Custom etiquette.") || strings.Contains(custom, "thoughtful human colleague") {
		t.Errorf("workspace override not applied:\n%s", custom)
	}
}
