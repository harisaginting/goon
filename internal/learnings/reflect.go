package learnings

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/harisaginting/goon/internal/agent"
	"github.com/harisaginting/goon/internal/executor"
	"github.com/harisaginting/goon/internal/llm"
	"github.com/harisaginting/goon/internal/logx"
	"github.com/harisaginting/goon/internal/memory"
	"github.com/harisaginting/goon/internal/notes"
	"github.com/harisaginting/goon/internal/tools"
	"github.com/harisaginting/goon/internal/usage"
)

// LearnedFilename is goon's self-learning notebook (re-exported from the notes
// package so callers that already import learnings don't need a second import).
const LearnedFilename = notes.LearnedFilename

// defaultReflectInterval is how often standby reflection runs by default.
// Overridable via GOON_LEARN_INTERVAL_HOURS.
const defaultReflectInterval = 24 * time.Hour

// ReflectInterval is the minimum gap between standby self-learning passes.
// Default 24h (once per idle day); override with GOON_LEARN_INTERVAL_HOURS
// (a positive integer number of hours). Invalid/empty values fall back to
// the default.
func ReflectInterval() time.Duration {
	if v := strings.TrimSpace(os.Getenv("GOON_LEARN_INTERVAL_HOURS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Hour
		}
	}
	return defaultReflectInterval
}

// ReflectEnabled mirrors the post-run distillation toggle: standby reflection
// is on unless GOON_AUTO_LEARN is set to an "off" value.
func ReflectEnabled() bool { return autoLearnEnabled() }

// ReflectOptions groups the dependencies the standby reflection agent needs.
// They mirror agent.Options; when LLM/Tools/Executor is nil Reflect is a
// no-op (there's nothing to run).
type ReflectOptions struct {
	LLM      llm.Provider
	Tools    *tools.Registry
	Executor *executor.Executor
	Memory   *memory.Memory
	Stdout   io.Writer
	Stderr   io.Writer
	Debug    bool

	// OpenLearningQuestions is how many learning questions are already
	// awaiting the user. Passed into the prompt so reflection doesn't pile
	// up unanswered questions.
	OpenLearningQuestions int
}

// Reflect runs goon's daily standby self-learning pass: it reviews what
// changed recently (git history, HISTORY.md, existing notes), writes durable
// findings to LEARNED.md, and asks the user (via the ask_user tool, kind
// "learning") about anything it can't figure out on its own. Best-effort:
// failures are logged, never returned, so a flaky reflection never disturbs
// the daemon's poll loop.
//
// The caller is responsible for throttling (see ReflectInterval) and for
// recording the run timestamp via Memory.SetLastReflect.
func Reflect(ctx context.Context, opts ReflectOptions) error {
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}
	if !ReflectEnabled() {
		return nil
	}
	if opts.LLM == nil || opts.Tools == nil || opts.Executor == nil {
		return nil
	}
	// The mock provider would consume scripted replies; keep tests hermetic.
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
	ctx = usage.WithLabel(ctx, "standby self-learning")
	if err := a.Run(ctx, buildReflectPrompt(opts.OpenLearningQuestions)); err != nil {
		logx.Warn("learnings.reflect_failed", "error", err.Error())
		fmt.Fprintf(opts.Stderr, "[learn] standby reflection failed (non-fatal): %v\n", err)
	}
	return nil
}

// buildReflectPrompt is the standby-reflection task. Kept in one place so the
// daemon and any future surface use identical wording.
func buildReflectPrompt(openQuestions int) string {
	askBudget := "You may ask the user a few questions if needed."
	if openQuestions > 0 {
		askBudget = fmt.Sprintf(
			"There are already %d learning question(s) awaiting the user — only ask something new if it's genuinely important; don't pile up.",
			openQuestions)
	}
	return fmt.Sprintf(`You are goon, reflecting on standby (no ticket is in progress). Goal:
keep learning about THIS project and about yourself, and record durable
knowledge so future runs are smarter.

Do this, briefly and concretely:
  1. Review what changed recently. Use run_command for read-only git, e.g.
     "git log --oneline -20", "git log --stat -5", "git diff HEAD~5 --stat".
     Also read HISTORY.md (your own task log) via memory_read.
  2. Read your existing notes first so you don't duplicate: memory_read SOUL.md,
     memory_read %[1]s, and memory_list for topic notes.
  3. Write NEW durable findings to %[1]s with memory_append — one tight bullet
     per insight. Good material:
       - conventions / patterns you noticed in recent changes
       - what the project is trending toward; recurring kinds of work
       - gotchas, mistakes to avoid, decisions the user made
       - things about how you (goon) should behave on this repo
     Skip anything already captured. Quality over quantity — a few sharp
     bullets beat a wall of text. Do NOT write to HISTORY.md (auto-managed).
  4. If something about the project or the user's intent is unclear and an
     answer would make you meaningfully better, call ask_user with
     kind="learning" (no ticket). %[2]s

When done, call finish with a one-line summary of what you learned (or
"nothing new to learn" if the repo is quiet).`, LearnedFilename, askBudget)
}
