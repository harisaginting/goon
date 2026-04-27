package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// pidFilePath returns ~/.goon/goon.pid (or $GOON_PID_FILE).
func pidFilePath() string {
	if v := strings.TrimSpace(os.Getenv("GOON_PID_FILE")); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "/tmp/goon.pid"
	}
	return filepath.Join(home, ".goon", "goon.pid")
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
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Sending signal 0 is a portable liveness check on Unix.
	if err := p.Signal(syscall.Signal(0)); err != nil {
		return !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH)
	}
	return true
}
