package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"
)

// runStop sends SIGTERM to the daemon recorded in the pid file and waits up
// to ~5 seconds for it to exit.
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
	if err := p.Signal(syscall.SIGTERM); err != nil {
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
