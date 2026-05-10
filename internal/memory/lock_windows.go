//go:build windows

package memory

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// lockFile acquires an exclusive lock on path using O_CREATE|O_EXCL on a
// sibling lockfile. This is "atomic enough" for single-machine multi-process
// coordination on Windows without pulling in golang.org/x/sys/windows for
// LockFileEx (we ship zero deps).
//
// Behavior:
//   - Tries to create path with O_CREATE|O_EXCL. On success the caller owns
//     the lock until release() is called.
//   - On EEXIST, busy-waits (50ms backoff) up to lockWaitTimeout.
//   - If the existing lockfile's mtime is older than lockStaleAfter we treat
//     it as abandoned (a previous process crashed without releasing) and
//     remove it before retrying.
//   - release() closes the descriptor and removes the file. Best-effort —
//     errors are swallowed so callers don't see partial cleanup as a failure.
const (
	lockWaitTimeout = 5 * time.Second
	lockStaleAfter  = 2 * time.Minute
	lockBackoff     = 50 * time.Millisecond
)

func lockFile(path string) (release func(), err error) {
	deadline := time.Now().Add(lockWaitTimeout)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err == nil {
			// Optional: write our pid for diagnostics. Failure is non-fatal.
			_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
			return func() {
				_ = f.Close()
				_ = os.Remove(path)
			}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return func() {}, err
		}
		// Lockfile exists. If it's stale, evict it and retry without
		// counting the eviction against our deadline.
		if info, statErr := os.Stat(path); statErr == nil {
			if time.Since(info.ModTime()) > lockStaleAfter {
				_ = os.Remove(path)
				continue
			}
		}
		if time.Now().After(deadline) {
			return func() {}, fmt.Errorf("memory: lockfile %s held by another process", path)
		}
		time.Sleep(lockBackoff)
	}
}
