package media

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func createTempFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func readMetaFile(t *testing.T, path string) StoredFileMeta {
	t.Helper()
	data, err := os.ReadFile(path + ".meta.json")
	if err != nil {
		t.Fatalf("ReadFile(meta): %v", err)
	}
	var meta StoredFileMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("Unmarshal(meta): %v", err)
	}
	return meta
}

func TestStore_CopiesIntoOrganizedLayout(t *testing.T) {
	root := t.TempDir()
	store := NewFileMediaStoreAt(root)
	source := createTempFile(t, t.TempDir(), "voice.ogg", "voice-data")

	storedPath, err := store.Store(source, MediaMeta{
		Filename:      "voice.ogg",
		ContentType:   "audio/ogg",
		Source:        "telegram",
		StorageBucket: "agent_main_telegram_group_-1003822706455_3",
	}, "telegram:-1003822706455/3:210")
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	if !strings.HasPrefix(storedPath, filepath.Join(root, time.Now().Format("20060102"))) {
		t.Fatalf("storedPath = %q, want prefix %q", storedPath, filepath.Join(root, time.Now().Format("20060102")))
	}
	if filepath.Base(storedPath) != "voice.ogg" {
		t.Fatalf("basename = %q", filepath.Base(storedPath))
	}
	if !strings.Contains(storedPath, filepath.Join(time.Now().Format("20060102"), "agent_main_telegram_group_-1003822706455_3")) {
		t.Fatalf("storedPath bucket mismatch: %q", storedPath)
	}
	meta := readMetaFile(t, storedPath)
	if meta.Scope != "telegram:-1003822706455/3:210" {
		t.Fatalf("scope = %q", meta.Scope)
	}
	if meta.Meta.Filename != "voice.ogg" {
		t.Fatalf("filename = %q", meta.Meta.Filename)
	}
	if meta.Meta.ContentType != "audio/ogg" {
		t.Fatalf("content_type = %q", meta.Meta.ContentType)
	}
	if meta.Meta.Source != "telegram" {
		t.Fatalf("source = %q", meta.Meta.Source)
	}
	if meta.SHA256 == "" {
		t.Fatal("expected sha256 to be populated")
	}
}

func TestStore_ReusesSameNameAndSameContent(t *testing.T) {
	root := t.TempDir()
	store := NewFileMediaStoreAt(root)
	source := createTempFile(t, t.TempDir(), "report.pdf", "same-content")

	storedPath1, err := store.Store(source, MediaMeta{Filename: "report.pdf", Source: "telegram"}, "telegram:chat:1")
	if err != nil {
		t.Fatalf("Store(first): %v", err)
	}
	meta1 := readMetaFile(t, storedPath1)
	time.Sleep(10 * time.Millisecond)

	storedPath2, err := store.Store(source, MediaMeta{Filename: "report.pdf", Source: "telegram"}, "telegram:chat:2")
	if err != nil {
		t.Fatalf("Store(second): %v", err)
	}
	meta2 := readMetaFile(t, storedPath2)

	if storedPath1 != storedPath2 {
		t.Fatalf("expected same stored path, got %q and %q", storedPath1, storedPath2)
	}
	if !meta2.UpdatedAt.After(meta1.UpdatedAt) {
		t.Fatalf("expected updated_at to advance: %v <= %v", meta2.UpdatedAt, meta1.UpdatedAt)
	}
	if !meta2.CreatedAt.Equal(meta1.CreatedAt) {
		t.Fatalf("created_at changed: %v != %v", meta2.CreatedAt, meta1.CreatedAt)
	}
}

func TestStore_ReusedPathAcrossScopesReleasesOnlyOnLastScope(t *testing.T) {
	root := t.TempDir()
	store := NewFileMediaStoreAt(root)
	source := createTempFile(t, t.TempDir(), "report.pdf", "same-content")

	storedPath, err := store.Store(source, MediaMeta{Filename: "report.pdf", Source: "telegram", StorageBucket: "shared"}, "scopeA")
	if err != nil {
		t.Fatalf("Store(scopeA): %v", err)
	}
	storedPath2, err := store.Store(source, MediaMeta{Filename: "report.pdf", Source: "telegram", StorageBucket: "shared"}, "scopeB")
	if err != nil {
		t.Fatalf("Store(scopeB): %v", err)
	}
	if storedPath != storedPath2 {
		t.Fatalf("expected same stored path, got %q and %q", storedPath, storedPath2)
	}

	if err := store.ReleaseAll("scopeA"); err != nil {
		t.Fatalf("ReleaseAll(scopeA): %v", err)
	}
	if _, err := os.Stat(storedPath); err != nil {
		t.Fatalf("expected file to remain after releasing first scope: %v", err)
	}
	if _, _, err := store.ResolveWithMeta(storedPath); err != nil {
		t.Fatalf("ResolveWithMeta after scopeA release: %v", err)
	}

	if err := store.ReleaseAll("scopeB"); err != nil {
		t.Fatalf("ReleaseAll(scopeB): %v", err)
	}
	if _, err := os.Stat(storedPath); err != nil {
		t.Fatalf("expected file to remain after releasing last scope, err=%v", err)
	}
	if _, err := os.Stat(storedPath + ".meta.json"); err != nil {
		t.Fatalf("expected meta to remain after releasing last scope, err=%v", err)
	}
}

func TestStore_AddsSuffixWhenSameNameHasDifferentContent(t *testing.T) {
	root := t.TempDir()
	store := NewFileMediaStoreAt(root)
	srcA := createTempFile(t, t.TempDir(), "report.pdf", "content-a")
	srcB := createTempFile(t, t.TempDir(), "report.pdf", "content-b")

	storedPath1, err := store.Store(srcA, MediaMeta{Filename: "report.pdf", Source: "telegram"}, "telegram:chat:1")
	if err != nil {
		t.Fatalf("Store(first): %v", err)
	}
	storedPath2, err := store.Store(srcB, MediaMeta{Filename: "report.pdf", Source: "telegram"}, "telegram:chat:2")
	if err != nil {
		t.Fatalf("Store(second): %v", err)
	}

	if filepath.Base(storedPath1) != "report.pdf" {
		t.Fatalf("basename(first) = %q", filepath.Base(storedPath1))
	}
	if filepath.Base(storedPath2) != "report.1.pdf" {
		t.Fatalf("basename(second) = %q", filepath.Base(storedPath2))
	}
	meta := readMetaFile(t, storedPath2)
	if meta.Meta.Filename != "report.pdf" {
		t.Fatalf("meta filename = %q", meta.Meta.Filename)
	}
}

func TestResolveWithMeta_ByStoredPath(t *testing.T) {
	root := t.TempDir()
	store := NewFileMediaStoreAt(root)
	src := createTempFile(t, t.TempDir(), "photo.jpg", "jpg-data")

	storedPath, err := store.Store(src, MediaMeta{Filename: "photo.jpg", ContentType: "image/jpeg", Source: "telegram"}, "telegram:chat:1")
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	resolvedPath, meta, err := store.ResolveWithMeta(storedPath)
	if err != nil {
		t.Fatalf("ResolveWithMeta: %v", err)
	}
	if resolvedPath != storedPath {
		t.Fatalf("resolvedPath = %q, want %q", resolvedPath, storedPath)
	}
	if meta.Filename != "photo.jpg" {
		t.Fatalf("filename = %q", meta.Filename)
	}
	if meta.ContentType != "image/jpeg" {
		t.Fatalf("content_type = %q", meta.ContentType)
	}
}

func TestReleaseAll_ForgetsEntriesButKeepsStoredFiles(t *testing.T) {
	root := t.TempDir()
	store := NewFileMediaStoreAt(root)
	src := createTempFile(t, t.TempDir(), "a.txt", "hello")

	storedPath, err := store.Store(src, MediaMeta{Filename: "a.txt", Source: "test"}, "scope1")
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if err := store.ReleaseAll("scope1"); err != nil {
		t.Fatalf("ReleaseAll: %v", err)
	}
	if _, err := os.Stat(storedPath); err != nil {
		t.Fatalf("expected stored file to remain, err=%v", err)
	}
	if _, err := os.Stat(storedPath + ".meta.json"); err != nil {
		t.Fatalf("expected meta file to remain, err=%v", err)
	}
}

func TestCleanExpired_ForgetsEntriesButKeepsFilesOnDisk(t *testing.T) {
	root := t.TempDir()
	store := NewFileMediaStoreAtWithCleanup(root, MediaCleanerConfig{MaxAge: time.Minute})
	store.nowFunc = func() time.Time { return time.Date(2026, 3, 22, 12, 0, 0, 0, time.UTC) }
	src := createTempFile(t, t.TempDir(), "a.txt", "hello")

	storedPath, err := store.Store(src, MediaMeta{Filename: "a.txt", Source: "test"}, "scope1")
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	store.nowFunc = func() time.Time { return time.Date(2026, 3, 22, 12, 2, 0, 0, time.UTC) }

	forgotten := store.CleanExpired()
	if forgotten != 1 {
		t.Fatalf("forgotten = %d, want 1", forgotten)
	}
	if _, err := os.Stat(storedPath); err != nil {
		t.Fatalf("expected stored file to remain on disk, err=%v", err)
	}
	if _, err := os.Stat(storedPath + ".meta.json"); err != nil {
		t.Fatalf("expected sidecar metadata to remain on disk, err=%v", err)
	}
}
