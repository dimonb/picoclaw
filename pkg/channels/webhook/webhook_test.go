package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
)

func newTestChannel(t *testing.T, secret string) (*WebhookChannel, *bus.MessageBus) {
	t.Helper()
	cfg := config.WebhookConfig{Enabled: true}
	if secret != "" {
		cfg.SetSharedSecret(secret)
	}
	mb := bus.NewMessageBus()
	c, err := NewWebhookChannel(cfg, mb)
	if err != nil {
		t.Fatalf("NewWebhookChannel: %v", err)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = c.Stop(context.Background()) })
	return c, mb
}

func doJSON(t *testing.T, c *WebhookChannel, method, path, auth, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	c.ServeHTTP(rec, req)
	return rec
}

func decodeJSON(t *testing.T, body io.Reader) map[string]string {
	t.Helper()
	out := map[string]string{}
	if err := json.NewDecoder(body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func drainInbound(mb *bus.MessageBus, want int, timeout time.Duration) []bus.InboundMessage {
	deadline := time.After(timeout)
	out := make([]bus.InboundMessage, 0, want)
	for len(out) < want {
		select {
		case msg, ok := <-mb.InboundChan():
			if !ok {
				return out
			}
			out = append(out, msg)
		case <-deadline:
			return out
		}
	}
	return out
}

func TestWebhook_AuthEnforced(t *testing.T) {
	c, _ := newTestChannel(t, "topsecret")

	// Missing header
	rec := doJSON(t, c, http.MethodPost, "/webhook/sess1", "", `{"content":"hi"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}

	// Wrong secret
	rec = doJSON(t, c, http.MethodPost, "/webhook/sess1", "Bearer wrong", `{"content":"hi"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong secret, got %d", rec.Code)
	}

	// Correct secret
	rec = doJSON(t, c, http.MethodPost, "/webhook/sess1", "Bearer topsecret", `{"content":"hi"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestWebhook_NoSecretAllows(t *testing.T) {
	c, _ := newTestChannel(t, "")
	rec := doJSON(t, c, http.MethodPost, "/webhook/sess1", "", `{"content":"hi"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202 without secret, got %d", rec.Code)
	}
}

func TestWebhook_SessionNameValidation(t *testing.T) {
	c, _ := newTestChannel(t, "")

	cases := []struct {
		path string
		want int
	}{
		{"/webhook/" + strings.Repeat("a", 129), http.StatusBadRequest},
		{"/webhook/bad~name", http.StatusBadRequest},
		{"/webhook/", http.StatusBadRequest},
	}
	for _, tc := range cases {
		rec := doJSON(t, c, http.MethodPost, tc.path, "", `{"content":"hi"}`)
		if rec.Code != tc.want {
			t.Errorf("path %q: want %d got %d", tc.path, tc.want, rec.Code)
		}
	}
}

func TestWebhook_EndToEnd(t *testing.T) {
	c, mb := newTestChannel(t, "")

	// POST creates a ticket
	rec := doJSON(t, c, http.MethodPost, "/webhook/sess1", "", `{"content":"hello agent"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST: expected 202, got %d", rec.Code)
	}
	body := decodeJSON(t, rec.Body)
	tid := body["ticket"]
	if tid == "" {
		t.Fatal("ticket missing in response")
	}
	if body["status"] != statusProcessing {
		t.Fatalf("status=%q want %q", body["status"], statusProcessing)
	}

	// Inbound should be on the bus
	inbound := drainInbound(mb, 1, 2*time.Second)
	if len(inbound) != 1 {
		t.Fatalf("expected 1 inbound, got %d", len(inbound))
	}
	if inbound[0].Content != "hello agent" {
		t.Errorf("inbound content=%q", inbound[0].Content)
	}
	if inbound[0].ChatID != "sess1" {
		t.Errorf("inbound chat_id=%q", inbound[0].ChatID)
	}
	if inbound[0].Channel != "webhook" {
		t.Errorf("inbound channel=%q", inbound[0].Channel)
	}

	// Poll while processing
	rec = doJSON(t, c, http.MethodGet, "/webhook/sess1/"+tid, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET processing: %d", rec.Code)
	}
	if got := decodeJSON(t, rec.Body); got["status"] != statusProcessing {
		t.Errorf("status=%q want %q", got["status"], statusProcessing)
	}

	// Simulate agent reply via Send
	if _, err := c.Send(context.Background(), bus.OutboundMessage{
		Channel: "webhook",
		ChatID:  "sess1",
		Content: "hi human",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Poll again — should be done
	rec = doJSON(t, c, http.MethodGet, "/webhook/sess1/"+tid, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET done: %d", rec.Code)
	}
	got := decodeJSON(t, rec.Body)
	if got["status"] != statusDone {
		t.Errorf("status=%q want %q", got["status"], statusDone)
	}
	if got["content"] != "hi human" {
		t.Errorf("content=%q", got["content"])
	}
}

func TestWebhook_FIFOOrdering(t *testing.T) {
	c, mb := newTestChannel(t, "")

	tids := make([]string, 3)
	for i := range tids {
		rec := doJSON(t, c, http.MethodPost, "/webhook/sess1", "",
			`{"content":"req-`+string(rune('A'+i))+`"}`)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("POST %d: %d", i, rec.Code)
		}
		tids[i] = decodeJSON(t, rec.Body)["ticket"]
	}

	// Drain inbound to keep the bus clear (otherwise it could block at 16 cap).
	_ = drainInbound(mb, 3, 2*time.Second)

	// Three Send calls should fill tickets in FIFO order.
	for i, want := range []string{"resp-A", "resp-B", "resp-C"} {
		ids, err := c.Send(context.Background(), bus.OutboundMessage{
			Channel: "webhook", ChatID: "sess1", Content: want,
		})
		if err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
		if len(ids) != 1 || ids[0] != tids[i] {
			t.Fatalf("Send %d returned %v, want [%s]", i, ids, tids[i])
		}
	}

	for i, tid := range tids {
		rec := doJSON(t, c, http.MethodGet, "/webhook/sess1/"+tid, "", "")
		got := decodeJSON(t, rec.Body)
		want := []string{"resp-A", "resp-B", "resp-C"}[i]
		if got["content"] != want {
			t.Errorf("ticket %d: content=%q want %q", i, got["content"], want)
		}
	}
}

func TestWebhook_SendWithoutPendingTicket(t *testing.T) {
	c, _ := newTestChannel(t, "")
	ids, err := c.Send(context.Background(), bus.OutboundMessage{
		Channel: "webhook", ChatID: "ghost", Content: "noop",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ids != nil {
		t.Errorf("expected nil ids, got %v", ids)
	}
}

func TestWebhook_GetUnknownTicket(t *testing.T) {
	c, _ := newTestChannel(t, "")
	rec := doJSON(t, c, http.MethodGet, "/webhook/sess1/deadbeef", "", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestWebhook_TTLEvictsExpired(t *testing.T) {
	c, _ := newTestChannel(t, "")
	c.timeout = 10 * time.Millisecond
	c.ttl = 10 * time.Millisecond

	rec := doJSON(t, c, http.MethodPost, "/webhook/sess1", "", `{"content":"hi"}`)
	tid := decodeJSON(t, rec.Body)["ticket"]

	time.Sleep(20 * time.Millisecond)
	c.evict(time.Now())

	c.ticketsMu.RLock()
	t2, ok := c.tickets[tid]
	c.ticketsMu.RUnlock()
	if !ok {
		t.Fatal("ticket evicted prematurely")
	}
	if t2.status != statusExpired {
		t.Errorf("status=%q want %q", t2.status, statusExpired)
	}

	// Second sweep after ttl removes it entirely.
	time.Sleep(20 * time.Millisecond)
	c.evict(time.Now())

	c.ticketsMu.RLock()
	_, stillThere := c.tickets[tid]
	c.ticketsMu.RUnlock()
	if stillThere {
		t.Error("expected ticket removed after TTL")
	}
}

func TestWebhook_RejectsLargeBody(t *testing.T) {
	c, _ := newTestChannel(t, "")
	big := bytes.Repeat([]byte("a"), maxBodySize+10)
	body := `{"content":"` + string(big) + `"}`
	rec := doJSON(t, c, http.MethodPost, "/webhook/sess1", "", body)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", rec.Code)
	}
}

func TestWebhook_MethodGuards(t *testing.T) {
	c, _ := newTestChannel(t, "")

	rec := doJSON(t, c, http.MethodGet, "/webhook/sess1", "", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET inbound: want 405, got %d", rec.Code)
	}

	rec = doJSON(t, c, http.MethodPost, "/webhook/sess1/abc", "", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST result: want 405, got %d", rec.Code)
	}
}
