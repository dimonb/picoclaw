package agent

import (
	"path/filepath"
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/media"
)

func TestBuildAllowReadPatternsCoversInboundMedia(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Media.Archive.Enabled = true
	cfg.Media.Archive.Root = "/srv/bot/media"
	cfg.Tools.AllowReadPaths = []string{"^/srv/bot/extra(?:/|$)"}

	patterns := buildAllowReadPatterns(cfg)
	allowed := func(path string) bool {
		for _, p := range patterns {
			if p.MatchString(path) {
				return true
			}
		}
		return false
	}

	for _, path := range []string{
		"/srv/bot/media/telegram/20261008/9d/photo.jpg",
		filepath.Join(media.TempDir(), "file_1.jpg"),
		"/srv/bot/extra/a.txt",
	} {
		if !allowed(path) {
			t.Errorf("%s should be readable", path)
		}
	}
	if allowed("/srv/bot/mediafake/x.jpg") || allowed("/srv/bot/config.json") {
		t.Error("patterns reach past the media dirs")
	}

	cfg.Media.Archive.Enabled = false
	n := len(buildAllowReadPatterns(cfg))
	if n != 2 {
		t.Errorf("archive off: %d patterns, want configured + temp dir", n)
	}
}
