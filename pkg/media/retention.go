package media

import (
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/logger"
)

// RetentionReaper periodically asks the archive to evict ephemeral entries
// that exceeded the configured TTL and were not recently resolved.
//
// Permanent entries are never evicted by this reaper.
type RetentionReaper struct {
	archive  MediaArchive
	ttl      time.Duration
	interval time.Duration

	stop      chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once

	now func() time.Time // for testing
}

// NewRetentionReaper builds a reaper. ttl/interval must both be positive for
// the reaper to do work; otherwise Start is a no-op.
func NewRetentionReaper(archive MediaArchive, ttl, interval time.Duration) *RetentionReaper {
	return &RetentionReaper{
		archive:  archive,
		ttl:      ttl,
		interval: interval,
		stop:     make(chan struct{}),
		now:      time.Now,
	}
}

// Start launches the background reaper. Safe to call multiple times; only
// the first call has effect.
func (r *RetentionReaper) Start() {
	if r.archive == nil {
		return
	}
	if r.ttl <= 0 || r.interval <= 0 {
		logger.WarnCF("media.archive", "reaper disabled: invalid ttl/interval", map[string]any{
			"ttl":      r.ttl.String(),
			"interval": r.interval.String(),
		})
		return
	}
	r.startOnce.Do(func() {
		logger.InfoCF("media.archive", "retention reaper started", map[string]any{
			"ttl":      r.ttl.String(),
			"interval": r.interval.String(),
		})
		go func() {
			ticker := time.NewTicker(r.interval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					r.RunOnce()
				case <-r.stop:
					return
				}
			}
		}()
	})
}

// Stop terminates the reaper. Safe to call multiple times.
func (r *RetentionReaper) Stop() {
	if r.stop == nil {
		return
	}
	r.stopOnce.Do(func() {
		close(r.stop)
	})
}

// RunOnce performs a single eviction pass. Exported so tests (and ops) can
// trigger it on demand without waiting for the ticker.
func (r *RetentionReaper) RunOnce() int {
	if r.archive == nil || r.ttl <= 0 {
		return 0
	}
	count := 0
	err := r.archive.IterateForEviction(r.now(), r.ttl, func(entry ArchiveEntry) bool {
		count++
		logger.InfoCF("media.archive", "evicting ephemeral entry", map[string]any{
			"path":      entry.ArchivePath,
			"source":    entry.Source,
			"stored_at": entry.StoredAt.Format(time.RFC3339),
		})
		return true
	})
	if err != nil {
		logger.WarnCF("media.archive", "eviction pass failed", map[string]any{
			"error": err.Error(),
		})
	}
	return count
}
