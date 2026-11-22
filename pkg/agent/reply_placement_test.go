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

// When the agent delivered its answer through the message tool (e.g. at the
// top level instead of the default thread), the final reply is replaced by a
// silent cleanup signal carrying the turn's inbound context, so the channel
// can remove the placeholder/typing it showed at the default location.
func TestPublishFinalResponse_MessageToolSentBecomesSilentCleanup(t *testing.T) {
	al, _, msgBus, _, cleanup := newTestAgentLoop(t)
	defer cleanup()
	al.channelManager = &recordingChannelManager{}

	mt := tools.NewMessageTool()
	mt.SetSendCallback(func(context.Context, string, string, string, string, []bus.MediaPart) ([]string, error) {
		return nil, nil
	})
	al.registry.GetDefaultAgent().Tools.Register(mt)
	if res := mt.Execute(tools.WithToolSessionContext(context.Background(), "main", "s1", nil), map[string]any{
		"content": "Announcement", "channel": "mattermost", "chat_id": "chan1",
	}); res == nil || res.IsError {
		t.Fatalf("message tool: %+v", res)
	}

	inbound := &bus.InboundContext{
		Channel: "mattermost", ChatID: "chan1", ChatType: "channel", MessageID: "chan1:p1",
	}
	if err := al.publishFinalResponse(context.Background(), inbound, "mattermost", "chan1", "s1", "Done."); err != nil {
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
