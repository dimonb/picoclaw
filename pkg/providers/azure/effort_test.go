package azure

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAzureResponsesEffortRequestIsolation(t *testing.T) {
	captured := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		captured <- body
		writeValidResponse(w)
	}))
	defer server.Close()
	p := NewProvider("unused", server.URL, "", "")
	if !p.SupportsThinking() {
		t.Fatal("capability")
	}
	for _, tt := range []struct{ model, level, want string }{
		{"gpt-5.2", "max", "xhigh"}, {"gpt-5.2", "", ""}, {"o3", "off", "low"},
		{"gpt-4o", "high", ""}, {"my-gpt5-deployment", "high", ""}, {"gpt-5.1", "off", "none"}, {"gpt-5.1", "", ""},
	} {
		_, err := p.Chat(t.Context(), []Message{{Role: "user", Content: "hi"}}, nil, tt.model, map[string]any{"thinking_level": tt.level})
		if err != nil {
			t.Fatal(err)
		}
		body := <-captured
		r, ok := body["reasoning"]
		if ok != (tt.want != "") || ok && r.(map[string]any)["effort"] != tt.want {
			t.Fatalf("%+v got %v", tt, body)
		}
		if _, ok := body["reasoning_effort"]; ok {
			t.Fatal("wrong wire shape")
		}
	}
}
