package cmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/harisaginting/goon/internal/daemon"
	"github.com/harisaginting/goon/internal/executor"
	"github.com/harisaginting/goon/internal/memory"
	"github.com/harisaginting/goon/internal/safety"
	"github.com/harisaginting/goon/internal/tools"
	"github.com/harisaginting/goon/internal/web"
)

// runStart launches the autonomous daemon. Blocks until ctx is cancelled.
//
//	goon start [--web=:8080] [--once] [--poll=5m] [--no-pr] [--debug]
//
// The daemon now starts even when LLM / board / git host config is missing,
// so the user can configure goon entirely from the web UI. Polling stays
// idle ("waiting for config") until the daemon's hot-reload picks up a
// valid configuration.
func runStart(ctx context.Context, args []string, stdout, stderr io.Writer, stdin io.Reader) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.SetOutput(stderr)
	webAddr := fs.String("web", "", `serve the web UI on this address (e.g. ":8080"); empty = disabled`)
	once := fs.Bool("once", false, "run a single poll cycle and exit (useful for cron)")
	pollFlag := fs.Duration("poll", 0, "override poll interval (e.g. 30s, 5m)")
	noPR := fs.Bool("no-pr", false, "do not open PRs (useful while testing)")
	debug := fs.Bool("debug", false, "verbose diagnostic output")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: goon start [--web=:8080] [--once] [--poll=5m] [--no-pr] [--debug]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Already running?
	pidPath := pidFilePath()
	if pid, err := readPIDFile(pidPath); err == nil && processAlive(pid) {
		return fmt.Errorf("goon already running (pid %d). Use 'goon stop' first", pid)
	}

	// Memory is the only thing genuinely required — the web UI and the CLI
	// both read+write here, and the daemon parks its status here.
	mem, err := memory.New(os.Getenv("GOON_MEMORY_PATH"))
	if err != nil {
		return fmt.Errorf("memory: %w", err)
	}

	// Tools registry. ask_user is bound to memory so the agent can queue
	// questions when the daemon is running headless.
	reg := tools.DefaultRegistry()
	reg.Register(tools.NewAskUser(mem))

	// Executor — daemon always runs in --auto mode (still safety-validated).
	exec := executor.New(executor.Options{
		Mode:      executor.ModeAuto,
		Validator: safety.Default(),
		Stdout:    stdout, Stderr: stderr,
		Stdin: stdin,
	})

	d := daemon.New(daemon.Options{
		Tools: reg, Executor: exec, Memory: mem,
		Stdout: stdout, Stderr: stderr,
		Debug:        *debug,
		PollInterval: *pollFlag,
		PRDisabled:   *noPR,
	})
	// First reconfigure pass: read current env, build providers if possible.
	d.Reconfigure()

	// Optional web server. The web UI is the primary onboarding surface,
	// so we always pass it the daemon — POST /api/config calls Reconfigure().
	var srv *web.Server
	if *webAddr != "" {
		srv = web.NewServer(web.Options{
			Addr: *webAddr, Memory: mem,
			Daemon: d, Stdout: stdout, Stderr: stderr,
		})
		go func() {
			if err := srv.Start(); err != nil {
				fmt.Fprintf(stderr, "web: %v\n", err)
			}
		}()
		fmt.Fprintf(stdout, "→ web UI at http://%s — open it to configure goon\n",
			strings.TrimPrefix(*webAddr, ":"))
	}

	if err := writePIDFile(pidPath); err != nil {
		fmt.Fprintf(stderr, "warning: cannot write pid file %s: %v\n", pidPath, err)
	} else {
		defer removePIDFile(pidPath)
	}

	if *once {
		oneShot, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		return d.RunOnce(oneShot)
	}

	if srv != nil {
		defer srv.Stop()
	}
	if err := d.Run(ctx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}
