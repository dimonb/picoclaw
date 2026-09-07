package logger

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func newTestRotator(t *testing.T, maxSizeMB, maxFiles int) (*rotatingFile, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gateway.log")
	r, err := newRotatingFile(path, FileLoggingOptions{MaxSizeMB: maxSizeMB, MaxFiles: maxFiles}.normalized())
	if err != nil {
		t.Fatalf("newRotatingFile: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r, path
}

// rotatingFile counts in bytes, so drive it with a 1 MB ceiling rather than
// trying to express a smaller one through MaxSizeMB.
func writeKB(t *testing.T, r *rotatingFile, kb int) {
	t.Helper()
	payload := append([]byte(strings.Repeat("x", kb*1024-1)), '\n')
	if _, err := r.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestRotatingFileRotatesAtCeiling(t *testing.T) {
	r, path := newTestRotator(t, 1, 3)

	// 1 MB ceiling: the first 900 KB fits, the second must rotate first.
	writeKB(t, r, 900)
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatalf("rotated too early: %v", err)
	}
	writeKB(t, r, 900)

	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("expected %s.1 after the ceiling was crossed: %v", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat active: %v", err)
	}
	if info.Size() > 1024*1024 {
		t.Errorf("active file = %d bytes, want <= 1 MiB", info.Size())
	}
}

// The whole point is a ceiling on total disk use, so old files must fall off
// rather than accumulate.
func TestRotatingFileKeepsAtMostMaxFiles(t *testing.T) {
	r, path := newTestRotator(t, 1, 2)

	for range 8 {
		writeKB(t, r, 900)
	}

	for _, suffix := range []string{".1", ".2"} {
		if _, err := os.Stat(path + suffix); err != nil {
			t.Errorf("expected %s%s to exist: %v", path, suffix, err)
		}
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Errorf("%s.3 should have been dropped (maxFiles=2)", path)
	}
}

// A record bigger than the ceiling has to survive intact: picoclaw logs a whole
// LLM request as one JSON line, and a split line is unreadable.
func TestRotatingFileWritesOversizedRecordWhole(t *testing.T) {
	r, path := newTestRotator(t, 1, 2)

	big := append([]byte(strings.Repeat("y", 3*1024*1024-1)), '\n')
	n, err := r.Write(big)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != len(big) {
		t.Fatalf("wrote %d of %d bytes", n, len(big))
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(data) != len(big) {
		t.Errorf("file holds %d bytes, want the record whole at %d", len(data), len(big))
	}
}

// An oversized file left behind by a build without rotation must not keep
// growing: the size on disk counts towards the ceiling from the first write.
func TestRotatingFileAdoptsExistingOversizedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("z", 2*1024*1024)), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	r, openErr := newRotatingFile(path, FileLoggingOptions{MaxSizeMB: 1, MaxFiles: 2}.normalized())
	if openErr != nil {
		t.Fatalf("newRotatingFile: %v", openErr)
	}
	defer r.Close()

	if _, err := r.Write([]byte("first line after restart\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("pre-existing oversized file should have been rotated away: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "first line after restart\n" {
		t.Errorf("active file = %q, want just the new record", string(data))
	}
}

// zerolog writes from every goroutine that logs, so Write must serialize
// against itself and against rotation.
func TestRotatingFileConcurrentWrites(t *testing.T) {
	r, _ := newTestRotator(t, 1, 3)

	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			line := []byte(fmt.Sprintf("goroutine %02d %s\n", i, strings.Repeat("p", 40*1024)))
			for range 8 {
				if _, err := r.Write(line); err != nil {
					t.Errorf("write: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestRotatingFileCloseIsIdempotent(t *testing.T) {
	r, _ := newTestRotator(t, 1, 2)
	if err := r.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Errorf("second close: %v", err)
	}
	if _, err := r.Write([]byte("after close\n")); err == nil {
		t.Error("expected a write after close to fail")
	}
}

func TestFileLoggingOptionsDefaults(t *testing.T) {
	got := FileLoggingOptions{}.normalized()
	if got.MaxSizeMB != defaultLogMaxSizeMB || got.MaxFiles != defaultLogMaxFiles {
		t.Errorf("defaults = %+v, want {%d %d}", got, defaultLogMaxSizeMB, defaultLogMaxFiles)
	}

	got = FileLoggingOptions{MaxSizeMB: -5, MaxFiles: -1}.normalized()
	if got.MaxSizeMB != defaultLogMaxSizeMB || got.MaxFiles != defaultLogMaxFiles {
		t.Errorf("negatives = %+v, want the defaults", got)
	}

	got = FileLoggingOptions{MaxSizeMB: 512, MaxFiles: 4}.normalized()
	if got.MaxSizeMB != 512 || got.MaxFiles != 4 {
		t.Errorf("explicit values = %+v, want {512 4}", got)
	}
}
