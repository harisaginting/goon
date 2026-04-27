// Package cmd wires command-line flags into the agent runtime.
//
// Modes (mutually exclusive, default is dry-run):
//
//	(none)     dry-run: print what would happen, never execute
//	--run      execute, but ask for "y/n" confirmation per step
//	--auto     execute without prompting (still safety-validated)
//	--explain  plan only: produce a step-by-step explanation, no tool calls
package cmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"goon/internal/agent"
	"goon/internal/executor"
	"goon/internal/llm"
	"goon/internal/memory"
	"goon/internal/safety"
	"goon/internal/tools"
)

// Execute is the entry point used by main.go.
func Execute() error {
	return run(os.Args[1:], os.Stdout, os.Stderr, os.Stdin)
}

func run(argv []string, stdout, stderr io.Writer, stdin io.Reader) error {
	// Load .env from multiple candidate locations, first match wins per key.
	// CWD takes precedence so a project-local .env can override globals.
	loadDotEnv(".env")
	if home, err := os.UserHomeDir(); err == nil {
		loadDotEnv(home + "/.config/goon/.env")
		loadDotEnv(home + "/.goon/.env")
	}

	// Subcommand dispatch — runs BEFORE flag parsing so the LLM agent isn't
	// invoked for built-in operations like self-update.
	if sub, sargs := splitSubcommand(argv); sub != "" {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		switch sub {
		case "update":
			return runUpdate(ctx, sargs, stdout, stderr)
		case "uninstall":
			return runUninstall(ctx, sargs, stdout, stderr, stdin)
		case "config":
			return runConfig(ctx, sargs, stdout, stderr)
		case "start":
			return runStart(ctx, sargs, stdout, stderr, stdin)
		case "stop":
			return runStop(ctx, sargs, stdout, stderr)
		case "status":
			return runStatus(ctx, sargs, stdout, stderr)
		case "train":
			return runTrain(ctx, sargs, stdout, stderr, stdin)
		}
	}

	fs := flag.NewFlagSet("goon", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		runFlag     = fs.Bool("run", false, "execute commands with per-step confirmation")
		autoFlag    = fs.Bool("auto", false, "execute commands without prompting (still validated)")
		explainFlag = fs.Bool("explain", false, "produce a plan only — no tool calls or execution")
		debugFlag   = fs.Bool("debug", false, "print verbose debug info")
		versionFlag = fs.Bool("version", false, "print version and exit")
	)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage:\n")
		fmt.Fprintf(stderr, "  goon \"<natural-language task>\" [flags]    one-shot agent run\n")
		fmt.Fprintf(stderr, "  goon start [--web=:8080] [--once]         autonomous daemon (poll → triage → plan → exec → verify → PR → notify)\n")
		fmt.Fprintf(stderr, "  goon stop                                 stop the running daemon\n")
		fmt.Fprintf(stderr, "  goon status                               daemon status, pending questions, recent workflows\n")
		fmt.Fprintf(stderr, "  goon train [--list|--all|answer <id> <a>] answer questions queued by the agent\n")
		fmt.Fprintf(stderr, "  goon update [<ref>]                       self-update from upstream master, branch, tag, or commit\n")
		fmt.Fprintf(stderr, "  goon uninstall [--yes] [--purge]          remove the binary (and optionally state)\n")
		fmt.Fprintf(stderr, "  goon config <action> [args]               show / get / set / unset / path / edit\n\n")
		fmt.Fprintf(stderr, "Flags (for one-shot agent run):\n")
		fs.PrintDefaults()
	}

	// Reorder argv so flags can appear before OR after the positional task.
	// Go's flag package stops parsing at the first non-flag, so we hoist
	// every -flag / --flag to the front before calling Parse.
	flags, positional := splitFlagsAndPositional(argv)
	if err := fs.Parse(append(flags, positional...)); err != nil {
		return err
	}

	if *versionFlag {
		fmt.Fprintln(stdout, "goon 0.1.0")
		return nil
	}

	input := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if input == "" {
		fs.Usage()
		return errors.New("missing task: provide a natural-language description")
	}

	mode := executor.ModeDryRun
	switch {
	case *explainFlag:
		mode = executor.ModeExplain
	case *autoFlag:
		mode = executor.ModeAuto
	case *runFlag:
		mode = executor.ModeRun
	}
	if *debugFlag {
		fmt.Fprintf(stderr, "[debug] input=%q mode=%s\n", input, mode)
	}

	// Build provider.
	prov, err := llm.NewFromEnv()
	if err != nil {
		return fmt.Errorf("llm provider: %w", err)
	}

	// Build tool registry.
	reg := tools.DefaultRegistry()

	// Memory store.
	mem, err := memory.New(os.Getenv("GOON_MEMORY_PATH"))
	if err != nil {
		fmt.Fprintf(stderr, "[warn] memory disabled: %v\n", err)
		mem = memory.Disabled()
	}

	// Safety validator.
	val := safety.Default()

	// Executor wires mode + safety + confirmation prompt.
	exec := executor.New(executor.Options{
		Mode:      mode,
		Validator: val,
		Stdin:     stdin,
		Stdout:    stdout,
		Stderr:    stderr,
	})

	// Build agent.
	ag := agent.New(agent.Options{
		LLM:      prov,
		Tools:    reg,
		Executor: exec,
		Memory:   mem,
		Stdout:   stdout,
		Stderr:   stderr,
		Debug:    *debugFlag,
	})

	// Cancel cleanly on SIGINT.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	return ag.Run(ctx, input)
}

// splitSubcommand checks if argv starts with a recognized subcommand token
// (a single word, no spaces, no leading "-"). It returns ("", nil) when the
// first arg is a quoted task or a flag, so the agent path keeps working.
func splitSubcommand(argv []string) (string, []string) {
	if len(argv) == 0 {
		return "", nil
	}
	first := argv[0]
	if first == "" || strings.HasPrefix(first, "-") || strings.ContainsAny(first, " \t") {
		return "", nil
	}
	switch first {
	case "update", "uninstall", "config", "start", "stop", "status", "train":
		return first, argv[1:]
	}
	return "", nil
}

// splitFlagsAndPositional partitions argv so all -flag / --flag tokens come
// first. A bare "--" terminator stops flag detection.
func splitFlagsAndPositional(argv []string) (flags, positional []string) {
	stopped := false
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if stopped {
			positional = append(positional, a)
			continue
		}
		if a == "--" {
			stopped = true
			continue
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			flags = append(flags, a)
			// If this flag expects a value (no '='), pull the next token too.
			// All our flags are bool, so nothing to pull. Defensive for future
			// non-bool flags: only pull if next token doesn't start with '-'.
			if !strings.Contains(a, "=") && i+1 < len(argv) {
				next := argv[i+1]
				if !strings.HasPrefix(next, "-") && isKnownNonBoolFlag(strings.TrimLeft(a, "-")) {
					flags = append(flags, next)
					i++
				}
			}
			continue
		}
		positional = append(positional, a)
	}
	return
}

// isKnownNonBoolFlag is a small allowlist for flags that take a value.
// All current flags are bool, so this returns false. Kept for future use.
func isKnownNonBoolFlag(string) bool { return false }

// loadDotEnv loads "KEY=VALUE" pairs from path into os.Environ if not already set.
// It is intentionally tiny — no quoting, no export. Comments start with '#'.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 1024)
	for {
		n, err := f.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			break
		}
	}
	for _, line := range strings.Split(string(buf), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		k := strings.TrimSpace(line[:eq])
		v := strings.TrimSpace(line[eq+1:])
		v = strings.Trim(v, `"'`)
		if _, exists := os.LookupEnv(k); !exists {
			_ = os.Setenv(k, v)
		}
	}
}
