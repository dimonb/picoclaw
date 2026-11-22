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
	fm.calls = append(fm.calls, call)
	fm.nextID++
	id := fmt.Sprintf("post%022d", fm.nextID)
	postCode := fm.postCode
	fm.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v4/users/me":
		_ = json.NewEncoder(w).Encode(map[string]string{"id": testBotID, "username": testBotName})
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
	if msg.Context.ChatID != testChanID+"/p1" || msg.Context.TopicID != "p1" {
		t.Errorf("chat=%q topic=%q, want thread rooted at p1", msg.Context.ChatID, msg.Context.TopicID)
	}
	if msg.Context.ChatType != "direct" || msg.Context.MessageID != "p1" {
		t.Errorf("chatType=%q messageID=%q", msg.Context.ChatType, msg.Context.MessageID)
	}
	if msg.Sender.CanonicalID != "mattermost:"+testUserID {
		t.Errorf("sender canonical = %q", msg.Sender.CanonicalID)
	}

	// A reply inside an existing thread keeps the thread root.
	sendPosted(t, conn, "D", post("p2", testUserID, "follow-up", "p1"), nil)
	msg = nextInbound(t, mb)
	if msg.Context.ChatID != testChanID+"/p1" {
		t.Errorf("thread reply chat = %q", msg.Context.ChatID)
	}
}

func TestInboundChannelMentionFiltering(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, mb := newTestChannel(t, fm, nil, nil)
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
	if msg.Context.MessageID != "c3" {
		t.Fatalf("first delivered message = %q, want c3 (unmentioned posts must be dropped)", msg.Context.MessageID)
	}
	if msg.Content != "what is up?" || !msg.Context.Mentioned || msg.Context.ChatType != "channel" {
		t.Errorf("content=%q mentioned=%v chatType=%q", msg.Content, msg.Context.Mentioned, msg.Context.ChatType)
	}
	if msg.Context.ChatID != testChanID+"/c3" {
		t.Errorf("chat = %q, want thread rooted at c3", msg.Context.ChatID)
	}

	msg = nextInbound(t, mb)
	if msg.Context.MessageID != "c4" || msg.Content != "thanks ." || msg.Context.ChatType != "group" {
		t.Errorf("got id=%q content=%q type=%q", msg.Context.MessageID, msg.Content, msg.Context.ChatType)
	}
	if msg.Context.ChatID != testChanID+"/c0" {
		t.Errorf("chat = %q, want existing root c0", msg.Context.ChatID)
	}
}

func TestInboundGroupTriggerPrefix(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, mb := newTestChannel(t, fm, nil, func(bc *config.Channel, _ *config.MattermostSettings) {
		bc.GroupTrigger.Prefixes = []string{"!ask "}
	})
	conn := startConnected(t, fm, ch)

	sendPosted(t, conn, "O", post("g1", testUserID, "no prefix", ""), nil)
	sendPosted(t, conn, "O", post("g2", testUserID, "!ask weather", ""), nil)
	msg := nextInbound(t, mb)
	if msg.Context.MessageID != "g2" || msg.Content != "weather" {
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

	if msg := nextInbound(t, mb); msg.Context.MessageID != "o4" {
		t.Fatalf("delivered %q, want only o4", msg.Context.MessageID)
	}
}

func TestInboundAllowFromRejects(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, mb := newTestChannel(t, fm, []string{testUserID}, nil)
	conn := startConnected(t, fm, ch)

	sendPosted(t, conn, "D", post("a1", testOtherID, "let me in", ""), nil)
	sendPosted(t, conn, "D", post("a2", testUserID, "allowed", ""), nil)
	if msg := nextInbound(t, mb); msg.Context.MessageID != "a2" {
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
	if msg := nextInbound(t, mb2); msg.Context.MessageID != "t2" || msg.Context.SpaceID != "teamid1" {
		t.Fatalf("delivered %q space=%q, want t2 in teamid1", msg.Context.MessageID, msg.Context.SpaceID)
	}
}

func TestSendRepliesInThread(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, _ := newTestChannel(t, fm, nil, nil)
	startConnected(t, fm, ch)

	ids, err := ch.Send(context.Background(), bus.OutboundMessage{ChatID: testChanID + "/root1", Content: "hi"})
	if err != nil || len(ids) != 1 {
		t.Fatalf("Send: ids=%v err=%v", ids, err)
	}
	// Plain channel ID with the thread in the context topic.
	_, err = ch.Send(context.Background(), bus.OutboundMessage{
		ChatID: testChanID, Content: "topic", Context: bus.InboundContext{TopicID: "root2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// No thread at all: top-level post.
	if _, err = ch.Send(context.Background(), bus.OutboundMessage{ChatID: testChanID, Content: "top"}); err != nil {
		t.Fatal(err)
	}

	posts := fm.callsTo(http.MethodPost, "/api/v4/posts")
	if len(posts) != 3 {
		t.Fatalf("posts = %d, want 3", len(posts))
	}
	if posts[0].Body["channel_id"] != testChanID || posts[0].Body["root_id"] != "root1" ||
		posts[0].Body["message"] != "hi" {
		t.Errorf("post 0 body = %v", posts[0].Body)
	}
	if posts[1].Body["root_id"] != "root2" {
		t.Errorf("post 1 root_id = %v", posts[1].Body["root_id"])
	}
	if _, ok := posts[2].Body["root_id"]; ok {
		t.Errorf("post 2 should be top-level, body = %v", posts[2].Body)
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

func TestEditDeletePlaceholderTyping(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, _ := newTestChannel(t, fm, nil, func(bc *config.Channel, _ *config.MattermostSettings) {
		bc.Placeholder.Enabled = true
		bc.Placeholder.Text = config.FlexibleStringSlice{"Thinking..."}
		bc.Typing.Enabled = true
	})
	startConnected(t, fm, ch)
	ctx := context.Background()
	chatID := testChanID + "/root9"

	phID, err := ch.SendPlaceholder(ctx, chatID)
	if err != nil || phID == "" {
		t.Fatalf("SendPlaceholder: id=%q err=%v", phID, err)
	}
	if err = ch.EditMessage(ctx, chatID, phID, "final answer"); err != nil {
		t.Fatalf("EditMessage: %v", err)
	}
	if err = ch.DeleteMessage(ctx, chatID, phID); err != nil {
		t.Fatalf("DeleteMessage: %v", err)
	}

	posts := fm.callsTo(http.MethodPost, "/api/v4/posts")
	if len(posts) != 1 || posts[0].Body["message"] != "Thinking..." || posts[0].Body["root_id"] != "root9" {
		t.Errorf("placeholder posts = %v", posts)
	}
	patches := fm.callsTo(http.MethodPut, "/api/v4/posts/")
	if len(patches) != 1 || patches[0].Path != "/api/v4/posts/"+phID+"/patch" ||
		patches[0].Body["message"] != "final answer" {
		t.Errorf("patches = %v", patches)
	}
	if dels := fm.callsTo(http.MethodDelete, "/api/v4/posts/"+phID); len(dels) != 1 {
		t.Errorf("deletes = %v", dels)
	}

	stop, err := ch.StartTyping(ctx, chatID)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(testWaitTime)
	for len(fm.callsTo(http.MethodPost, "/api/v4/users/me/typing")) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	stop() // idempotent
	typing := fm.callsTo(http.MethodPost, "/api/v4/users/me/typing")
	if len(typing) == 0 || typing[0].Body["channel_id"] != testChanID || typing[0].Body["parent_id"] != "root9" {
		t.Errorf("typing calls = %v", typing)
	}
}

func TestPlaceholderAndTypingDisabledByDefault(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, _ := newTestChannel(t, fm, nil, nil)
	startConnected(t, fm, ch)

	id, err := ch.SendPlaceholder(context.Background(), testChanID)
	if err != nil || id != "" {
		t.Errorf("placeholder should be disabled: id=%q err=%v", id, err)
	}
	stop, _ := ch.StartTyping(context.Background(), testChanID)
	stop()
	if n := len(fm.callsTo(http.MethodPost, "/api/v4/posts")) + len(fm.callsTo(http.MethodPost,
		"/api/v4/users/me/typing")); n != 0 {
		t.Errorf("unexpected API calls: %d", n)
	}
}

func TestReactToMessage(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, _ := newTestChannel(t, fm, nil, nil)
	startConnected(t, fm, ch)

	undo, err := ch.ReactToMessage(context.Background(), testChanID+"/p1", "p1")
	if err != nil {
		t.Fatal(err)
	}
	undo()
	undo()
	adds := fm.callsTo(http.MethodPost, "/api/v4/reactions")
	if len(adds) != 1 || adds[0].Body["post_id"] != "p1" || adds[0].Body["user_id"] != testBotID ||
		adds[0].Body["emoji_name"] != "eyes" {
		t.Errorf("reaction adds = %v", adds)
	}
	dels := fm.callsTo(http.MethodDelete, "/api/v4/users/"+testBotID+"/posts/p1/reactions/eyes")
	if len(dels) != 1 {
		t.Errorf("reaction removes = %v", dels)
	}
}

func TestStreamingEditsInPlace(t *testing.T) {
	fm := newFakeMattermost(t)
	ch, _ := newTestChannel(t, fm, nil, func(_ *config.Channel, s *config.MattermostSettings) {
		s.Streaming = config.StreamingConfig{Enabled: true, ThrottleSeconds: 0, MinGrowthChars: 1}
	})
	startConnected(t, fm, ch)
	ctx := context.Background()

	st, err := ch.BeginStream(ctx, testChanID+"/root")
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
	st2, _ := ch.BeginStream(ctx, testChanID+"/root")
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

func TestParseChatID(t *testing.T) {
	if c, r := parseChatID("chan/root"); c != "chan" || r != "root" {
		t.Errorf("got %q %q", c, r)
	}
	if c, r := parseChatID("chan"); c != "chan" || r != "" {
		t.Errorf("got %q %q", c, r)
	}
	if c, r := resolveTarget("", &bus.InboundContext{ChatID: "chan", TopicID: "t"}, ""); c != "chan" || r != "t" {
		t.Errorf("got %q %q", c, r)
	}
	if c, r := resolveTarget("chan", nil, "reply"); c != "chan" || r != "reply" {
		t.Errorf("got %q %q", c, r)
	}
}
