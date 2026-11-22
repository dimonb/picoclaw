package mattermost

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
)

// rootsOf returns the root_id of every created post, in order.
func rootsOf(fm *fakeMattermost) []string {
	posts := fm.callsTo(http.MethodPost, "/api/v4/posts")
	roots := make([]string, 0, len(posts))
	for _, p := range posts {
		root, _ := p.Body["root_id"].(string)
		roots = append(roots, root)
	}
	return roots
}

func waitTyping(t *testing.T, fm *fakeMattermost, n int) []apiCall {
	t.Helper()
	deadline := time.Now().Add(testWaitTime)
	for len(fm.callsTo(http.MethodPost, "/api/v4/users/me/typing")) < n && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	return fm.callsTo(http.MethodPost, "/api/v4/users/me/typing")
}

func withIndicators(bc *config.Channel, _ *config.MattermostSettings) {
	bc.Placeholder.Enabled = true
	bc.Placeholder.Text = config.FlexibleStringSlice{"Thinking..."}
	bc.Typing.Enabled = true
}

func TestDefaultReplyRoot(t *testing.T) {
	top := msgRef{channelID: "c", postID: "p"}
	inThread := msgRef{channelID: "c", rootID: "r", postID: "p"}
	for _, tc := range []struct {
		chatType string
		ref      msgRef
		want     string
	}{
		{"direct", top, ""},       // DM top-level: mirror, top-level reply
		{"direct", inThread, "r"}, // DM inside a thread: stay in it
		{"channel", top, "p"},     // channel: thread under the post
		{"channel", inThread, "r"},
		{"group", top, "p"},
		{"", top, "p"}, // unknown chat type: channel behavior
	} {
		if got := defaultReplyRoot(tc.chatType, tc.ref); got != tc.want {
			t.Errorf("defaultReplyRoot(%q, %+v) = %q, want %q", tc.chatType, tc.ref, got, tc.want)
		}
	}
}

// DM: a top-level post is answered top-level, a post in a thread in that
// thread; placeholder, typing and the final reply all follow, and the
// message tool with reply_to_message_id still moves the reply into a thread.
func TestDMRepliesMirrorTheUser(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, mb := newTestChannel(t, fm, nil, withIndicators)
	conn := startConnected(t, fm, ch)
	ctx := context.Background()

	// 1. Top-level DM.
	sendPosted(t, conn, "D", post("d1", testUserID, "hi", ""), nil)
	in := nextInbound(t, mb)
	if got := in.Context.Raw[bus.InboundRawReplyPlacement]; got != bus.ReplyPlacementTopLevel {
		t.Errorf("reply placement = %q, want top_level", got)
	}
	if typing := waitTyping(t, fm, 1); len(typing) == 0 || typing[0].Body["parent_id"] != "" {
		t.Errorf("typing = %v, want top-level (no parent_id)", typing)
	}
	final := in.Context
	final.ReplyToMessageID = "" // as the agent addresses final replies
	final.Raw = map[string]string{"outbound_kind": "final"}
	if _, err := ch.Send(ctx, bus.OutboundMessage{ChatID: testChanID, Content: "hello", Context: final}); err != nil {
		t.Fatal(err)
	}
	if roots := rootsOf(fm); len(roots) != 1 || roots[0] != "" {
		t.Fatalf("placeholder roots = %v, want one top-level post", roots)
	}
	if patches := fm.callsTo(http.MethodPut, "/api/v4/posts/"); len(patches) != 1 ||
		patches[0].Body["message"] != "hello" {
		t.Errorf("final reply must edit the top-level placeholder: %v", patches)
	}

	// 2. DM inside a thread.
	sendPosted(t, conn, "D", post("d3", testUserID, "more", "d2"), nil)
	in = nextInbound(t, mb)
	if got := in.Context.Raw[bus.InboundRawReplyPlacement]; got != bus.ReplyPlacementThread {
		t.Errorf("reply placement = %q, want thread", got)
	}
	final = in.Context
	final.ReplyToMessageID = ""
	final.Raw = map[string]string{"outbound_kind": "final"}
	if _, err := ch.Send(ctx, bus.OutboundMessage{ChatID: testChanID, Content: "sure", Context: final}); err != nil {
		t.Fatal(err)
	}
	if roots := rootsOf(fm); len(roots) != 2 || roots[1] != "d2" {
		t.Fatalf("roots = %v, want the second placeholder in thread d2", roots)
	}

	// 3. Message tool with reply_to_message_id into a top-level DM post.
	if _, err := ch.Send(ctx, bus.OutboundMessage{
		ChatID: testChanID, Content: "long answer", ReplyToMessageID: testChanID + ":d1",
		Context: bus.NewOutboundContext("mattermost", testChanID, testChanID+":d1"),
	}); err != nil {
		t.Fatal(err)
	}
	// 4. Message tool without reply_to_message_id: top level.
	if _, err := ch.Send(ctx, bus.OutboundMessage{
		ChatID: testChanID, Content: "back at top", Context: bus.NewOutboundContext("mattermost", testChanID, ""),
	}); err != nil {
		t.Fatal(err)
	}
	if roots := rootsOf(fm); len(roots) != 4 || roots[2] != "d1" || roots[3] != "" {
		t.Errorf("roots = %v, want message tool posts in thread d1 and at top level", roots)
	}
}

// Streaming in a top-level DM streams a top-level post.
func TestDMStreamingTopLevel(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, _ := newTestChannel(t, fm, nil, func(_ *config.Channel, s *config.MattermostSettings) {
		s.Streaming = config.StreamingConfig{Enabled: true, MinGrowthChars: 1}
	})
	startConnected(t, fm, ch)
	ctx := context.Background()
	turnCtx := bus.WithInboundContext(ctx, bus.InboundContext{
		ChatID: testChanID, ChatType: "direct", MessageID: testChanID + ":d1",
	})
	st, err := ch.BeginStream(turnCtx, testChanID)
	if err != nil {
		t.Fatal(err)
	}
	st.(*mattermostStreamer).throttle = 0
	if err := st.Update(ctx, "Hel"); err != nil {
		t.Fatal(err)
	}
	if err := st.Finalize(ctx, "Hello"); err != nil {
		t.Fatal(err)
	}
	if roots := rootsOf(fm); len(roots) != 1 || roots[0] != "" {
		t.Errorf("stream roots = %v, want top-level", roots)
	}
}

// Channels keep replying in the post's thread by default; the agent can still
// post at the top level through the message tool, after which the silent
// final reply clears the thread's placeholder and typing.
func TestChannelThreadByDefaultTopLevelOnRequest(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, mb := newTestChannel(t, fm, nil, withIndicators)
	conn := startConnected(t, fm, ch)
	ctx := context.Background()

	sendPosted(t, conn, "O", post("c1", testUserID, "@picobot news?", ""), []string{testBotID})
	in := nextInbound(t, mb)
	if got := in.Context.Raw[bus.InboundRawReplyPlacement]; got != bus.ReplyPlacementThread {
		t.Errorf("reply placement = %q, want thread", got)
	}
	if typing := waitTyping(t, fm, 1); len(typing) == 0 || typing[0].Body["parent_id"] != "c1" {
		t.Errorf("typing = %v, want in thread c1", typing)
	}
	if roots := rootsOf(fm); len(roots) != 1 || roots[0] != "c1" {
		t.Fatalf("placeholder roots = %v, want thread c1", roots)
	}
	placeholderID := fm.callsTo(http.MethodPost, "/api/v4/posts")[0].ID

	// The agent decides to answer the whole room at the top level.
	if _, err := ch.Send(ctx, bus.OutboundMessage{
		ChatID: testChanID, Content: "Announcement", Context: bus.NewOutboundContext("mattermost", testChanID, ""),
	}); err != nil {
		t.Fatal(err)
	}
	if roots := rootsOf(fm); len(roots) != 2 || roots[1] != "" {
		t.Fatalf("roots = %v, want the announcement at the top level", roots)
	}
	if patches := fm.callsTo(http.MethodPut, "/api/v4/posts/"); len(patches) != 0 {
		t.Errorf("top-level post must not take the thread's placeholder: %v", patches)
	}

	// Its final answer is then silent: the thread placeholder goes away.
	silent := bus.OutboundMessage{ChatID: testChanID, Context: in.Context}
	silent.Context.ReplyToMessageID = ""
	silent.Context.Raw = map[string]string{"message_kind": bus.MessageKindSilent, "outbound_kind": "final"}
	ch.HandleSilentReply(ctx, silent)
	if dels := fm.callsTo(http.MethodDelete, "/api/v4/posts/"+placeholderID); len(dels) != 1 {
		t.Errorf("thread placeholder not deleted: %v", dels)
	}
	if n := len(fm.callsTo(http.MethodPost, "/api/v4/posts")); n != 2 {
		t.Errorf("posts = %d, want 2", n)
	}
}
