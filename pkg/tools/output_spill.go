package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tokenizer"
)

// A tool result larger than the policy allows is spilled: the full text is
// written to a file under the agent workspace and the model gets a header,
// the head and tail of the output, and the path, so it can read or grep the
// rest on demand. This runs once, at the registry, so exec, MCP and every
// other tool are bounded the same way. The spill file holds the raw output;
// the sensitive-data filter only sees the preview, which is the same trust
// domain as the workspace the command already ran in.
const (
	defaultOutputSpillMaxTokens    = 8000
	defaultOutputSpillPreviewLines = 40
	defaultOutputSpillMaxAge       = 24 * time.Hour

	// outputSpillDirName is the directory under <workspace>/tmp/ that holds
	// spill files.
	outputSpillDirName = "tool-output"
	// outputSpillSweepInterval bounds how often a write scans the spill
	// directory for expired files.
	outputSpillSweepInterval = 10 * time.Minute
	// outputSpillMinPreviewBytes keeps a preview readable even under an
	// absurdly small token threshold.
	outputSpillMinPreviewBytes = 256
)

var (
	errNoOutputSpillDir = errors.New("no workspace configured for tool output")
	// errUserCopyNotSpilled marks the user-facing copy of a result whose text
	// differs from what the model sees: only one copy is written to a file.
	errUserCopyNotSpilled = errors.New("user-facing copy not saved separately")
	// errOutputSpillDirNotOwned marks a spill directory that is not a real
	// directory the workspace owns — a symlink there would send writes and
	// deletions outside the workspace.
	errOutputSpillDirNotOwned = errors.New("spill directory is not a directory inside the workspace")
)

// OutputSpillPolicy says when a tool result is too large to stay inline and
// how much of it the model still sees. Zero values mean the defaults.
type OutputSpillPolicy struct {
	// MaxTokens is the estimated-token size above which a result is spilled.
	MaxTokens int
	// PreviewLines is how many lines of the head and of the tail stay inline.
	PreviewLines int
	// MaxAge is how long spill files are kept before a later spill removes them.
	MaxAge time.Duration
}

func (p OutputSpillPolicy) withDefaults() OutputSpillPolicy {
	if p.MaxTokens <= 0 {
		p.MaxTokens = defaultOutputSpillMaxTokens
	}
	if p.PreviewLines <= 0 {
		p.PreviewLines = defaultOutputSpillPreviewLines
	}
	if p.MaxAge <= 0 {
		p.MaxAge = defaultOutputSpillMaxAge
	}
	return p
}

// previewBytes is the byte budget for each of the head and the tail. It is a
// quarter of the threshold, so at any sane threshold the preview stays well
// under it; the floor below wins only for a threshold so small that a
// readable preview matters more.
func (p OutputSpillPolicy) previewBytes() int {
	// The estimator counts 2.5 characters per token.
	thresholdChars := p.MaxTokens * 5 / 2
	if n := thresholdChars / 4; n > outputSpillMinPreviewBytes {
		return n
	}
	return outputSpillMinPreviewBytes
}

// outputSpiller applies one policy for one registry. dir is empty when the
// registry has no workspace; the policy then degrades to a head/tail cut
// with a marker instead of a file.
type outputSpiller struct {
	dir    string
	policy OutputSpillPolicy

	mu        sync.Mutex
	lastSweep time.Time
}

func newOutputSpiller(workspace string, policy OutputSpillPolicy) *outputSpiller {
	s := &outputSpiller{policy: policy.withDefaults()}
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		return s
	}
	if abs, err := filepath.Abs(workspace); err == nil {
		workspace = abs
	}
	s.dir = filepath.Join(workspace, "tmp", outputSpillDirName)
	return s
}

// spillHints names the registered tools the preview may point the model at.
type spillHints struct {
	readFile bool
	exec     bool
	sendFile bool
}

func estimateOutputTokens(text string) int {
	return tokenizer.EstimateMessageTokens(providers.Message{Content: text})
}

// apply rewrites result in place when its ForLLM exceeds the policy. ForUser
// gets the same preview when it mirrored ForLLM, and its own file-less
// head/tail cut when it is a different oversized text. Error results are
// treated the same and stay errors.
func (s *outputSpiller) apply(result *ToolResult, toolName string, hints spillHints) {
	if s == nil || result == nil {
		return
	}
	text := result.ForLLM
	if text == "" || estimateOutputTokens(text) <= s.policy.MaxTokens {
		return
	}

	mirrored := result.ForUser == text
	path, err := s.write(toolName, text)
	if err != nil && !errors.Is(err, errNoOutputSpillDir) {
		logger.WarnCF("tool", "Failed to save oversized tool output; keeping a head/tail preview only",
			map[string]any{"tool": toolName, "chars": utf8.RuneCountInString(text), "error": err.Error()})
	}
	preview := s.preview(text, toolName, path, err, result.IsError, hints)
	result.ForLLM = preview

	switch {
	case mirrored:
		result.ForUser = preview
	case result.ForUser != "" && estimateOutputTokens(result.ForUser) > s.policy.MaxTokens:
		result.ForUser = s.preview(result.ForUser, toolName, "", errUserCopyNotSpilled, result.IsError, spillHints{})
	}

	logger.InfoCF("tool", "Oversized tool output spilled",
		map[string]any{
			"tool":          toolName,
			"chars":         utf8.RuneCountInString(text),
			"preview_chars": utf8.RuneCountInString(preview),
			"path":          path,
		})
}

// applyOmitted covers the one case apply cannot see: normalization replaced a
// payload with a short marker before the policy ran, so the bytes are already
// gone from the result. Spilling the raw text keeps them reachable instead of
// dropping them, which is what the MCP-specific artifact writer used to do for
// base64-like payloads.
func (s *outputSpiller) applyOmitted(result *ToolResult, toolName, raw string, hints spillHints) {
	if s == nil || result == nil {
		return
	}
	if raw == "" || estimateOutputTokens(raw) <= s.policy.MaxTokens {
		return
	}
	if !strings.Contains(result.ForLLM, largeBase64OmittedMessage) {
		return
	}

	path, err := s.write(toolName, raw)
	if err != nil {
		if !errors.Is(err, errNoOutputSpillDir) {
			logger.WarnCF("tool", "Failed to save omitted tool payload",
				map[string]any{"tool": toolName, "chars": utf8.RuneCountInString(raw), "error": err.Error()})
		}
		return
	}

	note := fmt.Sprintf("[Full payload (%d chars) saved to %s]", utf8.RuneCountInString(raw), path)
	if hint := spillHintLine(hints); hint != "" {
		note += "\n" + hint
	}
	result.ForLLM = strings.TrimSpace(result.ForLLM) + "\n" + note

	logger.InfoCF("tool", "Omitted tool payload spilled",
		map[string]any{"tool": toolName, "chars": utf8.RuneCountInString(raw), "path": path})
}

// write stores text under the spill directory and returns its path. It
// sweeps expired files first, at most once per outputSpillSweepInterval.
func (s *outputSpiller) write(toolName, text string) (string, error) {
	if s.dir == "" {
		return "", errNoOutputSpillDir
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return "", err
	}
	// MkdirAll and CreateTemp follow a symlink, and so would the sweep's
	// deletions. A command the model ran could have planted one here, so
	// refuse anything that is not a real directory; the caller degrades to a
	// head/tail preview.
	if info, err := os.Lstat(s.dir); err != nil {
		return "", err
	} else if !info.IsDir() {
		return "", errOutputSpillDirNotOwned
	}
	s.sweep(time.Now())

	pattern := fmt.Sprintf("%s-%s-*.txt",
		sanitizeIdentifierComponent(toolName),
		time.Now().UTC().Format("20060102T150405"))
	f, err := os.CreateTemp(s.dir, pattern)
	if err != nil {
		return "", err
	}
	path := f.Name()
	if _, err = f.WriteString(text); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err = f.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

// sweep removes spill files older than the policy's MaxAge. Only regular
// .txt files are considered, so subdirectories, symlinks and other extensions
// are left alone; the directory is spill-owned, so any expired .txt in it goes.
func (s *outputSpiller) sweep(now time.Time) {
	s.mu.Lock()
	if now.Sub(s.lastSweep) < outputSpillSweepInterval {
		s.mu.Unlock()
		return
	}
	s.lastSweep = now
	s.mu.Unlock()

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	cutoff := now.Add(-s.policy.MaxAge)
	for _, entry := range entries {
		if !entry.Type().IsRegular() || filepath.Ext(entry.Name()) != ".txt" {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(s.dir, entry.Name()))
	}
}

// preview builds the inline replacement: a header with the size and the
// file path (or why there is none), the head, an omitted-middle marker, the
// tail, and a hint naming the tools that can reach the rest.
func (s *outputSpiller) preview(
	text, toolName, path string,
	writeErr error,
	isError bool,
	hints spillHints,
) string {
	maxBytes := s.policy.previewBytes()
	headEnd := outputHeadEnd(text, s.policy.PreviewLines, maxBytes)
	tailStart := outputTailStart(text, s.policy.PreviewLines, maxBytes)
	if tailStart < headEnd {
		tailStart = headEnd
	}

	totalChars := utf8.RuneCountInString(text)
	totalLines := countOutputLines(text)
	head := text[:headEnd]
	middle := text[headEnd:tailStart]
	tail := text[tailStart:]

	kind := "output"
	if isError {
		kind = "error output"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[%s %s too large for the context: %d chars, %d lines. ", toolName, kind, totalChars, totalLines)
	switch {
	case path != "":
		fmt.Fprintf(&b, "Full output saved to %s]\n", path)
	case errors.Is(writeErr, errNoOutputSpillDir):
		b.WriteString("Full output not saved (no workspace); only the head and tail are shown.]\n")
	case errors.Is(writeErr, errUserCopyNotSpilled):
		b.WriteString("This user-facing copy is not saved; only the head and tail are shown.]\n")
	default:
		b.WriteString("Full output not saved (write failed); only the head and tail are shown.]\n")
	}

	fmt.Fprintf(&b, "--- head (first %d lines) ---\n", countOutputLines(head))
	b.WriteString(head)
	if !strings.HasSuffix(head, "\n") {
		b.WriteString("\n")
	}
	if middle != "" {
		fmt.Fprintf(
			&b,
			"--- %d lines (%d chars) omitted ---\n",
			countOutputLines(middle),
			utf8.RuneCountInString(middle),
		)
	}
	fmt.Fprintf(&b, "--- tail (last %d lines) ---\n", countOutputLines(tail))
	b.WriteString(tail)
	if !strings.HasSuffix(tail, "\n") {
		b.WriteString("\n")
	}

	if path != "" {
		if hint := spillHintLine(hints); hint != "" {
			b.WriteString(hint)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// spillHintLine tells the model how to reach the rest of the output with
// the tools it actually has.
func spillHintLine(hints spillHints) string {
	parts := make([]string, 0, 3)
	if hints.readFile {
		parts = append(parts, "read_file on that path (partial reads are supported) to read more")
	}
	if hints.exec {
		parts = append(parts, "exec with grep -n or sed -n on it to find what you need")
	}
	if hints.sendFile {
		parts = append(parts, "send_file with that path to hand the whole output to the user")
	}
	if len(parts) == 0 {
		return ""
	}
	return "[Use " + strings.Join(parts, "; ") + ".]"
}

// outputHeadEnd returns the end of the prefix holding at most maxLines lines
// and maxBytes bytes, cut on a rune boundary.
func outputHeadEnd(text string, maxLines, maxBytes int) int {
	end := 0
	for lines := 0; end < len(text) && lines < maxLines; lines++ {
		nl := strings.IndexByte(text[end:], '\n')
		if nl < 0 {
			end = len(text)
			break
		}
		end += nl + 1
	}
	if end > maxBytes {
		end = maxBytes
		for end > 0 && !utf8.RuneStart(text[end]) {
			end--
		}
	}
	return end
}

// outputTailStart returns the start of the suffix holding at most maxLines
// lines and maxBytes bytes, cut on a rune boundary. A trailing newline
// belongs to the last line rather than starting an empty one.
func outputTailStart(text string, maxLines, maxBytes int) int {
	start := len(text)
	if start > 0 && text[start-1] == '\n' {
		start--
	}
	for lines := 0; ; {
		nl := strings.LastIndexByte(text[:start], '\n')
		if nl < 0 {
			start = 0
			break
		}
		lines++
		if lines == maxLines {
			start = nl + 1
			break
		}
		start = nl
	}
	if len(text)-start > maxBytes {
		start = len(text) - maxBytes
		for start < len(text) && !utf8.RuneStart(text[start]) {
			start++
		}
	}
	return start
}

// countOutputLines counts lines the way a pager would: a final line without
// a newline still counts, a trailing newline does not add an empty one.
func countOutputLines(text string) int {
	if text == "" {
		return 0
	}
	n := strings.Count(text, "\n")
	if !strings.HasSuffix(text, "\n") {
		n++
	}
	return n
}
