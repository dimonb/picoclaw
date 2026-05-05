package gateway

import (
	"fmt"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/media"
)

// initMediaStack creates the in-memory MediaStore, attaches the persistent
// archive (if enabled), and starts the retention reaper. The result is stored
// on `s` so subsequent shutdown can stop the started components.
func initMediaStack(s *services, cfg *config.Config) error {
	store := media.NewFileMediaStoreWithCleanup(media.MediaCleanerConfig{
		Enabled:  cfg.Tools.MediaCleanup.Enabled,
		MaxAge:   time.Duration(cfg.Tools.MediaCleanup.MaxAge) * time.Minute,
		Interval: time.Duration(cfg.Tools.MediaCleanup.Interval) * time.Minute,
	})
	store.Start()

	archiveCfg := cfg.Media.Archive
	if archiveCfg.Enabled {
		archive, err := media.NewSQLiteMediaArchive(archiveCfg.Root)
		if err != nil {
			store.Stop()
			return fmt.Errorf("media archive: %w", err)
		}
		store.SetArchive(archive)
		s.MediaArchive = archive
		logger.InfoCF("media.archive", "archive enabled", map[string]any{
			"root": archiveCfg.Root,
		})

		if archiveCfg.ReaperEnabled {
			ttl := time.Duration(archiveCfg.EphemeralTTLDays) * 24 * time.Hour
			interval := time.Duration(archiveCfg.ReaperIntervalMins) * time.Minute
			reaper := media.NewRetentionReaper(archive, ttl, interval)
			reaper.Start()
			s.MediaReaper = reaper
		}
	}

	s.MediaStore = store
	return nil
}

// stopMediaStack stops the reaper, closes the archive, and stops the store.
// Safe to call when no media stack is initialized.
func stopMediaStack(s *services) {
	if s == nil {
		return
	}
	if s.MediaReaper != nil {
		s.MediaReaper.Stop()
		s.MediaReaper = nil
	}
	if s.MediaStore != nil {
		if fms, ok := s.MediaStore.(*media.FileMediaStore); ok {
			fms.Stop()
		}
	}
	if s.MediaArchive != nil {
		if err := s.MediaArchive.Close(); err != nil {
			logger.WarnCF("media.archive", "archive close failed", map[string]any{
				"error": err.Error(),
			})
		}
		s.MediaArchive = nil
	}
}
