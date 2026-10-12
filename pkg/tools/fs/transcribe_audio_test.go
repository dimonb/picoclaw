package fstools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTranscribeAudioToolPassesHints(t *testing.T) {
	ws := t.TempDir()
	audio := filepath.Join(ws, "voice.ogg")
	if err := os.WriteFile(audio, []byte("OggS"), 0o600); err != nil {
		t.Fatal(err)
	}
	var gotPath, gotLang, gotPrompt string
	tool := NewTranscribeAudioTool(ws, true, func(_ context.Context, path, language, prompt string) (string, error) {
		gotPath, gotLang, gotPrompt = path, language, prompt
		return "שלום", nil
	})

	res := tool.Execute(context.Background(), map[string]any{"path": audio, "language": " HE ", "prompt": " урок "})
	if res.IsError {
		t.Fatalf("Execute: %s", res.ForLLM)
	}
	if res.ForLLM != "שלום" {
		t.Errorf("ForLLM = %q", res.ForLLM)
	}
	if gotPath != audio || gotLang != "he" || gotPrompt != "урок" {
		t.Errorf("transcribe got path=%q language=%q prompt=%q", gotPath, gotLang, gotPrompt)
	}
}

func TestTranscribeAudioToolRejectsPathsOutsideWorkspace(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "voice.ogg")
	if err := os.WriteFile(outside, []byte("OggS"), 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	tool := NewTranscribeAudioTool(t.TempDir(), true, func(context.Context, string, string, string) (string, error) {
		called = true
		return "", nil
	})
	res := tool.Execute(context.Background(), map[string]any{"path": outside})
	if !res.IsError || !strings.Contains(res.ForLLM, "invalid path") || called {
		t.Fatalf("outside path: error=%v called=%v result=%q", res.IsError, called, res.ForLLM)
	}
}
