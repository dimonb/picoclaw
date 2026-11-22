package mattermost

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sipeed/picoclaw/pkg/bus"
)

func sendEdited(t *testing.T, conn *websocket.Conn, post map[string]any) {
	t.Helper()
	post["edit_at"] = time.Now().UnixMilli()
	if _, ok := post["create_at"]; !ok {
		post["create_at"] = time.Now().Add(-time.Minute).UnixMilli()
	}
	postJSON, _ := json.Marshal(post)
	v, _ := seqCounter.LoadOrStore(conn, new(int64))
	seq := v.(*int64)
	*seq++
	if err := conn.WriteJSON(map[string]any{
		"event": "post_edited", "seq": *seq, "data": map[string]any{"post": string(postJSON)},
	}); err != nil {
		t.Fatalf("write post_edited: %v", err)
	}
}

func expectNoInbound(t *testing.T, mb *bus.MessageBus) {
	t.Helper()
	select {
	case msg := <-mb.InboundChan():
		t.Fatalf("unexpected inbound: %q", msg.Content)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestPostEditedReachesAgent(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, mb := newTestChannel(t, fm, nil, nil)
	conn := startConnected(t, fm, ch)

	// 1. A DM the bot handled is corrected: the agent gets the new text with
	// the same message ID, marked as an edit, without 👀 up front.
	sendPosted(t, conn, "D", post("d1", testUserID, "meet at 5", ""), nil)
	nextInbound(t, mb)
	reactionsBefore := len(fm.callsTo(http.MethodPost, "/api/v4/reactions"))
	sendEdited(t, conn, post("d1", testUserID, "meet at 6", ""))
	in := nextInbound(t, mb)
	if !strings.HasPrefix(in.Content, editedPrefix) || !strings.HasSuffix(in.Content, "meet at 6") {
		t.Errorf("edited content = %q", in.Content)
	}
	if in.Context.MessageID != testChanID+":d1" || in.Context.Raw["edited"] != "true" ||
		in.Context.ChatType != "direct" {
		t.Errorf("edited context = %+v", in.Context)
	}
	if n := len(fm.callsTo(http.MethodPost, "/api/v4/reactions")); n != reactionsBefore {
		t.Error("a correction must not get the automatic 👀")
	}

	// 2. Same text again (metadata update / repeated event): ignored.
	sendEdited(t, conn, post("d1", testUserID, "meet at 6", ""))
	expectNoInbound(t, mb)

	// 3. An old post the bot never handled: ignored.
	old := post("d0", testUserID, "ancient", "")
	old["create_at"] = time.Now().Add(-48 * time.Hour).UnixMilli()
	sendEdited(t, conn, old)
	expectNoInbound(t, mb)
}

// mention mode: a channel post edited to add the @mention now addresses the
// bot (channel type comes from the API since no event announced it).
func TestPostEditedAddsMention(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, mb := newTestChannel(t, fm, nil, nil)
	ch.groupMode = groupModeMention
	conn := startConnected(t, fm, ch)

	sendEdited(t, conn, post("c1", testUserID, "lunch?", ""))
	expectNoInbound(t, mb)
	sendEdited(t, conn, post("c1", testUserID, "@picobot lunch?", ""))
	in := nextInbound(t, mb)
	if !strings.HasSuffix(in.Content, "lunch?") || !in.Context.Mentioned || in.Context.ChatType != "channel" {
		t.Errorf("edit with mention: content=%q ctx=%+v", in.Content, in.Context)
	}
	if calls := fm.callsTo(http.MethodGet, "/api/v4/channels/"+testChanID); len(calls) != 1 {
		t.Errorf("channel lookups = %d, want 1", len(calls))
	}
}

// The agent edits/deletes its own posts by the IDs it saw (history or the
// message tool result), in DMs and channels, top-level and in threads.
func TestEditAndDeleteByHistoryIDs(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, _ := newTestChannel(t, fm, nil, nil)
	startConnected(t, fm, ch)
	ctx := context.Background()

	targets := []bus.InboundContext{
		{ChatID: testChanID, ChatType: "direct", MessageID: testChanID + ":d1"},     // DM top-level
		{ChatID: testChanID, ChatType: "direct", MessageID: testChanID + ":r1:d2"},  // DM thread
		{ChatID: testChanID, ChatType: "channel", MessageID: testChanID + ":c1"},    // channel → thread c1
		{ChatID: testChanID, ChatType: "channel", MessageID: testChanID + ":r2:c2"}, // channel thread
	}
	for _, target := range targets {
		final := target
		final.Raw = map[string]string{"outbound_kind": "final"}
		ids, err := ch.Send(ctx, bus.OutboundMessage{ChatID: testChanID, Content: "draft", Context: final})
		if err != nil || len(ids) != 1 {
			t.Fatalf("Send: %v %v", ids, err)
		}
		postID := parseMessageID(ids[0]).postID
		if err := ch.EditMessage(ctx, testChanID, ids[0], "fixed"); err != nil {
			t.Fatal(err)
		}
		if err := ch.DeleteMessage(ctx, testChanID, ids[0]); err != nil {
			t.Fatal(err)
		}
		if p := fm.callsTo(http.MethodPut, "/api/v4/posts/"+postID+"/patch"); len(p) != 1 ||
			p[0].Body["message"] != "fixed" {
			t.Errorf("%s: edit by %q = %v", target.MessageID, ids[0], p)
		}
		if d := fm.callsTo(http.MethodDelete, "/api/v4/posts/"+postID); len(d) != 1 {
			t.Errorf("%s: delete by %q = %v", target.MessageID, ids[0], d)
		}
	}
}
