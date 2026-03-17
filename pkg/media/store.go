package media

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/fileutil"
	"github.com/sipeed/picoclaw/pkg/logger"
)

// MediaMeta holds metadata about a stored media file.
type MediaMeta struct {
	Filename    string `json:"filename,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Source      string `json:"source,omitempty"` // "telegram", "discord", "tool:image-gen", etc.
}

// MediaStore manages the lifecycle of media files associated with processing scopes.
type MediaStore interface {
	// Store registers an existing local file under the given scope.
	// Returns a ref identifier (e.g. "media://<id>").
	// In disk-backed mode the file is copied into the managed media directory.
	Store(localPath string, meta MediaMeta, scope string) (ref string, err error)

	// Resolve returns the local path for a given ref.
	Resolve(ref string) (localPath string, err error)

	// ResolveWithMeta returns the local file path and metadata for a given ref.
	ResolveWithMeta(ref string) (localPath string, meta MediaMeta, err error)

	// ReleaseAll clears in-memory registrations for a scope.
	// In in-memory mode it also deletes the underlying files; in persistent mode
	// blobs and metadata remain on disk and can be resolved lazily again by ref.
	ReleaseAll(scope string) error
}

// mediaEntry holds the path and metadata for a stored media file.
type mediaEntry struct {
	path     string
	meta     MediaMeta
	storedAt time.Time
}

type persistedMediaEntry struct {
	ref   string
	scope string
	entry mediaEntry
}

type persistedEntry struct {
	Ref      string    `json:"ref"`
	Scope    string    `json:"scope"`
	StoredAt time.Time `json:"stored_at"`
	Meta     MediaMeta `json:"meta"`
}

// MediaCleanerConfig configures the background TTL cleanup.
type MediaCleanerConfig struct {
	Enabled  bool
	MaxAge   time.Duration
	Interval time.Duration
}

// FileMediaStore manages media refs in memory and can optionally persist blobs
// plus sidecar metadata to a directory on disk.
type FileMediaStore struct {
	mu          sync.RWMutex
	refs        map[string]mediaEntry
	scopeToRefs map[string]map[string]struct{}
	refToScope  map[string]string
	baseDir     string

	cleanerCfg MediaCleanerConfig
	stop       chan struct{}
	startOnce  sync.Once
	stopOnce   sync.Once
	nowFunc    func() time.Time // for testing
}

// NewFileMediaStore creates a new in-memory FileMediaStore without background cleanup.
func NewFileMediaStore() *FileMediaStore {
	return newFileMediaStore("", MediaCleanerConfig{}, false)
}

// NewPersistentFileMediaStore creates a FileMediaStore that persists media blobs
// and metadata in baseDir.
func NewPersistentFileMediaStore(baseDir string) *FileMediaStore {
	return newFileMediaStore(baseDir, MediaCleanerConfig{}, false)
}

// NewFileMediaStoreWithCleanup creates an in-memory FileMediaStore with TTL-based background cleanup.
func NewFileMediaStoreWithCleanup(cfg MediaCleanerConfig) *FileMediaStore {
	return newFileMediaStore("", cfg, true)
}

// NewPersistentFileMediaStoreWithCleanup creates a disk-backed FileMediaStore
// with TTL-based background cleanup.
func NewPersistentFileMediaStoreWithCleanup(baseDir string, cfg MediaCleanerConfig) *FileMediaStore {
	return newFileMediaStore(baseDir, cfg, true)
}

func newFileMediaStore(baseDir string, cfg MediaCleanerConfig, withCleanup bool) *FileMediaStore {
	s := &FileMediaStore{
		refs:        make(map[string]mediaEntry),
		scopeToRefs: make(map[string]map[string]struct{}),
		refToScope:  make(map[string]string),
		nowFunc:     time.Now,
	}
	if withCleanup {
		s.cleanerCfg = cfg
		s.stop = make(chan struct{})
	}
	if strings.TrimSpace(baseDir) != "" {
		cleanBaseDir := filepath.Clean(baseDir)
		if err := os.MkdirAll(cleanBaseDir, 0o700); err != nil {
			logger.WarnCF("media", "persistent store disabled: failed to create media directory", map[string]any{
				"path":  cleanBaseDir,
				"error": err.Error(),
			})
		} else {
			s.baseDir = cleanBaseDir
			s.loadPersistedEntries()
		}
	}
	return s
}

// Store registers a local file under the given scope. The file must exist.
func (s *FileMediaStore) Store(localPath string, meta MediaMeta, scope string) (string, error) {
	info, err := os.Stat(localPath)
	if err != nil {
		return "", fmt.Errorf("media store: %s: %w", localPath, err)
	}

	refID := uuid.New().String()
	ref := mediaRef(refID)
	storedAt := s.nowFunc()
	storedPath := localPath

	if s.baseDir != "" {
		storedPath = s.blobPath(refID)
		if err := s.persistMedia(localPath, storedPath, persistedEntry{
			Ref:      ref,
			Scope:    scope,
			StoredAt: storedAt,
			Meta:     meta,
		}, info.Mode().Perm()); err != nil {
			return "", err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.refs[ref] = mediaEntry{path: storedPath, meta: meta, storedAt: storedAt}
	if s.scopeToRefs[scope] == nil {
		s.scopeToRefs[scope] = make(map[string]struct{})
	}
	s.scopeToRefs[scope][ref] = struct{}{}
	s.refToScope[ref] = scope

	return ref, nil
}

// Resolve returns the local path for the given ref.
// Persistent stores lazily restore evicted refs from disk when needed.
func (s *FileMediaStore) Resolve(ref string) (string, error) {
	entry, err := s.resolveEntry(ref)
	if err != nil {
		return "", err
	}
	return entry.path, nil
}

// ResolveWithMeta returns the local path and metadata for the given ref.
// Persistent stores lazily restore evicted refs from disk when needed.
func (s *FileMediaStore) ResolveWithMeta(ref string) (string, MediaMeta, error) {
	entry, err := s.resolveEntry(ref)
	if err != nil {
		return "", MediaMeta{}, err
	}
	return entry.path, entry.meta, nil
}

// ReleaseAll clears in-memory mappings for a scope.
// In in-memory mode it also deletes files from disk.
func (s *FileMediaStore) ReleaseAll(scope string) error {
	entries := s.detachScope(scope)
	if s.baseDir != "" {
		return nil
	}
	for _, entry := range entries {
		s.deleteEntryFiles(entry.ref, entry.path)
	}
	return nil
}

// CleanExpired evicts entries older than MaxAge from memory.
// In in-memory mode the underlying files are deleted too; in persistent mode
// blobs and metadata remain on disk and can be reloaded on demand.
func (s *FileMediaStore) CleanExpired() int {
	if s.cleanerCfg.MaxAge <= 0 {
		return 0
	}

	s.mu.Lock()
	cutoff := s.nowFunc().Add(-s.cleanerCfg.MaxAge)
	var expired []detachedEntry

	for ref, entry := range s.refs {
		if entry.storedAt.Before(cutoff) {
			expired = append(expired, detachedEntry{ref: ref, path: entry.path})
			s.detachRefLocked(ref)
		}
	}
	s.mu.Unlock()

	if s.baseDir != "" {
		return len(expired)
	}

	for _, entry := range expired {
		s.deleteEntryFiles(entry.ref, entry.path)
	}

	return len(expired)
}

// Start begins the background cleanup goroutine if cleanup is enabled.
// Safe to call multiple times; only the first call starts the goroutine.
func (s *FileMediaStore) Start() {
	if !s.cleanerCfg.Enabled || s.stop == nil {
		return
	}
	if s.cleanerCfg.Interval <= 0 || s.cleanerCfg.MaxAge <= 0 {
		logger.WarnCF("media", "cleanup: skipped due to invalid config", map[string]any{
			"interval": s.cleanerCfg.Interval.String(),
			"max_age":  s.cleanerCfg.MaxAge.String(),
		})
		return
	}

	s.startOnce.Do(func() {
		logger.InfoCF("media", "cleanup enabled", map[string]any{
			"interval": s.cleanerCfg.Interval.String(),
			"max_age":  s.cleanerCfg.MaxAge.String(),
		})

		go func() {
			ticker := time.NewTicker(s.cleanerCfg.Interval)
			defer ticker.Stop()

			for {
				select {
				case <-ticker.C:
					if n := s.CleanExpired(); n > 0 {
						logger.InfoCF("media", "cleanup: evicted expired entries from memory", map[string]any{
							"count": n,
						})
					}
				case <-s.stop:
					return
				}
			}
		}()
	})
}

// Stop terminates the background cleanup goroutine.
// Safe to call multiple times; only the first call closes the channel.
func (s *FileMediaStore) Stop() {
	if s.stop == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stop)
	})
}

type detachedEntry struct {
	ref  string
	path string
}

func (s *FileMediaStore) detachScope(scope string) []detachedEntry {
	s.mu.Lock()
	defer s.mu.Unlock()

	refs, ok := s.scopeToRefs[scope]
	if !ok {
		return nil
	}

	entries := make([]detachedEntry, 0, len(refs))
	for ref := range refs {
		path := ""
		if entry, exists := s.refs[ref]; exists {
			path = entry.path
		}
		entries = append(entries, detachedEntry{ref: ref, path: path})
		s.detachRefLocked(ref)
	}
	return entries
}

func (s *FileMediaStore) detachRefLocked(ref string) {
	scope, ok := s.refToScope[ref]
	if ok {
		if scopeRefs, exists := s.scopeToRefs[scope]; exists {
			delete(scopeRefs, ref)
			if len(scopeRefs) == 0 {
				delete(s.scopeToRefs, scope)
			}
		}
	}
	delete(s.refs, ref)
	delete(s.refToScope, ref)
}

func (s *FileMediaStore) deleteEntryFiles(ref, path string) {
	if s.baseDir != "" {
		return
	}

	if path != "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			logger.WarnCF("media", "cleanup: failed to remove file", map[string]any{
				"path":  path,
				"error": err.Error(),
			})
		}
	}
}

func (s *FileMediaStore) loadPersistedEntries() {
	entries, err := os.ReadDir(s.baseDir)
	if err != nil {
		logger.WarnCF("media", "failed to read media directory", map[string]any{
			"path":  s.baseDir,
			"error": err.Error(),
		})
		return
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".meta.json") {
			continue
		}

		persisted, err := s.readPersistedEntryByID(strings.TrimSuffix(entry.Name(), ".meta.json"))
		if err != nil {
			logger.WarnCF("media", "skipping invalid persisted entry", map[string]any{
				"name":  entry.Name(),
				"error": err.Error(),
			})
			continue
		}

		s.rememberResolvedEntry(persisted)
	}
}

func (s *FileMediaStore) resolveEntry(ref string) (mediaEntry, error) {
	s.mu.RLock()
	entry, ok := s.refs[ref]
	s.mu.RUnlock()
	if ok {
		return entry, nil
	}
	if s.baseDir == "" {
		return mediaEntry{}, fmt.Errorf("media store: unknown ref: %s", ref)
	}

	id, ok := safeRefID(ref)
	if !ok {
		return mediaEntry{}, fmt.Errorf("media store: invalid ref: %s", ref)
	}

	persisted, err := s.readPersistedEntryByID(id)
	if err != nil {
		return mediaEntry{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.refs[ref]; ok {
		return entry, nil
	}

	s.rememberResolvedEntryLocked(persisted)
	return persisted.entry, nil
}

func (s *FileMediaStore) rememberResolvedEntry(p persistedMediaEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rememberResolvedEntryLocked(p)
}

func (s *FileMediaStore) rememberResolvedEntryLocked(p persistedMediaEntry) {
	s.refs[p.ref] = p.entry
	if p.scope != "" {
		if s.scopeToRefs[p.scope] == nil {
			s.scopeToRefs[p.scope] = make(map[string]struct{})
		}
		s.scopeToRefs[p.scope][p.ref] = struct{}{}
		s.refToScope[p.ref] = p.scope
	}
}

func (s *FileMediaStore) readPersistedEntryByID(id string) (persistedMediaEntry, error) {
	if s.baseDir == "" {
		return persistedMediaEntry{}, fmt.Errorf("media store: persistent storage is disabled")
	}
	if !isSafeRefID(id) {
		return persistedMediaEntry{}, fmt.Errorf("media store: invalid ref id: %s", id)
	}

	metaPath := s.metaPath(id)
	blobPath := s.blobPath(id)

	data, err := os.ReadFile(metaPath)
	if err != nil {
		return persistedMediaEntry{}, fmt.Errorf("media store: read metadata: %w", err)
	}

	var persisted persistedEntry
	if err := json.Unmarshal(data, &persisted); err != nil {
		return persistedMediaEntry{}, fmt.Errorf("media store: parse metadata: %w", err)
	}
	if persisted.Ref == "" {
		persisted.Ref = mediaRef(id)
	}
	if persisted.StoredAt.IsZero() {
		if info, statErr := os.Stat(metaPath); statErr == nil {
			persisted.StoredAt = info.ModTime()
		} else {
			persisted.StoredAt = time.Now()
		}
	}
	if _, err := os.Stat(blobPath); err != nil {
		return persistedMediaEntry{}, fmt.Errorf("media store: missing blob: %w", err)
	}

	return persistedMediaEntry{
		ref:   persisted.Ref,
		scope: persisted.Scope,
		entry: mediaEntry{
			path:     blobPath,
			meta:     persisted.Meta,
			storedAt: persisted.StoredAt,
		},
	}, nil
}

func (s *FileMediaStore) persistMedia(srcPath, blobPath string, meta persistedEntry, perm os.FileMode) error {
	if err := copyFileAtomic(srcPath, blobPath, perm); err != nil {
		return fmt.Errorf("media store: persist blob: %w", err)
	}

	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		_ = os.Remove(blobPath)
		return fmt.Errorf("media store: encode metadata: %w", err)
	}

	if err := fileutil.WriteFileAtomic(s.metaPath(refID(meta.Ref)), data, 0o600); err != nil {
		_ = os.Remove(blobPath)
		return fmt.Errorf("media store: persist metadata: %w", err)
	}

	return nil
}

func copyFileAtomic(srcPath, dstPath string, perm os.FileMode) error {
	if perm == 0 {
		perm = 0o600
	}

	srcFile, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer srcFile.Close()

	dir := filepath.Dir(dstPath)
	if mkdirErr := os.MkdirAll(dir, 0o700); mkdirErr != nil {
		return fmt.Errorf("create destination dir: %w", mkdirErr)
	}

	tmpFile, err := os.OpenFile(
		filepath.Join(dir, fmt.Sprintf(".tmp-%d-%d", os.Getpid(), time.Now().UnixNano())),
		os.O_WRONLY|os.O_CREATE|os.O_EXCL,
		perm,
	)
	if err != nil {
		return fmt.Errorf("create temp blob: %w", err)
	}

	tmpPath := tmpFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			tmpFile.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := io.Copy(tmpFile, srcFile); err != nil {
		return fmt.Errorf("copy blob: %w", err)
	}
	if err := tmpFile.Sync(); err != nil {
		return fmt.Errorf("sync temp blob: %w", err)
	}
	if err := tmpFile.Chmod(perm); err != nil {
		return fmt.Errorf("chmod temp blob: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp blob: %w", err)
	}
	if err := os.Rename(tmpPath, dstPath); err != nil {
		return fmt.Errorf("rename temp blob: %w", err)
	}
	if dirFile, err := os.Open(dir); err == nil {
		_ = dirFile.Sync()
		dirFile.Close()
	}

	cleanup = false
	return nil
}

func mediaRef(id string) string {
	return "media://" + id
}

func refID(ref string) string {
	return strings.TrimPrefix(ref, "media://")
}

func safeRefID(ref string) (string, bool) {
	id := refID(ref)
	if !strings.HasPrefix(ref, "media://") || id == "" {
		return "", false
	}
	if !isSafeRefID(id) {
		return "", false
	}
	return id, true
}

func isSafeRefID(id string) bool {
	if id == "" || strings.Contains(id, "..") {
		return false
	}
	return !strings.ContainsAny(id, `/\`)
}

func (s *FileMediaStore) blobPath(id string) string {
	return filepath.Join(s.baseDir, id+".blob")
}

func (s *FileMediaStore) metaPath(id string) string {
	return filepath.Join(s.baseDir, id+".meta.json")
}
