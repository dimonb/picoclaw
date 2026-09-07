package fstools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/media"
)

func TestLoadImage_PathRequired(t *testing.T) {
	tool := NewLoadImageTool("/tmp", false, 0, nil)
	ctx := WithToolContext(context.Background(), "test", "chat1")
	result := tool.Execute(ctx, map[string]any{})
	if !result.IsError {
		t.Fatal("expected error for missing path")
	}
}

func TestLoadImage_NilMediaStore(t *testing.T) {
	tool := NewLoadImageTool("/tmp", false, 0, nil)
	ctx := WithToolContext(context.Background(), "test", "chat1")
	result := tool.Execute(ctx, map[string]any{"path": "test.png"})
	if !result.IsError || result.ForLLM != "media store not configured" {
		t.Fatalf("expected media store error, got: %s", result.ForLLM)
	}
}

func TestLoadImage_NoChannelContext(t *testing.T) {
	store := media.NewFileMediaStore()
	tool := NewLoadImageTool("/tmp", false, 0, store)
	// No WithToolContext — should fail
	result := tool.Execute(context.Background(), map[string]any{"path": "test.png"})
	if !result.IsError || result.ForLLM != "no target channel/chat available" {
		t.Fatalf("expected channel error, got: %s", result.ForLLM)
	}
}

func TestLoadImage_NonImageFile(t *testing.T) {
	dir := t.TempDir()
	txtFile := filepath.Join(dir, "readme.txt")
	os.WriteFile(txtFile, []byte("hello"), 0o644)

	store := media.NewFileMediaStore()
	tool := NewLoadImageTool(dir, false, 0, store)
	ctx := WithToolContext(context.Background(), "test", "chat1")
	result := tool.Execute(ctx, map[string]any{"path": txtFile})
	if !result.IsError {
		t.Fatal("expected error for non-image file")
	}
}

func TestLoadImage_DefaultMaxSize(t *testing.T) {
	tool := NewLoadImageTool("/tmp", false, 0, nil)
	if tool.maxFileSize != config.DefaultMaxMediaSize {
		t.Errorf("expected default max size %d, got %d", config.DefaultMaxMediaSize, tool.maxFileSize)
	}
}

func TestLoadImage_FileTooLarge(t *testing.T) {
	dir := t.TempDir()
	bigFile := filepath.Join(dir, "big.png")
	// Create a file with PNG header but exceeding max size
	data := make([]byte, 1024)
	copy(data, []byte{0x89, 0x50, 0x4E, 0x47}) // PNG magic bytes
	os.WriteFile(bigFile, data, 0o644)

	store := media.NewFileMediaStore()
	tool := NewLoadImageTool(dir, false, 512, store) // maxSize = 512
	ctx := WithToolContext(context.Background(), "test", "chat1")
	result := tool.Execute(ctx, map[string]any{"path": bigFile})
	if !result.IsError {
		t.Fatal("expected error for oversized file")
	}
}

func TestLoadImage_SuccessPath(t *testing.T) {
	dir := t.TempDir()

	// Create a minimal valid PNG file (8-byte signature + minimal IHDR + IEND).
	// The PNG spec requires the 8-byte magic header: 0x89 P N G \r \n 0x1a \n
	pngSignature := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	// IHDR chunk: length(13) + "IHDR" + 1x1 px, 8-bit RGB, no interlace + CRC
	ihdr := []byte{
		0x00, 0x00, 0x00, 0x0D, // chunk length = 13
		0x49, 0x48, 0x44, 0x52, // "IHDR"
		0x00, 0x00, 0x00, 0x01, // width = 1
		0x00, 0x00, 0x00, 0x01, // height = 1
		0x08,             // bit depth = 8
		0x02,             // color type = RGB
		0x00, 0x00, 0x00, // compression, filter, interlace
		0x90, 0x77, 0x53, 0xDE, // CRC (valid for this IHDR)
	}
	// IEND chunk
	iend := []byte{
		0x00, 0x00, 0x00, 0x00, // chunk length = 0
		0x49, 0x45, 0x4E, 0x44, // "IEND"
		0xAE, 0x42, 0x60, 0x82, // CRC
	}

	pngData := make([]byte, 0, len(pngSignature)+len(ihdr)+len(iend))
	pngData = append(pngData, pngSignature...)
	pngData = append(pngData, ihdr...)
	pngData = append(pngData, iend...)

	imgPath := filepath.Join(dir, "test_image.png")
	if err := os.WriteFile(imgPath, pngData, 0o644); err != nil {
		t.Fatalf("failed to create test PNG: %v", err)
	}

	store := media.NewFileMediaStore()
	tool := NewLoadImageTool(dir, false, 0, store)
	ctx := WithToolContext(context.Background(), "test", "chat1")

	result := tool.Execute(ctx, map[string]any{"path": imgPath})

	// 1. Must not be an error
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.ForLLM)
	}

	// 2. Media must contain exactly one media:// ref
	if len(result.Media) != 1 {
		t.Fatalf("expected 1 media ref, got %d", len(result.Media))
	}
	if !strings.HasPrefix(result.Media[0], "media://") {
		t.Errorf("expected media ref to start with 'media://', got: %s", result.Media[0])
	}

	// 3. ForLLM must contain the [image: marker
	if !strings.Contains(result.ForLLM, "[image:") {
		t.Errorf("expected ForLLM to contain '[image:' marker, got: %s", result.ForLLM)
	}

	// 4. ForLLM should contain the generic [image: photo] placeholder
	//    (resolveMediaRefs will replace it with the actual path later)
	if !strings.Contains(result.ForLLM, "[image: photo]") {
		t.Errorf("expected ForLLM to contain '[image: photo]' placeholder, got: %s", result.ForLLM)
	}

	// 5. Verify the ref is resolvable in the store
	resolved, err := store.Resolve(result.Media[0])
	if err != nil {
		t.Fatalf("media ref not resolvable: %v", err)
	}
	if resolved != imgPath {
		t.Errorf("expected resolved path %q, got %q", imgPath, resolved)
	}
}

// minimalPNG returns the smallest file detectMediaType accepts as an image.
func minimalPNG() []byte {
	out := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	out = append(out, []byte{
		0x00, 0x00, 0x00, 0x0D,
		0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01,
		0x00, 0x00, 0x00, 0x01,
		0x08,
		0x02,
		0x00, 0x00, 0x00,
		0x90, 0x77, 0x53, 0xDE,
	}...)
	return append(out, []byte{
		0x00, 0x00, 0x00, 0x00,
		0x49, 0x45, 0x4E, 0x44,
		0xAE, 0x42, 0x60, 0x82,
	}...)
}

func storeWithArchiveForTool(t *testing.T) *media.FileMediaStore {
	t.Helper()
	return storeWithArchiveWithCleanup(t, media.MediaCleanerConfig{})
}

func storeWithArchiveWithCleanup(t *testing.T, cfg media.MediaCleanerConfig) *media.FileMediaStore {
	t.Helper()
	archive, err := media.NewSQLiteMediaArchive(t.TempDir())
	if err != nil {
		t.Fatalf("NewSQLiteMediaArchive: %v", err)
	}
	t.Cleanup(func() { _ = archive.Close() })

	store := media.NewFileMediaStoreWithCleanup(cfg)
	store.SetArchive(archive)
	return store
}

// The regression this guards. load_image embeds its ref in ForLLM, which lands
// in conversation history and is re-resolved on every later turn. It used to
// mint a fresh ref for a borrowed file, which — being ForgetOnly — never
// reached the archive and so died at the next in-memory TTL sweep, leaving
// history pointing at nothing. Reusing the archived ref keeps it resolvable.
func TestLoadImage_ReusesArchivedRefAndSurvivesCleanup(t *testing.T) {
	dir := t.TempDir()
	store := storeWithArchiveWithCleanup(t, media.MediaCleanerConfig{
		Enabled:  true,
		Interval: time.Minute,
		MaxAge:   time.Nanosecond,
	})

	// A channel received the image and archived it, as telegram does.
	inbound := filepath.Join(dir, "inbound.png")
	if err := os.WriteFile(inbound, minimalPNG(), 0o644); err != nil {
		t.Fatalf("write inbound: %v", err)
	}
	channelRef, err := store.Store(inbound, media.MediaMeta{
		Filename:       "inbound.png",
		ContentType:    "image/png",
		Source:         "telegram",
		CleanupPolicy:  media.CleanupPolicyDeleteOnCleanup,
		RetentionClass: media.RetentionClassPermanent,
	}, "telegram:-100:5629")
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	archivedPath, err := store.Resolve(channelRef)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// The model now asks to look at that archived file.
	tool := NewLoadImageTool(dir, false, 0, store)
	ctx := WithToolContext(context.Background(), "telegram", "-100")
	result := tool.Execute(ctx, map[string]any{"path": archivedPath})
	if result.IsError {
		t.Fatalf("load_image failed: %s", result.ForLLM)
	}
	if len(result.Media) != 1 {
		t.Fatalf("Media = %v, want exactly one ref", result.Media)
	}

	if result.Media[0] != channelRef {
		t.Errorf("minted a new ref %q instead of reusing the archived %q",
			result.Media[0], channelRef)
	}

	// The point of reusing it: an in-memory sweep must not orphan it. The
	// store is configured to expire everything immediately, so sweep and then
	// resolve as a later turn would.
	store.CleanExpired()

	if _, err := store.Resolve(result.Media[0]); err != nil {
		t.Errorf("ref did not survive the TTL sweep: %v", err)
	}
}

// A file the archive has never seen still has to work: fall back to
// registering a ref, and mark it permanent so it is not reaped once a future
// change lets ForgetOnly entries reach the archive.
func TestLoadImage_FallsBackWhenNothingArchived(t *testing.T) {
	dir := t.TempDir()
	store := storeWithArchiveForTool(t)

	imgPath := filepath.Join(dir, "fresh.png")
	if err := os.WriteFile(imgPath, minimalPNG(), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	tool := NewLoadImageTool(dir, false, 0, store)
	ctx := WithToolContext(context.Background(), "telegram", "-100")
	result := tool.Execute(ctx, map[string]any{"path": imgPath})
	if result.IsError {
		t.Fatalf("load_image failed: %s", result.ForLLM)
	}
	if len(result.Media) != 1 {
		t.Fatalf("Media = %v, want exactly one ref", result.Media)
	}

	path, meta, err := store.ResolveWithMeta(result.Media[0])
	if err != nil {
		t.Fatalf("ResolveWithMeta: %v", err)
	}
	if path != imgPath {
		t.Errorf("resolved path = %q, want the original %q", path, imgPath)
	}
	if meta.RetentionClass != media.RetentionClassPermanent {
		t.Errorf("RetentionClass = %q, want permanent", meta.RetentionClass)
	}
	// Still ForgetOnly: the file is the caller's, not ours to delete.
	if meta.CleanupPolicy != media.CleanupPolicyForgetOnly {
		t.Errorf("CleanupPolicy = %q, want forget_only", meta.CleanupPolicy)
	}
}

// A store without the capability (no archive support at all) must keep working.
func TestLoadImage_StoreWithoutDurableRefFinder(t *testing.T) {
	dir := t.TempDir()
	imgPath := filepath.Join(dir, "plain.png")
	if err := os.WriteFile(imgPath, minimalPNG(), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	tool := NewLoadImageTool(dir, false, 0, media.NewFileMediaStore())
	ctx := WithToolContext(context.Background(), "telegram", "-100")
	result := tool.Execute(ctx, map[string]any{"path": imgPath})
	if result.IsError {
		t.Fatalf("load_image failed: %s", result.ForLLM)
	}
	if len(result.Media) != 1 || !strings.HasPrefix(result.Media[0], "media://") {
		t.Fatalf("Media = %v, want one media:// ref", result.Media)
	}
}
