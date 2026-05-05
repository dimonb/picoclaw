package media

import (
	"os"
	"testing"
	"time"
)

func TestReaper_PermanentNotEvicted(t *testing.T) {
	a := newTestArchive(t)
	frozen := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return frozen }

	src := writeTempFile(t, "x.jpg", "perm-content")
	if _, err := a.Archive(src, "media://perm", MediaMeta{
		Source:         "telegram",
		CleanupPolicy:  CleanupPolicyDeleteOnCleanup,
		RetentionClass: RetentionClassPermanent,
	}, "scope"); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	reaper := NewRetentionReaper(a, 1*time.Hour, time.Minute)
	// Pretend we are 100 hours later — way past TTL.
	reaper.now = func() time.Time { return frozen.Add(100 * time.Hour) }

	count := reaper.RunOnce()
	if count != 0 {
		t.Errorf("expected 0 evictions for permanent entry, got %d", count)
	}
	if _, ok := a.LookupByRef("media://perm"); !ok {
		t.Error("permanent entry should still be indexed")
	}
}

func TestReaper_EphemeralEvictedPastTTL(t *testing.T) {
	a := newTestArchive(t)
	frozen := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return frozen }

	src := writeTempFile(t, "x.txt", "tmp-content")
	archivePath, err := a.Archive(src, "media://tmp", MediaMeta{
		Source:         "tool",
		CleanupPolicy:  CleanupPolicyDeleteOnCleanup,
		RetentionClass: RetentionClassEphemeral,
	}, "scope")
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}

	reaper := NewRetentionReaper(a, 1*time.Hour, time.Minute)
	reaper.now = func() time.Time { return frozen.Add(2 * time.Hour) }

	count := reaper.RunOnce()
	if count != 1 {
		t.Errorf("expected 1 eviction, got %d", count)
	}
	if _, ok := a.LookupByRef("media://tmp"); ok {
		t.Error("ephemeral entry should be removed from index")
	}
	if _, err := os.Stat(archivePath); !os.IsNotExist(err) {
		t.Errorf("archive file should be deleted, err=%v", err)
	}
	if _, err := os.Stat(SidecarPath(archivePath)); !os.IsNotExist(err) {
		t.Errorf("sidecar should be deleted, err=%v", err)
	}
}

func TestReaper_EphemeralRecentlyResolvedNotEvicted(t *testing.T) {
	a := newTestArchive(t)
	frozen := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return frozen }

	src := writeTempFile(t, "x.txt", "fresh")
	if _, err := a.Archive(src, "media://fresh", MediaMeta{
		Source:         "tool",
		CleanupPolicy:  CleanupPolicyDeleteOnCleanup,
		RetentionClass: RetentionClassEphemeral,
	}, "scope"); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	// Mark resolved 30m before "now" of the reaper (2h after frozen).
	a.now = func() time.Time { return frozen.Add(90 * time.Minute) }
	a.MarkResolved("media://fresh")

	reaper := NewRetentionReaper(a, 1*time.Hour, time.Minute)
	reaper.now = func() time.Time { return frozen.Add(2 * time.Hour) }

	count := reaper.RunOnce()
	if count != 0 {
		t.Errorf("expected 0 evictions when entry was recently resolved, got %d", count)
	}
}

// TestReaper_MixedRefsKeepsFile checks that when one ref is permanent and
// another is ephemeral past TTL pointing at the same archive_path, the file
// stays (because of the permanent ref).
func TestReaper_MixedRefsKeepsFile(t *testing.T) {
	a := newTestArchive(t)
	frozen := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return frozen }

	src1 := writeTempFile(t, "a.bin", "shared-bytes")
	src2 := writeTempFile(t, "b.bin", "shared-bytes")
	p1, err := a.Archive(src1, "media://perm", MediaMeta{
		Source: "telegram", CleanupPolicy: CleanupPolicyDeleteOnCleanup,
		RetentionClass: RetentionClassPermanent,
	}, "scope")
	if err != nil {
		t.Fatalf("Archive 1: %v", err)
	}
	if _, err := a.Archive(src2, "media://temp", MediaMeta{
		Source: "tool", CleanupPolicy: CleanupPolicyDeleteOnCleanup,
		RetentionClass: RetentionClassEphemeral,
	}, "scope"); err != nil {
		t.Fatalf("Archive 2: %v", err)
	}

	reaper := NewRetentionReaper(a, 1*time.Hour, time.Minute)
	reaper.now = func() time.Time { return frozen.Add(2 * time.Hour) }

	count := reaper.RunOnce()
	if count != 0 {
		t.Errorf("expected file kept due to permanent sibling, evicted=%d", count)
	}
	if _, err := os.Stat(p1); err != nil {
		t.Errorf("file should still exist: %v", err)
	}
	if _, ok := a.LookupByRef("media://perm"); !ok {
		t.Error("permanent ref should still be indexed")
	}
	// Ephemeral ref still indexed because the path was kept.
	if _, ok := a.LookupByRef("media://temp"); !ok {
		t.Error("ephemeral ref kept (its archive_path was preserved)")
	}
}

func TestReaper_NoArchiveOrDisabled(t *testing.T) {
	r := NewRetentionReaper(nil, time.Hour, time.Minute)
	r.Start() // no-op
	r.Stop()

	a := newTestArchive(t)
	r2 := NewRetentionReaper(a, 0, time.Minute) // ttl=0 => no-op
	r2.Start()
	r2.Stop()
}
