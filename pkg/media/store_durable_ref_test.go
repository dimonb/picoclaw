package media

import (
	"os"
	"testing"
)

func storeWithArchive(t *testing.T) *FileMediaStore {
	t.Helper()
	archive, err := NewSQLiteMediaArchive(t.TempDir())
	if err != nil {
		t.Fatalf("NewSQLiteMediaArchive: %v", err)
	}
	t.Cleanup(func() { _ = archive.Close() })

	store := NewFileMediaStore()
	store.SetArchive(archive)
	return store
}

// The case this exists for: a channel archived the file, and a tool now wants
// to show the same content to the model. It must get the archived ref back
// rather than registering a second, non-durable one.
func TestDurableRefForFile_ReturnsArchivedRefForSameContent(t *testing.T) {
	store := storeWithArchive(t)

	src := writeTempFile(t, "photo.jpg", "fake-jpeg-bytes")
	channelRef, err := store.Store(src, MediaMeta{
		Filename:       "photo.jpg",
		ContentType:    "image/jpeg",
		Source:         "telegram",
		CleanupPolicy:  CleanupPolicyDeleteOnCleanup,
		RetentionClass: RetentionClassPermanent,
	}, "telegram:-100:5629")
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	archivedPath, err := store.Resolve(channelRef)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	got, ok := store.DurableRefForFile(archivedPath)
	if !ok {
		t.Fatal("DurableRefForFile found nothing for content that is archived")
	}
	if got != channelRef {
		t.Errorf("ref = %q, want the archived ref %q", got, channelRef)
	}
}

// The same content reached the archive from a different path, which is the
// normal shape: the tool is handed a path, not a ref.
func TestDurableRefForFile_MatchesByContentNotPath(t *testing.T) {
	store := storeWithArchive(t)

	src := writeTempFile(t, "photo.jpg", "identical-bytes")
	channelRef, err := store.Store(src, MediaMeta{
		Filename:       "photo.jpg",
		ContentType:    "image/jpeg",
		Source:         "telegram",
		CleanupPolicy:  CleanupPolicyDeleteOnCleanup,
		RetentionClass: RetentionClassPermanent,
	}, "telegram:-100:5629")
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	// A copy elsewhere on disk with the same bytes.
	elsewhere := writeTempFile(t, "copy.jpg", "identical-bytes")

	got, ok := store.DurableRefForFile(elsewhere)
	if !ok {
		t.Fatal("DurableRefForFile did not match on content")
	}
	if got != channelRef {
		t.Errorf("ref = %q, want %q", got, channelRef)
	}
}

// A permanent sibling is the one worth handing out: the reaper evicts an
// ephemeral entry once its TTL passes, which would put us back where we
// started.
func TestDurableRefForFile_PrefersPermanentOverEphemeral(t *testing.T) {
	store := storeWithArchive(t)

	src := writeTempFile(t, "shared.jpg", "shared-bytes")
	ephemeralRef, err := store.Store(src, MediaMeta{
		Filename:       "shared.jpg",
		ContentType:    "image/jpeg",
		Source:         "tool:inline:whatever",
		CleanupPolicy:  CleanupPolicyDeleteOnCleanup,
		RetentionClass: RetentionClassEphemeral,
	}, "tool:1")
	if err != nil {
		t.Fatalf("Store ephemeral: %v", err)
	}

	src2 := writeTempFile(t, "shared2.jpg", "shared-bytes")
	permanentRef, err := store.Store(src2, MediaMeta{
		Filename:       "shared.jpg",
		ContentType:    "image/jpeg",
		Source:         "telegram",
		CleanupPolicy:  CleanupPolicyDeleteOnCleanup,
		RetentionClass: RetentionClassPermanent,
	}, "telegram:-100:5629")
	if err != nil {
		t.Fatalf("Store permanent: %v", err)
	}

	// Archive is allowed to consume the source via os.Rename, so probe with a
	// fresh copy of the same bytes rather than the path just stored.
	probe := writeTempFile(t, "probe.jpg", "shared-bytes")

	got, ok := store.DurableRefForFile(probe)
	if !ok {
		t.Fatal("DurableRefForFile found nothing")
	}
	if got == ephemeralRef {
		t.Error("returned the ephemeral ref while a permanent one exists")
	}
	if got != permanentRef {
		t.Errorf("ref = %q, want the permanent ref %q", got, permanentRef)
	}
}

// An index row whose file is gone must not be handed out: the caller would
// embed it in history and only discover the problem on resolution.
func TestDurableRefForFile_SkipsEntryWithMissingFile(t *testing.T) {
	store := storeWithArchive(t)

	src := writeTempFile(t, "gone.jpg", "will-be-deleted")
	ref, err := store.Store(src, MediaMeta{
		Filename:       "gone.jpg",
		ContentType:    "image/jpeg",
		Source:         "telegram",
		CleanupPolicy:  CleanupPolicyDeleteOnCleanup,
		RetentionClass: RetentionClassPermanent,
	}, "telegram:-100:5629")
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	archivedPath, err := store.Resolve(ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// Keep a copy to hash, then remove what the index points at.
	probe := writeTempFile(t, "probe.jpg", "will-be-deleted")
	if err := os.Remove(archivedPath); err != nil {
		t.Fatalf("remove archived file: %v", err)
	}

	if got, ok := store.DurableRefForFile(probe); ok {
		t.Errorf("returned %q for an entry whose file is gone", got)
	}
}

func TestDurableRefForFile_NoArchiveOrNoMatch(t *testing.T) {
	// No archive attached: the store has no durable record to offer.
	plain := NewFileMediaStore()
	if _, ok := plain.DurableRefForFile(writeTempFile(t, "x.jpg", "bytes")); ok {
		t.Error("a store without an archive reported a durable ref")
	}

	store := storeWithArchive(t)

	// Archive present but nothing matches this content.
	if _, ok := store.DurableRefForFile(writeTempFile(t, "unseen.jpg", "never-archived")); ok {
		t.Error("reported a durable ref for content that was never archived")
	}

	// Unreadable path: report false rather than failing the caller.
	if _, ok := store.DurableRefForFile("/nonexistent/path/to/file.jpg"); ok {
		t.Error("reported a durable ref for a path that does not exist")
	}
}

// FileMediaStore must actually satisfy the capability the tools type-assert on.
func TestFileMediaStoreImplementsDurableRefFinder(t *testing.T) {
	var _ DurableRefFinder = NewFileMediaStore()
}
