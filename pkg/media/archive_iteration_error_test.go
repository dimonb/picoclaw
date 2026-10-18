package media

import (
	"os"
	"testing"
	"time"
)

func TestArchive_QueryIterationFailureDoesNotEvict(t *testing.T) {
	a := newTestArchive(t)
	src := writeTempFile(t, "error.txt", "keep this file")
	path, err := a.Archive(src, "media://valid", MediaMeta{
		RetentionClass: RetentionClassEphemeral,
	}, "scope")
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := a.LookupByRef("media://valid")
	if !ok {
		t.Fatal("archived entry missing")
	}
	// Keep one valid row ahead of a row whose expression fails during Next.
	// The eviction candidate query does not select size_bytes and succeeds.
	_, err = a.db.Exec(`
		INSERT INTO media_archive (ref, archive_path, sha256, retention_class, stored_at)
		SELECT 'media://broken', archive_path, sha256, retention_class, stored_at
		FROM media_archive;
		ALTER TABLE media_archive RENAME TO archive_rows;
		CREATE VIEW media_archive AS
		SELECT ref, archive_path, sha256, filename, content_type, source, scope,
		       cleanup_policy, retention_class,
		       CASE WHEN ref = 'media://broken' THEN abs(-9223372036854775808)
		            ELSE size_bytes END AS size_bytes,
		       stored_at, last_resolved_at
		FROM archive_rows;
	`)
	if err != nil {
		t.Fatal(err)
	}
	entries, queryErr := a.entriesByArchivePath(path)
	if queryErr == nil || entries != nil {
		t.Fatalf("entries = %v, err = %v; want no partial entries and an error", entries, queryErr)
	}
	if entries := a.LookupBySHA(entry.SHA256); entries != nil {
		t.Fatalf("unexpected lookup entries: %v", entries)
	}
	called := false
	evictionErr := a.IterateForEviction(time.Now().Add(48*time.Hour), time.Hour, func(ArchiveEntry) bool {
		called = true
		return true
	})
	if evictionErr == nil || called {
		t.Fatalf("eviction err = %v, callback called = %v", evictionErr, called)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("archive file was removed: %v", statErr)
	}
}
