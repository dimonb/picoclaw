package integrationtools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	toolshared "github.com/sipeed/picoclaw/pkg/tools/shared"
)

func fakeOpenAISearch(t *testing.T, status int, output string, got *map[string]any, hdr *http.Header) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if got != nil {
			_ = json.Unmarshal(body, got)
		}
		if hdr != nil {
			*hdr = r.Header.Clone()
		}
		w.WriteHeader(status)
		if status == http.StatusOK {
			_ = json.NewEncoder(w).Encode(map[string]any{"output": output, "results": []any{}})
			return
		}
		_, _ = w.Write([]byte(`{"error":{"message":"nope"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func testOpenAIOpts(endpoint string) WebSearchToolOptions {
	return WebSearchToolOptions{
		OpenAIEnabled:     true,
		OpenAIModel:       "gpt-test",
		OpenAITokenSource: func() (string, string, error) { return "tok", "acc", nil },
		OpenAIEndpoint:    endpoint,
	}
}

func TestOpenAISearchProviderRequestAndOutput(t *testing.T) {
	var body map[string]any
	var hdr http.Header
	out := "Go 1.27 is released (https://go.dev/blog/go1.27)\nciteturn0search0 Today the Go team..."
	endpoint := fakeOpenAISearch(t, http.StatusOK, out, &body, &hdr)

	p := newOpenAISearchProvider(testOpenAIOpts(endpoint), http.DefaultClient)
	res, err := p.Search(context.Background(), "go 1.27", 3, "w")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if strings.ContainsAny(res, "") || strings.Contains(res, "turn0search0") {
		t.Fatalf("citation markers left in output: %q", res)
	}
	if !strings.HasPrefix(res, "Results for: go 1.27 (via OpenAI)") || !strings.Contains(res, "go.dev/blog/go1.27") {
		t.Fatalf("unexpected output: %q", res)
	}

	if hdr.Get("Authorization") != "Bearer tok" || hdr.Get("Chatgpt-Account-Id") != "acc" ||
		hdr.Get("Originator") != "codex_cli_rs" {
		t.Fatalf("headers = %v", hdr)
	}
	if body["model"] != "gpt-test" || body["id"] == "" {
		t.Fatalf("body = %v", body)
	}
	cmds := body["commands"].(map[string]any)
	q := cmds["search_query"].([]any)[0].(map[string]any)
	if q["q"] != "go 1.27" || q["recency"] != float64(7) || cmds["response_length"] != "short" {
		t.Fatalf("commands = %v", cmds)
	}
	settings := body["settings"].(map[string]any)
	if settings["external_web_access"] != true || body["max_output_tokens"] != float64(openAISearchDefaultMaxToken) {
		t.Fatalf("settings = %v, max_output_tokens = %v", settings, body["max_output_tokens"])
	}
}

func TestOpenAISearchProviderFollowsTurnModelAndSession(t *testing.T) {
	var body map[string]any
	endpoint := fakeOpenAISearch(t, http.StatusOK, "ok", &body, nil)
	opts := testOpenAIOpts(endpoint)
	opts.OpenAIModel = ""
	p := newOpenAISearchProvider(opts, http.DefaultClient)

	search := func(model, sessionKey string) (string, string) {
		ctx := toolshared.WithToolModel(context.Background(), model)
		ctx = toolshared.WithToolSessionContext(ctx, "main", sessionKey, nil)
		if _, err := p.Search(ctx, "q", 3, ""); err != nil {
			t.Fatalf("Search: %v", err)
		}
		return body["model"].(string), body["id"].(string)
	}

	model, idA := search("gpt-6.1-sol", "session-a")
	if model != "gpt-6.1-sol" {
		t.Fatalf("model = %q, want the turn's model", model)
	}
	if model, _ = search("openai/z-ai/glm-5.3", "session-a"); model != openAISearchDefaultModel {
		t.Fatalf("model = %q, want the default for a non-OpenAI turn model", model)
	}
	_, idA2 := search("gpt-6.1-sol", "session-a")
	_, idB := search("gpt-6.1-sol", "session-b")
	if idA != idA2 || idA == idB {
		t.Fatalf("session ids a=%q a2=%q b=%q: want stable per session, distinct across", idA, idA2, idB)
	}
}

func TestOpenAISearchProviderErrorStatus(t *testing.T) {
	endpoint := fakeOpenAISearch(t, http.StatusUnauthorized, "", nil, nil)
	p := newOpenAISearchProvider(testOpenAIOpts(endpoint), http.DefaultClient)
	if _, err := p.Search(context.Background(), "x", 3, ""); err == nil ||
		!strings.Contains(err.Error(), "status 401") {
		t.Fatalf("err = %v, want status 401", err)
	}
}

func TestOpenAIIsFirstAutoProviderWhenReady(t *testing.T) {
	opts := testOpenAIOpts("http://unused")
	opts.DuckDuckGoEnabled = true
	if name, _ := opts.resolveProviderName("anything"); name != "openai" {
		t.Fatalf("resolved %q, want openai", name)
	}
	opts.OpenAIEnabled = false
	if name, _ := opts.resolveProviderName("anything"); name == "openai" {
		t.Fatal("disabled openai must not be picked")
	}
}

type countingSearchProvider struct{ calls int }

func (s *countingSearchProvider) Search(context.Context, string, int, string) (string, error) {
	s.calls++
	return "stub results", nil
}

func TestWebSearchFallsBackWhenOpenAIFails(t *testing.T) {
	endpoint := fakeOpenAISearch(t, http.StatusInternalServerError, "", nil, nil)
	openai := newOpenAISearchProvider(testOpenAIOpts(endpoint), http.DefaultClient)
	stub := &countingSearchProvider{}
	tool := &WebSearchTool{
		provider:         openai,
		maxResults:       10,
		providerResolver: func(string) (SearchProvider, int) { return openai, 10 },
		fallbackResolver: func(string) (SearchProvider, int) { return stub, 5 },
	}

	res := tool.Execute(context.Background(), map[string]any{"query": "x"})
	if res.IsError || res.ForLLM != "stub results" || stub.calls != 1 {
		t.Fatalf("result = %+v, stub calls = %d", res, stub.calls)
	}
}
