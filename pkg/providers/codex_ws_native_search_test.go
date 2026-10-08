package providers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
)

const hostedSearchRejection = `{"type":"error","error":{"message":"Hosted tool 'web_search' requires ` +
	`authorization and metering that are not supported by rustponsesapi.","type":"invalid_request_error",` +
	`"param":"tools","code":"unsupported_parameter"},"status":400}`

// fakeCodexWS answers prewarms with a completed response and every real turn
// either with the hosted web_search rejection (when the request carries the
// hosted tool) or with a completed message. It records the tool types of each
// real turn.
func fakeCodexWS(t *testing.T) (*CodexWSProvider, func() [][]string) {
	t.Helper()
	var (
		mu    sync.Mutex
		turns [][]string
	)
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
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
			var types []string
			hosted := false
			for _, td := range req.Tools {
				types = append(types, td.Type+":"+td.Name)
				hosted = hosted || td.Type == "web_search"
			}
			mu.Lock()
			turns = append(turns, types)
			mu.Unlock()
			if hosted {
				_ = c.WriteMessage(websocket.TextMessage, []byte(hostedSearchRejection))
				continue
			}
			_ = c.WriteMessage(websocket.TextMessage, []byte(
				`{"type":"response.output_item.done","output_index":0,"item":{"id":"m1","type":"message",`+
					`"content":[{"type":"output_text","text":"ok"}]}}`))
			_ = c.WriteMessage(websocket.TextMessage, []byte(completed))
		}
	}))
	t.Cleanup(srv.Close)

	p := &CodexWSProvider{
		tokenSource:     func() (string, string, error) { return "tok", "", nil },
		enableWebSearch: true,
		baseURL:         "ws" + strings.TrimPrefix(srv.URL, "http"),
		sessions:        make(map[string]*wsSessionState),
		done:            make(chan struct{}),
		effortFallbacks: make(map[string]string),
	}
	return p, func() [][]string {
		mu.Lock()
		defer mu.Unlock()
		return append([][]string(nil), turns...)
	}
}

func TestCodexWSFallsBackWhenHostedSearchRejected(t *testing.T) {
	p, turns := fakeCodexWS(t)
	tools := []ToolDefinition{
		{Type: "function", Function: ToolFunctionDefinition{Name: "web_search"}},
		{Type: "function", Function: ToolFunctionDefinition{Name: "read_file"}},
	}
	opts := map[string]any{"native_search": true, "session_key": "s1"}

	resp, err := p.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, tools, "gpt-5.5", opts)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("content = %q, want ok", resp.Content)
	}
	got := turns()
	want := [][]string{
		{"function:read_file", "web_search:"},
		{"function:web_search", "function:read_file"},
	}
	if len(got) != len(want) || strings.Join(got[0], ",") != strings.Join(want[0], ",") ||
		strings.Join(got[1], ",") != strings.Join(want[1], ",") {
		t.Fatalf("turn tools = %v, want %v", got, want)
	}

	// The rejection is remembered: the next turn goes straight to the client-side tool.
	if _, err := p.Chat(context.Background(), []Message{{Role: "user", Content: "again"}}, tools, "gpt-5.5",
		map[string]any{"native_search": true, "session_key": "s2"}); err != nil {
		t.Fatalf("second Chat: %v", err)
	}
	if got := turns(); len(got) != 3 || strings.Join(got[2], ",") != "function:web_search,function:read_file" {
		t.Fatalf("second turn tools = %v", got)
	}
}

func TestIsHostedSearchRejection(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&wsServerError{StatusCode: 400, Msg: "Hosted tool 'web_search' requires authorization and metering"}, true},
		{&wsServerError{StatusCode: 400, Msg: "Unsupported value: 'none' is not supported"}, false},
		{errors.New("Hosted tool 'web_search' rejected"), false},
		{nil, false},
	}
	for _, c := range cases {
		if got := isHostedSearchRejection(c.err); got != c.want {
			t.Errorf("isHostedSearchRejection(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestWebSearchCallItemCarriesQueries(t *testing.T) {
	// Shape of a real response.output_item.done for a hosted search.
	raw := `{"id":"ws_1","type":"web_search_call","status":"completed",` +
		`"action":{"type":"search","queries":["picoclaw github","picoclaw docs"]}}`
	var item wsOutputItem
	if err := json.Unmarshal([]byte(raw), &item); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if item.Status != "completed" || item.Action == nil || item.Action.Type != "search" ||
		strings.Join(item.Action.Queries, "|") != "picoclaw github|picoclaw docs" {
		t.Fatalf("item = %+v, action = %+v", item, item.Action)
	}
	logHostedWebSearch(item) // must not panic on a full item
	logHostedWebSearch(wsOutputItem{Type: "web_search_call"})
}
