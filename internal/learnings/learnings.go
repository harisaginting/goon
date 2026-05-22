// Package learnings is the post-run "remember what happened" pass.
//
// Every successful agent.Run (one-shot CLI invocation, daemon workflow
// update_memory phase, or any future surface) should call Capture() at
// the end. Capture does two things:
//
//  1. Appends a single line to HISTORY.md:
//
//     2026-05-20 14:32 · "fix the login redirect" · ok
//
//     This is the running log of past tasks. The agent's system prompt
//     mentions HISTORY.md so the model knows it can consult it before
//     retrying something already attempted.
//
//  2. Optionally fires a short follow-up agent task that asks the LLM
//     "what's worth remembering from this run?" and lets it write to
//     SOUL.md / topic notes via the existing memory_* tools. This is
//     the steady-state knowledge growth loop: every run leaves the
//     system a little smarter about this repo.
//
// Capture is deliberately best-effort. A failure in either step is
// logged but never propagated — losing a learning is preferable to
// blocking the user's actual workflow on a memory-write hiccup.
//
// Opt-out: set GOON_AUTO_LEARN=0 (or "false"/"no"/"off") to disable
// the distillation step. The HISTORY.md line is always written
// because it's a cheap, deterministic local file append.
//
// The mock LLM provider auto-skips the distillation step so tests stay
// fast and hermetic — the mock would otherwise consume its scripted
// replies on the learnings pass, breaking the test's primary assertion.
package learnings

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/harisaginting/goon/internal/agent"
	"github.com/harisaginting/goon/internal/executor"
	"github.com/harisaginting/goon/internal/llm"
	"github.com/harisaginting/goon/internal/logx"
	"github.com/harisaginting/goon/internal/memory"
	"github.com/harisaginting/goon/internal/notes"
	"github.com/harisaginting/goon/internal/tools"
)

// HistoryFilename is the canonical name of the running task log. Kept
// exported so CLI surfaces (`goon memory read HISTORY.md`) and the web
// UI can reference it consistently.
const HistoryFilename = "HISTORY.md"

// Options groups everything Capture needs. All fields are optional;
// missing dependencies degrade Capture's behaviour rather than erroring.
//
//   - Task is the user-facing description of what the agent was asked
//     to do. Trimmed and truncated to ~200 chars before logging.
//   - Outcome is a short human-readable status ("ok", "failed: X", etc).
//     Empty defaults to "ok".
//   - LLM/Tools/Executor/Memory are the same dependencies agent.New
//     takes. When any of them is nil the distillation step is skipped
//     and only the HISTORY.md append runs.
//   - Stdout/Stderr are passed through to the distillation agent so
//     its tool calls print to the same place as the main run. Both
//     default to io.Discard when nil to avoid nil-pointer panics.
type Options struct {
	Task     string
	Outcome  string
	LLM      llm.Provider
	Tools    *tools.Registry
	Executor *executor.Executor
	Memory   *memory.Memory
	Stdout   io.Writer
	Stderr   io.Writer
	Debug    bool
}

// Capture runs the post-run learning pass: appends a HISTORY.md line,
// then (when enabled) fires a short agent task asking the LLM to
// distil durable knowledge into the notes store. Always returns nil;
// failures are logged, never propagated.
func Capture(ctx context.Context, opts Options) error {
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}

	// Step 1 — append to HISTORY.md. Cheap and always runs (even when
	// the LLM isn't configured), because the line is the user's record
	// of what goon has done lately, independent of any model.
	appendHistory(opts.Task, opts.Outcome)

	// Step 2 — short distillation agent. Skip when:
	//   - the user opted out via GOON_AUTO_LEARN=0
	//   - the LLM/tools/executor stack isn't wired up
	//   - the provider is the mock (tests don't want a second LLM call
	//     consuming their scripted replies)
	if !autoLearnEnabled() {
		return nil
	}
	if opts.LLM == nil || opts.Tools == nil || opts.Executor == nil {
		return nil
	}
	if opts.LLM.Name() == "mock" {
		return nil
	}

	a := agent.New(agent.Options{
		LLM:      opts.LLM,
		Tools:    opts.Tools,
		Executor: opts.Executor,
		Memory:   opts.Memory,
		Stdout:   opts.Stdout,
		Stderr:   opts.Stderr,
		Debug:    opts.Debug,
	})
	task := buildDistillPrompt(opts.Task, opts.Outcome)
	if err := a.Run(ctx, task); err != nil {
		// Best-effort: log but don't surface. The user's primary task
		// has already succeeded by the time we reach Capture.
		logx.Warn("learnings.distill_failed", "error", err.Error())
		fmt.Fprintf(opts.Stderr, "[learn] distillation failed (non-fatal): %v\n", err)
	}
	return nil
}

// autoLearnEnabled returns true unless GOON_AUTO_LEARN is set to a
// recognised "off" value. Default-on so users get the self-improvement
// loop without configuration.
func autoLearnEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GOON_AUTO_LEARN"))) {
	case "0", "false", "no", "off", "n":
		return false
	}
	return true
}

// appendHistory writes one line to HISTORY.md in the notes store.
// Format: "YYYY-MM-DD HH:MM · <task> · <outcome>" — picked so a human
// can grep, sort, and read it at a glance. Single line per run.
//
// Failures are swallowed and logged. We never want a missing notes
// dir or a transient ENOSPC to block the user's main task.
func appendHistory(task, outcome string) {
	store, err := notes.New("")
	if err != nil {
		logx.Warn("learnings.notes_open_failed", "error", err.Error())
		return
	}
	stamp := time.Now().Format("2006-01-02 15:04")
	t := strings.TrimSpace(task)
	if t == "" {
		t = "(empty task)"
	}
	t = truncate(singleLine(t), 200)
	o := strings.TrimSpace(outcome)
	if o == "" {
		o = "ok"
	}
	o = truncate(singleLine(o), 200)
	line := fmt.Sprintf("%s · %s · %s\n", stamp, t, o)
	if err := store.Append(HistoryFilename, line); err != nil {
		logx.Warn("learnings.history_append_failed", "error", err.Error())
	}
}

// buildDistillPrompt is the prompt the distillation agent sees. Kept
// in one place so the daemon workflow and the one-shot path use the
// exact same wording — easier to tune later.
func buildDistillPrompt(task, outcome string) string {
	return fmt.Sprintf(`Reflect on what you just did:
  TASK: %s
  OUTCOME: %s

If — and ONLY if — you learned something durable that would help on a
future task in this repo, persist it via memory_append (preferred) or
memory_write. Examples worth saving:
  - conventions discovered (build cmds, naming, layout)
  - file/path patterns that matter
  - non-obvious gotchas, bugs avoided
  - API / CLI quirks worth remembering
  - decisions the user explicitly made

Rules:
  - One topic per .md file, kebab-case filename.
  - Broadly invariant things (whole-repo rules) go in SOUL.md.
  - Skip notes that are trivial or only apply to this single task.
  - Do NOT re-record what's already in SOUL.md / HISTORY.md / topic
    notes — read first if unsure.
  - HISTORY.md is auto-managed by goon; never write to it directly.

Call finish when done. If nothing's worth saving, finish immediately
with "no durable learnings from this run".`,
		truncate(singleLine(task), 400),
		truncate(singleLine(outcome), 200))
}

func singleLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
