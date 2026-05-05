package media

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/sipeed/picoclaw/pkg/logger"
)

const archiveSchema = `
CREATE TABLE IF NOT EXISTS media_archive (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    ref             TEXT UNIQUE NOT NULL,
    archive_path    TEXT NOT NULL,
    sha256          TEXT NOT NULL,
    filename        TEXT,
    content_type    TEXT,
    source          TEXT,
    scope           TEXT,
    cleanup_policy  TEXT,
    retention_class TEXT NOT NULL,
    size_bytes      INTEGER,
    stored_at       INTEGER NOT NULL,
    last_resolved_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_media_archive_sha       ON media_archive(sha256);
CREATE INDEX IF NOT EXISTS idx_media_archive_source    ON media_archive(source);
CREATE INDEX IF NOT EXISTS idx_media_archive_scope     ON media_archive(scope);
CREATE INDEX IF NOT EXISTS idx_media_archive_stored_at ON media_archive(stored_at);
CREATE INDEX IF NOT EXISTS idx_media_archive_retention ON media_archive(retention_class, stored_at);
CREATE INDEX IF NOT EXISTS idx_media_archive_path      ON media_archive(archive_path);
`

// SQLiteMediaArchive is a sqlite-backed MediaArchive that lays out files under
// <root>/<source>/<YYYYMMDD>/<sha[:2]>/<sha><ext> with sidecar JSON.
type SQLiteMediaArchive struct {
	root string
	db   *sql.DB
	mu   sync.Mutex // serializes Archive() to make rename + insert atomic
	now  func() time.Time
}

// sidecarMeta is what we serialize next to each archived file.
type sidecarMeta struct {
	Ref            string         `json:"ref"`
	SHA256         string         `json:"sha256"`
	Filename       string         `json:"filename,omitempty"`
	ContentType    string         `json:"content_type,omitempty"`
	Source         string         `json:"source,omitempty"`
	Scope          string         `json:"scope,omitempty"`
	CleanupPolicy  CleanupPolicy  `json:"cleanup_policy,omitempty"`
	RetentionClass RetentionClass `json:"retention_class,omitempty"`
	SizeBytes      int64          `json:"size_bytes"`
	StoredAt       time.Time      `json:"stored_at"`
}

// NewSQLiteMediaArchive opens (or creates) the index DB inside `root` and
// ensures the archive root directory exists. The index DB lives at
// <root>/index.db.
func NewSQLiteMediaArchive(root string) (*SQLiteMediaArchive, error) {
	if root == "" {
		return nil, errors.New("media archive: empty root")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("media archive: create root: %w", err)
	}

	dbPath := filepath.Join(root, "index.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("media archive: open db: %w", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode = WAL;"); err != nil {
		db.Close()
		return nil, fmt.Errorf("media archive: enable WAL: %w", err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout = 5000;"); err != nil {
		db.Close()
		return nil, fmt.Errorf("media archive: set busy_timeout: %w", err)
	}
	if _, err := db.Exec("PRAGMA synchronous = NORMAL;"); err != nil {
		db.Close()
		return nil, fmt.Errorf("media archive: set synchronous: %w", err)
	}
	if _, err := db.Exec(archiveSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("media archive: migrate: %w", err)
	}

	return &SQLiteMediaArchive{root: root, db: db, now: time.Now}, nil
}

// Root returns the archive root directory.
func (a *SQLiteMediaArchive) Root() string { return a.root }

// Close closes the underlying DB.
func (a *SQLiteMediaArchive) Close() error {
	if a.db == nil {
		return nil
	}
	return a.db.Close()
}

// Archive moves localPath into the archive layout and indexes it.
//
// If a file with the same SHA already exists in the archive, the duplicate is
// collapsed: the source file is removed and the existing archive_path is
// reused for the new ref.
func (a *SQLiteMediaArchive) Archive(localPath, ref string, meta MediaMeta, scope string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	info, err := os.Stat(localPath)
	if err != nil {
		return "", fmt.Errorf("archive stat: %w", err)
	}

	sha, err := hashFile(localPath)
	if err != nil {
		return "", fmt.Errorf("archive sha: %w", err)
	}

	storedAt := a.now()
	ext := ExtensionFor(meta.Filename, meta.ContentType)
	archivePath := BuildArchivePath(a.root, meta.Source, sha, ext, storedAt)
	retention := normalizeRetentionClass(meta.RetentionClass)

	// Check whether this content already exists; if so, collapse onto the
	// existing archive_path (different days for the same SHA all collapse to
	// whichever path was written first).
	if existing, ok := a.lookupAnyBySHA(sha); ok {
		archivePath = existing.ArchivePath
		// Remove the source file: nothing more to write.
		if err := os.Remove(localPath); err != nil && !os.IsNotExist(err) {
			logger.WarnCF("media.archive", "could not remove duplicate source", map[string]any{
				"path":  localPath,
				"error": err.Error(),
			})
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(archivePath), 0o755); err != nil {
			return "", fmt.Errorf("archive mkdir: %w", err)
		}
		// Concurrent producers of the same SHA may race; if the destination
		// already exists, we treat the source as a duplicate.
		if _, err := os.Stat(archivePath); err == nil {
			if rmErr := os.Remove(localPath); rmErr != nil && !os.IsNotExist(rmErr) {
				logger.WarnCF("media.archive", "could not remove duplicate source", map[string]any{
					"path":  localPath,
					"error": rmErr.Error(),
				})
			}
		} else {
			if err := moveOrCopy(localPath, archivePath); err != nil {
				return "", fmt.Errorf("archive move: %w", err)
			}
		}
		// Sidecar is best-effort; failure does not abort the archive.
		if err := writeSidecar(archivePath, sidecarMeta{
			Ref:            ref,
			SHA256:         sha,
			Filename:       meta.Filename,
			ContentType:    meta.ContentType,
			Source:         meta.Source,
			Scope:          scope,
			CleanupPolicy:  meta.CleanupPolicy,
			RetentionClass: retention,
			SizeBytes:      info.Size(),
			StoredAt:       storedAt,
		}); err != nil {
			logger.WarnCF("media.archive", "sidecar write failed", map[string]any{
				"path":  archivePath,
				"error": err.Error(),
			})
		}
	}

	if _, err := a.db.Exec(`
        INSERT OR REPLACE INTO media_archive (
            ref, archive_path, sha256, filename, content_type, source, scope,
            cleanup_policy, retention_class, size_bytes, stored_at, last_resolved_at
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
		ref, archivePath, sha, meta.Filename, meta.ContentType, meta.Source, scope,
		string(meta.CleanupPolicy), string(retention), info.Size(), storedAt.UnixNano(),
	); err != nil {
		return "", fmt.Errorf("archive index: %w", err)
	}

	return archivePath, nil
}

// LookupByRef returns the entry for a ref or (entry{}, false).
func (a *SQLiteMediaArchive) LookupByRef(ref string) (ArchiveEntry, bool) {
	row := a.db.QueryRow(`
        SELECT ref, archive_path, sha256, filename, content_type, source, scope,
               cleanup_policy, retention_class, size_bytes, stored_at, last_resolved_at
        FROM media_archive WHERE ref = ? LIMIT 1`, ref)
	entry, err := scanEntry(row)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			logger.WarnCF("media.archive", "lookup by ref failed", map[string]any{
				"ref":   ref,
				"error": err.Error(),
			})
		}
		return ArchiveEntry{}, false
	}
	return entry, true
}

// LookupBySHA returns all entries that share a content hash.
func (a *SQLiteMediaArchive) LookupBySHA(sha string) []ArchiveEntry {
	rows, err := a.db.Query(`
        SELECT ref, archive_path, sha256, filename, content_type, source, scope,
               cleanup_policy, retention_class, size_bytes, stored_at, last_resolved_at
        FROM media_archive WHERE sha256 = ?`, sha)
	if err != nil {
		logger.WarnCF("media.archive", "lookup by sha failed", map[string]any{
			"sha":   sha,
			"error": err.Error(),
		})
		return nil
	}
	defer rows.Close()
	var out []ArchiveEntry
	for rows.Next() {
		entry, err := scanEntry(rows)
		if err != nil {
			logger.WarnCF("media.archive", "scan failed", map[string]any{"error": err.Error()})
			continue
		}
		out = append(out, entry)
	}
	return out
}

// lookupAnyBySHA returns one arbitrary entry with the given SHA, if any.
func (a *SQLiteMediaArchive) lookupAnyBySHA(sha string) (ArchiveEntry, bool) {
	row := a.db.QueryRow(`
        SELECT ref, archive_path, sha256, filename, content_type, source, scope,
               cleanup_policy, retention_class, size_bytes, stored_at, last_resolved_at
        FROM media_archive WHERE sha256 = ? LIMIT 1`, sha)
	entry, err := scanEntry(row)
	if err != nil {
		return ArchiveEntry{}, false
	}
	return entry, true
}

// Delete removes the index entry for ref. The physical file is removed only
// when the last ref pointing at the same archive_path is gone.
func (a *SQLiteMediaArchive) Delete(ref string) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	entry, ok := a.LookupByRef(ref)
	if !ok {
		return false, nil
	}

	if _, err := a.db.Exec(`DELETE FROM media_archive WHERE ref = ?`, ref); err != nil {
		return false, fmt.Errorf("archive delete: %w", err)
	}

	var remaining int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM media_archive WHERE archive_path = ?`,
		entry.ArchivePath).Scan(&remaining); err != nil {
		return false, fmt.Errorf("archive delete count: %w", err)
	}
	if remaining > 0 {
		return false, nil
	}

	deleted := false
	if err := os.Remove(entry.ArchivePath); err != nil && !os.IsNotExist(err) {
		logger.WarnCF("media.archive", "remove archive file failed", map[string]any{
			"path":  entry.ArchivePath,
			"error": err.Error(),
		})
	} else {
		deleted = true
	}
	if err := os.Remove(SidecarPath(entry.ArchivePath)); err != nil && !os.IsNotExist(err) {
		logger.WarnCF("media.archive", "remove sidecar failed", map[string]any{
			"path":  SidecarPath(entry.ArchivePath),
			"error": err.Error(),
		})
	}
	return deleted, nil
}

// MarkResolved updates last_resolved_at for the given ref. Best-effort.
func (a *SQLiteMediaArchive) MarkResolved(ref string) {
	if _, err := a.db.Exec(`UPDATE media_archive SET last_resolved_at = ? WHERE ref = ?`,
		a.now().UnixNano(), ref); err != nil {
		logger.WarnCF("media.archive", "mark resolved failed", map[string]any{
			"ref":   ref,
			"error": err.Error(),
		})
	}
}

// IterateForEviction yields ephemeral entries that were stored before now-ttl
// AND whose archive_path has no permanent ref AND whose path was not resolved
// within the TTL window. fn returns true to delete the file (and all entries
// for that path).
//
// Files where the most recent last_resolved_at on any ref for the archive_path
// is younger than `now - ttl` are kept (recently used).
func (a *SQLiteMediaArchive) IterateForEviction(
	now time.Time, ttl time.Duration, fn func(ArchiveEntry) bool,
) error {
	if ttl <= 0 {
		return nil
	}
	cutoff := now.Add(-ttl).UnixNano()

	// Group by archive_path; pick paths whose every entry is ephemeral AND
	// whose newest last_resolved_at is missing or < cutoff AND whose newest
	// stored_at is < cutoff.
	rows, err := a.db.Query(`
        SELECT archive_path,
               MIN(stored_at) AS min_stored,
               MAX(stored_at) AS max_stored,
               COALESCE(MAX(last_resolved_at), 0) AS max_resolved,
               SUM(CASE WHEN retention_class = 'permanent' THEN 1 ELSE 0 END) AS permanent_count
        FROM media_archive
        GROUP BY archive_path
        HAVING permanent_count = 0
           AND max_stored < ?
           AND max_resolved < ?`, cutoff, cutoff)
	if err != nil {
		return fmt.Errorf("eviction scan: %w", err)
	}
	defer rows.Close()

	var paths []string
	for rows.Next() {
		var path string
		var minStored, maxStored, maxResolved int64
		var permanentCount int64
		if err := rows.Scan(&path, &minStored, &maxStored, &maxResolved, &permanentCount); err != nil {
			return fmt.Errorf("eviction scan row: %w", err)
		}
		paths = append(paths, path)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("eviction scan iter: %w", err)
	}

	for _, path := range paths {
		entries := a.entriesByArchivePath(path)
		if len(entries) == 0 {
			continue
		}
		if !fn(entries[0]) {
			continue
		}
		// Remove all index rows for this path and delete file/sidecar.
		a.mu.Lock()
		if _, err := a.db.Exec(`DELETE FROM media_archive WHERE archive_path = ?`, path); err != nil {
			a.mu.Unlock()
			logger.WarnCF("media.archive", "evict delete index failed", map[string]any{
				"path":  path,
				"error": err.Error(),
			})
			continue
		}
		a.mu.Unlock()

		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			logger.WarnCF("media.archive", "evict remove file failed", map[string]any{
				"path":  path,
				"error": err.Error(),
			})
		}
		if err := os.Remove(SidecarPath(path)); err != nil && !os.IsNotExist(err) {
			logger.WarnCF("media.archive", "evict remove sidecar failed", map[string]any{
				"path":  SidecarPath(path),
				"error": err.Error(),
			})
		}
	}
	return nil
}

func (a *SQLiteMediaArchive) entriesByArchivePath(path string) []ArchiveEntry {
	rows, err := a.db.Query(`
        SELECT ref, archive_path, sha256, filename, content_type, source, scope,
               cleanup_policy, retention_class, size_bytes, stored_at, last_resolved_at
        FROM media_archive WHERE archive_path = ?`, path)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []ArchiveEntry
	for rows.Next() {
		entry, err := scanEntry(rows)
		if err != nil {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows (after Next()).
type rowScanner interface {
	Scan(dest ...any) error
}

func scanEntry(r rowScanner) (ArchiveEntry, error) {
	var (
		entry      ArchiveEntry
		filename   sql.NullString
		mime       sql.NullString
		source     sql.NullString
		scope      sql.NullString
		policy     sql.NullString
		size       sql.NullInt64
		storedAt   int64
		resolvedAt sql.NullInt64
		retention  string
	)
	if err := r.Scan(&entry.Ref, &entry.ArchivePath, &entry.SHA256,
		&filename, &mime, &source, &scope,
		&policy, &retention, &size, &storedAt, &resolvedAt); err != nil {
		return ArchiveEntry{}, err
	}
	entry.Filename = filename.String
	entry.ContentType = mime.String
	entry.Source = source.String
	entry.Scope = scope.String
	entry.CleanupPolicy = CleanupPolicy(policy.String)
	entry.RetentionClass = RetentionClass(retention)
	entry.SizeBytes = size.Int64
	entry.StoredAt = time.Unix(0, storedAt)
	if resolvedAt.Valid {
		entry.LastResolvedAt = time.Unix(0, resolvedAt.Int64)
	}
	return entry, nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// moveOrCopy renames src to dst. Falls back to copy+remove on any rename
// failure (cross-device, permission, etc.) so the archive is filesystem-
// agnostic.
func moveOrCopy(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyFile(src, dst); err != nil {
		return err
	}
	return os.Remove(src)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func writeSidecar(archivePath string, meta sidecarMeta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(SidecarPath(archivePath), data, 0o644)
}
