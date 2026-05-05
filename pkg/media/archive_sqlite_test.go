package media

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestArchive(t *testing.T) *SQLiteMediaArchive {
	t.Helper()
	dir := t.TempDir()
	a, err := NewSQLiteMediaArchive(dir)
	if err != nil {
		t.Fatalf("NewSQLiteMediaArchive: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// writeTempFile creates a file under t.TempDir() with the given content and
// returns its path. This simulates the "landing zone" that channels download
// into before calling Store.
func writeTempFile(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write tmp: %v", err)
	}
	return path
}

func TestArchive_BasicStoreAndLookup(t *testing.T) {
	a := newTestArchive(t)
	src := writeTempFile(t, "photo.jpg", "fake-jpeg")

	archivePath, err := a.Archive(src, "media://abc", MediaMeta{
		Filename:       "photo.jpg",
		ContentType:    "image/jpeg",
		Source:         "telegram",
		CleanupPolicy:  CleanupPolicyDeleteOnCleanup,
		RetentionClass: RetentionClassPermanent,
	}, "telegram:42:1")
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}

	if !strings.HasPrefix(archivePath, a.Root()) {
		t.Errorf("archivePath %q is outside root %q", archivePath, a.Root())
	}
	if !strings.Contains(archivePath, "telegram") {
		t.Errorf("archivePath does not include source: %q", archivePath)
	}

	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("source still exists after archive: err=%v", err)
	}
	if _, err := os.Stat(archivePath); err != nil {
		t.Errorf("archive file missing: %v", err)
	}
	if _, err := os.Stat(SidecarPath(archivePath)); err != nil {
		t.Errorf("sidecar missing: %v", err)
	}

	got, ok := a.LookupByRef("media://abc")
	if !ok {
		t.Fatal("LookupByRef returned false")
	}
	if got.ArchivePath != archivePath {
		t.Errorf("entry path %q != %q", got.ArchivePath, archivePath)
	}
	if got.Source != "telegram" {
		t.Errorf("Source = %q", got.Source)
	}
	if got.RetentionClass != RetentionClassPermanent {
		t.Errorf("RetentionClass = %q", got.RetentionClass)
	}
	if got.SizeBytes != int64(len("fake-jpeg")) {
		t.Errorf("SizeBytes = %d", got.SizeBytes)
	}
}

func TestArchive_DuplicateSHACollapses(t *testing.T) {
	a := newTestArchive(t)
	src1 := writeTempFile(t, "a.jpg", "same-bytes")
	src2 := writeTempFile(t, "b.jpg", "same-bytes")

	p1, err := a.Archive(src1, "media://r1", MediaMeta{
		Filename: "a.jpg", Source: "telegram",
		CleanupPolicy:  CleanupPolicyDeleteOnCleanup,
		RetentionClass: RetentionClassPermanent,
	}, "telegram:1:1")
	if err != nil {
		t.Fatalf("Archive 1: %v", err)
	}
	p2, err := a.Archive(src2, "media://r2", MediaMeta{
		Filename: "b.jpg", Source: "telegram",
		CleanupPolicy:  CleanupPolicyDeleteOnCleanup,
		RetentionClass: RetentionClassEphemeral,
	}, "telegram:2:1")
	if err != nil {
		t.Fatalf("Archive 2: %v", err)
	}
	if p1 != p2 {
		t.Fatalf("expected duplicate to collapse: %q vs %q", p1, p2)
	}
	if _, err := os.Stat(src2); !os.IsNotExist(err) {
		t.Errorf("source2 should be removed (duplicate): err=%v", err)
	}

	bySHA := a.LookupBySHA(mustHash(t, "same-bytes"))
	if len(bySHA) != 2 {
		t.Fatalf("expected 2 refs sharing SHA, got %d", len(bySHA))
	}
}

func TestArchive_DeleteRefcountsByPath(t *testing.T) {
	a := newTestArchive(t)
	src1 := writeTempFile(t, "a.jpg", "shared-bytes")
	src2 := writeTempFile(t, "b.jpg", "shared-bytes")

	p, err := a.Archive(src1, "media://r1", MediaMeta{
		Source: "telegram", CleanupPolicy: CleanupPolicyDeleteOnCleanup,
		RetentionClass: RetentionClassPermanent,
	}, "scope1")
	if err != nil {
		t.Fatalf("Archive 1: %v", err)
	}
	if _, err := a.Archive(src2, "media://r2", MediaMeta{
		Source: "telegram", CleanupPolicy: CleanupPolicyDeleteOnCleanup,
		RetentionClass: RetentionClassPermanent,
	}, "scope2"); err != nil {
		t.Fatalf("Archive 2: %v", err)
	}

	deleted, err := a.Delete("media://r1")
	if err != nil {
		t.Fatalf("Delete r1: %v", err)
	}
	if deleted {
		t.Error("file should NOT be deleted while r2 still references it")
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("file missing too early: %v", err)
	}

	deleted, err = a.Delete("media://r2")
	if err != nil {
		t.Fatalf("Delete r2: %v", err)
	}
	if !deleted {
		t.Error("file should be deleted on last ref")
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("file should be gone, err=%v", err)
	}
}

func TestArchive_MarkResolvedSetsTimestamp(t *testing.T) {
	a := newTestArchive(t)
	frozen := time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return frozen }

	src := writeTempFile(t, "x.txt", "hi")
	if _, err := a.Archive(src, "media://r", MediaMeta{
		Source: "tool", CleanupPolicy: CleanupPolicyDeleteOnCleanup,
		RetentionClass: RetentionClassEphemeral,
	}, "scope"); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	got, _ := a.LookupByRef("media://r")
	if !got.LastResolvedAt.IsZero() {
		t.Errorf("expected zero LastResolvedAt, got %v", got.LastResolvedAt)
	}

	later := frozen.Add(2 * time.Hour)
	a.now = func() time.Time { return later }
	a.MarkResolved("media://r")

	got, _ = a.LookupByRef("media://r")
	if !got.LastResolvedAt.Equal(later) {
		t.Errorf("LastResolvedAt = %v, want %v", got.LastResolvedAt, later)
	}
}

func TestArchive_LookupByRefMissing(t *testing.T) {
	a := newTestArchive(t)
	if _, ok := a.LookupByRef("media://nope"); ok {
		t.Error("expected miss")
	}
}

func mustHash(t *testing.T, content string) string {
	t.Helper()
	tmp := writeTempFile(t, "h", content)
	h, err := hashFile(tmp)
	if err != nil {
		t.Fatalf("hashFile: %v", err)
	}
	return h
}
