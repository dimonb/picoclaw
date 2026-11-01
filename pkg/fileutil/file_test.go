package fileutil

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestWriteFileAtomic_Basic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	data := []byte("hello picoclaw")

	err := WriteFileAtomic(path, data, 0o644)
	if err != nil {
		t.Fatalf("WriteFileAtomic failed: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("got %q, want %q", got, data)
	}
}

func TestWriteFileAtomic_Permissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.txt")

	err := WriteFileAtomic(path, []byte("secret"), 0o600)
	if err != nil {
		t.Fatalf("WriteFileAtomic failed: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	// On Unix, check file mode (ignoring directory bits)
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("permissions = %o, want %o", got, 0o600)
	}
}

func TestWriteFileAtomic_Overwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "overwrite.txt")

	// Write initial content
	if err := WriteFileAtomic(path, []byte("old"), 0o644); err != nil {
		t.Fatalf("first write failed: %v", err)
	}

	// Overwrite
	if err := WriteFileAtomic(path, []byte("new"), 0o644); err != nil {
		t.Fatalf("second write failed: %v", err)
	}

	got, _ := os.ReadFile(path)
	if string(got) != "new" {
		t.Errorf("got %q after overwrite, want %q", got, "new")
	}
}

func TestWriteFileAtomic_EmptyData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.txt")

	err := WriteFileAtomic(path, []byte{}, 0o644)
	if err != nil {
		t.Fatalf("WriteFileAtomic with empty data failed: %v", err)
	}

	got, _ := os.ReadFile(path)
	if len(got) != 0 {
		t.Errorf("expected empty file, got %d bytes", len(got))
	}
}

func TestWriteFileAtomic_CreatesParentDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "c", "deep.txt")

	err := WriteFileAtomic(path, []byte("deep"), 0o644)
	if err != nil {
		t.Fatalf("WriteFileAtomic with nested dirs failed: %v", err)
	}

	got, _ := os.ReadFile(path)
	if string(got) != "deep" {
		t.Errorf("got %q, want %q", got, "deep")
	}
}

func TestWriteFileAtomic_NoTempFileOnSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clean.txt")

	if err := WriteFileAtomic(path, []byte("data"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic failed: %v", err)
	}

	// Verify no temp files remain
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "clean.txt" {
			t.Errorf("unexpected file remaining: %s", e.Name())
		}
	}
}

func TestWriteFileAtomic_LargeFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large.bin")

	// 1MB of data
	data := make([]byte, 1<<20)
	for i := range data {
		data[i] = byte(i % 256)
	}

	if err := WriteFileAtomic(path, data, 0o644); err != nil {
		t.Fatalf("WriteFileAtomic with large file failed: %v", err)
	}

	got, _ := os.ReadFile(path)
	if len(got) != len(data) {
		t.Errorf("file size = %d, want %d", len(got), len(data))
	}
}

// A coarse clock can return the same timestamp to concurrent writers. Freeze
// time and occupy the former PID/timestamp name to reproduce that collision
// deterministically without depending on the host clock resolution or load.
func TestWriteFileAtomic_TimestampCollision(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		occupied := filepath.Join(dir, fmt.Sprintf(".tmp-%d-%d", os.Getpid(), time.Now().UnixNano()))
		if err := os.WriteFile(occupied, []byte("other writer"), 0o600); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "target")
		if err := WriteFileAtomic(path, []byte("new data"), 0o600); err != nil {
			t.Fatalf("write must not fail on a timestamp collision: %v", err)
		}
		for name, want := range map[string]string{path: "new data", occupied: "other writer"} {
			got, err := os.ReadFile(name)
			if err != nil || string(got) != want {
				t.Fatalf("%s = %q, want %q; err = %v", name, got, want, err)
			}
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 2 {
			t.Fatalf("unexpected directory entries: %v; err = %v", entries, err)
		}
	})
}

func TestWriteFileAtomic_Concurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "concurrent.txt")
	const writers = 64
	const rounds = 4
	payloads := make([][]byte, writers)
	for i := range payloads {
		payloads[i] = bytes.Repeat([]byte(fmt.Sprintf("writer-%02d\n", i)), 512)
	}
	if err := WriteFileAtomic(path, payloads[0], 0o600); err != nil {
		t.Fatal(err)
	}
	// Readers must only observe whole writer payloads, never a partially written
	// file, even while many writers race to replace the same target.
	validPayload := func(data []byte) bool {
		for _, payload := range payloads {
			if bytes.Equal(data, payload) {
				return true
			}
		}
		return false
	}
	done := make(chan struct{})
	readerErr := make(chan error, 1)
	go func() {
		defer close(readerErr)
		for {
			select {
			case <-done:
				return
			default:
			}
			data, err := os.ReadFile(path)
			if err != nil {
				readerErr <- err
				return
			}
			if !validPayload(data) {
				readerErr <- fmt.Errorf("reader observed incomplete/mixed payload (%d bytes)", len(data))
				return
			}
		}
	}()
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, writers)
	for i := range writers {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start
			for range rounds {
				if err := WriteFileAtomic(path, payloads[n], 0o600); err != nil {
					errs <- err
					return
				}
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(done)
	close(errs)
	for err := range errs {
		t.Errorf("concurrent write error: %v", err)
	}
	for err := range readerErr {
		t.Errorf("concurrent read error: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !validPayload(got) {
		t.Errorf("final file is not a complete writer payload")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "concurrent.txt" {
		t.Fatalf("temporary files remain: %v", entries)
	}
}

func TestWriteFileAtomic_ExactPermissionsOnReplace(t *testing.T) {
	for _, mode := range []os.FileMode{0o400, 0o600, 0o640, 0o644} {
		t.Run(fmt.Sprintf("%o", mode), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "target")
			if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := WriteFileAtomic(path, []byte("replacement"), mode); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != mode {
				t.Errorf("permissions = %o, want %o", got, mode)
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != "replacement" {
				t.Fatalf("replacement = %q, err = %v", got, err)
			}
		})
	}
}

func TestWriteFileAtomic_RenameFailurePreservesTargetAndCleansTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(path, "original")
	if err := os.WriteFile(original, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := WriteFileAtomic(path, []byte("replacement"), 0o644)
	if err == nil || !strings.Contains(err.Error(), "failed to rename temp file:") {
		t.Fatalf("expected wrapped rename error, got %v", err)
	}
	var linkErr *os.LinkError
	if !errors.As(err, &linkErr) {
		t.Fatalf("rename cause not preserved: %v", err)
	}
	got, err := os.ReadFile(original)
	if err != nil || string(got) != "untouched" {
		t.Fatalf("original = %q, err = %v", got, err)
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("target directory changed: %v, %v", info, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "target" {
		t.Fatalf("temporary files remain after failure: %v", entries)
	}
}

func TestWriteFileAtomic_InvalidPath(t *testing.T) {
	// /dev/null/impossible is not a valid path on any OS
	err := WriteFileAtomic("/dev/null/impossible/file.txt", []byte("data"), 0o644)
	if err == nil {
		t.Error("expected error for invalid path, got nil")
	}
}
