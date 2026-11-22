package mattermost

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
)

// Review fix 1: DeleteMessage only deletes the bot's own posts in the chat's
// channel, checked against the server (post.user_id, post.channel_id).
func TestDeleteMessageOnlyOwnPostsInChannel(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, _ := newTestChannel(t, fm, nil, nil)
	startConnected(t, fm, ch)
	ctx := context.Background()

	fm.addPost("mine", testBotID, testChanID)
	fm.addPost("theirs", testUserID, testChanID)
	fm.addPost("elsewhere", testBotID, "otherchannel00000000000000")

	if err := ch.DeleteMessage(ctx, testChanID, testChanID+":mine"); err != nil {
		t.Fatalf("own post: %v", err)
	}
	for _, id := range []string{
		testChanID + ":theirs",           // someone else's post
		"theirs",                         // bare ID, still someone else's
		"elsewhere",                      // the bot's post, other channel (bare ID)
		"otherchannel00000000000000:x:y", // encoded ID into another channel
		testChanID + ":missing",          // unknown post
	} {
		err := ch.DeleteMessage(ctx, testChanID, id)
		if err == nil {
			t.Errorf("delete %q must be refused", id)
		}
		if id != testChanID+":missing" && !errors.Is(err, channels.ErrNotOwnMessage) {
			t.Errorf("delete %q: err = %v, want ErrNotOwnMessage", id, err)
		}
	}
	dels := fm.callsTo(http.MethodDelete, "/api/v4/posts/")
	if len(dels) != 1 || dels[0].Path != "/api/v4/posts/mine" {
		t.Errorf("deletes = %v, want only the bot's own post", dels)
	}
	if own, err := ch.IsOwnMessage(ctx, testChanID, "theirs"); err != nil || own {
		t.Errorf("IsOwnMessage(theirs) = %v, %v", own, err)
	}
}

// Review fix 6: an edit in another thread while a turn runs waits for its own
// turn instead of being steered into the running one (wrong thread).
func TestPostEditedInOtherThreadIsDeferred(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, mb := newTestChannel(t, fm, nil, nil) // smart mode
	conn := startConnected(t, fm, ch)
	ctx := context.Background()

	sendPosted(t, conn, "O", post("eb", testUserID, "topic B", ""), nil)
	inB := nextInbound(t, mb)
	ch.HandleSilentReply(ctx, silentFor(inB)) // thread eb's turn is over

	sendPosted(t, conn, "O", post("ea", testUserID, "topic A", ""), nil)
	if inA := nextInbound(t, mb); inA.DeferWhileBusy {
		t.Fatal("first post of a new turn must not be deferred")
	} // thread ea's turn is now running

	sendEdited(t, conn, post("eb", testUserID, "topic B, corrected", ""))
	edit := nextInbound(t, mb)
	if edit.Context.Raw["edited"] != "true" || !edit.DeferWhileBusy {
		t.Errorf("edit in another thread: edited=%q defer=%v, want deferred", edit.Context.Raw["edited"],
			edit.DeferWhileBusy)
	}
	if adds := fm.callsTo(http.MethodPost, "/api/v4/reactions"); len(adds) != 0 {
		t.Errorf("a correction shows no indicators: %v", adds)
	}

	// A correction in the running thread itself is still steered into it.
	sendEdited(t, conn, post("ea", testUserID, "topic A, corrected", ""))
	if same := nextInbound(t, mb); same.DeferWhileBusy {
		t.Error("edit in the running thread must not be deferred")
	}
}

// Review fix 7: a tool-feedback post nobody claimed (e.g. the turn was
// stopped) is deleted after indicatorTTL.
func TestStaleToolFeedbackIsSwept(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, _ := newTestChannel(t, fm, nil, nil)
	startConnected(t, fm, ch)

	ch.indMu.Lock()
	ch.feedback[threadKey(testChanID, "old")] = feedbackPost{id: "stalefb", at: time.Now().Add(-2 * indicatorTTL)}
	ch.feedback[threadKey(testChanID, "new")] = feedbackPost{id: "freshfb", at: time.Now()}
	ch.indMu.Unlock()

	msg := bus.OutboundMessage{ChatID: testChanID, Content: "🔧 exec", Context: bus.InboundContext{
		ChatID: testChanID, MessageID: testChanID + ":p9", Raw: map[string]string{"message_kind": "tool_feedback"},
	}}
	if _, err := ch.Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if d := fm.callsTo(http.MethodDelete, "/api/v4/posts/stalefb"); len(d) != 1 {
		t.Errorf("stale feedback post not deleted: %v", d)
	}
	if d := fm.callsTo(http.MethodDelete, "/api/v4/posts/freshfb"); len(d) != 0 {
		t.Errorf("fresh feedback post deleted: %v", d)
	}
	ch.indMu.Lock()
	_, stale := ch.feedback[threadKey(testChanID, "old")]
	ch.indMu.Unlock()
	if stale {
		t.Error("stale entry still tracked")
	}
}

// Review fix 8: when full, postMemory evicts the oldest post, not a random one.
func TestPostMemoryEvictsOldest(t *testing.T) {
	for run := 0; run < 20; run++ { // map iteration order would make a random pick show up
		m := postMemory{max: 3}
		for _, id := range []string{"p1", "p2", "p3", "p4"} {
			m.markDelivered(id, "text "+id, false)
		}
		if k := m.classifyEdit("p1", "changed", false); k != editIgnore {
			t.Fatalf("oldest post p1 must be evicted (got %v)", k)
		}
		for _, id := range []string{"p2", "p3", "p4"} {
			if k := m.classifyEdit(id, "changed "+id, false); k != editCorrection {
				t.Fatalf("run %d: %s evicted instead of the oldest", run, id)
			}
		}
	}
}

// Review fix 9: with tool_feedback.separate_messages each feedback is its own
// post and stays; the answer neither reuses nor deletes it (Manager path).
func TestToolFeedbackSeparateMessages(t *testing.T) {
	for _, separate := range []bool{true, false} {
		fm := newFakeMattermost(t)
		ch, mb := newTestChannel(t, fm, nil, nil)
		cfg := &config.Config{}
		cfg.Agents.Defaults.ToolFeedback.Enabled = true
		cfg.Agents.Defaults.ToolFeedback.SeparateMessages = separate
		m, err := channels.NewManager(cfg, mb, nil)
		if err != nil {
			t.Fatal(err)
		}
		m.RegisterChannel("mattermost", ch)
		if err := m.StartAll(context.Background()); err != nil {
			t.Fatal(err)
		}
		inCtx := bus.InboundContext{Channel: "mattermost", ChatID: testChanID, MessageID: testChanID + ":q1"}
		send := func(content, kind string) {
			t.Helper()
			c := inCtx
			c.Raw = map[string]string{"message_kind": kind}
			if kind == "" {
				c.Raw = map[string]string{"outbound_kind": "final"}
			}
			if err := m.SendMessage(context.Background(), bus.OutboundMessage{
				Channel: "mattermost", ChatID: testChanID, Content: content, Context: c,
			}); err != nil {
				t.Fatal(err)
			}
		}
		send("🔧 one", "tool_feedback")
		send("🔧 two", "tool_feedback")
		send("answer", "")

		posts := len(fm.callsTo(http.MethodPost, "/api/v4/posts"))
		dels := len(fm.callsTo(http.MethodDelete, "/api/v4/posts/"))
		if separate && (posts != 3 || dels != 0) {
			t.Errorf("separate: posts=%d deletes=%d, want 3 posts kept", posts, dels)
		}
		if !separate && posts != 1 {
			t.Errorf("combined: posts=%d, want one feedback post edited into the answer", posts)
		}
		_ = m.StopAll(context.Background())
	}
}

// Same rule for edits: only the bot's own posts in the chat's channel.
func TestEditMessageOnlyOwnPostsInChannel(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, _ := newTestChannel(t, fm, nil, nil)
	startConnected(t, fm, ch)
	ctx := context.Background()
	fm.addPost("mine", testBotID, testChanID)
	fm.addPost("theirs", testUserID, testChanID)
	fm.addPost("elsewhere", testBotID, "otherchannel00000000000000")

	if err := ch.EditMessage(ctx, testChanID, testChanID+":mine", "fixed"); err != nil {
		t.Fatalf("own post: %v", err)
	}
	for _, id := range []string{"theirs", "elsewhere", "otherchannel00000000000000:y"} {
		if err := ch.EditMessage(ctx, testChanID, id, "hijack"); !errors.Is(err, channels.ErrNotOwnMessage) {
			t.Errorf("edit %q: err = %v, want ErrNotOwnMessage", id, err)
		}
	}
	if p := fm.callsTo(http.MethodPut, "/api/v4/posts/"); len(p) != 1 || p[0].Path != "/api/v4/posts/mine/patch" {
		t.Errorf("patches = %v, want only the bot's own post", p)
	}
}
