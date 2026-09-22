package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// smallSpillPolicy keeps test payloads small: 400 tokens is ~1000 chars,
// which leaves room for the minimum preview size on each side.
func smallSpillPolicy() OutputSpillPolicy {
	return OutputSpillPolicy{MaxTokens: 400, PreviewLines: 3, MaxAge: time.Hour}
}

func numberedLines(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "line %03d: some output text here\n", i)
	}
	return b.String()
}

var spillPathRe = regexp.MustCompile(`Full output saved to (\S+)\]`)

func spilledPath(t *testing.T, forLLM string) string {
	t.Helper()
	m := spillPathRe.FindStringSubmatch(forLLM)
	if m == nil {
		t.Fatalf("expected a saved-to path in the header, got:\n%s", forLLM)
	}
	return m[1]
}

func TestOutputSpill_UnderThresholdUntouched(t *testing.T) {
	workspace := t.TempDir()
	s := newOutputSpiller(workspace, smallSpillPolicy())
	text := numberedLines(5)
	result := UserResult(text)

	s.apply(result, "exec", spillHints{readFile: true, exec: true})

	if result.ForLLM != text || result.ForUser != text {
		t.Fatalf("expected result untouched under threshold, got ForLLM=%q ForUser=%q", result.ForLLM, result.ForUser)
	}
	if _, err := os.Stat(filepath.Join(workspace, "tmp", outputSpillDirName)); !os.IsNotExist(err) {
		t.Fatalf("expected no spill directory for an untouched result, stat err=%v", err)
	}
}

func TestOutputSpill_OverThresholdWritesFileAndPreviews(t *testing.T) {
	workspace := t.TempDir()
	s := newOutputSpiller(workspace, smallSpillPolicy())
	text := numberedLines(100)
	result := UserResult(text)

	s.apply(result, "exec", spillHints{readFile: true, exec: true, sendFile: true})

	path := spilledPath(t, result.ForLLM)
	if !strings.HasPrefix(path, filepath.Join(workspace, "tmp", outputSpillDirName)+string(os.PathSeparator)) {
		t.Fatalf("expected spill file under <workspace>/tmp/%s, got %q", outputSpillDirName, path)
	}
	if !strings.HasPrefix(filepath.Base(path), "exec-") || filepath.Ext(path) != ".txt" {
		t.Fatalf("expected <tool>-<timestamp>-<id>.txt, got %q", filepath.Base(path))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("spill file unreadable: %v", err)
	}
	if string(data) != text {
		t.Fatalf("spill file must hold the full untruncated output")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("expected spill file mode 0600, got %o", info.Mode().Perm())
	}

	got := result.ForLLM
	wantHeader := fmt.Sprintf(
		"[exec output too large for the context: %d chars, 100 lines.",
		utf8.RuneCountInString(text),
	)
	if !strings.HasPrefix(got, wantHeader) {
		t.Fatalf("unexpected header, got:\n%s", got)
	}
	for _, want := range []string{
		"--- head (first 3 lines) ---\n",
		"line 001:", "line 002:", "line 003:",
		"--- 94 lines (", ") omitted ---\n",
		"--- tail (last 3 lines) ---\n",
		"line 098:", "line 099:", "line 100:",
		"read_file on that path", "grep -n", "send_file with that path",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("preview missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "line 050:") {
		t.Errorf("middle of the output must not be inline:\n%s", got)
	}
	if result.ForUser != result.ForLLM {
		t.Fatalf("ForUser mirrored ForLLM and must get the same preview")
	}
	if estimateOutputTokens(got) > s.policy.MaxTokens {
		t.Fatalf("preview itself exceeds the threshold: %d tokens", estimateOutputTokens(got))
	}
}

func TestOutputSpill_ErrorResultKeepsErrorAndSpills(t *testing.T) {
	workspace := t.TempDir()
	s := newOutputSpiller(workspace, smallSpillPolicy())
	text := numberedLines(60) + "\n[Command exited with code 1]"
	result := ErrorResult(text)
	result.ForUser = text

	s.apply(result, "exec", spillHints{readFile: true})

	if !result.IsError {
		t.Fatalf("spilling must not clear IsError")
	}
	if !strings.HasPrefix(result.ForLLM, "[exec error output too large") {
		t.Fatalf("expected an error header, got:\n%s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "[Command exited with code 1]") {
		t.Fatalf("exit status at the end of the output must survive in the tail:\n%s", result.ForLLM)
	}
	data, err := os.ReadFile(spilledPath(t, result.ForLLM))
	if err != nil || string(data) != text {
		t.Fatalf("spill file must hold the full stderr, err=%v", err)
	}
}

func TestOutputSpill_NoWorkspaceFallsBackToPreview(t *testing.T) {
	s := newOutputSpiller("", smallSpillPolicy())
	text := numberedLines(100)
	result := UserResult(text)

	s.apply(result, "exec", spillHints{readFile: true, exec: true})

	got := result.ForLLM
	if !strings.Contains(got, "Full output not saved (no workspace)") {
		t.Fatalf("expected the no-workspace marker, got:\n%s", got)
	}
	if strings.Contains(got, "saved to") || strings.Contains(got, "read_file") {
		t.Fatalf("no file means no path and no read hint:\n%s", got)
	}
	for _, want := range []string{"line 001:", "line 003:", "--- 94 lines (", "line 098:", "line 100:"} {
		if !strings.Contains(got, want) {
			t.Errorf("fallback preview missing %q:\n%s", want, got)
		}
	}
	if result.ForUser != got {
		t.Fatalf("ForUser mirrored ForLLM and must get the same fallback preview")
	}
}

func TestOutputSpill_WriteFailureFallsBackToPreview(t *testing.T) {
	root := t.TempDir()
	notADir := filepath.Join(root, "file")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newOutputSpiller(notADir, smallSpillPolicy())
	result := UserResult(numberedLines(100))

	s.apply(result, "exec", spillHints{readFile: true})

	if !strings.Contains(result.ForLLM, "Full output not saved (write failed)") {
		t.Fatalf("expected the write-failed marker, got:\n%s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "line 001:") || !strings.Contains(result.ForLLM, "line 100:") {
		t.Fatalf("head and tail must still be shown:\n%s", result.ForLLM)
	}
}

func TestOutputSpill_DistinctOversizedForUserGetsOwnPreview(t *testing.T) {
	workspace := t.TempDir()
	s := newOutputSpiller(workspace, smallSpillPolicy())
	result := &ToolResult{
		ForLLM:  numberedLines(100),
		ForUser: strings.Repeat("user-facing text\n", 100),
	}

	s.apply(result, "custom", spillHints{readFile: true})

	if !strings.Contains(result.ForUser, "user-facing text") || strings.Contains(result.ForUser, "line 001:") {
		t.Fatalf("ForUser must be previewed from its own text:\n%s", result.ForUser)
	}
	if strings.Contains(result.ForUser, "saved to") || strings.Contains(result.ForUser, "read_file") {
		t.Fatalf("a distinct ForUser preview carries no path and no tool hint:\n%s", result.ForUser)
	}
	if !strings.Contains(result.ForUser, "This user-facing copy is not saved") {
		t.Fatalf("the reason must say the user copy was not saved, not blame a missing workspace:\n%s", result.ForUser)
	}
	if strings.Contains(result.ForUser, "no workspace") {
		t.Fatalf(
			"a workspace exists and the model's copy was written; the user must not be told otherwise:\n%s",
			result.ForUser,
		)
	}
	if estimateOutputTokens(result.ForUser) > s.policy.MaxTokens {
		t.Fatalf("ForUser preview exceeds the threshold")
	}
}

func TestOutputSpill_SmallDistinctForUserUntouched(t *testing.T) {
	s := newOutputSpiller(t.TempDir(), smallSpillPolicy())
	result := &ToolResult{ForLLM: numberedLines(100), ForUser: "done"}

	s.apply(result, "custom", spillHints{})

	if result.ForUser != "done" {
		t.Fatalf("a small distinct ForUser must not change, got %q", result.ForUser)
	}
}

func TestOutputSpill_CleanupRemovesOnlyExpiredSpillFiles(t *testing.T) {
	workspace := t.TempDir()
	s := newOutputSpiller(workspace, smallSpillPolicy())
	dir := s.dir
	if err := os.MkdirAll(filepath.Join(dir, "keep-subdir"), 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	write := func(name string, mtime time.Time) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		return p
	}
	expired := write("exec-20260101T000000-old.txt", old)
	fresh := write("exec-20260101T000000-new.txt", time.Now())
	foreign := write("notes.md", old)

	s.apply(UserResult(numberedLines(100)), "exec", spillHints{})

	if _, err := os.Stat(expired); !os.IsNotExist(err) {
		t.Errorf("expired spill file should be removed, stat err=%v", err)
	}
	for _, p := range []string{fresh, foreign, filepath.Join(dir, "keep-subdir")} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s must survive cleanup: %v", filepath.Base(p), err)
		}
	}
}

func TestOutputSpill_CleanupIsThrottled(t *testing.T) {
	workspace := t.TempDir()
	s := newOutputSpiller(workspace, smallSpillPolicy())
	s.apply(UserResult(numberedLines(100)), "exec", spillHints{})

	old := time.Now().Add(-2 * time.Hour)
	p := filepath.Join(s.dir, "exec-20260101T000000-old.txt")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}

	s.apply(UserResult(numberedLines(100)), "exec", spillHints{})
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("a second spill right after the first must not sweep again: %v", err)
	}

	s.lastSweep = time.Now().Add(-2 * outputSpillSweepInterval)
	s.apply(UserResult(numberedLines(100)), "exec", spillHints{})
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("once the interval has passed the sweep must run, stat err=%v", err)
	}
}

func TestOutputSpill_HintNamesOnlyRegisteredTools(t *testing.T) {
	cases := []struct {
		hints spillHints
		want  string
	}{
		{spillHints{}, ""},
		{spillHints{readFile: true}, "[Use read_file on that path (partial reads are supported) to read more.]"},
		{
			spillHints{exec: true, sendFile: true},
			"[Use exec with grep -n or sed -n on it to find what you need; send_file with that path to hand the whole output to the user.]",
		},
	}
	for _, c := range cases {
		if got := spillHintLine(c.hints); got != c.want {
			t.Errorf("spillHintLine(%+v) = %q, want %q", c.hints, got, c.want)
		}
	}
}

func TestOutputSpill_PreviewCutsOnRuneBoundaries(t *testing.T) {
	// One long line of multibyte runes: the line budget never triggers, the
	// byte budget does, and the cut must not split a rune.
	s := newOutputSpiller("", OutputSpillPolicy{MaxTokens: 200, PreviewLines: 3})
	text := strings.Repeat("日本語テキスト", 300)
	result := UserResult(text)

	s.apply(result, "exec", spillHints{})

	if !utf8.ValidString(result.ForLLM) {
		t.Fatalf("preview contains a split rune")
	}
	if !strings.Contains(result.ForLLM, "--- head (first 1 lines) ---") ||
		!strings.Contains(result.ForLLM, "--- tail (last 1 lines) ---") {
		t.Fatalf("a single long line must yield a one-line head and tail:\n%s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, ") omitted ---") {
		t.Fatalf("the middle of a single long line must be reported as omitted:\n%s", result.ForLLM)
	}
}

func TestOutputHeadEndAndTailStart(t *testing.T) {
	text := "a\nbb\nccc\ndddd\n"
	if got := outputHeadEnd(text, 2, 100); text[:got] != "a\nbb\n" {
		t.Errorf("head 2 lines = %q", text[:got])
	}
	if got := outputHeadEnd(text, 10, 100); got != len(text) {
		t.Errorf("head beyond the text must stop at its end, got %d", got)
	}
	if got := outputHeadEnd(text, 10, 4); text[:got] != "a\nbb" {
		t.Errorf("head byte cap = %q", text[:got])
	}
	if got := outputTailStart(text, 2, 100); text[got:] != "ccc\ndddd\n" {
		t.Errorf("tail 2 lines = %q", text[got:])
	}
	if got := outputTailStart("x\ny", 1, 100); "x\ny"[got:] != "y" {
		t.Errorf("tail without trailing newline = %q", "x\ny"[got:])
	}
	if got := outputTailStart(text, 10, 100); got != 0 {
		t.Errorf("tail beyond the text must start at 0, got %d", got)
	}
	if got := outputTailStart(text, 10, 3); text[got:] != "dd\n" {
		t.Errorf("tail byte cap = %q", text[got:])
	}
	if got := countOutputLines(text); got != 4 {
		t.Errorf("countOutputLines = %d, want 4", got)
	}
	if got := countOutputLines("x\ny"); got != 2 {
		t.Errorf("countOutputLines without trailing newline = %d, want 2", got)
	}
}

// --- registry integration ---

func TestRegistry_SpillsOversizedResultAndHintsRegisteredTools(t *testing.T) {
	workspace := t.TempDir()
	r := NewToolRegistry()
	r.SetOutputSpill(workspace, smallSpillPolicy())
	big := newMockTool("big", "returns a lot")
	big.result = UserResult(numberedLines(100))
	r.Register(big)
	r.Register(newMockTool("read_file", "reads"))
	r.Register(newMockTool("exec", "runs"))

	result := r.Execute(context.Background(), "big", nil)

	path := spilledPath(t, result.ForLLM)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("spill file missing: %v", err)
	}
	if !strings.Contains(result.ForLLM, "read_file on that path") || !strings.Contains(result.ForLLM, "grep -n") {
		t.Fatalf("hint must name the registered tools:\n%s", result.ForLLM)
	}
	if strings.Contains(result.ForLLM, "send_file") {
		t.Fatalf("send_file is not registered and must not be suggested:\n%s", result.ForLLM)
	}
}

func TestRegistry_DefaultPolicyWithoutWorkspaceStillBounds(t *testing.T) {
	r := NewToolRegistry()
	big := newMockTool("big", "returns a lot")
	// ~25K chars is over the 8000-token default (~20K chars).
	big.result = UserResult(numberedLines(800))
	r.Register(big)

	result := r.Execute(context.Background(), "big", nil)

	if !strings.Contains(result.ForLLM, "Full output not saved (no workspace)") {
		t.Fatalf("a registry without a workspace must still cut with a marker:\n%.300s", result.ForLLM)
	}
	if estimateOutputTokens(result.ForLLM) > defaultOutputSpillMaxTokens {
		t.Fatalf("fallback preview exceeds the default threshold")
	}
}

func TestRegistry_SpillsAsyncCallbackResult(t *testing.T) {
	workspace := t.TempDir()
	r := NewToolRegistry()
	r.SetOutputSpill(workspace, smallSpillPolicy())
	tool := &mockAsyncRegistryTool{mockRegistryTool: *newMockTool("bg", "async")}
	tool.result = AsyncResult("started")
	r.Register(tool)

	var delivered *ToolResult
	r.ExecuteWithContext(context.Background(), "bg", nil, "", "", func(_ context.Context, res *ToolResult) {
		delivered = res
	})
	if tool.lastCB == nil {
		t.Fatalf("async tool did not receive a callback")
	}
	tool.lastCB(context.Background(), UserResult(numberedLines(100)))

	if delivered == nil {
		t.Fatalf("callback result was not delivered")
	}
	if _, err := os.Stat(spilledPath(t, delivered.ForLLM)); err != nil {
		t.Fatalf("async result must be spilled like a sync one: %v", err)
	}
}

func TestRegistry_CloneKeepsOutputSpill(t *testing.T) {
	workspace := t.TempDir()
	r := NewToolRegistry()
	r.SetOutputSpill(workspace, smallSpillPolicy())
	big := newMockTool("big", "returns a lot")
	big.result = UserResult(numberedLines(100))
	r.Register(big)

	result := r.Clone().Execute(context.Background(), "big", nil)

	if !strings.HasPrefix(spilledPath(t, result.ForLLM), workspace) {
		t.Fatalf("clone must spill into the parent's workspace")
	}
}

func TestRegistry_ExecOutputSpilledNotTruncated(t *testing.T) {
	workspace := t.TempDir()
	r := NewToolRegistry()
	r.SetOutputSpill(workspace, OutputSpillPolicy{MaxTokens: 2000, PreviewLines: 5})
	execTool, err := NewExecTool(workspace, false)
	if err != nil {
		t.Fatalf("exec tool: %v", err)
	}
	r.Register(execTool)

	result := r.Execute(context.Background(), "exec", map[string]any{
		"action":  "run",
		"command": "seq 1 400 | sed -e 's/^/row /' -e 's/$/: 0123456789012345678901234567890123456789/'",
	})

	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
	if strings.Contains(result.ForLLM, "... (truncated") {
		t.Fatalf("the old hard cut must be gone:\n%s", result.ForLLM)
	}
	data, err := os.ReadFile(spilledPath(t, result.ForLLM))
	if err != nil {
		t.Fatalf("spill file: %v", err)
	}
	if !strings.Contains(string(data), "row 400:") || !strings.Contains(string(data), "row 1:") {
		t.Fatalf("spill file must hold the whole command output")
	}
	if !strings.Contains(result.ForLLM, "row 1:") || !strings.Contains(result.ForLLM, "row 400:") {
		t.Fatalf("preview must show head and tail:\n%s", result.ForLLM)
	}
	t.Logf("preview:\n%s", result.ForLLM)
}

// Normalization replaces a large base64-like payload with a short marker
// before the policy runs, so apply() sees only the marker. The bytes must
// still reach a file — dropping them is what the removed MCP artifact writer
// used to prevent.
func TestRegistry_OmittedBase64PayloadIsStillSpilled(t *testing.T) {
	workspace := t.TempDir()
	r := NewToolRegistry()
	r.SetOutputSpill(workspace, smallSpillPolicy())
	payload := strings.Repeat("QUJD", 4000)
	tool := newMockTool("dump", "returns base64")
	tool.result = SilentResult(payload)
	r.Register(tool)
	r.Register(newMockTool("read_file", "reads"))

	result := r.Execute(context.Background(), "dump", nil)

	if !strings.Contains(result.ForLLM, largeBase64OmittedMessage) {
		t.Fatalf("expected the payload to stay out of context, got:\n%.200s", result.ForLLM)
	}
	if strings.Contains(result.ForLLM, payload[:64]) {
		t.Fatalf("the payload itself must not be inline")
	}
	m := regexp.MustCompile(`saved to (\S+)\]`).FindStringSubmatch(result.ForLLM)
	if m == nil {
		t.Fatalf("expected a saved-to path for the omitted payload, got:\n%s", result.ForLLM)
	}
	data, err := os.ReadFile(m[1])
	if err != nil {
		t.Fatalf("spill file unreadable: %v", err)
	}
	if string(data) != payload {
		t.Fatalf("spill file must hold the raw payload, got %d of %d chars", len(data), len(payload))
	}
	if !strings.Contains(result.ForLLM, "read_file on that path") {
		t.Fatalf("expected the hint to name the registered tools:\n%s", result.ForLLM)
	}
}

func TestOutputSpill_OmittedPayloadUnderThresholdIsLeftAlone(t *testing.T) {
	workspace := t.TempDir()
	s := newOutputSpiller(workspace, smallSpillPolicy())
	result := SilentResult(largeBase64OmittedMessage)

	s.applyOmitted(result, "dump", "QUJD", spillHints{})

	if result.ForLLM != largeBase64OmittedMessage {
		t.Fatalf("a small raw payload needs no file, got %q", result.ForLLM)
	}
	if _, err := os.Stat(filepath.Join(workspace, "tmp", outputSpillDirName)); !os.IsNotExist(err) {
		t.Fatalf("expected no spill directory, stat err=%v", err)
	}
}

// A symlink planted at the spill directory would send both writes and the
// sweep's deletions outside the workspace.
func TestOutputSpill_RefusesSymlinkedSpillDir(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(victim, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(victim, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, "tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "tmp", outputSpillDirName)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	s := newOutputSpiller(workspace, smallSpillPolicy())
	result := UserResult(numberedLines(100))
	s.apply(result, "exec", spillHints{})

	if !strings.Contains(result.ForLLM, "Full output not saved (write failed)") {
		t.Fatalf("expected the write to be refused, got:\n%.300s", result.ForLLM)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("a file outside the workspace must not be removed: %v", err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("nothing may be written outside the workspace, found %d entries", len(entries))
	}
}
