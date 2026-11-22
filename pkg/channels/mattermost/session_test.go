package mattermost

import (
	"context"
	"net/http"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/routing"
	"github.com/sipeed/picoclaw/pkg/session"
)

const testDMChanID = "dmchannelid000000000000000"

func postIn(channelID, id, userID, message, rootID string) map[string]any {
	p := post(id, userID, message, rootID)
	p["channel_id"] = channelID
	return p
}

// sessionKeyFor runs an inbound context through the real routing and
// session allocation used by the agent (default config).
func sessionKeyFor(t *testing.T, inbound bus.InboundContext) session.Allocation {
	t.Helper()
	route := routing.NewRouteResolver(config.DefaultConfig()).ResolveRoute(inbound)
	return session.AllocateRouteSession(session.AllocationInput{
		AgentID:       route.AgentID,
		Context:       inbound,
		SessionPolicy: route.SessionPolicy,
	})
}

// Threads must not split sessions: every post of one Mattermost channel
// (any thread, top-level or reply) and every DM in one DM channel maps to the
// same session key, like a Telegram group / private chat.
func TestSessionKeySharedAcrossThreadsAndDMs(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, mb := newTestChannel(t, fm, nil, nil)
	conn := startConnected(t, fm, ch)

	sendPosted(t, conn, "O", postIn(testChanID, "a1", testUserID, "@picobot first topic", ""), []string{testBotID})
	sendPosted(t, conn, "O", postIn(testChanID, "b1", testUserID, "@picobot second topic", ""), []string{testBotID})
	sendPosted(t, conn, "O", postIn(testChanID, "a2", testUserID, "@picobot more on a", "a1"), []string{testBotID})
	sendPosted(t, conn, "D", postIn(testDMChanID, "d1", testUserID, "hi", ""), nil)
	sendPosted(t, conn, "D", postIn(testDMChanID, "d2", testUserID, "another top-level DM", ""), nil)
	sendPosted(t, conn, "D", postIn(testDMChanID, "d3", testUserID, "reply in thread", "d1"), nil)

	allocs := make(map[string]session.Allocation)
	for range 6 {
		msg := nextInbound(t, mb)
		allocs[msg.Context.MessageID] = sessionKeyFor(t, msg.Context)
	}

	channelKey := allocs[testChanID+":a1"].SessionKey
	dmKey := allocs[testDMChanID+":d1"].SessionKey
	if channelKey == "" || dmKey == "" || channelKey == dmKey {
		t.Fatalf("channel key %q, dm key %q: want distinct non-empty keys", channelKey, dmKey)
	}
	for _, id := range []string{testChanID + ":b1", testChanID + ":a1:a2"} {
		if got := allocs[id].SessionKey; got != channelKey {
			t.Errorf("%s session key = %q, want channel session %q", id, got, channelKey)
		}
		if got, want := allocs[id].SessionAliases, allocs[testChanID+":a1"].SessionAliases; !equalStrings(got, want) {
			t.Errorf("%s legacy aliases = %v, want %v", id, got, want)
		}
	}
	for _, id := range []string{testDMChanID + ":d2", testDMChanID + ":d1:d3"} {
		if got := allocs[id].SessionKey; got != dmKey {
			t.Errorf("%s session key = %q, want DM session %q", id, got, dmKey)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Two addressed posts in different threads of one channel: the second is
// deferred (own turn instead of being steered into the first), each thread
// gets its own placeholder, and each reply lands in its own thread.
func TestConcurrentThreadsAnsweredInOwnThread(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, mb := newTestChannel(t, fm, nil, func(bc *config.Channel, _ *config.MattermostSettings) {
		bc.Placeholder.Enabled = true
		bc.Placeholder.Text = config.FlexibleStringSlice{"Thinking..."}
	})
	conn := startConnected(t, fm, ch)
	ctx := context.Background()

	sendPosted(t, conn, "O", post("ta", testUserID, "@picobot question A", ""), []string{testBotID})
	sendPosted(t, conn, "O", post("tb", testUserID, "@picobot question B", ""), []string{testBotID})
	sendPosted(t, conn, "O", post("ta2", testUserID, "@picobot also A", "ta"), []string{testBotID})
	inA, inB, inA2 := nextInbound(t, mb), nextInbound(t, mb), nextInbound(t, mb)
	if inA.DeferWhileBusy || !inB.DeferWhileBusy || inA2.DeferWhileBusy {
		t.Errorf("defer flags A=%v B=%v A2=%v, want false/true/false (same-thread follow-up is steered)",
			inA.DeferWhileBusy, inB.DeferWhileBusy, inA2.DeferWhileBusy)
	}

	placeholders := map[string]string{} // root -> placeholder post id
	for _, p := range fm.callsTo(http.MethodPost, "/api/v4/posts") {
		root, _ := p.Body["root_id"].(string)
		placeholders[root] = p.ID
	}
	if len(placeholders) != 2 || placeholders["ta"] == "" || placeholders["tb"] == "" {
		t.Fatalf("want one placeholder per thread, got %v", placeholders)
	}

	// Replies arrive out of order; each carries its own turn context.
	final := map[string]string{"outbound_kind": "final"}
	inB.Context.Raw, inA.Context.Raw = final, final
	if _, err := ch.Send(
		ctx,
		bus.OutboundMessage{ChatID: testChanID, Content: "answer B", Context: inB.Context},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := ch.Send(
		ctx,
		bus.OutboundMessage{ChatID: testChanID, Content: "answer A", Context: inA.Context},
	); err != nil {
		t.Fatal(err)
	}
	for root, content := range map[string]string{"tb": "answer B", "ta": "answer A"} {
		patches := fm.callsTo(http.MethodPut, "/api/v4/posts/"+placeholders[root]+"/patch")
		if len(patches) != 1 || patches[0].Body["message"] != content {
			t.Errorf("thread %s placeholder patches = %v, want %q", root, patches, content)
		}
	}
	if n := len(fm.callsTo(http.MethodPost, "/api/v4/posts")); n != 2 {
		t.Errorf("replies must reuse the placeholders, got %d posts", n)
	}

	// Both turns are done: a new thread is no longer deferred.
	sendPosted(t, conn, "O", post("tc", testUserID, "@picobot question C", ""), []string{testBotID})
	if inC := nextInbound(t, mb); inC.DeferWhileBusy {
		t.Error("post after all turns finished should not be deferred")
	}
}
