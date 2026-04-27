//go:build !windows

package memory

import (
	"os"
	"syscall"
)

// lockFile acquires an exclusive advisory lock on path (creating the file if
// missing). The returned function releases the lock and closes the descriptor.
// Safe to call concurrently from multiple processes; flock waits.
func lockFile(path string) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return func() {}, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return func() {}, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
