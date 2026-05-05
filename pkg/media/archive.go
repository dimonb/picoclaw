package media

import "time"

// RetentionClass declares how long an archived file should live.
type RetentionClass string

const (
	// RetentionClassPermanent means the file should be kept indefinitely
	// regardless of TTL. Channels use this for inbound user-uploaded media
	// and tool outputs that get embedded in conversation history.
	RetentionClassPermanent RetentionClass = "permanent"
	// RetentionClassEphemeral means the file is subject to TTL eviction
	// once it has not been resolved for a configured period. This is the
	// default for tool intermediates and any caller that does not opt in
	// to permanent retention.
	RetentionClassEphemeral RetentionClass = "ephemeral"
)

// ArchiveEntry is a row in the archive index.
type ArchiveEntry struct {
	Ref             string
	ArchivePath     string
	SHA256          string
	Filename        string
	ContentType     string
	Source          string
	Scope           string
	CleanupPolicy   CleanupPolicy
	RetentionClass  RetentionClass
	SizeBytes       int64
	StoredAt        time.Time
	LastResolvedAt  time.Time // zero value if never resolved
}

// MediaArchive is the persistence layer behind FileMediaStore. Each managed
// file has at least one ArchiveEntry; multiple refs may share the same
// ArchivePath/SHA256 (refcounted by archive_path).
type MediaArchive interface {
	// Archive moves a local file into the archive layout under the given
	// ref/meta/scope and returns the archive path. The caller may delete the
	// original path afterwards (in fact Archive is allowed to consume the
	// source file via os.Rename). If a file with the same SHA already exists
	// in the archive, the duplicate is collapsed onto the existing path.
	Archive(localPath, ref string, meta MediaMeta, scope string) (archivePath string, err error)

	// LookupByRef returns the archive entry for a given ref, or (entry{}, false).
	LookupByRef(ref string) (ArchiveEntry, bool)

	// LookupBySHA returns all entries that share the given content hash.
	LookupBySHA(sha string) []ArchiveEntry

	// Delete removes the index entry for the given ref. The physical file is
	// removed only when the last ref pointing at the same archive path is
	// gone. Returns true if the underlying file (and sidecar) were removed.
	Delete(ref string) (deletedFile bool, err error)

	// MarkResolved updates last_resolved_at for the given ref. Best-effort:
	// errors are logged but never returned.
	MarkResolved(ref string)

	// IterateForEviction calls fn for each ephemeral entry that was stored
	// before `now - ttl` and has no permanent sibling pointing at the same
	// archive_path. fn returns true to delete the file (and all entries
	// pointing at it).
	IterateForEviction(now time.Time, ttl time.Duration, fn func(ArchiveEntry) bool) error

	// Close releases underlying resources (closes SQLite handle).
	Close() error
}

// normalizeRetentionClass returns a known retention class. Empty input maps to
// ephemeral so that callers who do not opt in get the safer (TTL-managed)
// behavior by default.
func normalizeRetentionClass(rc RetentionClass) RetentionClass {
	switch rc {
	case RetentionClassPermanent:
		return RetentionClassPermanent
	case RetentionClassEphemeral, "":
		return RetentionClassEphemeral
	default:
		return RetentionClassEphemeral
	}
}
