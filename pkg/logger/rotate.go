package logger

import (
	"fmt"
	"os"
	"sync"
)

// rotatingFile is an append-mode log file with a size ceiling.
//
// Writes are serialized because zerolog writes from every goroutine that logs,
// and rotation has to be atomic with respect to them: a rename racing a write
// would send records to a file nobody looks at any more.
type rotatingFile struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	maxFiles int
	file     *os.File
	size     int64
}

func newRotatingFile(path string, opts FileLoggingOptions) (*rotatingFile, error) {
	r := &rotatingFile{
		path:     path,
		maxBytes: int64(opts.MaxSizeMB) * 1024 * 1024,
		maxFiles: opts.MaxFiles,
	}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

// open attaches to the active path, adopting whatever is already there. The
// existing size counts towards the ceiling, so an oversized file left by an
// earlier build rotates on its first write rather than growing further.
func (r *rotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	var size int64
	if info, statErr := f.Stat(); statErr == nil {
		size = info.Size()
	}
	r.file = f
	r.size = size
	return nil
}

// Write appends p, rotating first when it would not fit.
//
// A record larger than the whole ceiling is still written in one piece, on an
// empty file, and only rotated away afterwards: picoclaw logs entire LLM
// requests as single JSON lines, and half a line is worth less than nothing to
// whoever reads the log next.
func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.file == nil {
		return 0, os.ErrClosed
	}
	if r.size > 0 && r.size+int64(len(p)) > r.maxBytes {
		if err := r.rotate(); err != nil {
			// Log to stderr and keep writing wherever we still can: dropping
			// records because housekeeping failed is the worse trade.
			fmt.Fprintf(os.Stderr, "log rotation failed: %v\n", err)
		}
	}
	if r.file == nil {
		return 0, os.ErrClosed
	}
	n, err := r.file.Write(p)
	r.size += int64(n)
	return n, err
}

// rotate shifts the active file into <path>.1, cascading the older ones up and
// dropping whatever falls past maxFiles, then starts a fresh active file.
// Called with the mutex held.
func (r *rotatingFile) rotate() error {
	if err := r.file.Close(); err != nil {
		return err
	}
	r.file = nil

	_ = os.Remove(r.backupPath(r.maxFiles))
	for i := r.maxFiles - 1; i >= 1; i-- {
		from, to := r.backupPath(i), r.backupPath(i+1)
		if _, err := os.Stat(from); err == nil {
			_ = os.Rename(from, to)
		}
	}

	if err := os.Rename(r.path, r.backupPath(1)); err != nil && !os.IsNotExist(err) {
		// Reopen the original so logging survives a failed rename (a
		// cross-device log directory, say) instead of going silent.
		if openErr := r.open(); openErr != nil {
			return fmt.Errorf("rename %s: %w (reopen also failed: %v)", r.path, err, openErr)
		}
		return fmt.Errorf("rename %s: %w", r.path, err)
	}

	return r.open()
}

func (r *rotatingFile) backupPath(n int) string {
	return fmt.Sprintf("%s.%d", r.path, n)
}

func (r *rotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file = nil
	return err
}
