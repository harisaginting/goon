package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/harisaginting/goon/internal/storage"
)

// pidFilePath returns the absolute path to goon's daemon PID file.
// Default: <storage.Root()>/goon.pid (i.e. ./storage/goon.pid). Override
// with $GOON_PID_FILE for ad-hoc placements (multi-daemon hosts, etc).
func pidFilePath() string {
	if v := strings.TrimSpace(os.Getenv("GOON_PID_FILE")); v != "" {
		return v
	}
	return storage.Path("goon.pid")
}

// writePIDFile records the current process id atomically.
func writePIDFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readPIDFile returns the recorded pid or an error.
func readPIDFile(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("bad pid file: %w", err)
	}
	return pid, nil
}

// removePIDFile is a best-effort cleanup.
func removePIDFile(path string) {
	_ = os.Remove(path)
}

// processAlive returns true if the pid is still running.
//
// Unix: send signal 0 — the canonical portable liveness check.
// Windows: signal 0 is not supported there, so processAliveWindows uses the
// fact that os.FindProcess actually opens a handle and fails for missing
// pids, with a stale-pid-file fallback to defend against pid reuse.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if runtime.GOOS == "windows" {
		return processAliveWindows(pid)
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := p.Signal(syscall.Signal(0)); err != nil {
		return !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH)
	}
	return true
}

// processAliveWindows: signal(0) is not supported on Windows. We rely on
// os.FindProcess — unlike on Unix, it actually opens a handle (OpenProcess)
// and fails when the pid is gone. We immediately release the handle to
// avoid leaking it. As a backstop, when the pid file has been gone for a
// long time we treat the daemon as dead even if a handle still opens.
func processAliveWindows(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Best-effort handle release — on Unix this is a no-op; on Windows it
	// closes the OpenProcess handle we just acquired.
	_ = p.Release()
	if info, err := os.Stat(pidFilePath()); err == nil {
		// 24h after a daemon write is "obviously stale" — Windows pid reuse
		// could in theory return us a handle to an unrelated process.
		if time.Since(info.ModTime()) > 24*time.Hour {
			return false
		}
	}
	return true
}
