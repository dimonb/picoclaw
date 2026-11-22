package mattermost

import (
	"context"
	"net/http"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
)

func silentFor(in bus.InboundMessage) bus.OutboundMessage {
	msg := bus.OutboundMessage{ChatID: in.Context.ChatID, Context: in.Context}
	msg.Context.ReplyToMessageID = ""
	msg.Context.Raw = map[string]string{"message_kind": bus.MessageKindSilent, "outbound_kind": "final"}
	return msg
}

func TestDisableInboundReaction(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, mb := newTestChannel(t, fm, nil, func(bc *config.Channel, s *config.MattermostSettings) {
		bc.Typing.Enabled = true
		s.DisableInboundReaction = true
	})
	conn := startConnected(t, fm, ch)

	sendPosted(t, conn, "D", post("d1", testUserID, "hi", ""), nil)
	nextInbound(t, mb)
	sendPosted(t, conn, "O", post("c1", testUserID, "@picobot hi", ""), []string{testBotID})
	nextInbound(t, mb)

	if typing := waitTyping(t, fm, 2); len(typing) < 2 {
		t.Errorf("typing must still be shown, got %v", typing)
	}
	if adds := fm.callsTo(http.MethodPost, "/api/v4/reactions"); len(adds) != 0 {
		t.Errorf("no automatic reaction expected, got %v", adds)
	}
	// The agent can still react.
	if err := ch.SetMessageReaction(context.Background(), testChanID, testChanID+":d1", "👍"); err != nil {
		t.Fatal(err)
	}
	if adds := fm.callsTo(http.MethodPost, "/api/v4/reactions"); len(adds) != 1 {
		t.Errorf("explicit reaction = %v", adds)
	}
}

// Reaction + NO_REPLY: the agent reacts silently. The cleanup removes only the
// automatic 👀, never a reaction the agent placed, even when it chose 👀.
func TestReactionWithSilentReplyKeepsAgentReactions(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, mb := newTestChannel(t, fm, nil, nil)
	conn := startConnected(t, fm, ch)
	ctx := context.Background()
	autoEyesRemoved := func(postID string) int {
		return len(fm.callsTo(http.MethodDelete, "/api/v4/users/"+testBotID+"/posts/"+postID+"/reactions/eyes"))
	}

	// 1. DM, top-level, the agent reacts with 👍 by the ID from history.
	sendPosted(t, conn, "D", post("d1", testUserID, "thanks!", ""), nil)
	in := nextInbound(t, mb)
	if err := ch.SetMessageReaction(ctx, in.Context.ChatID, in.Context.MessageID, "👍"); err != nil {
		t.Fatal(err)
	}
	ch.HandleSilentReply(ctx, silentFor(in))
	if autoEyesRemoved("d1") != 1 {
		t.Error("automatic 👀 must be removed after the silent reply")
	}
	if dels := fm.callsTo(http.MethodDelete, "/api/v4/users/"+testBotID+"/posts/d1/reactions/+1"); len(dels) != 0 {
		t.Errorf("agent's 👍 must stay: %v", dels)
	}

	// 2. Channel thread, the agent itself picks 👀 (ID "<channel>:<root>:<post>").
	sendPosted(t, conn, "O", post("c2", testUserID, "@picobot look", "c1"), []string{testBotID})
	in = nextInbound(t, mb)
	if in.Context.MessageID != testChanID+":c1:c2" {
		t.Fatalf("message id = %q", in.Context.MessageID)
	}
	if err := ch.SetMessageReaction(ctx, in.Context.ChatID, in.Context.MessageID, "👀"); err != nil {
		t.Fatal(err)
	}
	ch.HandleSilentReply(ctx, silentFor(in))
	if n := autoEyesRemoved("c2"); n != 0 {
		t.Errorf("agent's own 👀 was removed (%d deletes)", n)
	}
	if posts := fm.callsTo(http.MethodPost, "/api/v4/posts"); len(posts) != 0 {
		t.Errorf("silent reactions must not post: %v", posts)
	}
}

// Tool feedback is one post per thread, edited in place; it does not take
// the placeholder or stop typing, and is cleaned up when the answer arrives.
func TestToolFeedbackOnePostPerThread(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, mb := newTestChannel(t, fm, nil, withIndicators)
	conn := startConnected(t, fm, ch)
	ctx := context.Background()

	sendPosted(t, conn, "O", post("c1", testUserID, "@picobot search", ""), []string{testBotID})
	in := nextInbound(t, mb)
	placeholder := fm.callsTo(http.MethodPost, "/api/v4/posts")[0].ID

	feedback := func(text string) {
		t.Helper()
		msg := bus.OutboundMessage{ChatID: testChanID, Content: text, Context: in.Context}
		msg.Context.Raw = map[string]string{"message_kind": "tool_feedback"}
		if _, err := ch.Send(ctx, msg); err != nil {
			t.Fatal(err)
		}
	}
	feedback("🔧 web_search")
	feedback("🔧 web_fetch")

	posts := fm.callsTo(http.MethodPost, "/api/v4/posts")
	if len(posts) != 2 || posts[1].Body["root_id"] != "c1" {
		t.Fatalf("posts = %v, want placeholder + one feedback post in thread c1", posts)
	}
	feedbackID := posts[1].ID
	if p := fm.callsTo(http.MethodPut, "/api/v4/posts/"+feedbackID+"/patch"); len(p) != 1 ||
		p[0].Body["message"] != "🔧 web_fetch" {
		t.Errorf("second feedback must edit the feedback post: %v", p)
	}
	ch.indMu.Lock()
	_, pending := ch.indicators[threadKey(testChanID, "c1")]
	ch.indMu.Unlock()
	if !pending {
		t.Error("tool feedback must not consume the placeholder/typing")
	}

	final := in.Context
	final.ReplyToMessageID = ""
	final.Raw = map[string]string{"outbound_kind": "final"}
	if _, err := ch.Send(ctx, bus.OutboundMessage{ChatID: testChanID, Content: "result", Context: final}); err != nil {
		t.Fatal(err)
	}
	if p := fm.callsTo(http.MethodPut, "/api/v4/posts/"+placeholder+"/patch"); len(p) != 1 ||
		p[0].Body["message"] != "result" {
		t.Errorf("answer must edit the placeholder: %v", p)
	}
	if d := fm.callsTo(http.MethodDelete, "/api/v4/posts/"+feedbackID); len(d) != 1 {
		t.Errorf("feedback post must be deleted: %v", d)
	}

	// Without a placeholder the answer reuses the feedback post.
	sendPosted(t, conn, "D", post("d1", testUserID, "again", ""), nil)
	in = nextInbound(t, mb)
	ch.takeIndicators(testChanID, "") // drop this thread's placeholder
	feedback("🔧 exec")
	posts = fm.callsTo(http.MethodPost, "/api/v4/posts")
	feedbackID = posts[len(posts)-1].ID
	final = in.Context
	final.Raw = map[string]string{"outbound_kind": "final"}
	ids, err := ch.Send(ctx, bus.OutboundMessage{ChatID: testChanID, Content: "done", Context: final})
	if err != nil || len(ids) != 1 || parseMessageID(ids[0]).postID != feedbackID {
		t.Errorf("answer must reuse the feedback post %s: ids=%v err=%v", feedbackID, ids, err)
	}

	// The Manager addresses the tracked post per thread.
	if key := ch.ToolFeedbackMessageChatID(testChanID, &final); key != testChanID+"/" {
		t.Errorf("tool feedback key = %q", key)
	}
}
