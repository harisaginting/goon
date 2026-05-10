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
	"github.com/harisaginting/goon/internal/telegram"
	"github.com/harisaginting/goon/internal/tools"
	"github.com/harisaginting/goon/internal/web"
	"github.com/harisaginting/goon/internal/workflow"
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

	// Announce which workflow is in use BEFORE anything else, so the very
	// first line of the daemon's output identifies the active pipeline.
	// The same info is mirrored to the log file via logx.
	workflow.Announce("", stdout)

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
			// recover() so a panic deep inside an htmx handler can never
			// crash the daemon — the web UI is convenience, the daemon is
			// the load-bearing piece. Panic still surfaces in stderr so
			// the operator notices.
			defer func() {
				if r := recover(); r != nil {
					fmt.Fprintf(stderr, "web: panic: %v\n", r)
				}
			}()
			if err := srv.Start(); err != nil {
				fmt.Fprintf(stderr, "web: %v\n", err)
			}
		}()
		fmt.Fprintf(stdout, "→ web UI at http://%s — open it to configure goon\n",
			strings.TrimPrefix(*webAddr, ":"))
	}

	// Optional Telegram bot. Auto-starts when both the bot token and the
	// shared secret are present in the env. Snapshot the daemon's current
	// providers so /run and PR review have something to talk to. Reconfig
	// after this point requires a `goon stop && goon start` to pick up.
	botCancel := startTelegramBot(ctx, d, reg, exec, mem, stdout, stderr, *debug)
	if botCancel != nil {
		defer botCancel()
	}

	if err := writePIDFile(pidPath); err != nil {
		fmt.Fprintf(stderr, "warning: cannot write pid file %s: %v\n", pidPath, err)
	} else {
		defer removePIDFile(pidPath)
	}

	// Stop the web server on every exit path — including --once. Previously
	// this defer sat after the *once branch, leaking the listener goroutine
	// until process exit. The Telegram bot has the same shape (botCancel
	// deferred earlier, well before *once).
	if srv != nil {
		defer srv.Stop()
	}

	if *once {
		oneShot, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		return d.RunOnce(oneShot)
	}
	if err := d.Run(ctx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

// startTelegramBot spawns the inbound Telegram bot in a goroutine when both
// TELEGRAM_BOT_TOKEN and GOON_TELEGRAM_SECRET are set. Returns a cancel func
// that stops the bot (or nil when the bot was not started). Failures during
// New() are logged but do not block daemon startup — the user can fix env
// and restart.
func startTelegramBot(parent context.Context, d *daemon.Daemon,
	reg *tools.Registry, exec *executor.Executor, mem *memory.Memory,
	stdout, stderr io.Writer, debug bool) context.CancelFunc {
	token := strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
	secret := strings.TrimSpace(os.Getenv("GOON_TELEGRAM_SECRET"))
	if token == "" || secret == "" {
		if token != "" || secret != "" {
			fmt.Fprintln(stderr, "telegram bot: need BOTH TELEGRAM_BOT_TOKEN and GOON_TELEGRAM_SECRET — bot disabled")
		}
		return nil
	}
	llmProv, _, host := d.Snapshot()
	bot, err := telegram.New(telegram.Options{
		Token:    token,
		Secret:   secret,
		Memory:   mem,
		LLM:      llmProv,
		Tools:    reg,
		Executor: exec,
		Host:     host,
		Stdout:   stdout,
		Stderr:   stderr,
		Debug:    debug,
	})
	if err != nil {
		fmt.Fprintf(stderr, "telegram bot: %v\n", err)
		return nil
	}
	botCtx, cancel := context.WithCancel(parent)
	go func() {
		// Same recover() shape as the web goroutine — a malformed
		// Telegram update or a model panic during a /run task should
		// never take the whole daemon down with it.
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(stderr, "telegram bot: panic: %v\n", r)
			}
		}()
		if err := bot.Start(botCtx); err != nil &&
			!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			fmt.Fprintf(stderr, "telegram bot: %v\n", err)
		}
	}()
	return cancel
}
