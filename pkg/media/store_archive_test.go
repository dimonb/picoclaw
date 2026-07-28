package media

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStore_WithArchive_MovesIntoArchive verifies that when an archive is
// attached, Store() moves the source file into the archive layout and Resolve
// returns the archive path (not the original temp path).
func TestStore_WithArchive_MovesIntoArchive(t *testing.T) {
	store := NewFileMediaStore()
	archive := newTestArchive(t)
	store.SetArchive(archive)

	src := writeTempFile(t, "photo.jpg", "fake-jpeg")

	ref, err := store.Store(src, MediaMeta{
		Filename:       "photo.jpg",
		ContentType:    "image/jpeg",
		Source:         "telegram",
		CleanupPolicy:  CleanupPolicyDeleteOnCleanup,
		RetentionClass: RetentionClassPermanent,
	}, "telegram:1:1")
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	// Source file should be gone from temp.
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("source temp file still exists: err=%v", err)
	}

	resolved, err := store.Resolve(ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !strings.HasPrefix(resolved, archive.Root()) {
		t.Errorf("resolved path %q is outside archive root %q", resolved, archive.Root())
	}
	if _, err := os.Stat(resolved); err != nil {
		t.Errorf("archive file missing: %v", err)
	}

	// Archive index has the entry.
	entry, ok := archive.LookupByRef(ref)
	if !ok {
		t.Fatal("archive lookup failed for archived ref")
	}
	if entry.RetentionClass != RetentionClassPermanent {
		t.Errorf("archive RetentionClass = %q", entry.RetentionClass)
	}
}

// TestStore_WithArchive_ReleaseAllKeepsArchiveFile verifies that ReleaseAll
// drops the in-memory ref but does NOT delete the archived file or its
// archive index entry.
func TestStore_WithArchive_ReleaseAllKeepsArchiveFile(t *testing.T) {
	store := NewFileMediaStore()
	archive := newTestArchive(t)
	store.SetArchive(archive)

	src := writeTempFile(t, "x.txt", "hello")
	ref, err := store.Store(src, MediaMeta{
		Source:         "telegram",
		CleanupPolicy:  CleanupPolicyDeleteOnCleanup,
		RetentionClass: RetentionClassPermanent,
	}, "scope-A")
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	resolved, _ := store.Resolve(ref)

	if err := store.ReleaseAll("scope-A"); err != nil {
		t.Fatalf("ReleaseAll: %v", err)
	}

	if _, err := os.Stat(resolved); err != nil {
		t.Errorf("archive file should still exist after ReleaseAll: %v", err)
	}
	if _, ok := archive.LookupByRef(ref); !ok {
		t.Error("archive index entry should still exist after ReleaseAll")
	}

	// The in-memory ref is gone, but the archive deliberately kept both the
	// file and the index entry — so Resolve still finds it through the archive
	// fallback. That is the point: conversation history outlives any one scope
	// (and any one process), and refusing to resolve media the archive is
	// still holding is what dropped attachments out of prompts after every
	// restart. ReleaseAll frees store-managed disk; it does not retract
	// archived content.
	resolvedAfterRelease, err := store.Resolve(ref)
	if err != nil {
		t.Errorf("Resolve after ReleaseAll: %v — archived media must stay reachable", err)
	}
	if resolvedAfterRelease != resolved {
		t.Errorf("resolved %q after ReleaseAll, want the archived path %q", resolvedAfterRelease, resolved)
	}
}

// TestStore_WithArchive_ForgetOnlyNotArchived verifies that paths registered
// with CleanupPolicyForgetOnly are NOT moved into the archive — they are
// borrowed paths (e.g. workspace files) whose lifecycle the store does not
// own.
func TestStore_WithArchive_ForgetOnlyNotArchived(t *testing.T) {
	store := NewFileMediaStore()
	archive := newTestArchive(t)
	store.SetArchive(archive)

	// Use a path under a "workspace" directory that should not move.
	workspace := t.TempDir()
	src := filepath.Join(workspace, "workspace.txt")
	if err := os.WriteFile(src, []byte("workspace content"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	ref, err := store.Store(src, MediaMeta{
		Source:        "tool:bash",
		CleanupPolicy: CleanupPolicyForgetOnly,
	}, "scope-B")
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	// Source path must remain untouched.
	if _, err := os.Stat(src); err != nil {
		t.Errorf("workspace file moved/removed unexpectedly: %v", err)
	}

	resolved, _ := store.Resolve(ref)
	if resolved != src {
		t.Errorf("forget_only resolved = %q, want original %q", resolved, src)
	}
	if _, ok := archive.LookupByRef(ref); ok {
		t.Error("forget_only ref should NOT appear in archive index")
	}
}

// TestStore_WithoutArchive_LegacyPath verifies that with no archive attached
// the store behaves exactly as before (no move, ReleaseAll deletes file).
func TestStore_WithoutArchive_LegacyPath(t *testing.T) {
	store := NewFileMediaStore()
	src := writeTempFile(t, "x.txt", "hello")

	ref, err := store.Store(src, MediaMeta{
		Source:        "telegram",
		CleanupPolicy: CleanupPolicyDeleteOnCleanup,
	}, "scope-C")
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	resolved, _ := store.Resolve(ref)
	if resolved != src {
		t.Errorf("no-archive resolved = %q, want original %q", resolved, src)
	}

	if err := store.ReleaseAll("scope-C"); err != nil {
		t.Fatalf("ReleaseAll: %v", err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("expected file deleted on ReleaseAll without archive: err=%v", err)
	}
}
