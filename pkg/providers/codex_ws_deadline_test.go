package providers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// drainAgainst runs drainStream against a fake server whose handler writes
// events through send; it returns what drainStream returned and how long it took.
func drainAgainst(t *testing.T, serve func(send func(string))) (string, error, time.Duration) {
	t.Helper()
	upgrader := websocket.Upgrader{}
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		serve(func(evt string) { _ = c.WriteMessage(websocket.TextMessage, []byte(evt)) })
		<-done
	}))
	defer srv.Close()
	defer close(done)

	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	resp.Body.Close()
	defer conn.Close()

	start := time.Now()
	var text string
	_, _, err = (&CodexWSProvider{}).drainStream(
		&wsSessionState{conn: conn},
		func(s string) { text = s },
		nil,
	)
	return text, err, time.Since(start)
}

func withReadTimeouts(t *testing.T, idle, total time.Duration) {
	t.Helper()
	oldIdle, oldTotal := wsReadIdleTimeout, wsResponseMaxDuration
	wsReadIdleTimeout, wsResponseMaxDuration = idle, total
	t.Cleanup(func() { wsReadIdleTimeout, wsResponseMaxDuration = oldIdle, oldTotal })
}

const (
	evtCreated   = `{"type":"response.created","response":{"id":"resp_1"}}`
	evtDelta     = `{"type":"response.output_text.delta","output_index":0,"delta":"x"}`
	evtCompleted = `{"type":"response.completed","response":{"id":"resp_1"}}`
)

// A response that keeps streaming for longer than the idle timeout must not be
// cut off: the idle deadline restarts on every event.
func TestDrainStreamIdleDeadlineRestartsPerEvent(t *testing.T) {
	withReadTimeouts(t, 150*time.Millisecond, 5*time.Second)

	text, err, _ := drainAgainst(t, func(send func(string)) {
		send(evtCreated)
		for range 8 { // ~400ms of streaming, well past one idle window
			time.Sleep(50 * time.Millisecond)
			send(evtDelta)
		}
		send(evtCompleted)
	})
	if err != nil {
		t.Fatalf("progressing stream was cut off: %v", err)
	}
	if text != strings.Repeat("x", 8) {
		t.Fatalf("text = %q, want 8 deltas", text)
	}
}

// A stream that goes silent fails after the idle timeout, not the overall cap.
func TestDrainStreamIdleTimeoutOnSilence(t *testing.T) {
	withReadTimeouts(t, 150*time.Millisecond, 5*time.Second)

	_, err, took := drainAgainst(t, func(send func(string)) {
		send(evtCreated)
		send(evtDelta)
	})
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("err = %v, want a read timeout", err)
	}
	if took > 2*time.Second {
		t.Fatalf("silence detected after %v, want ~idle timeout", took)
	}
}

// A stream that trickles events forever still ends at the overall cap.
func TestDrainStreamOverallCap(t *testing.T) {
	withReadTimeouts(t, 150*time.Millisecond, 400*time.Millisecond)

	_, err, took := drainAgainst(t, func(send func(string)) {
		send(evtCreated)
		for range 40 { // 2s of trickle, far past the cap
			time.Sleep(50 * time.Millisecond)
			send(evtDelta)
		}
	})
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("err = %v, want a read timeout at the cap", err)
	}
	if took > 1500*time.Millisecond {
		t.Fatalf("cap hit after %v, want ~400ms", took)
	}
}
