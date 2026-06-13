// Package daemon is goon's background autonomous loop.
//
// It polls the configured Board, picks the next Open ticket, and feeds it to
// the workflow engine. Memory acts as the source of truth for status,
// tickets, workflows, and questions — both CLI (`goon status`) and the web UI
// read from there.
//
// The daemon tolerates missing/incomplete configuration: when no LLM
// provider or no Board is set up, it logs a "waiting for config" line each
// tick instead of crashing. Reconfigure() rebuilds providers from current
// env (loaded by the web UI's POST /api/config), so the user can drive the
// whole onboarding from the browser.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/harisaginting/goon/internal/boards"
	"github.com/harisaginting/goon/internal/executor"
	"github.com/harisaginting/goon/internal/githost"
	"github.com/harisaginting/goon/internal/learnings"
	"github.com/harisaginting/goon/internal/llm"
	"github.com/harisaginting/goon/internal/logx"
	"github.com/harisaginting/goon/internal/memory"
	"github.com/harisaginting/goon/internal/tools"
	"github.com/harisaginting/goon/internal/usage"
	"github.com/harisaginting/goon/internal/workflow"
)

// PollInterval is how often the daemon checks the board for new tickets.
// Defaults to 5 minutes; override with GOON_POLL_SECONDS.
func PollInterval() time.Duration {
	if v := os.Getenv("GOON_POLL_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 5 * time.Minute
}

// Options bundles the daemon's dependencies.
type Options struct {
	// LLM, Board, Host are optional. Reconfigure() rebuilds them from env.
	LLM      llm.Provider
	Board    boards.Board
	Host     githost.Host
	Tools    *tools.Registry
	Executor *executor.Executor
	Memory   *memory.Memory
	Stdout   io.Writer
	Stderr   io.Writer
	Debug    bool

	// PollInterval overrides the env var — used in tests.
	PollInterval time.Duration
	// VerifyRunsOverride passes through to workflow.Engine. 0 = use env default.
	VerifyRunsOverride int
	// PRDisabled — when true, never construct a git host. Used for --no-pr.
	PRDisabled bool
}

// Daemon is the long-running loop.
type Daemon struct {
	opts  Options
	mu    sync.Mutex // serializes pollAndRun and Reconfigure
	rcMu  sync.RWMutex
	llm   llm.Provider
	board boards.Board
	host  githost.Host
	// wakeCh nudges Run() to call pollAndRun immediately instead of
	// waiting for the next ticker. Buffer=1 with select-default in
	// Wake() means we coalesce bursts without blocking the sender.
	wakeCh chan struct{}
	// runNowCh carries an automation name to run immediately (the web
	// "run now" button), handled in Run()'s select like wakeCh. Buffered
	// so the sender never blocks.
	runNowCh chan string
}

// New wires a Daemon. LLM / Board / Host may be nil — Reconfigure can fill
// them in later from environment variables.
func New(opts Options) *Daemon {
	if opts.PollInterval == 0 {
		opts.PollInterval = PollInterval()
	}
	return &Daemon{
		opts:   opts,
		llm:    opts.LLM,
		board:  opts.Board,
		host:   opts.Host,
		wakeCh:   make(chan struct{}, 1),
		runNowCh: make(chan string, 8),
	}
}

// Wake nudges the daemon to run pollAndRun on the next loop iteration
// instead of waiting for the poll interval ticker. Used by the web /
// Telegram answer handlers so a workflow paused at an approval gate
// resumes within a second of the user replying, not within minutes.
// Safe to call from any goroutine; bursts coalesce into one wake.
func (d *Daemon) Wake() {
	if d == nil || d.wakeCh == nil {
		return
	}
	select {
	case d.wakeCh <- struct{}{}:
	default:
		// already a pending wake — fine, the next tick handles it
	}
}

// RunAutomationNow asks the loop to run one automation by name immediately,
// regardless of its schedule. Non-blocking; drops the request if the buffer is
// full (the user can click again). Safe to call from any goroutine.
func (d *Daemon) RunAutomationNow(name string) {
	if d == nil || d.runNowCh == nil || strings.TrimSpace(name) == "" {
		return
	}
	select {
	case d.runNowCh <- name:
	default:
		// buffer full — a batch of run-now requests is already queued
	}
}

// automationEngine builds an Engine for scheduled / manual automation runs, or
// returns ok=false when no LLM provider is configured (automations run an agent
// and need one). Shared by scheduleTick and runAutomationByName.
func (d *Daemon) automationEngine() (*workflow.Engine, bool) {
	prov, board, host := d.Snapshot()
	if prov == nil {
		return nil, false
	}
	return &workflow.Engine{
		LLM: prov, Tools: d.opts.Tools, Executor: d.opts.Executor,
		Memory: d.opts.Memory, Board: board, Host: host,
		Stdout: d.opts.Stdout, Stderr: d.opts.Stderr, Debug: d.opts.Debug,
		VerifyRunsOverride: d.opts.VerifyRunsOverride,
	}, true
}

// runAutomationByName runs a single automation now (web "run now"). Unlike the
// scheduler it ignores Due() and the enabled flag — an explicit click means
// "run it" — but it still respects pause and runs under the same lock as
// pollAndRun/scheduleTick so engine runs never overlap.
func (d *Daemon) runAutomationByName(ctx context.Context, name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.opts.Memory == nil {
		return
	}
	if st := d.opts.Memory.GetStatus(); st.Paused {
		fmt.Fprintf(d.opts.Stderr, "[schedule] run-now %q skipped — daemon paused\n", name)
		return
	}
	var cfg workflow.WorkflowConfig
	found := false
	for _, a := range workflow.LoadAutomations() {
		if a.Config.Name == name || workflow.AutomationSlug(a.Config.Name) == workflow.AutomationSlug(name) {
			cfg = a.Config
			found = true
			break
		}
	}
	if !found {
		fmt.Fprintf(d.opts.Stderr, "[schedule] run-now: automation %q not found\n", name)
		return
	}
	eng, ok := d.automationEngine()
	if !ok {
		fmt.Fprintf(d.opts.Stderr, "[schedule] run-now %q skipped — no LLM provider\n", name)
		return
	}
	d.opts.Memory.MarkScheduledRun(cfg.Name, time.Now())
	fmt.Fprintf(d.opts.Stdout, "[schedule] running automation %q now (manual)\n", cfg.Name)
	if _, err := eng.RunJob(ctx, cfg); err != nil {
		fmt.Fprintf(d.opts.Stderr, "[schedule] automation %q failed: %v\n", cfg.Name, err)
	}
}

// Circuit-breaker tuning. Kept conservative: a handful of failures
// before self-pausing, backoff that starts gentle and caps well under an
// hour so a recovered provider is retried reasonably soon.
const (
	maxConsecutiveFails = 5
	baseBackoff         = 30 * time.Second
	maxBackoff          = 15 * time.Minute
)

// reconcileMaxList guards the post-poll ticket reconciliation: if a board
// returns at least this many tickets the page may be truncated (the Jira
// adapter caps at 50), so we skip reconciling to avoid dropping matches
// that simply didn't fit on the first page.
const reconcileMaxList = 50

// backoffDelay returns the wait before the next retry given how many
// consecutive failures we've seen: exponential (base·2^(n-1)) capped at
// maxBackoff. fails<=0 yields 0.
func backoffDelay(fails int) time.Duration {
	if fails <= 0 {
		return 0
	}
	d := baseBackoff
	for i := 1; i < fails; i++ {
		d *= 2
		if d >= maxBackoff {
			return maxBackoff
		}
	}
	if d > maxBackoff {
		d = maxBackoff
	}
	return d
}

// classifyProviderError buckets an error into a human class and reports
// whether it's *infrastructural* — i.e. a provider/board/network problem
// that should trip the circuit breaker. Code/test failures of a single
// ticket are NOT infrastructural and must not pause the whole daemon.
func classifyProviderError(err error) (class string, infra bool) {
	if err == nil {
		return "", false
	}
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "connection refused"), strings.Contains(s, "no such host"),
		strings.Contains(s, "dial tcp"), strings.Contains(s, "i/o timeout"),
		strings.Contains(s, "timeout"), strings.Contains(s, "connection reset"),
		strings.Contains(s, "eof"), strings.Contains(s, "network is unreachable"):
		return "network", true
	case strings.Contains(s, "401"), strings.Contains(s, "unauthorized"),
		strings.Contains(s, "invalid api key"), strings.Contains(s, "invalid_api_key"),
		strings.Contains(s, "403"), strings.Contains(s, "forbidden"),
		strings.Contains(s, "authentication"):
		return "auth", true
	case strings.Contains(s, "429"), strings.Contains(s, "rate limit"),
		strings.Contains(s, "rate_limit"), strings.Contains(s, "quota"),
		strings.Contains(s, "insufficient_quota"):
		return "rate_limit", true
	case strings.Contains(s, "500"), strings.Contains(s, "502"),
		strings.Contains(s, "503"), strings.Contains(s, "overloaded"),
		strings.Contains(s, "service unavailable"):
		return "model", true
	}
	return "other", false
}

// errorClassHint maps a class to a plain-English fix the UI can show.
func errorClassHint(class string) string {
	switch class {
	case "network":
		return "Can't reach the provider/board — check the URL is right and the service (or proxy) is running."
	case "auth":
		return "Authentication failed — the API key or token looks wrong or expired. Update it in Setup."
	case "rate_limit":
		return "Rate limit or quota hit — goon will retry with backoff; check your plan limits if it persists."
	case "model":
		return "The provider returned a server error — usually transient; goon will retry."
	case "config":
		return "A required provider/board isn't configured yet — finish Setup."
	default:
		return ""
	}
}

// recordFail registers an infrastructural poll failure: bumps the
// consecutive counter, records class + message, schedules the next retry
// via exponential backoff, and self-pauses the daemon once the failures
// cross maxConsecutiveFails (marking AutoPaused so the UI explains it and
// a manual resume clears it).
func (d *Daemon) recordFail(class, msg string) {
	if d == nil || d.opts.Memory == nil {
		return
	}
	st := d.opts.Memory.GetStatus()
	st.ConsecutiveFails++
	st.LastError = msg
	st.LastErrorAt = time.Now()
	st.ErrorClass = class
	st.NextRetryAt = time.Now().Add(backoffDelay(st.ConsecutiveFails))
	// Self-pause only on genuine infrastructural classes. A "config"
	// failure (provider/board not set up yet) must NOT auto-pause — the
	// user is mid-onboarding, and a forced wake after they save config
	// has to be able to proceed (a paused daemon would bail before it
	// ever re-checks the now-valid config).
	infra := class == "network" || class == "auth" || class == "rate_limit" || class == "model"
	if infra && st.ConsecutiveFails >= maxConsecutiveFails {
		st.Paused = true
		st.AutoPaused = true
	}
	d.opts.Memory.SetStatus(st)
}

// recordSuccess clears the breaker once a poll completes without an
// infrastructural failure. Leaves the pause flags alone (a user pause
// stays a user pause). No-op when nothing was set.
func (d *Daemon) recordSuccess() {
	if d == nil || d.opts.Memory == nil {
		return
	}
	st := d.opts.Memory.GetStatus()
	if st.ConsecutiveFails == 0 && st.LastError == "" && st.NextRetryAt.IsZero() && st.ErrorClass == "" {
		return
	}
	st.ConsecutiveFails = 0
	st.LastError = ""
	st.LastErrorAt = time.Time{}
	st.ErrorClass = ""
	st.NextRetryAt = time.Time{}
	d.opts.Memory.SetStatus(st)
}

// afterRun records breaker state from a workflow run outcome: a clean run
// (or a non-infrastructural ticket failure) means the provider/board are
// healthy → clear; an infrastructural error (provider down mid-run) →
// trip the breaker.
func (d *Daemon) afterRun(runErr error) {
	if runErr == nil {
		d.recordSuccess()
		return
	}
	if class, infra := classifyProviderError(runErr); infra {
		d.recordFail(class, "workflow failed: "+runErr.Error())
		return
	}
	// Single ticket failed for non-infra reasons (code/test) — infra is
	// fine, so don't trip the breaker; the failure shows on the workflow.
	d.recordSuccess()
}

// humanizeUntil renders a short "in 30s" / "in 4m" for a future time.
func humanizeUntil(t time.Time) string {
	d := time.Until(t)
	if d <= 0 {
		return "now"
	}
	if d < time.Minute {
		return fmt.Sprintf("in %ds", int(d.Seconds()))
	}
	return fmt.Sprintf("in %dm", int(d.Minutes())+1)
}

// Reconfigure rebuilds the LLM provider, Board, and Host from environment
// variables. Safe to call concurrently with Run; existing in-flight workflow
// calls keep using the previous instances.
//
// Returns a list of human-readable status lines (one per provider) for the
// caller to surface to the user.
func (d *Daemon) Reconfigure() []string {
	d.rcMu.Lock()
	defer d.rcMu.Unlock()

	notes := []string{}

	// LLM provider.
	if prov, err := llm.NewFromEnv(); err == nil {
		d.llm = prov
		notes = append(notes, "✓ LLM provider: "+prov.Name())
	} else {
		notes = append(notes, "✗ LLM provider: "+err.Error())
	}

	// Board.
	if b, err := boards.NewFromEnv(); err == nil {
		d.board = b
		notes = append(notes, "✓ board: "+b.Name())
	} else {
		if errors.Is(err, boards.ErrNoBoard) {
			notes = append(notes, "✗ board: not configured (set GOON_BOARD)")
		} else {
			notes = append(notes, "✗ board: "+err.Error())
		}
	}

	// Git host (optional unless --no-pr).
	if d.opts.PRDisabled {
		d.host = nil
		notes = append(notes, "(PR creation disabled via --no-pr)")
	} else if h, err := githost.NewFromEnv(); err == nil {
		d.host = h
		notes = append(notes, "✓ git host: "+h.Name())
	} else if errors.Is(err, githost.ErrNoHost) {
		d.host = nil
		notes = append(notes, "✗ git host: not configured (PR creation skipped)")
	} else {
		notes = append(notes, "✗ git host: "+err.Error())
	}

	// Persist the new state into memory so the UI reflects it.
	st := d.opts.Memory.GetStatus()
	if d.board != nil {
		st.BoardName = d.board.Name()
	} else {
		st.BoardName = ""
	}
	if d.host != nil {
		st.HostName = d.host.Name()
	} else {
		st.HostName = ""
	}
	d.opts.Memory.SetStatus(st)
	return notes
}

// Snapshot returns the daemon's current providers in a thread-safe way.
func (d *Daemon) Snapshot() (llm.Provider, boards.Board, githost.Host) {
	d.rcMu.RLock()
	defer d.rcMu.RUnlock()
	return d.llm, d.board, d.host
}

// Configured reports whether the daemon has the minimum providers needed to
// actually do work (LLM + Board).
func (d *Daemon) Configured() bool {
	prov, board, _ := d.Snapshot()
	return prov != nil && board != nil
}

// RunOnce performs a single poll-and-execute cycle and returns. Useful for
// `goon start --once` and cron-driven setups.
func (d *Daemon) RunOnce(ctx context.Context) error {
	if d.opts.Memory == nil {
		return errors.New("daemon: memory required")
	}
	d.start()
	defer d.stop()
	// Single explicit cycle (cron / --once) — force through any backoff
	// window so an invoked run always attempts work.
	d.pollAndRun(ctx, true)
	return nil
}

// Run blocks until ctx is cancelled. It polls the board on every tick.
// When the daemon isn't fully configured, the tick logs "waiting for config"
// and skips the workflow run instead of crashing.
func (d *Daemon) Run(ctx context.Context) error {
	if d.opts.Memory == nil {
		return errors.New("daemon: memory required")
	}
	d.start()
	defer d.stop()

	// Initial poll is forced (ignore any persisted backoff window) so a
	// freshly-started daemon always attempts work immediately.
	d.pollAndRun(ctx, true)

	t := time.NewTicker(d.opts.PollInterval)
	defer t.Stop()
	// Independent minute ticker for scheduled automations (cron / interval),
	// so schedule granularity doesn't depend on PollInterval.
	sched := time.NewTicker(time.Minute)
	defer sched.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			// Scheduled tick respects the circuit-breaker backoff window.
			d.pollAndRun(ctx, false)
		case <-sched.C:
			d.scheduleTick(ctx)
		case <-d.wakeCh:
			// User just answered a gate question, saved config, or
			// resumed — run immediately, bypassing the backoff window,
			// because an explicit nudge means "try now".
			d.pollAndRun(ctx, true)
		case name := <-d.runNowCh:
			// Web "run now" on an automation — run that one immediately,
			// ignoring its schedule.
			d.runAutomationByName(ctx, name)
		}
	}
}

// scheduleTick fires every due automation (scheduled workflows under
// storage/workflows/) once. It runs them sequentially under the same lock as
// pollAndRun so engine runs never overlap. Automations need an LLM but no
// board; a paused daemon skips them.
func (d *Daemon) scheduleTick(ctx context.Context) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.opts.Memory == nil {
		return
	}
	if st := d.opts.Memory.GetStatus(); st.Paused {
		return
	}
	autos := workflow.LoadAutomations()
	if len(autos) == 0 {
		return
	}
	eng, ok := d.automationEngine()
	if !ok {
		return // automations run an agent — need an LLM provider
	}
	now := time.Now()
	for _, a := range autos {
		cfg := a.Config
		if !cfg.IsScheduled() || !cfg.IsEnabled() {
			continue
		}
		// Fire every automation whose minute matches (cron only matches its own
		// minute, so we can't defer to a later tick), sequentially.
		if !cfg.Trigger.Due(d.opts.Memory.LastScheduledRun(cfg.Name), now) {
			continue
		}
		d.opts.Memory.MarkScheduledRun(cfg.Name, now)
		fmt.Fprintf(d.opts.Stdout, "[schedule] firing automation %q (%s)\n", cfg.Name, cfg.Trigger.ScheduleHint())
		if _, err := eng.RunJob(ctx, cfg); err != nil {
			fmt.Fprintf(d.opts.Stderr, "[schedule] automation %q failed: %v\n", cfg.Name, err)
		}
	}
}

func (d *Daemon) start() {
	st := d.opts.Memory.GetStatus()
	st.Running = true
	st.PID = os.Getpid()
	if st.StartedAt.IsZero() {
		st.StartedAt = time.Now()
	}
	prov, board, host := d.Snapshot()
	if board != nil {
		st.BoardName = board.Name()
	}
	if host != nil {
		st.HostName = host.Name()
	}
	d.opts.Memory.SetStatus(st)
	llmName := "(unconfigured)"
	if prov != nil {
		llmName = prov.Name()
	}
	boardName := "(unconfigured)"
	if board != nil {
		boardName = board.Name()
	}
	hostName := "(none)"
	if host != nil {
		hostName = host.Name()
	}
	fmt.Fprintf(d.opts.Stdout, "→ goon daemon started (pid=%d, poll=%s, llm=%s, board=%s, host=%s)\n",
		st.PID, d.opts.PollInterval, llmName, boardName, hostName)
}

func (d *Daemon) stop() {
	st := d.opts.Memory.GetStatus()
	st.Running = false
	st.ActiveWorkflow = ""
	// Clear Paused on shutdown so a fresh `goon start` doesn't inherit
	// a stale pause from a previous session. Pause is a runtime control,
	// not durable config.
	st.Paused = false
	d.opts.Memory.SetStatus(st)
	fmt.Fprintln(d.opts.Stdout, "→ goon daemon stopped")
}

func (d *Daemon) pollAndRun(ctx context.Context, forced bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	pollStart := time.Now()
	logx.Info("daemon.poll_start")
	defer func() {
		logx.Info("daemon.poll_end", "duration_ms", time.Since(pollStart).Milliseconds())
	}()

	// Pick up any answers / new questions that the CLI or web UI may have
	// written since the last tick. Also picks up the Paused flag from
	// `goon pause` / web UI / Telegram /pause — the three control
	// surfaces all flip the same memory.json field, and Reload is how
	// the daemon learns about it.
	d.opts.Memory.Reload()

	if d.opts.Memory.IsPaused() {
		// Don't update LastPoll while paused — operators reading the
		// status see "stuck" timestamps and worry. Paused is its own
		// state; LastPoll keeps its last-real value.
		fmt.Fprintln(d.opts.Stdout, "[poll] paused — run `goon resume` (or use the web UI / Telegram /resume) to pick up new tickets")
		return
	}

	// Circuit-breaker backoff window. After a run of infrastructural
	// failures (provider/board unreachable) we space out retries instead
	// of hammering a dead endpoint every tick. A scheduled tick inside
	// the window is skipped; a forced poll (startup, wake on config save
	// / answer / resume) ignores the window and tries now.
	if !forced {
		if st := d.opts.Memory.GetStatus(); !st.NextRetryAt.IsZero() && time.Now().Before(st.NextRetryAt) {
			fmt.Fprintf(d.opts.Stdout, "[poll] backing off after %d failure(s) — next retry %s\n",
				st.ConsecutiveFails, humanizeUntil(st.NextRetryAt))
			return
		}
	}

	now := time.Now()
	st := d.opts.Memory.GetStatus()
	st.LastPoll = now
	d.opts.Memory.SetStatus(st)

	prov, board, host := d.Snapshot()
	// Local tickets (created in the UI, no external board) are first-class
	// work items. A user with just an LLM provider can create one and goon
	// runs it — the lowest-friction path to first value. So a board is NOT
	// required when local tickets exist.
	local := d.opts.Memory.ListLocalTickets()
	if prov == nil || (board == nil && len(local) == 0) {
		// Tell the user what to do, regardless of whether --web is on. The
		// web UI link is one option; `goon doctor` also surfaces missing
		// providers + the exact env vars to set.
		st := d.opts.Memory.GetStatus()
		fix := "run `goon doctor` to see which providers are missing"
		if st.WebAddr != "" {
			fix = "open http://" + strings.TrimPrefix(st.WebAddr, ":") + " or run `goon doctor`"
		}
		missing := "LLM provider"
		if prov != nil && board == nil {
			missing = "a board, or create a local ticket on the Tickets tab"
		} else if prov == nil && board == nil {
			missing = "LLM provider (and a board or a local ticket)"
		}
		d.recordFail("config", missing+" not configured — check Setup ("+fix+")")
		fmt.Fprintf(d.opts.Stdout, "[poll] waiting for config — %s\n", fix)
		return
	}

	eng := &workflow.Engine{
		LLM: prov, Tools: d.opts.Tools, Executor: d.opts.Executor,
		Memory: d.opts.Memory, Board: board, Host: host,
		Stdout: d.opts.Stdout, Stderr: d.opts.Stderr, Debug: d.opts.Debug,
		VerifyRunsOverride: d.opts.VerifyRunsOverride,
	}

	// Resume path — a previously-paused workflow whose approval question is
	// now answered takes priority over picking up new tickets. We process at
	// most one workflow per tick to keep the daemon's behaviour predictable.
	if wf, ok := d.opts.Memory.ResumableWorkflow(); ok {
		t, err := d.resolveTicket(ctx, board, wf.TicketID)
		if err != nil {
			fmt.Fprintf(d.opts.Stderr, "[poll] cannot resume %s: %v\n", wf.ID, err)
		} else {
			fmt.Fprintf(d.opts.Stdout, "[poll] resuming %s at %s — %s\n", wf.TicketKey, wf.Stage, wf.Title)
			st = d.opts.Memory.GetStatus()
			st.LastTicket = wf.TicketID
			st.ActiveWorkflow = wf.ID
			d.opts.Memory.SetStatus(st)
			resumed, runErr := eng.Run(usage.WithLabel(ctx, "workflow "+wf.TicketKey), t)
			st = d.opts.Memory.GetStatus()
			st.ActiveWorkflow = resumed.ID
			d.opts.Memory.SetStatus(st)
			if runErr != nil {
				fmt.Fprintf(d.opts.Stderr, "[poll] workflow %s failed: %v\n", resumed.ID, runErr)
			}
			// Update the circuit breaker from this run's outcome (an
			// infrastructural failure trips it; a clean run or a non-infra
			// ticket failure clears it).
			d.afterRun(runErr)
			// Drain the backlog fast: if more answered workflows are ready,
			// schedule another poll immediately instead of waiting a full
			// PollInterval. One workflow per tick keeps each tick bounded
			// and the mutex short-held, but a bulk-approve still flows
			// through quickly.
			if _, more := d.opts.Memory.ResumableWorkflow(); more {
				d.Wake()
			}
			return
		}
	}

	// Manual pick — a ticket the user explicitly queued from the Tickets
	// tab (with repos pre-assigned). Runs ahead of the recency-based
	// auto-pick so "Pick" feels immediate. One per tick; wake again if more.
	if tid, _, ok := d.opts.Memory.NextPick(); ok {
		d.opts.Memory.ClearPick(tid)
		if t, err := d.resolveTicket(ctx, board, tid); err != nil {
			fmt.Fprintf(d.opts.Stderr, "[poll] cannot run picked ticket %s: %v\n", tid, err)
		} else if !d.opts.Memory.HasOpenWorkflowFor(t.ID) && !d.opts.Memory.HasCompletedWorkflowFor(t.ID) {
			fmt.Fprintf(d.opts.Stdout, "[poll] manual pick %s — %s\n", t.Key, t.Title)
			st = d.opts.Memory.GetStatus()
			st.LastTicket = t.ID
			d.opts.Memory.SetStatus(st)
			wf, runErr := eng.Run(usage.WithLabel(ctx, "workflow "+t.Key), t)
			st = d.opts.Memory.GetStatus()
			st.ActiveWorkflow = wf.ID
			d.opts.Memory.SetStatus(st)
			if runErr != nil {
				fmt.Fprintf(d.opts.Stderr, "[poll] workflow %s failed: %v\n", wf.ID, runErr)
			}
			d.afterRun(runErr)
			if _, _, more := d.opts.Memory.NextPick(); more {
				d.Wake()
			}
			return
		}
	}

	var tickets []boards.Ticket
	if board != nil {
		bt, lerr := board.List(ctx)
		if lerr != nil {
			class, _ := classifyProviderError(lerr)
			d.recordFail(class, "board "+board.Name()+" unreachable: "+lerr.Error())
			fmt.Fprintf(d.opts.Stderr, "[poll] error: %v\n", lerr)
			return
		}
		tickets = bt
		logx.Info("daemon.tickets_listed", "count", len(tickets))
		for _, t := range tickets {
			// Copy every field that chat / web UI / /tickets can render —
			// dropping Assignee/Labels/Project caused "assigned to me"
			// queries to fall apart because memory had nothing to filter
			// on. boards.Ticket already carries these from Jira/GitHub.
			d.opts.Memory.SeenTicket(memory.TicketSnapshot{
				ID: t.ID, Source: t.Source, Key: t.Key,
				Title: t.Title, URL: t.URL, Status: string(t.Status),
				Assignee:  t.Assignee,
				Labels:    t.Labels,
				Project:   t.Project,
				UpdatedAt: t.UpdatedAt, LastSeen: now,
			})
		}
		// Reconcile the cached inventory to the live filter: drop snapshots
		// for THIS board that the current JQL/query no longer returns, so a
		// tightened filter (e.g. assignee=currentUser) stops showing stale,
		// now-excluded tickets. Skip when the page looks truncated (>= the
		// board page size) to avoid dropping legitimate page-2 matches.
		if len(bt) < reconcileMaxList {
			ids := make([]string, 0, len(bt))
			for _, t := range bt {
				ids = append(ids, t.ID)
			}
			if n := d.opts.Memory.ReconcileTickets(board.Name(), ids); n > 0 {
				logx.Info("daemon.tickets_reconciled", "source", board.Name(), "removed", n)
			}
		}
	}
	// Merge user-created local tickets — these need no external board, so a
	// fresh user gets value from just an LLM provider + one local ticket.
	for _, lt := range local {
		tickets = append(tickets, localToTicket(lt))
	}
	pick := d.nextTicket(tickets)
	if pick == nil {
		// Board listed fine and nothing to do — infra is demonstrably
		// healthy, so clear the breaker here. (We deliberately do NOT
		// clear right after board.List when a workflow is about to run:
		// otherwise a provider that's down *during* the run would have
		// its failure count reset every tick and never trip the breaker.)
		d.recordSuccess()
		fmt.Fprintf(d.opts.Stdout, "[poll] %d ticket(s); none actionable\n", len(tickets))
		// Idle — spend the lull learning about the project + itself.
		d.maybeReflect(ctx, prov)
		return
	}
	if d.hasUnansweredQuestion(pick.ID) {
		// Board healthy, just nothing runnable this tick — clear breaker.
		d.recordSuccess()
		fmt.Fprintf(d.opts.Stdout, "[poll] %s blocked on user question; skipping\n", pick.Key)
		return
	}

	fmt.Fprintf(d.opts.Stdout, "[poll] picking %s — %s\n", pick.Key, pick.Title)

	st = d.opts.Memory.GetStatus()
	st.LastTicket = pick.ID
	d.opts.Memory.SetStatus(st)

	wf, runErr := eng.Run(usage.WithLabel(ctx, "workflow "+pick.Key), *pick)
	st = d.opts.Memory.GetStatus()
	st.ActiveWorkflow = wf.ID
	d.opts.Memory.SetStatus(st)
	if runErr != nil {
		fmt.Fprintf(d.opts.Stderr, "[poll] workflow %s failed: %v\n", wf.ID, runErr)
	}
	// Update the circuit breaker from this run's outcome.
	d.afterRun(runErr)
}

// maxOpenLearningQuestions caps how many unanswered learning questions
// standby reflection will let accumulate. Past this, reflection still runs
// (and can write notes) but is told not to ask anything new — so the
// Questions tab never becomes a flood the user tunes out.
const maxOpenLearningQuestions = 5

// reflectTimeout bounds a single standby-reflection agent run so a stuck
// model call can't wedge the poll loop indefinitely.
const reflectTimeout = 5 * time.Minute

// maybeReflect runs goon's standby self-learning pass while the daemon is
// idle, throttled to once per learnings.ReflectInterval (default daily). It
// holds the poll mutex (caller already does), which is fine because the
// daemon has nothing else to do while idle. Best-effort: never returns an
// error, never blocks longer than reflectTimeout.
func (d *Daemon) maybeReflect(ctx context.Context, prov llm.Provider) {
	if !learnings.ReflectEnabled() {
		return
	}
	if prov == nil || d.opts.Tools == nil || d.opts.Executor == nil {
		return
	}
	last := d.opts.Memory.LastReflect()
	if !last.IsZero() && time.Since(last) < learnings.ReflectInterval() {
		return
	}
	// If the user is already behind on answering learning questions, don't
	// reflect (and don't burn the throttle) until they catch up — keeps the
	// Questions tab from becoming noise.
	open := len(d.opts.Memory.PendingLearningQuestions())
	if open >= maxOpenLearningQuestions {
		return
	}
	// Record the timestamp BEFORE running so a long/failed pass doesn't loop
	// on every poll until it finally succeeds.
	d.opts.Memory.SetLastReflect(time.Now())
	fmt.Fprintln(d.opts.Stdout, "[learn] standby — reflecting on recent changes…")
	rctx, cancel := context.WithTimeout(ctx, reflectTimeout)
	defer cancel()
	_ = learnings.Reflect(rctx, learnings.ReflectOptions{
		LLM:                   prov,
		Tools:                 d.opts.Tools,
		Executor:              d.opts.Executor,
		Memory:                d.opts.Memory,
		Stdout:                d.opts.Stdout,
		Stderr:                d.opts.Stderr,
		Debug:                 d.opts.Debug,
		OpenLearningQuestions: open,
	})
}

// allowedTicketStatuses returns the set of Status values that nextTicket
// should consider. Configurable via GOON_TICKET_STATUSES (comma-separated
// canonical goon status names: open, in_progress, in_review, blocked, done).
// Defaults to open,in_progress when the env var is unset or empty.
// StatusUnknown is always implicitly included regardless of configuration —
// many boards return it for custom statuses that MapStatus cannot map, and
// silently dropping those tickets would cause confusing gaps in the queue.
func allowedTicketStatuses() map[boards.Status]bool {
	raw := strings.TrimSpace(os.Getenv("GOON_TICKET_STATUSES"))
	if raw == "" {
		return map[boards.Status]bool{
			boards.StatusOpen:       true,
			boards.StatusInProgress: true,
			boards.StatusUnknown:    true,
		}
	}
	out := map[boards.Status]bool{
		boards.StatusUnknown: true, // always included; see docstring above
	}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(strings.ToLower(part))
		switch boards.Status(part) {
		case boards.StatusOpen, boards.StatusInProgress, boards.StatusInReview,
			boards.StatusBlocked, boards.StatusDone, boards.StatusUnknown:
			out[boards.Status(part)] = true
		}
	}
	return out
}

// nextTicket picks the most-recently-updated ticket whose status is in the
// configured allowed set (GOON_TICKET_STATUSES) and doesn't already have an
// in-flight workflow. Ignored tickets (set via the web Tickets-tab "🚫 ignore"
// toggle or the CLI) are filtered out so they never get picked, even if they're
// the freshest thing on the board.
// localToTicket converts a user-created local ticket into a boards.Ticket
// so the same workflow engine runs it. Source "local" marks its origin;
// Project is empty so triage / the confirm_repo gate decide the repo.
func localToTicket(lt memory.LocalTicket) boards.Ticket {
	return boards.Ticket{
		ID:          lt.ID,
		Source:      "local",
		Key:         lt.ID,
		Title:       lt.Title,
		Description: lt.Description,
		Status:      boards.Status(lt.Status),
		Labels:      lt.Labels,
		UpdatedAt:   lt.UpdatedAt,
	}
}

// resolveTicket fetches a ticket by id, checking local tickets first (so
// resume works with no board configured) and falling back to the board.
func (d *Daemon) resolveTicket(ctx context.Context, board boards.Board, id string) (boards.Ticket, error) {
	for _, lt := range d.opts.Memory.ListLocalTickets() {
		if lt.ID == id {
			return localToTicket(lt), nil
		}
	}
	if board == nil {
		return boards.Ticket{}, fmt.Errorf("ticket %s not found (no board configured)", id)
	}
	return board.Get(ctx, id)
}

func (d *Daemon) nextTicket(tickets []boards.Ticket) *boards.Ticket {
	allowed := allowedTicketStatuses()
	var best *boards.Ticket
	for i := range tickets {
		t := &tickets[i]
		if !allowed[t.Status] {
			continue
		}
		if d.opts.Memory.HasOpenWorkflowFor(t.ID) {
			continue
		}
		if d.opts.Memory.HasCompletedWorkflowFor(t.ID) {
			continue
		}
		// User-driven opt-out. Both the key (e.g. "EB-4978") and the
		// id (board-internal, often the same as key) are checked so
		// boards that surface different identifiers stay consistent.
		if d.opts.Memory.IsTicketIgnored(t.Key) || d.opts.Memory.IsTicketIgnored(t.ID) {
			continue
		}
		if best == nil || t.UpdatedAt.After(best.UpdatedAt) {
			best = t
		}
	}
	return best
}

func (d *Daemon) hasUnansweredQuestion(ticketID string) bool {
	for _, q := range d.opts.Memory.PendingQuestions() {
		if q.TicketID == ticketID {
			return true
		}
	}
	return false
}
