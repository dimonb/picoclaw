package asr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
)

// The language and prompt hints reach the /audio/transcriptions request, and a
// gpt-*-transcribe model chosen by name goes through that endpoint too.
func TestTranscriberForModelSendsLanguageAndPrompt(t *testing.T) {
	got := map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("ParseMultipartForm: %v", err)
		}
		for _, k := range []string{"model", "language", "prompt"} {
			got[k] = r.FormValue(k)
		}
		_ = json.NewEncoder(w).Encode(TranscriptionResponse{Text: "שלום"})
	}))
	defer server.Close()

	cfg := &config.Config{ModelList: []*config.ModelConfig{{
		ModelName: "gpt-transcribe",
		Model:     "openai/gpt-transcribe",
		APIBase:   server.URL,
		APIKeys:   config.SimpleSecureStrings("sk-test"),
	}}}
	tr := TranscriberForModel(cfg, "gpt-transcribe")
	if tr == nil {
		t.Fatal("gpt-transcribe was not accepted as a transcription model")
	}

	audio := filepath.Join(t.TempDir(), "voice.ogg")
	if err := os.WriteFile(audio, []byte("OggS"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := WithPrompt(WithLanguage(context.Background(), "he"), "שיעור עברית")
	res, err := tr.Transcribe(ctx, audio)
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if res.Text != "שלום" {
		t.Errorf("text = %q", res.Text)
	}
	want := map[string]string{"model": "gpt-transcribe", "language": "he", "prompt": "שיעור עברית"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}

	// Without hints the fields are left out, so the model auto-detects.
	got = map[string]string{}
	if _, err := tr.Transcribe(context.Background(), audio); err != nil {
		t.Fatalf("Transcribe without hints: %v", err)
	}
	if got["language"] != "" || got["prompt"] != "" {
		t.Errorf("hints sent without being asked: %v", got)
	}
}

// The legacy model_list scan that turns on transcription of every voice
// message must not start picking up transcribe models on its own.
func TestDetectTranscriberIgnoresTranscribeModelsInLegacyScan(t *testing.T) {
	cfg := &config.Config{ModelList: []*config.ModelConfig{{
		ModelName: "gpt-transcribe",
		Model:     "openai/gpt-transcribe",
		APIKeys:   config.SimpleSecureStrings("sk-test"),
	}}}
	if tr := DetectTranscriber(cfg); tr != nil {
		t.Fatalf("DetectTranscriber picked %s without voice.model_name", tr.Name())
	}
}
