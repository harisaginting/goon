// Package agentpool manages a pool of isolated child goon agents running in
// parallel. Each child is a subprocess of the current binary with its own
// GOON_STORAGE_DIR so state never bleeds between agents. The pool is
// concurrency-capped (default 4, override with GOON_MAX_AGENTS).
//
// Typical LLM usage:
//
//	spawn_agents  tasks="refactor auth\nwrite tests for auth"  wait=true
//	→ blocks until both finish, returns combined output.
//
// Non-blocking usage:
//
//	spawn_agents  tasks="long task"  wait=false
//	→ returns agent IDs immediately; poll with agent_status.
package agentpool

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/harisaginting/goon/internal/logx"
)

// State values for a child agent.
const (
	StateQueued    = "queued"
	StateRunning   = "running"
	StateDone      = "done"
	StateFailed    = "failed"
	StateCancelled = "cancelled"
)

// Agent tracks one child goon process.
type Agent struct {
	ID         string
	Task       string
	WorkDir    string
	StorageDir string
	State      string
	StartedAt  time.Time
	FinishedAt time.Time
	ExitCode   int
	Output     string // capped combined stdout+stderr

	mu     sync.Mutex
	cancel context.CancelFunc
}

// Snapshot returns a copy of the agent's public fields without holding the lock
// beyond the call, safe for concurrent reads by the web layer or tool handlers.
func (a *Agent) Snapshot() Agent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return Agent{
		ID:         a.ID,
		Task:       a.Task,
		WorkDir:    a.WorkDir,
		StorageDir: a.StorageDir,
		State:      a.State,
		StartedAt:  a.StartedAt,
		FinishedAt: a.FinishedAt,
		ExitCode:   a.ExitCode,
		Output:     a.Output,
	}
}

// Pool manages child goon agents.
type Pool struct {
	mu      sync.RWMutex
	agents  map[string]*Agent
	sem     chan struct{} // concurrency semaphore
	binary  string       // path to this goon binary
	tmpRoot string       // base dir for per-agent storage under os.TempDir()
	seq     int64        // monotonic counter for stable IDs (mu protects)
}

// globalPool is lazily initialised on first use.
var (
	globalOnce sync.Once
	globalPool *Pool
	globalErr  error
)

// Global returns the process-wide agent pool, creating it on the first call.
// GOON_MAX_AGENTS (default 4) caps concurrent children.
func Global() (*Pool, error) {
	globalOnce.Do(func() {
		max := 4
		if v := strings.TrimSpace(os.Getenv("GOON_MAX_AGENTS")); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				max = n
			}
		}
		globalPool, globalErr = newPool(max)
	})
	return globalPool, globalErr
}

func newPool(maxParallel int) (*Pool, error) {
	bin, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("agentpool: cannot locate own binary: %w", err)
	}
	// Resolve symlinks so we always exec the real binary.
	if resolved, err2 := filepath.EvalSymlinks(bin); err2 == nil {
		bin = resolved
	}
	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("goon-pool-%d", os.Getpid()))
	return &Pool{
		agents:  make(map[string]*Agent),
		sem:     make(chan struct{}, maxParallel),
		binary:  bin,
		tmpRoot: tmp,
	}, nil
}

// SpawnOptions configures an individual child agent.
type SpawnOptions struct {
	// WorkDir is the working directory for the child process. Defaults to the
	// parent's cwd when empty.
	WorkDir string
	// Timeout caps how long the child may run. 0 means 30 minutes.
	Timeout time.Duration
	// Env is extra KEY=VALUE pairs injected into the child environment on top
	// of the (filtered) parent environment. Callers can use this to override
	// GOON_BOARD, GOON_LLM_PROVIDER, etc. per agent.
	Env []string
}

// Spawn queues a child agent for the given task and returns immediately.
// The agent runs asynchronously; use WaitAll or poll Get to observe completion.
func (p *Pool) Spawn(task string, opts SpawnOptions) (*Agent, error) {
	if strings.TrimSpace(task) == "" {
		return nil, fmt.Errorf("agentpool: task must not be empty")
	}

	p.mu.Lock()
	p.seq++
	id := fmt.Sprintf("agent-%d-%d", time.Now().UnixNano(), p.seq)
	a := &Agent{
		ID:      id,
		Task:    task,
		WorkDir: opts.WorkDir,
		State:   StateQueued,
	}
	p.agents[id] = a
	p.mu.Unlock()

	go p.run(a, opts)
	return a, nil
}

// SpawnAll spawns one agent per task and returns their IDs in order.
func (p *Pool) SpawnAll(tasks []string, opts SpawnOptions) ([]string, error) {
	ids := make([]string, 0, len(tasks))
	for _, task := range tasks {
		a, err := p.Spawn(task, opts)
		if err != nil {
			return ids, err
		}
		ids = append(ids, a.ID)
	}
	return ids, nil
}

func (p *Pool) run(a *Agent, opts SpawnOptions) {
	// Acquire concurrency slot — blocks if at cap.
	p.sem <- struct{}{}
	defer func() { <-p.sem }()

	storageDir := filepath.Join(p.tmpRoot, a.ID, "storage")
	if err := os.MkdirAll(storageDir, 0o755); err != nil {
		p.finish(a, -1, "", "failed to create storage dir: "+err.Error(), StateFailed, nil)
		return
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)

	a.mu.Lock()
	a.StorageDir = storageDir
	a.State = StateRunning
	a.StartedAt = time.Now().UTC()
	a.cancel = cancel
	a.mu.Unlock()

	workDir := opts.WorkDir
	if workDir == "" {
		if wd, err := os.Getwd(); err == nil {
			workDir = wd
		}
	}

	// Build child environment: parent env minus any GOON_STORAGE_DIR override,
	// then inject a fresh isolated one. Extra opts.Env pairs come last (win).
	childEnv := filteredEnv()
	childEnv = append(childEnv, "GOON_STORAGE_DIR="+storageDir)
	childEnv = append(childEnv, opts.Env...)

	//nolint:gosec // binary is our own resolved executable path
	cmd := exec.CommandContext(ctx, p.binary, a.Task)
	cmd.Dir = workDir
	cmd.Env = childEnv

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	logx.Info("agentpool.child_start", "id", a.ID, "task", a.Task, "workdir", workDir)
	err := cmd.Run()
	cancel() // free ctx resources regardless

	output := buf.String()
	const maxOut = 24 * 1024 // 24 KB cap
	if len(output) > maxOut {
		output = "[…output truncated…]\n" + output[len(output)-maxOut:]
	}

	exitCode := 0
	state := StateDone
	if err != nil {
		state = StateFailed
		var ee *exec.ExitError
		if isExitErr(err, &ee) && ee != nil {
			exitCode = ee.ExitCode()
		} else {
			exitCode = -1
		}
		// Context cancellation → cancelled by user, not a failure.
		if ctx.Err() != nil {
			a.mu.Lock()
			if a.State == StateCancelled {
				state = StateCancelled
			}
			a.mu.Unlock()
		}
	}
	logx.Info("agentpool.child_done", "id", a.ID, "state", state, "exit", exitCode)
	p.finish(a, exitCode, storageDir, output, state, nil)
}

func (p *Pool) finish(a *Agent, exitCode int, storageDir, output, state string, cancel context.CancelFunc) {
	a.mu.Lock()
	defer a.mu.Unlock()
	// Don't overwrite an explicit cancel the user set.
	if a.State != StateCancelled {
		a.State = state
	}
	a.ExitCode = exitCode
	a.Output = output
	a.FinishedAt = time.Now().UTC()
	if storageDir != "" {
		a.StorageDir = storageDir
	}
}

// Get returns the agent with the given ID.
func (p *Pool) Get(id string) (*Agent, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	a, ok := p.agents[id]
	return a, ok
}

// List returns snapshots of all agents sorted by start time (newest first).
func (p *Pool) List() []Agent {
	p.mu.RLock()
	raw := make([]*Agent, 0, len(p.agents))
	for _, a := range p.agents {
		raw = append(raw, a)
	}
	p.mu.RUnlock()

	out := make([]Agent, len(raw))
	for i, a := range raw {
		out[i] = a.Snapshot()
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt.After(out[j].StartedAt)
	})
	return out
}

// Cancel signals the child process for the given agent ID to stop.
// Returns false if the ID is unknown.
func (p *Pool) Cancel(id string) bool {
	p.mu.RLock()
	a, ok := p.agents[id]
	p.mu.RUnlock()
	if !ok {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.State = StateCancelled
	if a.cancel != nil {
		a.cancel()
	}
	return true
}

// WaitAll blocks until every agent in ids reaches a terminal state (done /
// failed / cancelled) or the timeout elapses, then returns snapshots keyed by
// agent ID. Agents still running at timeout remain in the map with their
// current state.
func (p *Pool) WaitAll(ids []string, timeout time.Duration) map[string]Agent {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		allDone := true
		for _, id := range ids {
			a, ok := p.Get(id)
			if !ok {
				continue
			}
			a.mu.Lock()
			state := a.State
			a.mu.Unlock()
			if state == StateQueued || state == StateRunning {
				allDone = false
				break
			}
		}
		if allDone {
			break
		}
		time.Sleep(400 * time.Millisecond)
	}

	out := make(map[string]Agent, len(ids))
	for _, id := range ids {
		if a, ok := p.Get(id); ok {
			out[id] = a.Snapshot()
		}
	}
	return out
}

// Cleanup removes per-agent storage dirs for terminal agents older than age.
// Call periodically (e.g. from the daemon tick) to reclaim tmp space.
func (p *Pool) Cleanup(age time.Duration) {
	cutoff := time.Now().Add(-age)
	p.mu.Lock()
	var toRemove []string
	for id, a := range p.agents {
		a.mu.Lock()
		terminal := a.State == StateDone || a.State == StateFailed || a.State == StateCancelled
		old := !a.FinishedAt.IsZero() && a.FinishedAt.Before(cutoff)
		a.mu.Unlock()
		if terminal && old {
			toRemove = append(toRemove, id)
		}
	}
	for _, id := range toRemove {
		a := p.agents[id]
		delete(p.agents, id)
		if a.StorageDir != "" {
			_ = os.RemoveAll(filepath.Dir(a.StorageDir)) // remove agent-<id>/ dir
		}
	}
	p.mu.Unlock()
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// filteredEnv returns os.Environ() with GOON_STORAGE_DIR stripped so the child
// always gets a fresh isolated one injected afterward.
func filteredEnv() []string {
	src := os.Environ()
	out := make([]string, 0, len(src))
	for _, e := range src {
		if strings.HasPrefix(e, "GOON_STORAGE_DIR=") {
			continue
		}
		out = append(out, e)
	}
	return out
}

// isExitErr is a type-assertion helper that avoids the errors.As generic form
// to stay compatible with Go 1.21's type inference rules.
func isExitErr(err error, target **exec.ExitError) bool {
	ee, ok := err.(*exec.ExitError)
	if ok && target != nil {
		*target = ee
	}
	return ok
}
