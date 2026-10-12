package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// An idle connection must keep answering the server's pings: the Codex backend
// closes one that does not ("1011 keepalive ping timeout"), and the next turn
// then loses the server-side prompt cache.
func TestWSConnAnswersPingsWhileIdle(t *testing.T) {
	var pongs atomic.Int32
	serverDone := make(chan struct{})
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		c.SetPongHandler(func(string) error { pongs.Add(1); return nil })
		go func() {
			for {
				if _, _, err := c.ReadMessage(); err != nil {
					return
				}
			}
		}()
		for range 5 {
			if err := c.WriteControl(websocket.PingMessage, nil, time.Now().Add(time.Second)); err != nil {
				return
			}
			time.Sleep(30 * time.Millisecond)
		}
		<-serverDone
	}))
	defer srv.Close()
	defer close(serverDone)

	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	resp.Body.Close()
	wc := newWSConn(conn)
	defer wc.Close()

	deadline := time.Now().Add(2 * time.Second)
	for pongs.Load() < 5 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := pongs.Load(); got < 5 {
		t.Fatalf("server got %d pongs for 5 pings while the connection sat idle", got)
	}
	if err := wc.lost(); err != nil {
		t.Fatalf("idle connection was lost: %v", err)
	}
}

// When the server closes a connection between turns, the next turn replaces it
// at once — no failed send, no backoff.
func TestCodexWSReconnectsAtOnceAfterIdleClose(t *testing.T) {
	var conns atomic.Int32
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		conns.Add(1)
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			var req wsRequest
			if err := json.Unmarshal(data, &req); err != nil {
				t.Errorf("bad request: %v", err)
				return
			}
			completed := `{"type":"response.completed","response":{"id":"resp_1"}}`
			if req.Generate != nil && !*req.Generate {
				_ = c.WriteMessage(websocket.TextMessage, []byte(completed))
				continue
			}
			_ = c.WriteMessage(websocket.TextMessage, []byte(
				`{"type":"response.output_item.done","output_index":0,"item":{"id":"m1","type":"message",`+
					`"content":[{"type":"output_text","text":"ok"}]}}`))
			_ = c.WriteMessage(websocket.TextMessage, []byte(completed))
			// Like the real backend after its ping timeout: drop the connection
			// once the turn is over.
			_ = c.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "keepalive ping timeout"))
			return
		}
	}))
	t.Cleanup(srv.Close)

	p := &CodexWSProvider{
		tokenSource:     func() (string, string, error) { return "tok", "", nil },
		baseURL:         "ws" + strings.TrimPrefix(srv.URL, "http"),
		sessions:        make(map[string]*wsSessionState),
		done:            make(chan struct{}),
		effortFallbacks: make(map[string]string),
	}
	t.Cleanup(p.Close)
	opts := map[string]any{"session_key": "s1"}
	history := make([]Message, 0, 3)
	history = append(history, Message{Role: "user", Content: "hi"})

	if _, err := p.Chat(context.Background(), history, nil, "gpt-5.5", opts); err != nil {
		t.Fatalf("first Chat: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // let the close frame arrive

	history = append(history, Message{Role: "assistant", Content: "ok"}, Message{Role: "user", Content: "again"})
	start := time.Now()
	resp, err := p.Chat(context.Background(), history, nil, "gpt-5.5", opts)
	if err != nil {
		t.Fatalf("second Chat: %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("content = %q, want ok", resp.Content)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("second turn took %v; a closed idle connection must be replaced without a backoff", took)
	}
	if got := conns.Load(); got != 2 {
		t.Fatalf("connections = %d, want 2", got)
	}
}

func TestConnLostOnlyForConnectionErrors(t *testing.T) {
	if connLost(errWSReadTimeout) {
		t.Error("a read timeout is not a lost connection")
	}
	if !connLost(&wsConnLostError{err: &websocket.CloseError{Code: websocket.CloseInternalServerErr}}) {
		t.Error("a server close is a lost connection")
	}
}
