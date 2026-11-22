package mattermost

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
)

// Review 2, fix 2: a feedback post that is still being updated is not stale;
// a successful patch refreshes its timestamp.
func TestToolFeedbackPatchRefreshesTimestamp(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, _ := newTestChannel(t, fm, nil, nil)
	startConnected(t, fm, ch)

	key := threadKey(testChanID, "p9")
	old := time.Now().Add(-indicatorTTL + time.Minute) // close to expiring
	ch.indMu.Lock()
	ch.feedback[key] = feedbackPost{id: "livefb", at: old}
	ch.indMu.Unlock()

	msg := bus.OutboundMessage{ChatID: testChanID, Content: "🔧 step 42", Context: bus.InboundContext{
		ChatID: testChanID, MessageID: testChanID + ":p9", Raw: map[string]string{"message_kind": "tool_feedback"},
	}}
	if _, err := ch.Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if p := fm.callsTo(http.MethodPut, "/api/v4/posts/livefb/patch"); len(p) != 1 {
		t.Fatalf("feedback not patched: %v", p)
	}
	ch.indMu.Lock()
	fb := ch.feedback[key]
	ch.indMu.Unlock()
	if fb.id != "livefb" || !fb.at.After(old.Add(30*time.Second)) {
		t.Errorf("timestamp not refreshed: %+v (was %v)", fb, old)
	}
}

// Review 2, fix 3: a channel post that was only "@bot" (no content, not
// handed to the agent) and is then edited into a real question is a new
// request.
func TestPostEditedFromBareMentionIsNewRequest(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, mb := newTestChannel(t, fm, nil, nil)
	conn := startConnected(t, fm, ch)

	sendPosted(t, conn, "O", post("m1", testUserID, "@picobot", ""), []string{testBotID})
	expectNoInbound(t, mb)
	sendEdited(t, conn, post("m1", testUserID, "@picobot what is X?", ""))
	in := nextInbound(t, mb)
	if !strings.HasSuffix(in.Content, "what is X?") || !in.Context.Mentioned {
		t.Errorf("edited bare mention: content=%q ctx=%+v", in.Content, in.Context)
	}
}

func TestMattermostCanonicalMessageRef(t *testing.T) {
	ch := &MattermostChannel{}
	for _, tc := range []struct{ chatID, ref, want string }{
		{"chanA", "chanA:post1", "chanA:post1"},
		{"chanA", "chanA:root:post1", "chanA:post1"},
		{"chanA/root", "post1", "chanA:post1"},
		{"chanA", "chanB:post1", "chanB:post1"},
		{"chanA", "", ""},
	} {
		if got := ch.CanonicalMessageRef(tc.chatID, tc.ref); got != tc.want {
			t.Errorf("CanonicalMessageRef(%q, %q) = %q, want %q", tc.chatID, tc.ref, got, tc.want)
		}
	}
}
