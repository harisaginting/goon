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

	"github.com/harisaginting/goon/internal/agent"
	"github.com/harisaginting/goon/internal/executor"
	"github.com/harisaginting/goon/internal/learnings"
	"github.com/harisaginting/goon/internal/llm"
	"github.com/harisaginting/goon/internal/logx"
	"github.com/harisaginting/goon/internal/memory"
	"github.com/harisaginting/goon/internal/notes"
	"github.com/harisaginting/goon/internal/repository"
	"github.com/harisaginting/goon/internal/safety"
	"github.com/harisaginting/goon/internal/skills"
	"github.com/harisaginting/goon/internal/storage"
	"github.com/harisaginting/goon/internal/tools"
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

	// Initialize the structured logger as early as possible so even early
	// failures (missing API keys, bad config) get captured to disk.
	if lg, err := logx.New(logx.Config{}); err == nil {
		logx.SetDefault(lg)
	}
	logx.Info("cli.start", "argv", argv)

	// First-run seeds — idempotent, safe to call on every boot.
	//
	// SOUL.md is the single always-loaded context file (character +
	// project knowledge in one place). SeedSoulTemplate handles two
	// one-shot migrations: legacy PINNED.md → SOUL.md, and the older
	// personal.md → folded into SOUL.md under a "## Character" header
	// (the merge call below). After migration the original
	// personal.md is renamed to personal.md.bak so users can verify.
	if store, err := notes.New(""); err == nil {
		// Order matters: merge the legacy personal.md FIRST so its
		// content lands in a fresh SOUL.md (no duplicate-seed). If
		// SeedSoulTemplate ran first it would write the default
		// template and the merge would then prepend on top, which
		// is fine but produces an awkward stub at the bottom.
		if _, err := store.MergePersonalIntoSoul(storage.Path("personal.md")); err != nil {
			logx.Warn("notes.merge_personal_failed", "error", err.Error())
		}
		if _, err := store.SeedSoulTemplate(); err != nil {
			logx.Warn("notes.seed_failed", "error", err.Error())
		}
	}
	if _, err := repository.SeedDefault(); err != nil {
		logx.Warn("repository.seed_failed", "error", err.Error())
	}
	if err := skills.SeedDefaults(); err != nil {
		logx.Warn("skills.seed_failed", "error", err.Error())
	}

	// Tolerate users typing the program name twice. Common when invoked via
	// `go run . goon workflow init` — the user's mental model is "I'm typing
	// the full command", but `go run .` is already goon, so the leading
	// "goon" becomes argv[0] and shadows the real subcommand. Strip it and
	// drop a one-line hint so they learn the right form.
	if len(argv) > 1 && argv[0] == "goon" {
		fmt.Fprintln(stderr, "hint: you don't need the leading 'goon' — `goon "+strings.Join(argv[1:], " ")+"` is the correct form.")
		argv = argv[1:]
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
		case "doctor":
			return runDoctor(ctx, sargs, stdout, stderr)
		case "workflow":
			return runWorkflow(ctx, sargs, stdout, stderr)
		case "logs":
			return runLogs(ctx, sargs, stdout, stderr)
		case "memory":
			return runMemory(ctx, sargs, stdout, stderr, stdin)
		case "repo":
			return runRepo(ctx, sargs, stdout, stderr)
		case "review-prs":
			return runReviewPRs(ctx, sargs, stdout, stderr)
		case "notifications":
			return runNotifications(ctx, sargs, stdout, stderr)
		case "pause":
			return runPause(ctx, sargs, stdout, stderr)
		case "resume":
			return runResume(ctx, sargs, stdout, stderr)
		case "help":
			printUsage(stdout)
			return nil
		case "version":
			printVersion(stdout)
			return nil
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
		printUsage(stderr)
		fmt.Fprintf(stderr, "\nFlags (for one-shot agent run):\n")
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
		printVersion(stdout)
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
		return onboardingError(err)
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

	runErr := ag.Run(ctx, input)

	// Self-improvement loop. Runs after every one-shot agent invocation
	// regardless of success/failure — HISTORY.md records both, and the
	// distillation pass might learn something even from a failed run.
	//
	// Skipped in --explain mode because the agent never actually ran:
	// nothing happened, nothing to learn. Also skipped when the user
	// aborted via SIGINT (ctx already cancelled), to avoid surprising
	// them with extra LLM calls after Ctrl-C.
	if mode != executor.ModeExplain && ctx.Err() == nil {
		outcome := "ok"
		if runErr != nil {
			outcome = "failed: " + runErr.Error()
		}
		// Use a fresh context with a short cap so a misbehaving
		// distillation can't hang the CLI. Inherit cancellation
		// signals from the parent.
		learnCtx, learnCancel := signalAwareContext(ctx)
		_ = learnings.Capture(learnCtx, learnings.Options{
			Task:     input,
			Outcome:  outcome,
			LLM:      prov,
			Tools:    reg,
			Executor: exec,
			Memory:   mem,
			Stdout:   stdout,
			Stderr:   stderr,
			Debug:    *debugFlag,
		})
		learnCancel()
	}
	return runErr
}

// signalAwareContext returns a child context that inherits the
// parent's signal-based cancellation (so Ctrl-C still works during
// the learning pass) but starts with a fresh deadline-free state.
// Caller MUST call the returned cancel to release resources.
func signalAwareContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithCancel(parent)
}

// printUsage writes goon's command surface in a stable order. Both the
// flag parser's auto-generated help and the explicit `goon help` /
// `goon --help` subcommand share this so there is exactly one source
// of truth for first-run UX.
func printUsage(w io.Writer) {
	fmt.Fprintln(w, "goon — autonomous AI engineer")
	fmt.Fprintln(w, FullVersion())
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, `  goon "<natural-language task>" [flags]              one-shot agent run`)
	fmt.Fprintln(w, "  goon start [--web=:8080] [--once] [--no-pr]         autonomous daemon")
	fmt.Fprintln(w, "  goon stop                                           stop the running daemon")
	fmt.Fprintln(w, "  goon pause | resume                                 toggle the daemon's poll loop")
	fmt.Fprintln(w, "  goon status                                         daemon + queue snapshot")
	fmt.Fprintln(w, "  goon doctor [--json] [--quiet]                      live-probe every provider")
	fmt.Fprintln(w, "  goon train [--list|--all|answer <id> <a>]           answer questions queued by the agent")
	fmt.Fprintln(w, "  goon workflow <show|path|init|edit|hooks>           customize the per-ticket workflow")
	fmt.Fprintln(w, "  goon memory <list|read|write|append|search|edit|delete|path|init>  manage markdown notes")
	fmt.Fprintln(w, "  goon repo <list|forget <project>|clear>             manage learned project→repo mappings")
	fmt.Fprintln(w, "  goon review-prs [--watch] [--telegram] [--all]      draft AI reviews for PRs awaiting you")
	fmt.Fprintln(w, "  goon notifications [--watch] [--telegram] [--all]   forward git review-requests + mentions")
	fmt.Fprintln(w, "  goon logs [--tail=N|--follow|--clear|--path]        browse the structured log file")
	fmt.Fprintln(w, "  goon config <show|get|set|unset|path|edit>          ~/.config/goon/.env")
	fmt.Fprintln(w, "  goon update [<ref>]                                 rebuild from upstream (needs git + go)")
	fmt.Fprintln(w, "  goon uninstall [--yes] [--purge]                    remove the binary (+ optional state)")
	fmt.Fprintln(w, "  goon help | --help | -h                             show this help")
	fmt.Fprintln(w, "  goon version | --version | -v                       show version + build info")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Quick start:")
	fmt.Fprintln(w, "  cp .env.example .env && edit it, then `goon doctor` to verify, then `goon start --web=:8080`")
	fmt.Fprintln(w, "  Full docs at http://localhost:8080/docs once the daemon is running")
}

// splitSubcommand checks if argv starts with a recognized subcommand token
// (a single word, no spaces, no leading "-"). It returns ("", nil) when the
// first arg is a quoted task or a flag, so the agent path keeps working.
//
// "help" / "--help" / "-h" are treated as subcommand-like — they print
// usage and exit. Without this dispatch, `goon help` would be sent to the
// LLM as a literal task, which is a brutal first-run footgun.
func splitSubcommand(argv []string) (string, []string) {
	if len(argv) == 0 {
		return "", nil
	}
	first := argv[0]
	// Help routing — consume `help`, `--help`, `-h`, `-help` here so the
	// flag parser handles them, not the agent.
	switch first {
	case "help", "--help", "-h", "-help":
		return "help", argv[1:]
	case "version", "--version", "-v":
		return "version", argv[1:]
	}
	if first == "" || strings.HasPrefix(first, "-") || strings.ContainsAny(first, " \t") {
		return "", nil
	}
	switch first {
	case "update", "uninstall", "config", "start", "stop", "status", "train", "doctor", "workflow", "logs", "memory", "repo", "pause", "resume", "review-prs", "notifications":
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
		v = stripInlineComment(v)
		v = strings.Trim(v, `"'`)
		if _, exists := os.LookupEnv(k); !exists {
			_ = os.Setenv(k, v)
		}
	}
}

// stripInlineComment removes a trailing "# comment" from a value
// unless the # is inside quotes. Without this, a user copying
//
//	BITBUCKET_API_URL=                # default https://api.bitbucket.org/2.0
//
// verbatim from .env.example ended up with the API URL literally
// being "# default https://api.bitbucket.org/2.0", which then
// produced HTTP 400s with cryptic "unsupported protocol scheme ''"
// errors deep in the host adapter.
//
// Rules:
//   - leading and trailing whitespace already trimmed by the caller
//   - a quoted value (starts with ' or ") is returned untouched —
//     we let the caller strip the quotes
//   - otherwise: the first '#' that is preceded by whitespace OR is
//     at position 0 marks the start of a comment; everything from
//     there to the end is dropped, and the surviving prefix is
//     re-trimmed
func stripInlineComment(v string) string {
	if v == "" {
		return v
	}
	// Quoted values: keep as-is.
	if v[0] == '"' || v[0] == '\'' {
		return v
	}
	// Walk forward looking for a comment marker.
	for i := 0; i < len(v); i++ {
		if v[i] != '#' {
			continue
		}
		// Position 0 OR preceded by whitespace counts as a comment.
		if i == 0 || v[i-1] == ' ' || v[i-1] == '\t' {
			return strings.TrimRight(v[:i], " \t")
		}
	}
	return v
}

// onboardingError wraps an llm.NewFromEnv failure with a copy-pasteable
// hint for new users. Hits on the most common first-run failure: no API
// key set anywhere.
func onboardingError(cause error) error {
	msg := cause.Error()
	hint := ""
	switch {
	case strings.Contains(msg, "OPENAI_API_KEY"):
		hint = "\n\nFirst run? Pick one:\n" +
			"  1. Use OpenAI:    export OPENAI_API_KEY=sk-... (or `goon config set OPENAI_API_KEY sk-...`)\n" +
			"  2. Use Anthropic: export GOON_LLM_PROVIDER=anthropic ANTHROPIC_API_KEY=sk-...\n" +
			"  3. Use a local model via Ollama: export GOON_LLM_PROVIDER=ollama (then `ollama serve`)\n" +
			"  4. Just try it offline: export GOON_LLM_PROVIDER=mock GOON_MOCK_REPLIES='{\"tool\":\"finish\",\"args\":{\"message\":\"hi\"}}'\n" +
			"\nFull config reference: copy .env.example to ~/.config/goon/.env and edit. Run `goon doctor` to verify."
	case strings.Contains(msg, "ANTHROPIC_API_KEY"):
		hint = "\n\nSet it with: `export ANTHROPIC_API_KEY=sk-...` or `goon config set ANTHROPIC_API_KEY sk-...`"
	case strings.Contains(msg, "unknown GOON_LLM_PROVIDER"):
		hint = "\n\nValid values: openai, anthropic, ollama, mock"
	}
	return fmt.Errorf("%w%s", cause, hint)
}
