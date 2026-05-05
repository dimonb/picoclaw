package media

import (
	"path/filepath"
	"strings"
	"time"
)

const sidecarSuffix = ".meta.json"

// BuildArchivePath returns the absolute path of the archived file in the layout
// <root>/<source>/<YYYYMMDD>/<sha[:2]>/<sha><ext>.
//
// `source` is sanitized to a single path segment so that arbitrary input from
// MediaMeta.Source cannot escape the archive root.
func BuildArchivePath(root, source, sha, ext string, storedAt time.Time) string {
	src := sanitizeSourceSegment(source)
	day := storedAt.UTC().Format("20060102")
	prefix := sha
	if len(prefix) >= 2 {
		prefix = sha[:2]
	}
	name := sha + ext
	return filepath.Join(root, src, day, prefix, name)
}

// SidecarPath returns the path of the metadata sidecar for an archived file.
func SidecarPath(archivePath string) string {
	return archivePath + sidecarSuffix
}

// ExtensionFor picks a file extension for the archived file using, in order:
//  1. the extension already present on the original filename, if any;
//  2. a small MIME → extension table for common types;
//  3. ".bin" as a final fallback.
func ExtensionFor(filename, contentType string) string {
	if ext := filepath.Ext(filename); ext != "" {
		return strings.ToLower(ext)
	}

	switch strings.ToLower(strings.TrimSpace(contentType)) {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/heic":
		return ".heic"
	case "audio/ogg":
		return ".ogg"
	case "audio/mpeg", "audio/mp3":
		return ".mp3"
	case "audio/wav", "audio/x-wav":
		return ".wav"
	case "audio/aac":
		return ".aac"
	case "audio/flac":
		return ".flac"
	case "video/mp4":
		return ".mp4"
	case "video/quicktime":
		return ".mov"
	case "video/webm":
		return ".webm"
	case "text/plain":
		return ".txt"
	case "application/json":
		return ".json"
	case "application/pdf":
		return ".pdf"
	case "application/zip":
		return ".zip"
	}

	return ".bin"
}

// sanitizeSourceSegment turns an arbitrary `MediaMeta.Source` string into a
// single safe directory name. Path separators are replaced and characters
// outside [a-z0-9._-] are replaced with `_`.
func sanitizeSourceSegment(source string) string {
	src := strings.ToLower(strings.TrimSpace(source))
	if src == "" {
		return "unknown"
	}
	var b strings.Builder
	b.Grow(len(src))
	for _, r := range src {
		switch {
		case r >= 'a' && r <= 'z',
			r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		case r == '/' || r == ':':
			b.WriteRune('_')
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if out == "" {
		return "unknown"
	}
	return out
}
