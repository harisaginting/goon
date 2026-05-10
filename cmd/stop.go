package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"syscall"
	"time"
)

// runStop asks the daemon recorded in the pid file to exit gracefully and
// waits up to ~5 seconds. On POSIX we send SIGTERM; on Windows there is no
// SIGTERM, so we send os.Interrupt (which Go's runtime translates into a
// CTRL_BREAK_EVENT for the target console). If the process is still alive
// after the grace period we hard-kill in both cases.
func runStop(_ context.Context, _ []string, stdout, stderr io.Writer) error {
	pidPath := pidFilePath()
	pid, err := readPIDFile(pidPath)
	if err != nil {
		// No pid file = no daemon running. Treat as success, not an error.
		if os.IsNotExist(err) {
			fmt.Fprintln(stdout, "goon: no daemon running")
			return nil
		}
		return fmt.Errorf("stop: %w", err)
	}
	if !processAlive(pid) {
		fmt.Fprintf(stdout, "goon: pid %d not running; cleaning up pid file\n", pid)
		removePIDFile(pidPath)
		return nil
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := p.Signal(stopSignal()); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			removePIDFile(pidPath)
			return nil
		}
		return err
	}
	for i := 0; i < 50; i++ {
		if !processAlive(pid) {
			fmt.Fprintf(stdout, "goon: stopped pid %d\n", pid)
			removePIDFile(pidPath)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Fprintf(stderr, "goon: pid %d did not exit within 5s; sending SIGKILL\n", pid)
	_ = p.Kill()
	removePIDFile(pidPath)
	return nil
}

// stopSignal returns the platform-appropriate "graceful stop" signal.
// On POSIX: SIGTERM. On Windows: os.Interrupt — sending syscall.SIGTERM via
// os.Process.Signal on Windows returns "not supported", whereas
// os.Interrupt is honoured (translated to CTRL_BREAK_EVENT).
func stopSignal() os.Signal {
	if runtime.GOOS == "windows" {
		return os.Interrupt
	}
	return syscall.SIGTERM
}
