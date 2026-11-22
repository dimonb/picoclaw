package mattermost

import (
	"context"
	"net/http"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
)

// Smart mode (default): every channel post reaches the agent; only addressed
// posts are flagged Mentioned and get the 👀/typing/placeholder up front.
func TestSmartModeDeliversEveryPost(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, mb := newTestChannel(t, fm, nil, nil)
	conn := startConnected(t, fm, ch)
	if ch.groupMode != groupModeSmart {
		t.Fatalf("default group mode = %q, want smart", ch.groupMode)
	}

	sendPosted(t, conn, "O", post("s1", testUserID, "anyone tried the new build?", ""), nil)
	chatter := nextInbound(t, mb)
	if chatter.Context.MessageID != testChanID+":s1" || chatter.Context.Mentioned || chatter.DeferWhileBusy {
		t.Errorf("chatter: id=%q mentioned=%v deferred=%v", chatter.Context.MessageID, chatter.Context.Mentioned,
			chatter.DeferWhileBusy)
	}
	if n := len(fm.callsTo(http.MethodPost, "/api/v4/reactions")); n != 0 {
		t.Errorf("non-addressed post got %d reactions, want none up front", n)
	}

	sendPosted(t, conn, "O", post("s2", testUserID, "@picobot can you check?", ""), []string{testBotID})
	addressed := nextInbound(t, mb)
	if !addressed.Context.Mentioned || addressed.Content != "can you check?" {
		t.Errorf("mention: mentioned=%v content=%q", addressed.Context.Mentioned, addressed.Content)
	}
	if adds := fm.callsTo(http.MethodPost, "/api/v4/reactions"); len(adds) != 1 || adds[0].Body["post_id"] != "s2" {
		t.Errorf("addressed post reactions = %v", adds)
	}
	// A further chatter post in another thread is steered (one reply per
	// burst), an addressed one would be deferred.
	sendPosted(t, conn, "O", post("s3", testUserID, "lol", ""), nil)
	if burst := nextInbound(t, mb); burst.DeferWhileBusy {
		t.Error("non-addressed burst posts must not be deferred")
	}
}

// Mention mode drops non-addressed channel posts; in both modes a follow-up
// in a thread the bot takes part in counts as addressed.
func TestThreadFollowUpCountsAsAddressed(t *testing.T) {
	for _, mode := range []string{"mention", "smart"} {
		t.Run(mode, func(t *testing.T) {
			fm := newFakeMattermost(t)
			ch, mb := newTestChannel(t, fm, nil, func(_ *config.Channel, s *config.MattermostSettings) {
				s.GroupMode = mode
			})
			conn := startConnected(t, fm, ch)

			// The bot answers a mention in thread m1 ...
			sendPosted(t, conn, "O", post("m1", testUserID, "@picobot hi", ""), []string{testBotID})
			in := nextInbound(t, mb)
			if _, err := ch.Send(context.Background(), bus.OutboundMessage{
				ChatID: testChanID, Content: "hello", Context: in.Context,
			}); err != nil {
				t.Fatal(err)
			}
			// ... a post in an unrelated thread is not addressed ...
			sendPosted(t, conn, "O", post("x2", testUserID, "unrelated reply", "other"), nil)
			// ... a follow-up in m1 without a mention is, and so is a thread the
			// bot replied in before a restart (looked up via the API).
			sendPosted(t, conn, "O", post("m2", testUserID, "and what about tomorrow?", "m1"), nil)
			sendPosted(t, conn, "O", post("b2", testUserID, "still broken", "botroot"), nil)

			if mode == "smart" {
				if got := nextInbound(t, mb); got.Context.MessageID != testChanID+":other:x2" || got.Context.Mentioned {
					t.Errorf("unrelated: id=%q mentioned=%v", got.Context.MessageID, got.Context.Mentioned)
				}
			}
			for _, want := range []string{testChanID + ":m1:m2", testChanID + ":botroot:b2"} {
				got := nextInbound(t, mb)
				if got.Context.MessageID != want || !got.Context.Mentioned {
					t.Errorf("got id=%q mentioned=%v, want addressed %q", got.Context.MessageID,
						got.Context.Mentioned, want)
				}
			}
		})
	}
}

// NO_REPLY for an addressed post removes the thread's placeholder and 👀.
func TestHandleSilentReplyCleansThread(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, mb := newTestChannel(t, fm, nil, func(bc *config.Channel, _ *config.MattermostSettings) {
		bc.Placeholder.Enabled = true
		bc.Placeholder.Text = config.FlexibleStringSlice{"Thinking..."}
	})
	conn := startConnected(t, fm, ch)

	sendPosted(t, conn, "O", post("n1", testUserID, "@picobot thanks!", ""), []string{testBotID})
	in := nextInbound(t, mb)
	placeholders := fm.callsTo(http.MethodPost, "/api/v4/posts")
	if len(placeholders) != 1 {
		t.Fatalf("placeholders = %v", placeholders)
	}

	silent := bus.OutboundMessage{ChatID: testChanID, Context: in.Context}
	silent.Context.Raw = map[string]string{"message_kind": bus.MessageKindSilent, "outbound_kind": "final"}
	ch.HandleSilentReply(context.Background(), silent)

	if dels := fm.callsTo(http.MethodDelete, "/api/v4/posts/"+placeholders[0].ID); len(dels) != 1 {
		t.Errorf("placeholder not deleted: %v", dels)
	}
	if dels := fm.callsTo(http.MethodDelete, "/api/v4/users/"+testBotID+"/posts/n1/reactions/eyes"); len(dels) != 1 {
		t.Errorf("reaction not removed: %v", dels)
	}
	if n := len(fm.callsTo(http.MethodPost, "/api/v4/posts")); n != 1 {
		t.Errorf("silent reply must not post, got %d posts", n)
	}
}

func TestMessageReactor(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, _ := newTestChannel(t, fm, nil, nil)
	startConnected(t, fm, ch)
	ctx := context.Background()

	if err := ch.SetMessageReaction(ctx, testChanID, testChanID+":r:p1", "👍"); err != nil {
		t.Fatal(err)
	}
	if err := ch.SetMessageReaction(ctx, testChanID, "p2", ":tada:"); err != nil {
		t.Fatal(err)
	}
	if err := ch.SetMessageReaction(ctx, testChanID, "p3", "🦄"); err == nil {
		t.Error("unsupported emoji should fail")
	}
	adds := fm.callsTo(http.MethodPost, "/api/v4/reactions")
	if len(adds) != 2 || adds[0].Body["post_id"] != "p1" || adds[0].Body["emoji_name"] != "+1" ||
		adds[1].Body["emoji_name"] != "tada" {
		t.Errorf("reactions = %v", adds)
	}
	if err := ch.RemoveMessageReaction(ctx, testChanID, testChanID+":p1", "👍"); err != nil {
		t.Fatal(err)
	}
	if dels := fm.callsTo(http.MethodDelete, "/api/v4/users/"+testBotID+"/posts/p1/reactions/+1"); len(dels) != 1 {
		t.Errorf("remove = %v", dels)
	}
	if support := ch.GetReactionSupport(ctx, testChanID); support.AnyUnicode ||
		len(support.Allowed) != len(reactionNames) {
		t.Errorf("support = %+v", support)
	}
}
