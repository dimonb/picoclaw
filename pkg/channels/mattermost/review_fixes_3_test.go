package mattermost

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
)

// Review 3, fix 3: one tool call can run longer than indicatorTTL. Activity
// in another thread must not sweep the running turn's progress post or its
// placeholder; once the turn ends (or is abandoned past activeTurnMaxAge)
// stale posts are swept as before.
func TestRunningTurnPostsAreNotSwept(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, mb := newTestChannel(t, fm, nil, withIndicators)
	conn := startConnected(t, fm, ch)
	ctx := context.Background()
	toolFeedback := func(in bus.InboundMessage, text string) {
		t.Helper()
		msg := bus.OutboundMessage{ChatID: testChanID, Content: text, Context: in.Context}
		msg.Context.Raw = map[string]string{"message_kind": "tool_feedback"}
		if _, err := ch.Send(ctx, msg); err != nil {
			t.Fatal(err)
		}
	}
	age := func(key string) {
		ch.indMu.Lock()
		defer ch.indMu.Unlock()
		if fb, ok := ch.feedback[key]; ok {
			ch.feedback[key] = feedbackPost{id: fb.id, at: time.Now().Add(-2 * indicatorTTL)}
		}
		if ind, ok := ch.indicators[key]; ok {
			ind.created = time.Now().Add(-2 * indicatorTTL)
		}
	}

	// Thread A: a turn with one tool call that has been running for an hour.
	sendPosted(t, conn, "O", post("a1", testUserID, "@picobot long task", ""), []string{testBotID})
	inA := nextInbound(t, mb)
	toolFeedback(inA, "🔧 exec (long)")
	keyA := threadKey(testChanID, "a1")
	ch.indMu.Lock()
	fbA, placeholderA := ch.feedback[keyA].id, ch.indicators[keyA].placeholderID
	ch.indMu.Unlock()
	age(keyA)

	// Thread B: a new request and its tool feedback, both trigger the sweeps.
	sendPosted(t, conn, "O", post("b1", testUserID, "@picobot quick", ""), []string{testBotID})
	inB := nextInbound(t, mb)
	toolFeedback(inB, "🔧 web_search")

	if d := fm.callsTo(http.MethodDelete, "/api/v4/posts/"+fbA); len(d) != 0 {
		t.Errorf("running turn's feedback post deleted: %v", d)
	}
	ch.indMu.Lock()
	_, fbTracked := ch.feedback[keyA]
	_, indTracked := ch.indicators[keyA]
	ch.indMu.Unlock()
	if !fbTracked || !indTracked {
		t.Fatalf("running turn lost its posts: feedback=%v indicators=%v", fbTracked, indTracked)
	}

	// A's answer still reuses its placeholder and removes the feedback post.
	final := inA.Context
	final.ReplyToMessageID = ""
	final.Raw = map[string]string{"outbound_kind": "final"}
	if _, err := ch.Send(ctx, bus.OutboundMessage{ChatID: testChanID, Content: "done", Context: final}); err != nil {
		t.Fatal(err)
	}
	if p := fm.callsTo(http.MethodPut, "/api/v4/posts/"+placeholderA+"/patch"); len(p) != 1 {
		t.Errorf("answer must edit A's placeholder: %v", p)
	}
	if d := fm.callsTo(http.MethodDelete, "/api/v4/posts/"+fbA); len(d) != 1 {
		t.Errorf("A's feedback post must be removed by its answer: %v", d)
	}

	// B is abandoned (e.g. stopped): a later turn of the channel ends, so
	// B's turn is over and its stale feedback post is swept.
	keyB := threadKey(testChanID, "b1")
	ch.indMu.Lock()
	fbB := ch.feedback[keyB].id
	ch.indMu.Unlock()
	age(keyB)
	ch.threads.admit(testChanID, "c1", true)
	ch.threads.done(testChanID, "c1")
	ch.sweepFeedback(ctx)
	if d := fm.callsTo(http.MethodDelete, "/api/v4/posts/"+fbB); len(d) != 1 {
		t.Errorf("abandoned turn's stale feedback post not swept: %v", d)
	}
}

// An unfinished turn counts as running only up to activeTurnMaxAge.
func TestTurnActiveExpires(t *testing.T) {
	var q threadQueue
	q.admit("ch", "r1", true)
	key := threadKey("ch", "r1")
	if !q.turnActive(key) {
		t.Fatal("admitted thread must be active")
	}
	q.mu.Lock()
	q.active[key] = time.Now().Add(-activeTurnMaxAge - time.Minute)
	q.mu.Unlock()
	if q.turnActive(key) {
		t.Error("turn older than activeTurnMaxAge must not count as running")
	}
	q.admit("ch", "r2", true)
	q.done("ch", "r2")
	if q.turnActive(threadKey("ch", "r2")) {
		t.Error("finished turn still active")
	}
}
