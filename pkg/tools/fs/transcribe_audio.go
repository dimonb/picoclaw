package fstools

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// TranscribeFunc turns a local audio file into text. language is an
// ISO-639-1 hint and prompt is context for the recognizer; both may be empty.
type TranscribeFunc func(ctx context.Context, path, language, prompt string) (string, error)

// TranscribeAudioTool lets the model transcribe a voice message itself, so it
// chooses the language and the context instead of a fixed pre-pass guessing
// them. Auto-detection is what breaks on a learner's speech: short Hebrew
// answers come back as Russian transliteration unless the language is named.
type TranscribeAudioTool struct {
	workspace  string
	restrict   bool
	allowPaths []*regexp.Regexp
	transcribe TranscribeFunc
}

func NewTranscribeAudioTool(
	workspace string,
	restrict bool,
	transcribe TranscribeFunc,
	allowPaths ...[]*regexp.Regexp,
) *TranscribeAudioTool {
	var patterns []*regexp.Regexp
	if len(allowPaths) > 0 {
		patterns = allowPaths[0]
	}
	return &TranscribeAudioTool{
		workspace:  workspace,
		restrict:   restrict,
		allowPaths: patterns,
		transcribe: transcribe,
	}
}

func (t *TranscribeAudioTool) Name() string { return "transcribe_audio" }

func (t *TranscribeAudioTool) Description() string {
	return "Transcribe a voice message or other audio file to text. Pass the [audio:/path] path. " +
		"Name the spoken language whenever you can predict it: auto-detection fails on short or " +
		"accented speech (Hebrew from a Russian speaker comes back as Russian transliteration). " +
		"prompt gives the recognizer context: topic, names, rare words. Do not put the answer you " +
		"expect into prompt when checking someone's speech — it can make the recognizer hear it."
}

func (t *TranscribeAudioTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Path to the audio file, as given in the [audio:/path] tag.",
			},
			"language": map[string]any{
				"type":        "string",
				"description": "Spoken language as an ISO-639-1 code (he, ru, en, ...). Omit to auto-detect.",
			},
			"prompt": map[string]any{
				"type":        "string",
				"description": "Optional context for the recognizer: topic, names, vocabulary in that language.",
			},
		},
		"required": []string{"path"},
	}
}

func (t *TranscribeAudioTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	path, _ := args["path"].(string)
	if strings.TrimSpace(path) == "" {
		return ErrorResult("path is required")
	}
	language, _ := args["language"].(string)
	prompt, _ := args["prompt"].(string)
	if t.transcribe == nil {
		return ErrorResult("no transcription model configured (tools.transcribe_audio.model_name)")
	}

	resolved, err := validatePathWithAllowPaths(path, t.workspace, t.restrict, t.allowPaths)
	if err != nil {
		return ErrorResult(fmt.Sprintf("invalid path: %v", err))
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return ErrorResult(fmt.Sprintf("file not found: %v", err))
	}
	if info.IsDir() {
		return ErrorResult("path is a directory, expected an audio file")
	}

	text, err := t.transcribe(ctx, resolved, strings.ToLower(strings.TrimSpace(language)), strings.TrimSpace(prompt))
	if err != nil {
		return ErrorResult(fmt.Sprintf("transcription failed: %v", err))
	}
	if strings.TrimSpace(text) == "" {
		return NewToolResult("(no speech recognized)")
	}
	return NewToolResult(text)
}
