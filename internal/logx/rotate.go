package logx

import (
	"fmt"
	"os"
	"sync"
)

// rotatingFile is a tiny io.WriteCloser that appends to `path` and, when
// the file grows past maxBytes, renames it to path.1 (shifting older
// rotations: .1→.2, .2→.3, etc., dropping anything past `keep`).
//
// We rotate inline on Write rather than via SIGHUP so there's no setup
// burden on operators. The check is cheap (one Stat per Write) and the
// rotation itself is rare (every ~10 MB by default).
//
// No external deps — keep goon's zero-dependency promise.
type rotatingFile struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	keep     int
	f        *os.File
	size     int64
}

func newRotatingFile(path string, maxBytes int64, keep int) (*rotatingFile, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &rotatingFile{
		path: path, maxBytes: maxBytes, keep: keep,
		f: f, size: st.Size(),
	}, nil
}

// Write appends p to the underlying file, rotating first if the new size
// would exceed maxBytes. Errors from rotation are non-fatal — we log to
// stderr and proceed (better to lose rotation than lose log entries).
func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size+int64(len(p)) > r.maxBytes {
		if err := r.rotateLocked(); err != nil {
			fmt.Fprintf(os.Stderr, "logx: rotation failed: %v (continuing on current file)\n", err)
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// Close flushes and closes the underlying file.
func (r *rotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

// rotateLocked performs the rename chain. Caller must hold r.mu.
//
// Sequence (path=goon.log, keep=3):
//  1. close current file
//  2. delete goon.log.3 (drop oldest)
//  3. rename goon.log.2 → goon.log.3
//  4. rename goon.log.1 → goon.log.2
//  5. rename goon.log   → goon.log.1
//  6. open fresh goon.log
func (r *rotatingFile) rotateLocked() error {
	if r.f != nil {
		_ = r.f.Close()
		r.f = nil
	}

	// Drop the oldest if it exists.
	oldest := fmt.Sprintf("%s.%d", r.path, r.keep)
	_ = os.Remove(oldest)

	// Shift .N-1 → .N, ... , .1 → .2.
	for i := r.keep - 1; i >= 1; i-- {
		from := fmt.Sprintf("%s.%d", r.path, i)
		to := fmt.Sprintf("%s.%d", r.path, i+1)
		_ = os.Rename(from, to) // ignore: missing files are fine
	}
	// Move current to .1.
	_ = os.Rename(r.path, r.path+".1")

	// Open fresh.
	f, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	r.f = f
	r.size = 0
	return nil
}
