package oauthprovider

import (
	"encoding/json"
	"testing"
)

func TestBuildCodexParamsEffort(t *testing.T) {
	p := &CodexProvider{}
	if !p.SupportsThinking() {
		t.Fatal("capability")
	}
	for _, level := range []string{"max", "", "off", ""} {
		params := buildCodexParams(nil, nil, "gpt-5.2", map[string]any{"thinking_level": level}, false)
		data, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		var wire map[string]any
		if err = json.Unmarshal(data, &wire); err != nil {
			t.Fatal(err)
		}
		r, ok := wire["reasoning"]
		if ok != (level != "") {
			t.Fatalf("%q: %s", level, data)
		}
		if ok {
			want := "xhigh"
			if level == "off" {
				want = "none"
			}
			if r.(map[string]any)["effort"] != want {
				t.Fatalf("%s", data)
			}
		}
	}
}
