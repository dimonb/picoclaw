package media

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolve_RehydratesFromArchiveAfterRestart is the case that broke on the
// beta bot.
//
// The ref→path map lives only in memory, so a restart forgot every ref the
// previous process had registered. Conversation history still carried those
// media:// refs and the files still sat in the archive for their full 720h
// retention, but resolution failed anyway and the attachments were dropped
// from the prompt — the model answered as though nothing had been sent. The
// archive index is the durable record and must be consulted before a ref is
// declared unknown.
func TestResolve_RehydratesFromArchiveAfterRestart(t *testing.T) {
	archiveRoot := t.TempDir()

	first, err := NewSQLiteMediaArchive(archiveRoot)
	if err != nil {
		t.Fatalf("NewSQLiteMediaArchive: %v", err)
	}
	oldStore := NewFileMediaStore()
	oldStore.SetArchive(first)

	src := writeTempFile(t, "hebrew.jpg", "fake-jpeg-bytes")
	ref, err := oldStore.Store(src, MediaMeta{
		Filename:       "hebrew.jpg",
		ContentType:    "image/jpeg",
		Source:         "telegram",
		CleanupPolicy:  CleanupPolicyDeleteOnCleanup,
		RetentionClass: RetentionClassEphemeral,
	}, "telegram:1:2508")
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	archivedPath, err := oldStore.Resolve(ref)
	if err != nil {
		t.Fatalf("Resolve before restart: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Restart: a brand new store and archive handle over the same root. The
	// process has no memory of the ref; only the archive index does.
	second, err := NewSQLiteMediaArchive(archiveRoot)
	if err != nil {
		t.Fatalf("reopen archive: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	newStore := NewFileMediaStore()
	newStore.SetArchive(second)

	resolved, meta, err := newStore.ResolveWithMeta(ref)
	if err != nil {
		t.Fatalf("ResolveWithMeta after restart: %v — history media is lost on every restart", err)
	}
	if resolved != archivedPath {
		t.Errorf("resolved %q, want the archived path %q", resolved, archivedPath)
	}
	if _, err := os.Stat(resolved); err != nil {
		t.Errorf("resolved path is not on disk: %v", err)
	}
	// Metadata drives MIME detection downstream; losing it turns an image into
	// an unrecognized blob.
	if meta.ContentType != "image/jpeg" {
		t.Errorf("meta.ContentType = %q, want image/jpeg", meta.ContentType)
	}
	if meta.Filename != "hebrew.jpg" {
		t.Errorf("meta.Filename = %q, want hebrew.jpg", meta.Filename)
	}

	// A second call must be served from memory, not re-queried from SQLite.
	if _, err := newStore.Resolve(ref); err != nil {
		t.Errorf("second Resolve: %v", err)
	}
}

// An index row whose file has been reaped must read as a miss. Handing back a
// path that is not there moves the failure downstream, where it surfaces as a
// stat error with no hint of why the file is missing.
func TestResolve_ArchiveEntryWithoutFileIsAMiss(t *testing.T) {
	archiveRoot := t.TempDir()
	archive, err := NewSQLiteMediaArchive(archiveRoot)
	if err != nil {
		t.Fatalf("NewSQLiteMediaArchive: %v", err)
	}
	t.Cleanup(func() { _ = archive.Close() })

	oldStore := NewFileMediaStore()
	oldStore.SetArchive(archive)
	src := writeTempFile(t, "gone.jpg", "fake-jpeg-bytes")
	ref, err := oldStore.Store(src, MediaMeta{
		Filename:      "gone.jpg",
		ContentType:   "image/jpeg",
		CleanupPolicy: CleanupPolicyDeleteOnCleanup,
	}, "scope")
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	archivedPath, err := oldStore.Resolve(ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := os.Remove(archivedPath); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	newStore := NewFileMediaStore()
	newStore.SetArchive(archive)
	if _, err := newStore.Resolve(ref); err == nil {
		t.Error("Resolve succeeded for an archived entry whose file is gone")
	}
}

// Without an archive attached the store must behave exactly as before.
func TestResolve_UnknownRefWithoutArchive(t *testing.T) {
	store := NewFileMediaStore()
	if _, err := store.Resolve("media://" + filepath.Base(t.TempDir())); err == nil {
		t.Error("Resolve succeeded for an unknown ref with no archive attached")
	}
}
