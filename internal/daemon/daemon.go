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
	"log"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/harisaginting/goon/internal/boards"
	"github.com/harisaginting/goon/internal/executor"
	"github.com/harisaginting/goon/internal/githost"
	"github.com/harisaginting/goon/internal/llm"
	"github.com/harisaginting/goon/internal/logx"
	"github.com/harisaginting/goon/internal/memory"
	"github.com/harisaginting/goon/internal/tools"
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
}

// New wires a Daemon. LLM / Board / Host may be nil — Reconfigure can fill
// them in later from environment variables.
func New(opts Options) *Daemon {
	if opts.PollInterval == 0 {
		opts.PollInterval = PollInterval()
	}
	return &Daemon{
		opts:  opts,
		llm:   opts.LLM,
		board: opts.Board,
		host:  opts.Host,
	}
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
	d.pollAndRun(ctx)
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

	d.pollAndRun(ctx)

	t := time.NewTicker(d.opts.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			d.pollAndRun(ctx)
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
	d.opts.Memory.SetStatus(st)
	fmt.Fprintln(d.opts.Stdout, "→ goon daemon stopped")
}

func (d *Daemon) pollAndRun(ctx context.Context) {
	d.mu.Lock()
	defer d.mu.Unlock()
	pollStart := time.Now()
	logx.Info("daemon.poll_start")
	defer func() {
		logx.Info("daemon.poll_end", "duration_ms", time.Since(pollStart).Milliseconds())
	}()

	// Pick up any answers / new questions that the CLI or web UI may have
	// written since the last tick.
	d.opts.Memory.Reload()

	now := time.Now()
	st := d.opts.Memory.GetStatus()
	st.LastPoll = now
	d.opts.Memory.SetStatus(st)

	prov, board, host := d.Snapshot()
	if prov == nil || board == nil {
		fmt.Fprintf(d.opts.Stdout, "[poll] waiting for config — open the web UI to configure goon\n")
		return
	}

	tickets, err := board.List(ctx)
	if err != nil {
		fmt.Fprintf(d.opts.Stderr, "[poll] error: %v\n", err)
		return
	}
	for _, t := range tickets {
		d.opts.Memory.SeenTicket(memory.TicketSnapshot{
			ID: t.ID, Source: t.Source, Key: t.Key,
			Title: t.Title, URL: t.URL, Status: string(t.Status),
			UpdatedAt: t.UpdatedAt, LastSeen: now,
		})
	}
	pick := d.nextTicket(tickets)
	if pick == nil {
		fmt.Fprintf(d.opts.Stdout, "[poll] %d ticket(s); none actionable\n", len(tickets))
		return
	}
	if d.hasUnansweredQuestion(pick.ID) {
		fmt.Fprintf(d.opts.Stdout, "[poll] %s blocked on user question; skipping\n", pick.Key)
		return
	}

	fmt.Fprintf(d.opts.Stdout, "[poll] picking %s — %s\n", pick.Key, pick.Title)

	st = d.opts.Memory.GetStatus()
	st.LastTicket = pick.ID
	d.opts.Memory.SetStatus(st)

	eng := &workflow.Engine{
		LLM: prov, Tools: d.opts.Tools, Executor: d.opts.Executor,
		Memory: d.opts.Memory, Board: board, Host: host,
		Stdout: d.opts.Stdout, Stderr: d.opts.Stderr, Debug: d.opts.Debug,
		VerifyRunsOverride: d.opts.VerifyRunsOverride,
	}
	wf, runErr := eng.Run(ctx, *pick)
	st = d.opts.Memory.GetStatus()
	st.ActiveWorkflow = wf.ID
	d.opts.Memory.SetStatus(st)
	if runErr != nil {
		fmt.Fprintf(d.opts.Stderr, "[poll] workflow %s failed: %v\n", wf.ID, runErr)
	}
}

// nextTicket picks the most-recently-updated Open ticket that doesn't already
// have an in-flight workflow.
func (d *Daemon) nextTicket(tickets []boards.Ticket) *boards.Ticket {
	var best *boards.Ticket
	for i := range tickets {
		t := &tickets[i]
		log.Println("====== status", t.Status)
		if t.Status != boards.StatusOpen && t.Status != boards.StatusUnknown && t.Status != boards.StatusInProgress {
			log.Println("====== 1")
			continue
		}
		if d.opts.Memory.HasOpenWorkflowFor(t.ID) {
			log.Println("====== 2")
			continue
		}
		if d.opts.Memory.HasCompletedWorkflowFor(t.ID) {
			log.Println("====== 3")
			continue
		}
		if best == nil || t.UpdatedAt.After(best.UpdatedAt) {
			log.Println("====== 4")
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
