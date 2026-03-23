package media

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/logger"
)

// MediaMeta holds metadata about a stored media file.
type MediaMeta struct {
	Filename      string `json:"Filename"`
	ContentType   string `json:"ContentType,omitempty"`
	Source        string `json:"Source,omitempty"`
	StorageBucket string `json:"-"`
}

// StoredFileMeta is serialized alongside each stored file as <name>.meta.json.
type StoredFileMeta struct {
	Scope     string    `json:"scope"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	SHA256    string    `json:"sha256"`
	Meta      MediaMeta `json:"meta"`
}

// MediaStore manages the lifecycle of media files associated with processing scopes.
type MediaStore interface {
	// Store copies an existing local file into the managed media storage under the
	// given scope and returns the stored local path.
	Store(localPath string, meta MediaMeta, scope string) (storedPath string, err error)

	// Resolve returns the local file path for a given stored path.
	Resolve(ref string) (localPath string, err error)

	// ResolveWithMeta returns the local file path and metadata for a stored path.
	ResolveWithMeta(ref string) (localPath string, meta MediaMeta, err error)

	// ReleaseAll forgets all refs registered under the given scope.
	// Files remain on disk for manual inspection and explicit retention management.
	ReleaseAll(scope string) error
}

type mediaEntry struct {
	path      string
	meta      MediaMeta
	createdAt time.Time
	updatedAt time.Time
	sha256    string
}

type pathRefState struct {
	refCount int
}

// MediaCleanerConfig configures the background TTL cleanup.
type MediaCleanerConfig struct {
	Enabled  bool
	MaxAge   time.Duration
	Interval time.Duration
}

// FileMediaStore stores media as normal local files inside an organized on-disk tree.
type FileMediaStore struct {
	mu          sync.RWMutex
	rootDir     string
	entries     map[string]mediaEntry
	scopeToRefs map[string]map[string]struct{}
	refToScopes map[string]map[string]struct{}
	pathStates  map[string]pathRefState

	cleanerCfg MediaCleanerConfig
	stop       chan struct{}
	startOnce  sync.Once
	stopOnce   sync.Once
	nowFunc    func() time.Time // for testing
}

// NewFileMediaStore creates a new FileMediaStore rooted in a temporary directory.
func NewFileMediaStore() *FileMediaStore {
	root, err := os.MkdirTemp("", "picoclaw-media-*")
	if err != nil {
		panic(fmt.Sprintf("media store: failed to create temp root: %v", err))
	}
	return newFileMediaStore(root, MediaCleanerConfig{})
}

// NewFileMediaStoreAt creates a new FileMediaStore rooted at the provided directory.
func NewFileMediaStoreAt(root string) *FileMediaStore {
	return newFileMediaStore(root, MediaCleanerConfig{})
}

// NewFileMediaStoreWithCleanup creates a FileMediaStore in a temporary directory with TTL cleanup.
func NewFileMediaStoreWithCleanup(cfg MediaCleanerConfig) *FileMediaStore {
	root, err := os.MkdirTemp("", "picoclaw-media-*")
	if err != nil {
		panic(fmt.Sprintf("media store: failed to create temp root: %v", err))
	}
	return newFileMediaStore(root, cfg)
}

// NewFileMediaStoreAtWithCleanup creates a FileMediaStore rooted at the provided directory with TTL cleanup.
func NewFileMediaStoreAtWithCleanup(root string, cfg MediaCleanerConfig) *FileMediaStore {
	return newFileMediaStore(root, cfg)
}

func newFileMediaStore(root string, cfg MediaCleanerConfig) *FileMediaStore {
	if strings.TrimSpace(root) == "" {
		panic("media store: root directory is empty")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		panic(fmt.Sprintf("media store: failed to create root directory %q: %v", root, err))
	}
	store := &FileMediaStore{
		rootDir:     root,
		entries:     make(map[string]mediaEntry),
		scopeToRefs: make(map[string]map[string]struct{}),
		refToScopes: make(map[string]map[string]struct{}),
		pathStates:  make(map[string]pathRefState),
		cleanerCfg:  cfg,
		nowFunc:     time.Now,
	}
	if cfg.Enabled {
		store.stop = make(chan struct{})
	}
	return store
}

// RootDir returns the root directory managed by this store.
func (s *FileMediaStore) RootDir() string {
	return s.rootDir
}

// Store copies a local file into the managed media tree and returns the stored local path.
func (s *FileMediaStore) Store(localPath string, meta MediaMeta, scope string) (string, error) {
	info, err := os.Stat(localPath)
	if err != nil {
		return "", fmt.Errorf("media store: %s: %w", localPath, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("media store: %s: is a directory", localPath)
	}
	if strings.TrimSpace(meta.Filename) == "" {
		meta.Filename = filepath.Base(localPath)
	}

	now := s.nowFunc()
	shaValue, err := sha256File(localPath)
	if err != nil {
		return "", fmt.Errorf("media store: failed to hash %s: %w", localPath, err)
	}

	bucket := ""
	if strings.TrimSpace(meta.StorageBucket) != "" {
		bucket = sanitizeBucket(meta.StorageBucket)
	} else {
		bucket = deriveBucketFromScope(scope)
	}
	if bucket == "" {
		bucket = "unknown"
	}

	dayDir := now.Format("2006-01-02")
	targetDir := filepath.Join(s.rootDir, dayDir, bucket)
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return "", fmt.Errorf("media store: failed to create target dir: %w", err)
	}

	originalName := meta.Filename
	safeName := sanitizeStoredFilename(originalName)
	targetPath := filepath.Join(targetDir, safeName)

	createdAt := now
	reused := false
	if sidecar, readErr := readStoredMeta(sidecarPath(targetPath)); readErr == nil && sidecar.SHA256 == shaValue {
		createdAt = sidecar.CreatedAt
		reused = true
	}
	if !reused {
		if _, statErr := os.Stat(targetPath); statErr == nil {
			targetPath, err = nextAvailablePath(targetDir, safeName)
			if err != nil {
				return "", fmt.Errorf("media store: %w", err)
			}
		} else if !os.IsNotExist(statErr) {
			return "", fmt.Errorf("media store: failed to inspect target path: %w", statErr)
		}
		if err := copyFile(localPath, targetPath); err != nil {
			return "", fmt.Errorf("media store: failed to copy file: %w", err)
		}
	}

	storedMeta := StoredFileMeta{
		Scope:     scope,
		CreatedAt: createdAt,
		UpdatedAt: now,
		SHA256:    shaValue,
		Meta: MediaMeta{
			Filename:    originalName,
			ContentType: meta.ContentType,
			Source:      meta.Source,
		},
	}
	if err := writeStoredMeta(sidecarPath(targetPath), storedMeta); err != nil {
		return "", fmt.Errorf("media store: failed to write metadata: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.entries[targetPath] = mediaEntry{
		path:      targetPath,
		meta:      storedMeta.Meta,
		createdAt: storedMeta.CreatedAt,
		updatedAt: storedMeta.UpdatedAt,
		sha256:    shaValue,
	}
	if s.scopeToRefs[scope] == nil {
		s.scopeToRefs[scope] = make(map[string]struct{})
	}
	if _, exists := s.scopeToRefs[scope][targetPath]; !exists {
		s.scopeToRefs[scope][targetPath] = struct{}{}
		if s.refToScopes[targetPath] == nil {
			s.refToScopes[targetPath] = make(map[string]struct{})
		}
		s.refToScopes[targetPath][scope] = struct{}{}
		pathState := s.pathStates[targetPath]
		pathState.refCount++
		s.pathStates[targetPath] = pathState
	}

	return targetPath, nil
}

// Resolve returns the local path for the given stored reference.
func (s *FileMediaStore) Resolve(ref string) (string, error) {
	resolved := strings.TrimSpace(ref)
	if resolved == "" {
		return "", fmt.Errorf("media store: empty path")
	}
	if _, err := os.Stat(resolved); err != nil {
		return "", fmt.Errorf("media store: %s: %w", resolved, err)
	}
	return resolved, nil
}

// ResolveWithMeta returns the local path and metadata for the given stored path.
func (s *FileMediaStore) ResolveWithMeta(ref string) (string, MediaMeta, error) {
	resolved, err := s.Resolve(ref)
	if err != nil {
		return "", MediaMeta{}, err
	}

	s.mu.RLock()
	if entry, ok := s.entries[resolved]; ok {
		s.mu.RUnlock()
		return resolved, entry.meta, nil
	}
	s.mu.RUnlock()

	sidecar, err := readStoredMeta(sidecarPath(resolved))
	if err == nil {
		meta := sidecar.Meta
		if strings.TrimSpace(meta.Filename) == "" {
			meta.Filename = filepath.Base(resolved)
		}
		return resolved, meta, nil
	}

	return resolved, MediaMeta{Filename: filepath.Base(resolved)}, nil
}

// ReleaseAll forgets all refs under the given scope but intentionally keeps files on disk.
func (s *FileMediaStore) ReleaseAll(scope string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	refs, ok := s.scopeToRefs[scope]
	if !ok {
		return nil
	}
	for ref := range refs {
		s.releaseScopeRefLocked(ref, scope)
	}
	delete(s.scopeToRefs, scope)
	return nil
}

// CleanExpired forgets entries whose updated_at is older than MaxAge.
//
// Important: TTL cleanup intentionally does NOT delete files from disk.
// It only drops in-memory scope/path mappings so the on-disk media tree remains
// inspectable and can be cleaned manually or by a future explicit retention job.
func (s *FileMediaStore) CleanExpired() int {
	if s.cleanerCfg.MaxAge <= 0 {
		return 0
	}

	cutoff := s.nowFunc().Add(-s.cleanerCfg.MaxAge)

	s.mu.RLock()
	candidates := make([]string, 0)
	for path, entry := range s.entries {
		if entry.updatedAt.Before(cutoff) {
			candidates = append(candidates, path)
		}
	}
	s.mu.RUnlock()

	if len(candidates) == 0 {
		return 0
	}

	forgotten := 0
	s.mu.Lock()
	for _, path := range candidates {
		entry, ok := s.entries[path]
		if !ok || !entry.updatedAt.Before(cutoff) {
			continue
		}
		s.releaseAllScopesLocked(path)
		forgotten++
	}
	s.mu.Unlock()

	return forgotten
}

func (s *FileMediaStore) releaseScopeRefLocked(ref, scope string) {
	scopes, ok := s.refToScopes[ref]
	if !ok {
		return
	}
	if _, exists := scopes[scope]; !exists {
		return
	}
	delete(scopes, scope)
	if len(scopes) == 0 {
		delete(s.refToScopes, ref)
	}

	pathState, ok := s.pathStates[ref]
	if !ok {
		return
	}
	if pathState.refCount <= 1 {
		delete(s.pathStates, ref)
		delete(s.entries, ref)
		return
	}

	pathState.refCount--
	s.pathStates[ref] = pathState
}

func (s *FileMediaStore) releaseAllScopesLocked(ref string) {
	scopes := s.refToScopes[ref]
	for scope := range scopes {
		if scopeRefs, ok := s.scopeToRefs[scope]; ok {
			delete(scopeRefs, ref)
			if len(scopeRefs) == 0 {
				delete(s.scopeToRefs, scope)
			}
		}
	}
	delete(s.refToScopes, ref)
	delete(s.pathStates, ref)
	delete(s.entries, ref)
}

// Start begins the background cleanup goroutine if cleanup is enabled.
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
			"root":     s.rootDir,
		})

		go func() {
			ticker := time.NewTicker(s.cleanerCfg.Interval)
			defer ticker.Stop()

			for {
				select {
				case <-ticker.C:
					if n := s.CleanExpired(); n > 0 {
						logger.InfoCF("media", "cleanup: forgot expired entries (files kept on disk)", map[string]any{"count": n})
					}
				case <-s.stop:
					return
				}
			}
		}()
	})
}

// Stop terminates the background cleanup goroutine.
func (s *FileMediaStore) Stop() {
	if s.stop == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stop)
	})
}

func sidecarPath(path string) string {
	return path + ".meta.json"
}

func readStoredMeta(path string) (StoredFileMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return StoredFileMeta{}, err
	}
	var meta StoredFileMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return StoredFileMeta{}, err
	}
	return meta, nil
}

func writeStoredMeta(path string, meta StoredFileMeta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func sha256File(path string) (string, error) {
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

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}

	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func sanitizeStoredFilename(name string) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "file.bin"
	}
	trimmed = filepath.Base(trimmed)
	replacer := strings.NewReplacer("/", "_", "\\", "_", ":", "_")
	trimmed = replacer.Replace(trimmed)
	trimmed = strings.TrimSpace(trimmed)
	trimmed = strings.Trim(trimmed, ".")
	if trimmed == "" {
		return "file.bin"
	}
	return trimmed
}

func sanitizeBucket(bucket string) string {
	trimmed := strings.TrimSpace(bucket)
	if trimmed == "" {
		return ""
	}
	replacer := strings.NewReplacer(":", "_", "/", "_", "\\", "_", " ", "_")
	trimmed = replacer.Replace(trimmed)
	trimmed = strings.Trim(trimmed, "._")
	if trimmed == "" {
		return ""
	}
	return trimmed
}

func deriveBucketFromScope(scope string) string {
	trimmed := strings.TrimSpace(scope)
	if trimmed == "" {
		return ""
	}
	if idx := strings.LastIndex(trimmed, ":"); idx > 0 {
		trimmed = trimmed[:idx]
	}
	return sanitizeBucket(trimmed)
}

func nextAvailablePath(dir, filename string) (string, error) {
	ext := filepath.Ext(filename)
	base := strings.TrimSuffix(filename, ext)
	if base == "" {
		base = "file"
	}
	for i := 1; i <= 1000; i++ {
		candidate := fmt.Sprintf("%s.%d%s", base, i, ext)
		candidatePath := filepath.Join(dir, candidate)
		if _, err := os.Stat(candidatePath); os.IsNotExist(err) {
			return candidatePath, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("failed to allocate unique filename for %q in %q after 1000 attempts", filename, dir)
}
