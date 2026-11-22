package cliprovider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// createSessionCLI records each invocation's args and stdin, echoes the
// session id it was given, and fails a --resume of the id in failResume.
func createSessionCLI(t *testing.T, dir string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("mock CLI scripts not supported on Windows")
	}
	script := filepath.Join(dir, "claude")
	content := fmt.Sprintf(`#!/bin/sh
D='%s'
n=$(ls "$D" | grep -c '^args')
echo "$@" > "$D/args$n"
cat > "$D/stdin$n"
sid=none
prev=""
for a in "$@"; do
  if [ "$prev" = "--session-id" ] || [ "$prev" = "--resume" ]; then sid="$a"; fi
  if [ "$prev" = "--resume" ] && [ -f "$D/failResume" ] && [ "$(cat "$D/failResume")" = "$a" ]; then
    echo "No conversation found with session ID: $a" >&2; exit 1
  fi
  prev="$a"
done
printf '{"type":"result","result":"reply%%s","session_id":"%%s","num_turns":1,"usage":{"input_tokens":5,"output_tokens":1}}' "$n" "$sid"
`, dir)
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

func readCall(t *testing.T, dir string, n int) (string, string) {
	t.Helper()
	a, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("args%d", n)))
	if err != nil {
		t.Fatalf("call %d not recorded: %v", n, err)
	}
	s, _ := os.ReadFile(filepath.Join(dir, fmt.Sprintf("stdin%d", n)))
	return string(a), string(s)
}

func newSessionTestProvider(t *testing.T) (*ClaudeCliProvider, string) {
	dir := t.TempDir()
	p := NewClaudeCliProvider(t.TempDir())
	p.command = createSessionCLI(t, dir)
	ids := 0
	p.newSessionID = func() string { ids++; return fmt.Sprintf("sid-%d", ids) }
	return p, dir
}

func TestChat_ResumesSessionWithDelta(t *testing.T) {
	p, dir := newSessionTestProvider(t)
	opts := map[string]any{"session_key": "chat-1"}
	sys := Message{Role: "system", Content: "sys"}
	h1 := []Message{sys, {Role: "user", Content: "first"}}

	r1, err := p.Chat(context.Background(), h1, nil, "", opts)
	if err != nil {
		t.Fatal(err)
	}
	args, _ := readCall(t, dir, 0)
	if !strings.Contains(args, "--session-id sid-1") || strings.Contains(args, "--resume") {
		t.Fatalf("first call should start session sid-1, got %s", args)
	}

	// The system prompt may change between turns; only the conversation counts.
	h2 := []Message{
		{Role: "system", Content: "sys v2"},
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: r1.Content},
		{Role: "user", Content: "second"},
	}
	if _, err := p.Chat(context.Background(), h2, nil, "", opts); err != nil {
		t.Fatal(err)
	}
	args, stdin := readCall(t, dir, 1)
	if !strings.Contains(args, "--resume sid-1") {
		t.Fatalf("second call should resume sid-1, got %s", args)
	}
	if stdin != "second" {
		t.Fatalf("resumed call should send only the new message, got %q", stdin)
	}
}

func TestChat_FreshSessionWhenHistoryDiverges(t *testing.T) {
	p, dir := newSessionTestProvider(t)
	opts := map[string]any{"session_key": "chat-1"}
	if _, err := p.Chat(context.Background(), []Message{
		{Role: "user", Content: "a"}, {Role: "assistant", Content: "b"}, {Role: "user", Content: "c"},
	}, nil, "", opts); err != nil {
		t.Fatal(err)
	}
	// History was summarized: the prefix no longer matches.
	if _, err := p.Chat(context.Background(), []Message{
		{Role: "user", Content: "summary of a/b"},
		{Role: "user", Content: "c"},
		{Role: "assistant", Content: "reply0"},
		{Role: "user", Content: "d"},
	}, nil, "", opts); err != nil {
		t.Fatal(err)
	}
	args, stdin := readCall(t, dir, 1)
	if strings.Contains(args, "--resume") || !strings.Contains(args, "--session-id sid-2") {
		t.Fatalf("diverged history should start a fresh session, got %s", args)
	}
	if !strings.Contains(stdin, "summary of a/b") || !strings.Contains(stdin, "User: d") {
		t.Fatalf("fresh session should receive full history, got %q", stdin)
	}
}

func TestChat_MissingSessionFallsBackToFresh(t *testing.T) {
	p, dir := newSessionTestProvider(t)
	opts := map[string]any{"session_key": "chat-1"}
	if _, err := p.Chat(context.Background(), []Message{{Role: "user", Content: "a"}}, nil, "", opts); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "failResume"), []byte("sid-1"), 0o644); err != nil {
		t.Fatal(err)
	}
	resp, err := p.Chat(context.Background(), []Message{
		{Role: "user", Content: "a"}, {Role: "assistant", Content: "reply0"}, {Role: "user", Content: "b"},
	}, nil, "", opts)
	if err != nil {
		t.Fatalf("expected fallback to succeed, got %v", err)
	}
	if resp.Content != "reply2" {
		t.Fatalf("unexpected reply %q", resp.Content)
	}
	args, stdin := readCall(t, dir, 2)
	if !strings.Contains(args, "--session-id sid-2") || !strings.Contains(stdin, "User: a") {
		t.Fatalf("fallback should resend full history in a new session, got %s / %q", args, stdin)
	}
}

func TestChat_NoSessionKeyDisablesPersistence(t *testing.T) {
	p, dir := newSessionTestProvider(t)
	if _, err := p.Chat(context.Background(), []Message{{Role: "user", Content: "a"}}, nil, "", nil); err != nil {
		t.Fatal(err)
	}
	args, _ := readCall(t, dir, 0)
	if !strings.Contains(args, "--no-session-persistence") || strings.Contains(args, "--session-id") {
		t.Fatalf("without session_key no session should be stored, got %s", args)
	}
}

func TestChat_PassesEffort(t *testing.T) {
	cases := map[string]string{"high": "--effort high", "off": "--effort low", "max": "--effort max", "adaptive": ""}
	for level, want := range cases {
		p, dir := newSessionTestProvider(t)
		if _, err := p.Chat(context.Background(), []Message{{Role: "user", Content: "a"}}, nil, "",
			map[string]any{"thinking_level": level}); err != nil {
			t.Fatal(err)
		}
		args, _ := readCall(t, dir, 0)
		if want == "" && strings.Contains(args, "--effort") {
			t.Errorf("%s: expected no --effort, got %s", level, args)
		}
		if want != "" && !strings.Contains(args, want) {
			t.Errorf("%s: expected %q, got %s", level, want, args)
		}
	}
	if !NewClaudeCliProvider("").SupportsThinking() {
		t.Error("claude cli provider should support thinking")
	}
}

// A silent group turn stores an assistant "NO_REPLY" row and the group note
// changes the system prompt; the next turn must still resume the session.
func TestChat_ResumesAfterSilentReply(t *testing.T) {
	p, dir := newSessionTestProvider(t)
	opts := map[string]any{"session_key": "group-1"}
	h1 := []Message{{Role: "system", Content: "sys"}, {Role: "user", Content: "lunch at 12?"}}
	if _, err := p.Chat(context.Background(), h1, nil, "", opts); err != nil {
		t.Fatal(err)
	}
	h2 := []Message{
		{Role: "system", Content: "sys\n\n## Group Chat\n... addressed to you: yes"},
		{Role: "user", Content: "lunch at 12?"},
		{Role: "assistant", Content: "NO_REPLY"},
		{Role: "user", Content: "@bot what do you think?"},
	}
	if _, err := p.Chat(context.Background(), h2, nil, "", opts); err != nil {
		t.Fatal(err)
	}
	args, stdin := readCall(t, dir, 1)
	if !strings.Contains(args, "--resume sid-1") || stdin != "@bot what do you think?" {
		t.Fatalf("expected resume with only the new message, args=%s stdin=%q", args, stdin)
	}
}
