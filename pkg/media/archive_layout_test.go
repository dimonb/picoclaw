package media

import (
	"path/filepath"
	"testing"
	"time"
)

func TestBuildArchivePath(t *testing.T) {
	stored := time.Date(2026, 5, 5, 11, 30, 0, 0, time.UTC)
	got := BuildArchivePath("/var/picoclaw/media", "telegram",
		"abcd1234ef567890abcd1234ef567890abcd1234ef567890abcd1234ef567890",
		".jpg", stored)
	want := filepath.Join(
		"/var/picoclaw/media", "telegram", "20260505", "ab",
		"abcd1234ef567890abcd1234ef567890abcd1234ef567890abcd1234ef567890.jpg",
	)
	if got != want {
		t.Fatalf("BuildArchivePath = %q, want %q", got, want)
	}
}

func TestBuildArchivePath_ShortSHA(t *testing.T) {
	stored := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	got := BuildArchivePath("/r", "tg", "a", ".png", stored)
	want := filepath.Join("/r", "tg", "20260102", "a", "a.png")
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBuildArchivePath_SanitizesSource(t *testing.T) {
	stored := time.Date(2026, 5, 5, 0, 0, 0, 0, time.UTC)
	got := BuildArchivePath("/r", "tool:mcp:fetch/data", "ab", ".bin", stored)
	want := filepath.Join("/r", "tool_mcp_fetch_data", "20260505", "ab", "ab.bin")
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBuildArchivePath_EmptySource(t *testing.T) {
	stored := time.Date(2026, 5, 5, 0, 0, 0, 0, time.UTC)
	got := BuildArchivePath("/r", "", "ab", ".bin", stored)
	want := filepath.Join("/r", "unknown", "20260505", "ab", "ab.bin")
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSidecarPath(t *testing.T) {
	got := SidecarPath("/r/tg/20260505/ab/ab.jpg")
	want := "/r/tg/20260505/ab/ab.jpg.meta.json"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestExtensionFor(t *testing.T) {
	cases := []struct {
		filename, contentType, want string
	}{
		{"photo.jpg", "", ".jpg"},
		{"PHOTO.JPG", "", ".jpg"},
		{"voice.ogg", "audio/ogg", ".ogg"},
		{"", "image/png", ".png"},
		{"", "image/jpeg", ".jpg"},
		{"", "audio/mpeg", ".mp3"},
		{"", "video/mp4", ".mp4"},
		{"", "application/pdf", ".pdf"},
		{"", "text/plain", ".txt"},
		{"", "", ".bin"},
		{"document", "application/octet-stream", ".bin"},
	}
	for _, c := range cases {
		got := ExtensionFor(c.filename, c.contentType)
		if got != c.want {
			t.Errorf("ExtensionFor(%q, %q) = %q, want %q", c.filename, c.contentType, got, c.want)
		}
	}
}

func TestSanitizeSourceSegment(t *testing.T) {
	cases := []struct{ in, want string }{
		{"telegram", "telegram"},
		{"Telegram", "telegram"},
		{"tool:mcp:fetch", "tool_mcp_fetch"},
		{"tool/mcp/fetch", "tool_mcp_fetch"},
		{"  ", "unknown"},
		{"", "unknown"},
		{"a..b__c--d", "a..b__c--d"},
		{"привет", "______"},
	}
	for _, c := range cases {
		got := sanitizeSourceSegment(c.in)
		if got != c.want {
			t.Errorf("sanitizeSourceSegment(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
