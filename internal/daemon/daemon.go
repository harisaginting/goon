// Package daemon is goon's background autonomous loop.
//
// It polls the configured Board, picks the next Open ticket, and feeds it to
// the workflow engine. Memory acts as the source of truth for status,
// tickets, workflows, and questions — both CLI (`goon status`) and the web UI
// read from there.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"time"

	"goon/internal/boards"
	"goon/internal/executor"
	"goon/internal/githost"
	"goon/internal/llm"
	"goon/internal/memory"
	"goon/internal/tools"
	"goon/internal/workflow"
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
	LLM      llm.Provider
	Tools    *tools.Registry
	Executor *executor.Executor
	Memory   *memory.Memory
	Board    boards.Board
	Host     githost.Host
	Stdout   io.Writer
	Stderr   io.Writer
	Debug    bool

	// PollInterval overrides the env var — used in tests.
	PollInterval time.Duration
	// VerifyRunsOverride passes through to workflow.Engine. 0 = use env default.
	VerifyRunsOverride int
}

// Daemon is the long-running loop.
type Daemon struct {
	opts Options
	mu   sync.Mutex
}

// New wires a Daemon.
func New(opts Options) *Daemon {
	if opts.PollInterval == 0 {
		opts.PollInterval = PollInterval()
	}
	return &Daemon{opts: opts}
}

// RunOnce performs a single poll-and-execute cycle and returns. Useful for
// `goon start --once` and cron-driven setups.
func (d *Daemon) RunOnce(ctx context.Context) error {
	if d.opts.Memory == nil {
		return errors.New("daemon: memory required")
	}
	if d.opts.Board == nil {
		return errors.New("daemon: board required")
	}
	d.start()
	defer d.stop()
	d.pollAndRun(ctx)
	return nil
}

// Run blocks until ctx is cancelled. It polls the board on every tick, picks
// the most-recently-updated open ticket without an in-flight workflow, and
// runs the workflow engine on it. Status is persisted to memory throughout.
func (d *Daemon) Run(ctx context.Context) error {
	if d.opts.Memory == nil {
		return errors.New("daemon: memory required")
	}
	if d.opts.Board == nil {
		return errors.New("daemon: board required")
	}
	d.start()
	defer d.stop()

	// First poll happens immediately, then on the ticker.
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
	if d.opts.Board != nil {
		st.BoardName = d.opts.Board.Name()
	}
	if d.opts.Host != nil {
		st.HostName = d.opts.Host.Name()
	}
	d.opts.Memory.SetStatus(st)
	fmt.Fprintf(d.opts.Stdout, "→ goon daemon started (pid=%d, poll=%s, board=%s, host=%s)\n",
		st.PID, d.opts.PollInterval, st.BoardName, st.HostName)
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

	// Pick up any answers / new questions that the CLI or web UI may have
	// written since the last tick.
	d.opts.Memory.Reload()

	tickets, err := d.opts.Board.List(ctx)
	now := time.Now()
	st := d.opts.Memory.GetStatus()
	st.LastPoll = now
	d.opts.Memory.SetStatus(st)

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
		LLM: d.opts.LLM, Tools: d.opts.Tools, Executor: d.opts.Executor,
		Memory: d.opts.Memory, Board: d.opts.Board, Host: d.opts.Host,
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
		if t.Status != boards.StatusOpen && t.Status != boards.StatusUnknown {
			continue
		}
		if d.opts.Memory.HasOpenWorkflowFor(t.ID) {
			continue
		}
		if d.opts.Memory.HasCompletedWorkflowFor(t.ID) {
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
