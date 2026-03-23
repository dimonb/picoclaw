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

// CleanupPolicy controls how the MediaStore treats the underlying file when
// a ref is released or expires.
type CleanupPolicy string

const (
	// CleanupPolicyDeleteOnCleanup means the file is store-managed and may be
	// deleted once the final ref for that path is gone.
	CleanupPolicyDeleteOnCleanup CleanupPolicy = "delete_on_cleanup"
	// CleanupPolicyForgetOnly means the store should only drop ref mappings and
	// must never delete the underlying file.
	CleanupPolicyForgetOnly CleanupPolicy = "forget_only"
)

func normalizeCleanupPolicy(p CleanupPolicy) CleanupPolicy {
	if p == "" {
		return CleanupPolicyDeleteOnCleanup
	}
	return p
}

func effectiveCleanupPolicy(meta MediaMeta) CleanupPolicy {
	return normalizeCleanupPolicy(meta.CleanupPolicy)
}

// MediaMeta holds metadata about a stored media file.
type MediaMeta struct {
	Filename      string
	ContentType   string
	Source        string        // "telegram", "discord", "tool:image-gen", etc.
	CleanupPolicy CleanupPolicy // defaults to CleanupPolicyDeleteOnCleanup
}

// MediaStore manages the lifecycle of media files associated with processing scopes.
type MediaStore interface {
	// Store registers an existing local file under the given scope.
	// Returns a ref identifier (e.g. "media://<id>").
	// Store does not move or copy the file; it only records the mapping.
	// If meta.CleanupPolicy is empty, CleanupPolicyDeleteOnCleanup is assumed.
	Store(localPath string, meta MediaMeta, scope string) (ref string, err error)

	// Resolve returns the local file path for a given ref.
	Resolve(ref string) (localPath string, err error)

	// ResolveWithMeta returns the local file path and metadata for a given ref.
	ResolveWithMeta(ref string) (localPath string, meta MediaMeta, err error)

	// ReleaseAll deletes all files registered under the given scope
	// and removes the mapping entries. File-not-exist errors are ignored.
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

type pathRefState struct {
	refCount       int
	deleteEligible bool
}

// MediaCleanerConfig configures the background TTL cleanup.
type MediaCleanerConfig struct {
	Enabled  bool
	MaxAge   time.Duration
	Interval time.Duration
}

// FileMediaStore is a pure in-memory implementation of MediaStore.
// Files are expected to already exist on disk (e.g. in /tmp/picoclaw_media/).
type FileMediaStore struct {
	mu          sync.RWMutex
	refs        map[string]mediaEntry
	scopeToRefs map[string]map[string]struct{}
	refToScope  map[string]string
	refToPath   map[string]string
	pathStates  map[string]pathRefState
	baseDir     string

	cleanerCfg MediaCleanerConfig
	stop       chan struct{}
	startOnce  sync.Once
	stopOnce   sync.Once
	nowFunc    func() time.Time // for testing
}

func newFileMediaStore(baseDir string, cfg MediaCleanerConfig, withCleanup bool) *FileMediaStore {
	s := &FileMediaStore{
		refs:        make(map[string]mediaEntry),
		scopeToRefs: make(map[string]map[string]struct{}),
		refToScope:  make(map[string]string),
		refToPath:   make(map[string]string),
		pathStates:  make(map[string]pathRefState),
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

// NewFileMediaStore creates a new FileMediaStore without background cleanup.
func NewFileMediaStore() *FileMediaStore {
	return newFileMediaStore("", MediaCleanerConfig{}, false)
}

// NewPersistentFileMediaStore creates a FileMediaStore that persists media blobs
// and metadata in baseDir.
func NewPersistentFileMediaStore(baseDir string) *FileMediaStore {
	return newFileMediaStore(baseDir, MediaCleanerConfig{}, false)
}

// NewFileMediaStoreWithCleanup creates a FileMediaStore with TTL-based background cleanup.
func NewFileMediaStoreWithCleanup(cfg MediaCleanerConfig) *FileMediaStore {
	return newFileMediaStore("", cfg, true)
}

// NewPersistentFileMediaStoreWithCleanup creates a disk-backed FileMediaStore
// with TTL-based background cleanup.
func NewPersistentFileMediaStoreWithCleanup(baseDir string, cfg MediaCleanerConfig) *FileMediaStore {
	return newFileMediaStore(baseDir, cfg, true)
}

// Store registers a local file under the given scope. The file must exist.
func (s *FileMediaStore) Store(localPath string, meta MediaMeta, scope string) (string, error) {
	info, err := os.Stat(localPath)
	if err != nil {
		return "", fmt.Errorf("media store: %s: %w", localPath, err)
	}

	refID := uuid.New().String()
	ref := mediaRef(refID)
	policy := effectiveCleanupPolicy(meta)
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
	s.refToPath[ref] = storedPath

	pathState := s.pathStates[storedPath]
	if pathState.refCount == 0 {
		pathState.deleteEligible = policy == CleanupPolicyDeleteOnCleanup
	} else if policy == CleanupPolicyForgetOnly {
		// Be conservative: once a path is borrowed externally, never let this
		// lifecycle auto-delete it even if store-managed refs also exist.
		pathState.deleteEligible = false
	}
	if s.baseDir != "" {
		pathState.deleteEligible = false
	}
	pathState.refCount++
	s.pathStates[storedPath] = pathState

	return ref, nil
}

// Resolve returns the local path for the given ref.
func (s *FileMediaStore) Resolve(ref string) (string, error) {
	entry, err := s.resolveEntry(ref)
	if err != nil {
		return "", err
	}
	return entry.path, nil
}

// ResolveWithMeta returns the local path and metadata for the given ref.
func (s *FileMediaStore) ResolveWithMeta(ref string) (string, MediaMeta, error) {
	entry, err := s.resolveEntry(ref)
	if err != nil {
		return "", MediaMeta{}, err
	}
	return entry.path, entry.meta, nil
}

// ReleaseAll removes refs for the given scope.
// In in-memory mode, store-managed files are deleted once the final ref goes away.
// In persistent mode, blobs/metadata remain on disk and can be resolved again later.
func (s *FileMediaStore) ReleaseAll(scope string) error {
	var paths []string

	s.mu.Lock()
	refs, ok := s.scopeToRefs[scope]
	if !ok {
		s.mu.Unlock()
		return nil
	}

	for ref := range refs {
		fallbackPath := ""
		if entry, exists := s.refs[ref]; exists {
			fallbackPath = entry.path
		}
		if removablePath, shouldDelete := s.releaseRefLocked(ref, fallbackPath); shouldDelete {
			paths = append(paths, removablePath)
		}
	}
	delete(s.scopeToRefs, scope)
	s.mu.Unlock()

	if s.baseDir != "" {
		return nil
	}

	for _, p := range paths {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			logger.WarnCF("media", "cleanup: failed to remove file", map[string]any{
				"path":  p,
				"error": err.Error(),
			})
		}
	}

	return nil
}

// releaseRefLocked removes ref mappings. Returns the physical file path and true
// if the final reference to a store-managed path was dropped.
// The caller is responsible for deleting the returned path.
func (s *FileMediaStore) releaseRefLocked(ref string, fallbackPath string) (path string, shouldDelete bool) {
	path = fallbackPath
	if mapped, ok := s.refToPath[ref]; ok {
		path = mapped
	}

	delete(s.refs, ref)
	delete(s.refToPath, ref)
	if scope, ok := s.refToScope[ref]; ok {
		delete(s.refToScope, ref)
		if s.scopeToRefs[scope] != nil {
			delete(s.scopeToRefs[scope], ref)
		}
	}

	if path == "" {
		return "", false
	}

	state, ok := s.pathStates[path]
	if !ok {
		return "", false
	}

	state.refCount--
	if state.refCount <= 0 {
		delete(s.pathStates, path)
		if state.deleteEligible {
			return path, true
		}
	} else {
		s.pathStates[path] = state
	}

	return "", false
}

// CleanExpired evicts entries older than MaxAge.
// In in-memory mode, store-managed files are deleted once their final ref goes away.
// In persistent mode, blobs/metadata remain on disk and can be lazily reloaded.
func (s *FileMediaStore) CleanExpired() int {
	if s.cleanerCfg.MaxAge <= 0 {
		return 0
	}

	// Phase 1: collect expired entries under lock
	type expiredEntry struct {
		ref        string
		deletePath string
	}

	s.mu.Lock()
	cutoff := s.nowFunc().Add(-s.cleanerCfg.MaxAge)
	var expired []expiredEntry

	for ref, entry := range s.refs {
		if entry.storedAt.Before(cutoff) {
			if scope, ok := s.refToScope[ref]; ok {
				if scopeRefs, ok := s.scopeToRefs[scope]; ok {
					delete(scopeRefs, ref)
					if len(scopeRefs) == 0 {
						delete(s.scopeToRefs, scope)
					}
				}
			}

			expiredItem := expiredEntry{ref: ref}
			if deletePath, shouldDelete := s.releaseRefLocked(ref, entry.path); shouldDelete {
				expiredItem.deletePath = deletePath
			}
			expired = append(expired, expiredItem)
		}
	}
	s.mu.Unlock()

	if s.baseDir != "" {
		return len(expired)
	}

	for _, e := range expired {
		if e.deletePath == "" {
			continue
		}
		if err := os.Remove(e.deletePath); err != nil && !os.IsNotExist(err) {
			logger.WarnCF("media", "cleanup: failed to remove file", map[string]any{
				"path":  e.deletePath,
				"error": err.Error(),
			})
		}
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
// Safe to call multiple times or if cleanup was not started.
func (s *FileMediaStore) Stop() {
	if s.stop == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stop)
	})
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

	s.refToPath[p.ref] = p.entry.path
	policy := effectiveCleanupPolicy(p.entry.meta)
	pathState := s.pathStates[p.entry.path]
	if pathState.refCount == 0 {
		pathState.deleteEligible = policy == CleanupPolicyDeleteOnCleanup
	} else if policy == CleanupPolicyForgetOnly {
		pathState.deleteEligible = false
	}
	if s.baseDir != "" {
		pathState.deleteEligible = false
	}
	pathState.refCount++
	s.pathStates[p.entry.path] = pathState
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
		return persistedMediaEntry{}, fmt.Errorf("media store: blob not found: %w", err)
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
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create destination dir: %w", err)
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
		return fmt.Errorf("copy content: %w", err)
	}

	if err := tmpFile.Sync(); err != nil {
		return fmt.Errorf("sync temp file: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	cleanup = false

	if err := os.Rename(tmpPath, dstPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename to destination: %w", err)
	}

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
