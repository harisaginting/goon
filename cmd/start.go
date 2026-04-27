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

	"goon/internal/boards"
	"goon/internal/daemon"
	"goon/internal/executor"
	"goon/internal/githost"
	"goon/internal/llm"
	"goon/internal/memory"
	"goon/internal/safety"
	"goon/internal/tools"
	"goon/internal/web"
)

// runStart launches the autonomous daemon. Blocks until ctx is cancelled.
//
//	goon start [--web=:8080] [--once] [--poll=5m] [--no-pr] [--debug]
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

	// LLM provider.
	prov, err := llm.NewFromEnv()
	if err != nil {
		return fmt.Errorf("llm provider: %w", err)
	}

	// Board (required).
	board, err := boards.NewFromEnv()
	if err != nil {
		return fmt.Errorf("board: %w (set GOON_BOARD=jira|github|mock)", err)
	}

	// Memory.
	mem, err := memory.New(os.Getenv("GOON_MEMORY_PATH"))
	if err != nil {
		return fmt.Errorf("memory: %w", err)
	}

	// Tools — register ask_user with memory bound.
	reg := tools.DefaultRegistry()
	reg.Register(tools.NewAskUser(mem))

	// Executor — daemon always runs in --auto mode (still safety-validated).
	exec := executor.New(executor.Options{
		Mode:      executor.ModeAuto,
		Validator: safety.Default(),
		Stdout:    stdout, Stderr: stderr,
		Stdin: stdin,
	})

	// Optional git host. --no-pr forces nil.
	var host githost.Host
	if !*noPR {
		host, err = githost.NewFromEnv()
		if err != nil && !errors.Is(err, githost.ErrNoHost) {
			return fmt.Errorf("git host: %w", err)
		}
	}

	d := daemon.New(daemon.Options{
		LLM: prov, Tools: reg, Executor: exec,
		Memory: mem, Board: board, Host: host,
		Stdout: stdout, Stderr: stderr,
		Debug:        *debug,
		PollInterval: *pollFlag,
	})

	// Optional web server.
	var srv *web.Server
	if *webAddr != "" {
		srv = web.NewServer(web.Options{
			Addr: *webAddr, Memory: mem, Board: board,
			Daemon: d, Stdout: stdout, Stderr: stderr,
		})
		go func() {
			if err := srv.Start(); err != nil {
				fmt.Fprintf(stderr, "web: %v\n", err)
			}
		}()
		fmt.Fprintf(stdout, "→ web UI at http://%s\n", strings.TrimPrefix(*webAddr, ":"))
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
