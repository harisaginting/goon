package tools

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/harisaginting/goon/internal/agentpool"
)

// ─── spawn_agents ─────────────────────────────────────────────────────────────

// SpawnAgentsTool launches one child goon process per task in parallel, each
// in an isolated GOON_STORAGE_DIR. When wait=true (default) it blocks until all
// finish and returns a combined result. When wait=false it returns immediately
// with the agent IDs so the caller can poll via agent_status.
type SpawnAgentsTool struct{}

func (*SpawnAgentsTool) Name() string { return "spawn_agents" }
func (*SpawnAgentsTool) Description() string {
	return "spawn one or more child goon agents in parallel, each with an isolated " +
		"terminal environment. " +
		"Separate multiple tasks with a newline or | character. " +
		"Set wait=false to fire-and-forget and poll with agent_status. " +
		"Use workdir to override the working directory for all children. " +
		"Use timeout to set max seconds per agent (default 1800)."
}
func (*SpawnAgentsTool) Schema() map[string]string {
	return map[string]string{
		"tasks":   "newline- or pipe-separated list of task descriptions, one per agent",
		"wait":    "(optional) true (default) to block until all done; false to return immediately",
		"workdir": "(optional) working directory for child agents; defaults to current dir",
		"timeout": "(optional) seconds each agent may run; default 1800",
	}
}

func (*SpawnAgentsTool) Run(ctx context.Context, args map[string]string) (Result, error) {
	raw := strings.TrimSpace(args["tasks"])
	if raw == "" {
		e := fmt.Errorf("spawn_agents: \"tasks\" arg is required")
		return Result{ToolName: "spawn_agents", Err: e}, e
	}

	// Split on newlines or pipes, trim whitespace, drop empties.
	sep := "\n"
	if !strings.Contains(raw, "\n") && strings.Contains(raw, "|") {
		sep = "|"
	}
	var tasks []string
	for _, t := range strings.Split(raw, sep) {
		t = strings.TrimSpace(t)
		if t != "" {
			tasks = append(tasks, t)
		}
	}
	if len(tasks) == 0 {
		e := fmt.Errorf("spawn_agents: no non-empty tasks found in \"tasks\" arg")
		return Result{ToolName: "spawn_agents", Err: e}, e
	}

	waitForDone := true
	if w := strings.TrimSpace(args["wait"]); w == "false" || w == "0" {
		waitForDone = false
	}

	timeoutSec := 1800
	if v := strings.TrimSpace(args["timeout"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			timeoutSec = n
		}
	}

	pool, err := agentpool.Global()
	if err != nil {
		return Result{ToolName: "spawn_agents", Err: err}, err
	}

	opts := agentpool.SpawnOptions{
		WorkDir: strings.TrimSpace(args["workdir"]),
		Timeout: time.Duration(timeoutSec) * time.Second,
	}

	ids, err := pool.SpawnAll(tasks, opts)
	if err != nil {
		return Result{ToolName: "spawn_agents", Err: err}, err
	}

	if !waitForDone {
		var b strings.Builder
		fmt.Fprintf(&b, "spawned %d agent(s) — poll with agent_status:\n", len(ids))
		for i, id := range ids {
			fmt.Fprintf(&b, "  [%d] %s  task: %s\n", i+1, id, tasks[i])
		}
		return Result{ToolName: "spawn_agents", Stdout: b.String()}, nil
	}

	// Block until all agents finish or timeout.
	results := pool.WaitAll(ids, time.Duration(timeoutSec)*time.Second)

	var b strings.Builder
	allOK := true
	for i, id := range ids {
		snap, ok := results[id]
		if !ok {
			continue
		}
		state := snap.State
		if state != agentpool.StateDone {
			allOK = false
		}
		dur := snap.FinishedAt.Sub(snap.StartedAt).Round(time.Second)
		fmt.Fprintf(&b, "─── agent %d/%d  id=%s  state=%s  exit=%d  duration=%s ───\n",
			i+1, len(ids), id, state, snap.ExitCode, dur)
		fmt.Fprintf(&b, "task: %s\n", snap.Task)
		if snap.Output != "" {
			fmt.Fprintf(&b, "output:\n%s\n", snap.Output)
		}
	}

	res := Result{ToolName: "spawn_agents", Stdout: b.String()}
	if !allOK {
		res.Err = fmt.Errorf("one or more agents did not complete successfully")
		res.ExitCode = 1
	}
	return res, nil
}

// ─── agent_status ─────────────────────────────────────────────────────────────

// AgentStatusTool reports the current state of one or more agents by ID.
type AgentStatusTool struct{}

func (*AgentStatusTool) Name() string { return "agent_status" }
func (*AgentStatusTool) Description() string {
	return "report state and output of child agents spawned with spawn_agents. " +
		"Pass a single agent ID or comma-separated IDs, or omit id to list all agents."
}
func (*AgentStatusTool) Schema() map[string]string {
	return map[string]string{
		"id": "(optional) agent ID or comma-separated IDs; omit to list all",
	}
}

func (*AgentStatusTool) Run(_ context.Context, args map[string]string) (Result, error) {
	pool, err := agentpool.Global()
	if err != nil {
		return Result{ToolName: "agent_status", Err: err}, err
	}

	rawID := strings.TrimSpace(args["id"])

	var snaps []agentpool.Agent
	if rawID == "" {
		snaps = pool.List()
		if len(snaps) == 0 {
			return Result{ToolName: "agent_status", Stdout: "(no agents spawned yet)"}, nil
		}
	} else {
		for _, id := range strings.Split(rawID, ",") {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			a, ok := pool.Get(id)
			if !ok {
				snaps = append(snaps, agentpool.Agent{ID: id, State: "unknown"})
				continue
			}
			snaps = append(snaps, a.Snapshot())
		}
	}

	var b strings.Builder
	for _, s := range snaps {
		dur := "—"
		if !s.StartedAt.IsZero() {
			if s.FinishedAt.IsZero() {
				dur = "running " + time.Since(s.StartedAt).Round(time.Second).String()
			} else {
				dur = s.FinishedAt.Sub(s.StartedAt).Round(time.Second).String()
			}
		}
		fmt.Fprintf(&b, "id:      %s\n", s.ID)
		fmt.Fprintf(&b, "task:    %s\n", s.Task)
		fmt.Fprintf(&b, "state:   %s  exit=%d  duration=%s\n", s.State, s.ExitCode, dur)
		if s.Output != "" {
			fmt.Fprintf(&b, "output:\n%s\n", s.Output)
		}
		b.WriteString("\n")
	}
	return Result{ToolName: "agent_status", Stdout: strings.TrimRight(b.String(), "\n")}, nil
}

// ─── agent_cancel ─────────────────────────────────────────────────────────────

// AgentCancelTool cancels a running child agent by ID.
type AgentCancelTool struct{}

func (*AgentCancelTool) Name() string { return "agent_cancel" }
func (*AgentCancelTool) Description() string {
	return "cancel a running child agent by ID, sending SIGKILL to its process."
}
func (*AgentCancelTool) Schema() map[string]string {
	return map[string]string{
		"id": "agent ID returned by spawn_agents",
	}
}

func (*AgentCancelTool) Run(_ context.Context, args map[string]string) (Result, error) {
	id := strings.TrimSpace(args["id"])
	if id == "" {
		e := fmt.Errorf("agent_cancel: \"id\" arg is required")
		return Result{ToolName: "agent_cancel", Err: e}, e
	}
	pool, err := agentpool.Global()
	if err != nil {
		return Result{ToolName: "agent_cancel", Err: err}, err
	}
	if !pool.Cancel(id) {
		msg := fmt.Sprintf("agent %q not found", id)
		return Result{ToolName: "agent_cancel", Stdout: msg}, nil
	}
	return Result{ToolName: "agent_cancel", Stdout: fmt.Sprintf("cancelled %s", id)}, nil
}

// RegisterAgentTools adds the three agent tools to a registry.
func RegisterAgentTools(r *Registry) {
	r.Register(&SpawnAgentsTool{})
	r.Register(&AgentStatusTool{})
	r.Register(&AgentCancelTool{})
}
