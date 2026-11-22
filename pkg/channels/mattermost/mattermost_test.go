package mattermost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
)

const (
	testToken    = "test-bot-token"
	testBotID    = "botuserid0000000000000000a"
	testBotName  = "picobot"
	testUserID   = "humanuserid000000000000000"
	testOtherID  = "otheruserid000000000000000"
	testChanID   = "channelid00000000000000000"
	testWaitTime = 3 * time.Second
)

type apiCall struct {
	Method string
	Path   string
	Body   map[string]any
	ID     string // ID returned for created posts
}

// fakeMattermost emulates the subset of the Mattermost v4 REST and
// WebSocket APIs used by the channel.
type fakeMattermost struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	calls    []apiCall
	nextID   int
	postCode int // when non-zero, POST /posts fails with this status

	wsAuth  chan string
	wsConns chan *websocket.Conn
}

func newFakeMattermost(t *testing.T) *fakeMattermost {
	t.Helper()
	fm := &fakeMattermost{
		t:       t,
		wsAuth:  make(chan string, 4),
		wsConns: make(chan *websocket.Conn, 4),
	}
	fm.srv = httptest.NewServer(http.HandlerFunc(fm.handle))
	t.Cleanup(fm.srv.Close)
	return fm
}

func (fm *fakeMattermost) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/v4/websocket" {
		fm.serveWS(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		http.Error(w, `{"message":"invalid token"}`, http.StatusUnauthorized)
		return
	}

	call := apiCall{Method: r.Method, Path: r.URL.Path}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &call.Body)
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(1 << 20); err == nil {
			call.Body = map[string]any{"channel_id": r.FormValue("channel_id")}
			if fhs := r.MultipartForm.File["files"]; len(fhs) > 0 {
				call.Body["filename"] = fhs[0].Filename
			}
		}
	}

	fm.mu.Lock()
	fm.nextID++
	id := fmt.Sprintf("post%022d", fm.nextID)
	call.ID = id
	fm.calls = append(fm.calls, call)
	postCode := fm.postCode
	fm.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v4/users/me":
		_ = json.NewEncoder(w).Encode(map[string]string{"id": testBotID, "username": testBotName})
	case r.Method == http.MethodGet && r.URL.Path == "/api/v4/users/"+testUserID:
		_ = json.NewEncoder(w).Encode(map[string]string{
			"id": testUserID, "username": "alice", "first_name": "Alice", "last_name": "Smith",
		})
	case r.Method == http.MethodGet && r.URL.Path == "/api/v4/posts/botroot/thread":
		_ = json.NewEncoder(w).Encode(map[string]any{"posts": map[string]any{
			"botroot": map[string]string{"user_id": testUserID},
			"botrep":  map[string]string{"user_id": testBotID},
		}})
	case r.Method == http.MethodGet && r.URL.Path == "/api/v4/teams/name/myteam":
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "teamid1"})
	case r.Method == http.MethodPost && r.URL.Path == "/api/v4/posts":
		if postCode != 0 {
			w.WriteHeader(postCode)
			_, _ = w.Write([]byte(`{"message":"boom"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
	case r.Method == http.MethodPost && r.URL.Path == "/api/v4/files":
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"file_infos": []map[string]string{{"id": "file-" + id}}})
	default:
		_, _ = w.Write([]byte(`{"status":"OK"}`))
	}
}

func (fm *fakeMattermost) serveWS(w http.ResponseWriter, r *http.Request) {
	fm.wsAuth <- r.Header.Get("Authorization")
	up := websocket.Upgrader{}
	conn, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	if err := conn.WriteJSON(map[string]any{
		"event": "hello", "seq": 0, "data": map[string]string{"connection_id": "conn1"},
	}); err != nil {
		return
	}
	fm.wsConns <- conn
	// Drain client frames (auth challenge, control frames) until closed.
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

func (fm *fakeMattermost) callsTo(method, prefix string) []apiCall {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	var out []apiCall
	for _, c := range fm.calls {
		if c.Method == method && strings.HasPrefix(c.Path, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func newTestChannel(
	t *testing.T,
	fm *fakeMattermost,
	allowFrom []string,
	mutate func(*config.Channel, *config.MattermostSettings),
) (*MattermostChannel, *bus.MessageBus) {
	t.Helper()
	mb := bus.NewMessageBus()
	bc := &config.Channel{Type: config.ChannelMattermost, Enabled: true, AllowFrom: allowFrom}
	cfg := &config.MattermostSettings{URL: fm.srv.URL + "/"}
	cfg.BotToken = *config.NewSecureString(testToken)
	if mutate != nil {
		mutate(bc, cfg)
	}
	ch, err := NewMattermostChannel(bc, cfg, mb)
	if err != nil {
		t.Fatalf("NewMattermostChannel: %v", err)
	}
	return ch, mb
}

// startConnected starts the channel and returns the server side of its
// WebSocket connection.
func startConnected(t *testing.T, fm *fakeMattermost, ch *MattermostChannel) *websocket.Conn {
	t.Helper()
	if err := ch.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), testWaitTime)
		defer cancel()
		if err := ch.Stop(ctx); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	select {
	case auth := <-fm.wsAuth:
		if auth != "Bearer "+testToken {
			t.Fatalf("websocket Authorization = %q", auth)
		}
	case <-time.After(testWaitTime):
		t.Fatal("websocket was not dialed")
	}
	select {
	case conn := <-fm.wsConns:
		return conn
	case <-time.After(testWaitTime):
		t.Fatal("websocket was not established")
	}
	return nil
}

var seqCounter sync.Map // *websocket.Conn -> *int64

func sendPosted(t *testing.T, conn *websocket.Conn, channelType string, post map[string]any, mentions []string) {
	t.Helper()
	postJSON, _ := json.Marshal(post)
	data := map[string]any{
		"channel_type": channelType,
		"channel_name": "town-square",
		"post":         string(postJSON),
		"sender_name":  "@alice",
		"team_id":      "teamid1",
	}
	if mentions != nil {
		m, _ := json.Marshal(mentions)
		data["mentions"] = string(m)
	}
	v, _ := seqCounter.LoadOrStore(conn, new(int64))
	seq := v.(*int64)
	*seq++
	if err := conn.WriteJSON(map[string]any{"event": "posted", "seq": *seq, "data": data}); err != nil {
		t.Fatalf("write posted event: %v", err)
	}
}

func post(id, userID, message, rootID string) map[string]any {
	return map[string]any{
		"id": id, "user_id": userID, "channel_id": testChanID,
		"root_id": rootID, "message": message, "type": "",
	}
}

func nextInbound(t *testing.T, mb *bus.MessageBus) bus.InboundMessage {
	t.Helper()
	select {
	case msg := <-mb.InboundChan():
		return msg
	case <-time.After(testWaitTime):
		t.Fatal("timed out waiting for inbound message")
	}
	return bus.InboundMessage{}
}

func TestNewMattermostChannelValidation(t *testing.T) {
	mb := bus.NewMessageBus()
	bc := &config.Channel{Type: config.ChannelMattermost}
	tok := *config.NewSecureString("x")

	if _, err := NewMattermostChannel(bc, &config.MattermostSettings{BotToken: tok}, mb); err == nil {
		t.Error("expected error for missing url")
	}
	if _, err := NewMattermostChannel(bc, &config.MattermostSettings{URL: "mm.example.com", BotToken: tok}, mb); err == nil {
		t.Error("expected error for url without scheme")
	}
	if _, err := NewMattermostChannel(bc, &config.MattermostSettings{URL: "https://mm.example.com"}, mb); err == nil {
		t.Error("expected error for missing bot_token")
	}
	if _, err := NewMattermostChannel(bc, &config.MattermostSettings{
		URL: "https://mm.example.com", BotToken: tok, GroupMode: "loud",
	}, mb); err == nil {
		t.Error("expected error for unknown group_mode")
	}
	ch, err := NewMattermostChannel(bc, &config.MattermostSettings{URL: "https://mm.example.com/", BotToken: tok}, mb)
	if err != nil {
		t.Fatalf("valid config: %v", err)
	}
	if ch.Name() != "mattermost" || ch.IsRunning() {
		t.Errorf("unexpected initial state: name=%q running=%v", ch.Name(), ch.IsRunning())
	}
	if got := ch.websocketURL("", 0); got != "wss://mm.example.com/api/v4/websocket" {
		t.Errorf("websocketURL = %q", got)
	}
	if got := ch.websocketURL("abc", 7); got != "wss://mm.example.com/api/v4/websocket?connection_id=abc&sequence_number=7" {
		t.Errorf("websocketURL resume = %q", got)
	}
}

func TestStartFailsWithBadToken(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, _ := newTestChannel(t, fm, nil, func(_ *config.Channel, s *config.MattermostSettings) {
		s.BotToken = *config.NewSecureString("wrong")
	})
	if err := ch.Start(context.Background()); err == nil {
		t.Fatal("expected Start to fail with an invalid token")
	}
	if ch.IsRunning() {
		t.Error("channel must not be running after failed Start")
	}
}

func TestInboundDirectMessage(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, mb := newTestChannel(t, fm, nil, nil)
	conn := startConnected(t, fm, ch)

	sendPosted(t, conn, "D", post("p1", testUserID, "hello there", ""), nil)
	msg := nextInbound(t, mb)

	if msg.Content != "hello there" {
		t.Errorf("content = %q", msg.Content)
	}
	// Session scope is the channel; the thread travels with the message.
	if msg.Context.ChatID != testChanID || msg.Context.TopicID != "" {
		t.Errorf("chat=%q topic=%q, want plain channel and no topic", msg.Context.ChatID, msg.Context.TopicID)
	}
	if msg.Context.ChatType != "direct" || msg.Context.MessageID != testChanID+":p1" ||
		msg.Context.ReplyToMessageID != "" {
		t.Errorf("chatType=%q messageID=%q replyTo=%q",
			msg.Context.ChatType, msg.Context.MessageID, msg.Context.ReplyToMessageID)
	}
	if msg.Sender.CanonicalID != "mattermost:"+testUserID || msg.Sender.Username != "alice" ||
		msg.Sender.DisplayName != "Alice Smith" {
		t.Errorf("sender = %+v", msg.Sender)
	}

	// A reply inside an existing thread: same chat, root in the IDs.
	sendPosted(t, conn, "D", post("p2", testUserID, "follow-up", "p1"), nil)
	msg = nextInbound(t, mb)
	if msg.Context.ChatID != testChanID || msg.Context.MessageID != testChanID+":p1:p2" ||
		msg.Context.ReplyToMessageID != testChanID+":p1" {
		t.Errorf("thread reply chat=%q msg=%q replyTo=%q",
			msg.Context.ChatID, msg.Context.MessageID, msg.Context.ReplyToMessageID)
	}
}

func TestInboundChannelMentionFiltering(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, mb := newTestChannel(t, fm, nil, func(_ *config.Channel, s *config.MattermostSettings) {
		s.GroupMode = "mention"
	})
	conn := startConnected(t, fm, ch)

	// Not mentioned: ignored.
	sendPosted(t, conn, "O", post("c1", testUserID, "just chatting", ""), nil)
	// Mentioning someone else / a longer username / an e-mail: ignored.
	sendPosted(t, conn, "O", post("c2", testUserID, "@picobot-two hi, mail picobot@example.com", ""),
		[]string{testOtherID})
	// Channel-wide mention: the bot is in the mentions list but was not addressed.
	sendPosted(t, conn, "O", post("c2b", testUserID, "@here standup in 5", ""), []string{testBotID, testOtherID})
	// Mentioned via the mentions list.
	sendPosted(t, conn, "P", post("c3", testUserID, "@PicoBot what is up?", ""), []string{testBotID})
	// Mentioned via text only (no mentions field), in a thread.
	sendPosted(t, conn, "G", post("c4", testUserID, "thanks @picobot.", "c0"), nil)

	msg := nextInbound(t, mb)
	if msg.Context.MessageID != testChanID+":c3" {
		t.Fatalf("first delivered message = %q, want c3 (unmentioned posts must be dropped)", msg.Context.MessageID)
	}
	if msg.Content != "what is up?" || !msg.Context.Mentioned || msg.Context.ChatType != "channel" {
		t.Errorf("content=%q mentioned=%v chatType=%q", msg.Content, msg.Context.Mentioned, msg.Context.ChatType)
	}
	if msg.Context.ChatID != testChanID {
		t.Errorf("chat = %q, want plain channel", msg.Context.ChatID)
	}

	msg = nextInbound(t, mb)
	if msg.Context.MessageID != testChanID+":c0:c4" || msg.Content != "thanks ." || msg.Context.ChatType != "group" {
		t.Errorf("got id=%q content=%q type=%q", msg.Context.MessageID, msg.Content, msg.Context.ChatType)
	}
	if msg.Context.ReplyToMessageID != testChanID+":c0" {
		t.Errorf("replyTo = %q, want thread root c0", msg.Context.ReplyToMessageID)
	}
}

func TestInboundGroupTriggerPrefix(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, mb := newTestChannel(t, fm, nil, func(bc *config.Channel, s *config.MattermostSettings) {
		bc.GroupTrigger.Prefixes = []string{"!ask "}
		s.GroupMode = "mention"
	})
	conn := startConnected(t, fm, ch)

	sendPosted(t, conn, "O", post("g1", testUserID, "no prefix", ""), nil)
	sendPosted(t, conn, "O", post("g2", testUserID, "!ask weather", ""), nil)
	msg := nextInbound(t, mb)
	if msg.Context.MessageID != testChanID+":g2" || msg.Content != "weather" {
		t.Errorf("got id=%q content=%q", msg.Context.MessageID, msg.Content)
	}
}

func TestInboundIgnoresOwnSystemAndBotPosts(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, mb := newTestChannel(t, fm, nil, nil)
	conn := startConnected(t, fm, ch)

	sendPosted(t, conn, "D", post("o1", testBotID, "my own reply", ""), nil)
	sys := post("o2", testUserID, "alice joined the channel", "")
	sys["type"] = "system_join_channel"
	sendPosted(t, conn, "D", sys, nil)
	bot := post("o3", testOtherID, "beep", "")
	bot["props"] = map[string]any{"from_bot": "true"}
	sendPosted(t, conn, "D", bot, nil)
	sendPosted(t, conn, "D", post("o4", testUserID, "real message", ""), nil)

	if msg := nextInbound(t, mb); msg.Context.MessageID != testChanID+":o4" {
		t.Fatalf("delivered %q, want only o4", msg.Context.MessageID)
	}
}

func TestInboundAllowFromRejects(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, mb := newTestChannel(t, fm, []string{testUserID}, nil)
	conn := startConnected(t, fm, ch)

	sendPosted(t, conn, "D", post("a1", testOtherID, "let me in", ""), nil)
	sendPosted(t, conn, "D", post("a2", testUserID, "allowed", ""), nil)
	if msg := nextInbound(t, mb); msg.Context.MessageID != testChanID+":a2" {
		t.Fatalf("delivered %q, want a2 (a1 sender is not in allow_from)", msg.Context.MessageID)
	}
}

func TestInboundTeamFilter(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, mb := newTestChannel(t, fm, nil, func(_ *config.Channel, s *config.MattermostSettings) {
		s.Team = "otherteam-name-that-does-not-resolve"
	})
	if err := ch.Start(context.Background()); err == nil {
		t.Fatal("expected Start to fail for unresolvable team")
	}

	fm2 := newFakeMattermost(t)
	ch2, mb2 := newTestChannel(t, fm2, nil, func(_ *config.Channel, s *config.MattermostSettings) {
		s.Team = "myteam"
	})
	_ = mb
	conn := startConnected(t, fm2, ch2)
	if ch2.teamID != "teamid1" {
		t.Fatalf("teamID = %q", ch2.teamID)
	}
	postJSON, _ := json.Marshal(post("t1", testUserID, "@picobot other team", ""))
	_ = conn.WriteJSON(map[string]any{"event": "posted", "seq": 1, "data": map[string]any{
		"channel_type": "O", "post": string(postJSON), "team_id": "teamid2",
		"mentions": `["` + testBotID + `"]`,
	}})
	sendPosted(t, conn, "O", post("t2", testUserID, "@picobot my team", ""), []string{testBotID})
	if msg := nextInbound(t, mb2); msg.Context.MessageID != testChanID+":t2" || msg.Context.SpaceID != "teamid1" {
		t.Fatalf("delivered %q space=%q, want t2 in teamid1", msg.Context.MessageID, msg.Context.SpaceID)
	}
}

func TestSendRepliesInThread(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, _ := newTestChannel(t, fm, nil, nil)
	startConnected(t, fm, ch)
	ctx := context.Background()

	send := func(msg bus.OutboundMessage) []string {
		t.Helper()
		ids, err := ch.Send(ctx, msg)
		if err != nil || len(ids) != 1 {
			t.Fatalf("Send(%+v): ids=%v err=%v", msg, ids, err)
		}
		return ids
	}
	// 1. Turn reply to a top-level post: thread rooted at that post.
	ids := send(bus.OutboundMessage{
		ChatID: testChanID, Content: "a",
		Context: bus.InboundContext{ChatID: testChanID, MessageID: testChanID + ":p1"},
	})
	// 2. Turn reply to a post inside thread r2.
	send(bus.OutboundMessage{ChatID: testChanID, Content: "b", Context: bus.InboundContext{
		ChatID: testChanID, MessageID: testChanID + ":r2:p5", ReplyToMessageID: testChanID + ":r2",
	}})
	// 3. Message tool with an explicit reply_to_message_id (a bot reply ID from #1).
	send(bus.OutboundMessage{
		ChatID: testChanID, Content: "c", ReplyToMessageID: ids[0],
		Context: bus.NewOutboundContext("mattermost", testChanID, ids[0]),
	})
	// 4. Message tool with a bare channel and no reply target: top-level.
	send(bus.OutboundMessage{
		ChatID: testChanID, Content: "d",
		Context: bus.NewOutboundContext("mattermost", testChanID, ""),
	})
	// 5. Explicit "<channel>/<root>" chat ID.
	send(bus.OutboundMessage{ChatID: testChanID + "/root5", Content: "e"})
	// 6. A reply target from another channel is ignored.
	send(bus.OutboundMessage{ChatID: testChanID, Content: "f", ReplyToMessageID: "otherchan:x1"})

	want := []string{"p1", "r2", "p1", "", "root5", ""}
	posts := fm.callsTo(http.MethodPost, "/api/v4/posts")
	if len(posts) != len(want) {
		t.Fatalf("posts = %d, want %d", len(posts), len(want))
	}
	for i, p := range posts {
		root, _ := p.Body["root_id"].(string)
		if root != want[i] || p.Body["channel_id"] != testChanID {
			t.Errorf("post %d (%v): root_id=%q channel=%v, want root %q", i, p.Body["message"], root,
				p.Body["channel_id"], want[i])
		}
	}
	if !strings.HasPrefix(ids[0], testChanID+":p1:") {
		t.Errorf("returned id %q, want <channel>:<root>:<post>", ids[0])
	}
}

func TestSendSplitsLongMessages(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, _ := newTestChannel(t, fm, nil, nil)
	startConnected(t, fm, ch)

	para := strings.Repeat("word ", 200) + "\n"
	content := strings.Repeat(para, 40) // ~40k chars
	ids, err := ch.Send(context.Background(), bus.OutboundMessage{ChatID: testChanID + "/r", Content: content})
	if err != nil {
		t.Fatal(err)
	}
	posts := fm.callsTo(http.MethodPost, "/api/v4/posts")
	if len(ids) < 3 || len(posts) != len(ids) {
		t.Fatalf("ids=%d posts=%d, want >=3 equal", len(ids), len(posts))
	}
	total := 0
	for _, p := range posts {
		msg, _ := p.Body["message"].(string)
		if n := len([]rune(msg)); n > maxMessageLength {
			t.Errorf("chunk length %d exceeds %d", n, maxMessageLength)
		}
		if p.Body["root_id"] != "r" {
			t.Errorf("chunk not in thread: %v", p.Body["root_id"])
		}
		total += len(strings.Fields(msg))
	}
	if total != 200*40 {
		t.Errorf("words delivered = %d, want %d", total, 200*40)
	}
}

func TestSendErrorClassification(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, _ := newTestChannel(t, fm, nil, nil)
	startConnected(t, fm, ch)

	for code, want := range map[int]error{
		http.StatusTooManyRequests:     channels.ErrRateLimit,
		http.StatusBadGateway:          channels.ErrTemporary,
		http.StatusForbidden:           channels.ErrSendFailed,
		http.StatusInternalServerError: channels.ErrTemporary,
	} {
		fm.mu.Lock()
		fm.postCode = code
		fm.mu.Unlock()
		_, err := ch.Send(context.Background(), bus.OutboundMessage{ChatID: testChanID, Content: "x"})
		if !errors.Is(err, want) {
			t.Errorf("HTTP %d: err = %v, want %v", code, err, want)
		}
	}

	if _, err := ch.Send(context.Background(), bus.OutboundMessage{ChatID: "", Content: "x"}); err == nil {
		t.Error("expected error for empty chat ID")
	}
}

func TestSendNotRunning(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, _ := newTestChannel(t, fm, nil, nil)
	if _, err := ch.Send(context.Background(), bus.OutboundMessage{ChatID: testChanID, Content: "x"}); !errors.Is(
		err, channels.ErrNotRunning) {
		t.Errorf("err = %v, want ErrNotRunning", err)
	}
}

func TestIndicatorsFollowTheThread(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, mb := newTestChannel(t, fm, nil, func(bc *config.Channel, _ *config.MattermostSettings) {
		bc.Placeholder.Enabled = true
		bc.Placeholder.Text = config.FlexibleStringSlice{"Thinking..."}
		bc.Typing.Enabled = true
	})
	conn := startConnected(t, fm, ch)
	ctx := context.Background()

	sendPosted(t, conn, "D", post("q1", testUserID, "question", "root9"), nil)
	in := nextInbound(t, mb)

	posts := fm.callsTo(http.MethodPost, "/api/v4/posts")
	if len(posts) != 1 || posts[0].Body["message"] != "Thinking..." || posts[0].Body["root_id"] != "root9" {
		t.Fatalf("placeholder posts = %v", posts)
	}
	if adds := fm.callsTo(http.MethodPost, "/api/v4/reactions"); len(adds) != 1 ||
		adds[0].Body["post_id"] != "q1" || adds[0].Body["emoji_name"] != "eyes" {
		t.Errorf("reactions = %v", adds)
	}
	deadline := time.Now().Add(testWaitTime)
	for len(fm.callsTo(http.MethodPost, "/api/v4/users/me/typing")) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if typing := fm.callsTo(http.MethodPost, "/api/v4/users/me/typing"); len(typing) == 0 ||
		typing[0].Body["channel_id"] != testChanID || typing[0].Body["parent_id"] != "root9" {
		t.Errorf("typing = %v", typing)
	}

	// The reply (carrying the turn's inbound context) edits the placeholder.
	ids, err := ch.Send(ctx, bus.OutboundMessage{ChatID: in.Context.ChatID, Content: "answer", Context: in.Context})
	if err != nil || len(ids) != 1 {
		t.Fatalf("Send: %v %v", ids, err)
	}
	if n := len(fm.callsTo(http.MethodPost, "/api/v4/posts")); n != 1 {
		t.Errorf("reply created %d posts, want the placeholder to be edited instead", n-1)
	}
	patches := fm.callsTo(http.MethodPut, "/api/v4/posts/")
	if len(patches) != 1 || patches[0].Body["message"] != "answer" {
		t.Errorf("patches = %v", patches)
	}
	if dels := fm.callsTo(http.MethodDelete, "/api/v4/users/"+testBotID+"/posts/q1/reactions/eyes"); len(dels) != 1 {
		t.Errorf("reaction not removed: %v", dels)
	}

	// Edit and delete accept the encoded IDs Send returned.
	if err = ch.EditMessage(ctx, testChanID, ids[0], "edited"); err != nil {
		t.Fatal(err)
	}
	if err = ch.DeleteMessage(ctx, testChanID, ids[0]); err != nil {
		t.Fatal(err)
	}
	postID := parseMessageID(ids[0]).postID
	if p := fm.callsTo(http.MethodPut, "/api/v4/posts/"+postID+"/patch"); len(p) != 2 {
		t.Errorf("edit by encoded id: %v", p)
	}
	if d := fm.callsTo(http.MethodDelete, "/api/v4/posts/"+postID); len(d) != 1 {
		t.Errorf("delete by encoded id: %v", d)
	}
}

func TestPlaceholderAndTypingDisabledByDefault(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, mb := newTestChannel(t, fm, nil, nil)
	conn := startConnected(t, fm, ch)

	sendPosted(t, conn, "D", post("d1", testUserID, "hi", ""), nil)
	nextInbound(t, mb)
	if n := len(fm.callsTo(http.MethodPost, "/api/v4/posts")) + len(fm.callsTo(http.MethodPost,
		"/api/v4/users/me/typing")); n != 0 {
		t.Errorf("unexpected placeholder/typing calls: %d", n)
	}
	if adds := fm.callsTo(http.MethodPost, "/api/v4/reactions"); len(adds) != 1 {
		t.Errorf("reaction should still be added: %v", adds)
	}
}

func TestStreamingEditsInPlace(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, _ := newTestChannel(t, fm, nil, func(_ *config.Channel, s *config.MattermostSettings) {
		s.Streaming = config.StreamingConfig{Enabled: true, ThrottleSeconds: 0, MinGrowthChars: 1}
	})
	startConnected(t, fm, ch)
	ctx := context.Background()

	// The thread comes from the turn's inbound context carried in ctx.
	turnCtx := bus.WithInboundContext(ctx, bus.InboundContext{
		ChatID: testChanID, MessageID: testChanID + ":root:p7", ReplyToMessageID: testChanID + ":root",
	})
	st, err := ch.BeginStream(turnCtx, testChanID)
	if err != nil {
		t.Fatal(err)
	}
	// Defaults (2s throttle) apply when zero; override for the test.
	ms := st.(*mattermostStreamer)
	ms.throttle = 0

	for _, partial := range []string{"Hel", "Hello", "Hello wor"} {
		if err := st.Update(ctx, partial); err != nil {
			t.Fatalf("Update: %v", err)
		}
	}
	final := "Hello world\n" + strings.Repeat("x ", maxMessageLength/2+10)
	if err := st.Finalize(ctx, final); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	st.Cancel(ctx) // must not delete the finalized post

	posts := fm.callsTo(http.MethodPost, "/api/v4/posts")
	patches := fm.callsTo(http.MethodPut, "/api/v4/posts/")
	if len(posts) != 2 {
		t.Fatalf("posts = %d, want 2 (initial partial + overflow chunk)", len(posts))
	}
	if posts[0].Body["message"] != "Hel" || posts[0].Body["root_id"] != "root" {
		t.Errorf("first stream post = %v", posts[0].Body)
	}
	if len(patches) != 3 {
		t.Fatalf("patches = %d, want 3 (two updates + finalize)", len(patches))
	}
	if last, _ := patches[2].Body["message"].(string); !strings.HasPrefix(last, "Hello world") {
		t.Errorf("final patch = %q", last)
	}
	if dels := fm.callsTo(http.MethodDelete, "/api/v4/posts/"); len(dels) != 0 {
		t.Errorf("finalized stream must not be deleted: %v", dels)
	}
	if ids := ms.DeliveredMessageIDs(); len(ids) != 2 {
		t.Errorf("delivered ids = %v", ids)
	}

	// A canceled stream removes its partial post.
	st2, _ := ch.BeginStream(turnCtx, testChanID)
	_ = st2.Update(ctx, "partial")
	st2.Cancel(ctx)
	if dels := fm.callsTo(http.MethodDelete, "/api/v4/posts/"); len(dels) != 1 {
		t.Errorf("canceled stream deletes = %v", dels)
	}
}

func TestStreamingDisabledByDefault(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, _ := newTestChannel(t, fm, nil, nil)
	if _, err := ch.BeginStream(context.Background(), testChanID); err == nil {
		t.Error("BeginStream should fail when streaming is disabled")
	}
}

func TestReconnectsAfterServerDrop(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, mb := newTestChannel(t, fm, nil, nil)
	ch.reconnectMin = 10 * time.Millisecond
	conn := startConnected(t, fm, ch)
	_ = conn.Close()

	select {
	case <-fm.wsAuth:
	case <-time.After(testWaitTime):
		t.Fatal("channel did not reconnect")
	}
	conn2 := <-fm.wsConns
	sendPosted(t, conn2, "D", post("r1", testUserID, "after reconnect", ""), nil)
	if msg := nextInbound(t, mb); msg.Content != "after reconnect" {
		t.Errorf("content = %q", msg.Content)
	}
}

func TestStripBotMention(t *testing.T) {
	ch := &MattermostChannel{}
	ch.botUsername = testBotName
	ch.mentionRe = mustMentionRe(testBotName)
	tests := map[string]string{
		"@picobot hello":            "hello",
		"hey @PICOBOT, how are you": "hey , how are you",
		"@picobot-two hello":        "@picobot-two hello",
		"mail picobot@x.org":        "mail picobot@x.org",
		"ask @picobot.":             "ask .",
		"@picobot.smith hi":         "@picobot.smith hi",
	}
	for in, want := range tests {
		if got := ch.stripBotMention(in); got != want {
			t.Errorf("stripBotMention(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMessageIDCodecAndTarget(t *testing.T) {
	if got := encodeMessageID("c", "", "p"); got != "c:p" {
		t.Errorf("top-level id = %q", got)
	}
	if got := encodeMessageID("c", "p", "p"); got != "c:p" {
		t.Errorf("root post id = %q", got)
	}
	if got := encodeMessageID("c", "r", "p"); got != "c:r:p" {
		t.Errorf("thread id = %q", got)
	}
	for in, want := range map[string]msgRef{
		"p":       {postID: "p"},
		"c:p":     {channelID: "c", postID: "p"},
		"c:r:p":   {channelID: "c", rootID: "r", postID: "p"},
		"a:b:c:d": {},
	} {
		if got := parseMessageID(in); got != want {
			t.Errorf("parseMessageID(%q) = %+v, want %+v", in, got, want)
		}
	}
	if c, r := resolveTarget("chan", nil, ""); c != "chan" || r != "" {
		t.Errorf("bare channel: %q %q", c, r)
	}
	if c, r := resolveTarget("", &bus.InboundContext{ChatID: "chan", MessageID: "chan:t:p"}, ""); c != "chan" ||
		r != "t" {
		t.Errorf("from context: %q %q", c, r)
	}
	if c, r := resolveTarget("chan", &bus.InboundContext{MessageID: "chan:p"}, "chan:x:y"); c != "chan" || r != "x" {
		t.Errorf("explicit reply wins: %q %q", c, r)
	}
	if _, r := resolveTarget("chan", &bus.InboundContext{MessageID: "other:p"}, ""); r != "" {
		t.Errorf("foreign context must be ignored, got root %q", r)
	}
}
