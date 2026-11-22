package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/tools"
)

func newMessageToolLoop(t *testing.T, sent ...string) (*AgentLoop, *bus.MessageBus) {
	t.Helper()
	al, _, msgBus, _, cleanup := newTestAgentLoop(t)
	t.Cleanup(cleanup)
	al.channelManager = &recordingChannelManager{}
	mt := tools.NewMessageTool()
	mt.SetSendCallback(func(context.Context, string, string, string, string, []bus.MediaPart) ([]string, error) {
		return nil, nil
	})
	al.registry.GetDefaultAgent().Tools.Register(mt)
	for _, content := range sent {
		if res := mt.Execute(tools.WithToolSessionContext(context.Background(), "main", "s1", nil), map[string]any{
			"content": content, "channel": "mattermost", "chat_id": "chan1",
		}); res == nil || res.IsError {
			t.Fatalf("message tool: %+v", res)
		}
	}
	return al, msgBus
}

var placementInbound = &bus.InboundContext{
	Channel: "mattermost", ChatID: "chan1", ChatType: "channel", MessageID: "chan1:p1",
}

// The agent posted its answer through the message tool (e.g. at the top level)
// and repeats it as the final: the final is replaced by a silent cleanup
// signal carrying the turn's context, so the channel removes what it showed
// at the default location.
func TestPublishFinalResponse_DuplicateOfMessageToolBecomesSilentCleanup(t *testing.T) {
	al, msgBus := newMessageToolLoop(t, "Release 1.2 is out: faster sync and a new settings page.")
	if err := al.publishFinalResponse(context.Background(), placementInbound, "mattermost", "chan1", "s1",
		"Release 1.2 is out: faster sync and a new settings page"); err != nil {
		t.Fatal(err)
	}
	select {
	case out := <-msgBus.OutboundChan():
		if out.Content != "" || out.Context.Raw[metadataKeyMessageKind] != bus.MessageKindSilent ||
			out.Context.Raw[metadataKeyOutboundKind] != outboundKindFinal || out.Context.MessageID != "chan1:p1" {
			t.Errorf("want a silent final signal for chan1:p1, got %+v", out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no silent cleanup signal published")
	}
}

// Drafting through the message tool and then answering something else
// ("done"): the final must reach the user (it used to be dropped).
func TestPublishFinalResponse_DifferentFinalAfterMessageToolIsDelivered(t *testing.T) {
	al, msgBus := newMessageToolLoop(t, "DRAFT", "FINAL")
	if err := al.publishFinalResponse(context.Background(), placementInbound, "mattermost", "chan1", "s1",
		"Готово."); err != nil {
		t.Fatal(err)
	}
	select {
	case out := <-msgBus.OutboundChan():
		if out.Content != "Готово." || out.Context.MessageID != "chan1:p1" ||
			out.Context.Raw[metadataKeyMessageKind] == bus.MessageKindSilent {
			t.Errorf("final not delivered as a normal reply: %+v", out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("final reply was dropped")
	}
}

// Answered at the top level through the message tool, then NO_REPLY: silent
// cleanup with the turn's context (the thread placeholder goes away).
func TestPublishFinalResponse_NoReplyAfterMessageToolIsSilentCleanup(t *testing.T) {
	al, msgBus := newMessageToolLoop(t, "Announcement for everyone")
	if err := al.publishFinalResponse(context.Background(), placementInbound, "mattermost", "chan1", "s1",
		"NO_REPLY"); err != nil {
		t.Fatal(err)
	}
	select {
	case out := <-msgBus.OutboundChan():
		if out.Content != "" || out.Context.Raw[metadataKeyMessageKind] != bus.MessageKindSilent ||
			out.Context.MessageID != "chan1:p1" {
			t.Errorf("want a silent final signal, got %+v", out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no silent cleanup signal published")
	}
}

func TestIsDuplicateOfSent(t *testing.T) {
	long := "Here is the summary of the meeting: we ship on Friday and Bob owns the rollout."
	for _, tc := range []struct {
		resp string
		sent []string
		want bool
	}{
		{"Done.", []string{"done"}, true},
		{"  Hello   World!! ", []string{"hello world"}, true},
		{long, []string{long + " 🙂"}, true},
		{"Готово.", []string{"DRAFT", "FINAL"}, false},
		{"ok", []string{"ok, sending the file now and the report after lunch"}, false},
		{"anything", nil, false},
	} {
		if got := isDuplicateOfSent(tc.resp, tc.sent); got != tc.want {
			t.Errorf("isDuplicateOfSent(%q, %q) = %v, want %v", tc.resp, tc.sent, got, tc.want)
		}
	}
}

func TestThreadsNote(t *testing.T) {
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

	// Channels without the choice (e.g. Telegram) get no note.
	if s := system(PromptBuildRequest{Channel: "telegram", ChatID: "1", ChatType: "direct"}); strings.Contains(
		s, "## Threads") {
		t.Error("no reply placement: threads note must be absent")
	}

	dm := system(PromptBuildRequest{
		Channel: "mattermost", ChatID: "d", ChatType: "direct", ReplyPlacement: bus.ReplyPlacementTopLevel,
	})
	if !strings.Contains(dm, "## Threads") || !strings.Contains(dm, "mirror the user") ||
		!strings.Contains(dm, "goes at the top level.") ||
		!strings.Contains(dm, "no reply_to_message_id puts it at the top level") {
		t.Errorf("DM threads note:\n%s", dm)
	}
	group := system(PromptBuildRequest{
		Channel: "mattermost", ChatID: "c", ChatType: "channel", ReplyPlacement: bus.ReplyPlacementThread,
	})
	if !strings.Contains(group, "Usually answer in the message's thread") ||
		!strings.Contains(group, "goes in its thread.") || strings.Contains(group, "mirror the user") {
		t.Errorf("group threads note:\n%s", group)
	}

	if err := os.WriteFile(filepath.Join(workspace, directThreadsGuidanceFile),
		[]byte("## Threads\nCustom DM threads."), 0o600); err != nil {
		t.Fatal(err)
	}
	custom := system(PromptBuildRequest{
		Channel: "mattermost", ChatID: "d", ChatType: "direct", ReplyPlacement: bus.ReplyPlacementThread,
	})
	if !strings.Contains(custom, "Custom DM threads.") || strings.Contains(custom, "mirror the user") ||
		!strings.Contains(custom, "message tool") {
		t.Errorf("workspace override not applied (mechanism line must stay):\n%s", custom)
	}
}
