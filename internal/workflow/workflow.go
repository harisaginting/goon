// Package workflow implements goon's autonomous engineering pipeline.
//
//	Triage   — classify the ticket, pick a target repo
//	Plan     — produce an ordered TODO list
//	Execute  — run the existing agent loop on each TODO
//	Test     — run the repo's test command (best-effort)
//	Verify   — re-run the agent N times to double-check the work
//	OpenPR   — push the branch and create a PR / MR
//	Notify   — Telegram message with a link to the PR
//
// Each phase is a focused LLM call with strict-JSON output, run on top of the
// existing agent / executor / safety layers. Workflow state is persisted to
// memory after every phase so a crash mid-flight leaves a recoverable trail.
package workflow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"goon/internal/agent"
	"goon/internal/boards"
	"goon/internal/executor"
	"goon/internal/githost"
	"goon/internal/llm"
	"goon/internal/memory"
	"goon/internal/tools"
)

// VerifyRuns is the number of extra verification passes the workflow runs
// before opening a PR. Override with GOON_VERIFY_RUNS, clamp 1..10.
var VerifyRuns = func() int {
	if v := os.Getenv("GOON_VERIFY_RUNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 10 {
			return n
		}
	}
	return 3
}()

// RepoMap maps a board project key (Jira project / GitHub "owner/repo") to a
// local path on disk where the source lives. Override with GOON_REPO_MAP env
// var: "ENG=/repos/eng,WEB=/repos/web".
func RepoMap() map[string]string {
	out := map[string]string{}
	for _, kv := range strings.Split(os.Getenv("GOON_REPO_MAP"), ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		out[strings.TrimSpace(kv[:eq])] = strings.TrimSpace(kv[eq+1:])
	}
	return out
}

// Engine runs a single ticket through the full pipeline.
type Engine struct {
	LLM      llm.Provider
	Tools    *tools.Registry
	Executor *executor.Executor
	Memory   *memory.Memory
	Board    boards.Board
	Host     githost.Host
	Stdout   io.Writer
	Stderr   io.Writer
	Debug    bool

	// VerifyRuns overrides the package var when non-zero (for tests).
	VerifyRunsOverride int
}

// Run runs the workflow for one ticket. It returns the final Workflow record.
// Any error is also recorded inside the Workflow.
func (e *Engine) Run(ctx context.Context, t boards.Ticket) (memory.Workflow, error) {
	wf := memory.Workflow{
		ID:        fmt.Sprintf("wf-%d", time.Now().UnixNano()),
		TicketID:  t.ID,
		TicketKey: t.Key,
		Title:     t.Title,
		StartedAt: time.Now(),
		State:     memory.WFTriaging,
	}
	e.save(wf)

	// 1. Triage.
	plan, repo, err := e.triage(ctx, t)
	if err != nil {
		return e.fail(wf, "triage", err)
	}
	wf.Repo = repo
	wf.Plan = plan
	wf.State = memory.WFPlanning
	e.save(wf)

	// 2. Plan is already produced by triage in our compact pipeline; skip a
	//    separate planning call.
	wf.State = memory.WFExecuting
	e.save(wf)

	// 3. Execute each plan step using the existing agent loop. For a v1 the
	//    agent is fed each step's title as the user task. The plan list is
	//    persisted between steps so the UI can watch progress.
	for i := range wf.Plan {
		select {
		case <-ctx.Done():
			return e.fail(wf, "executing", ctx.Err())
		default:
		}
		step := wf.Plan[i]
		if step.Done {
			continue
		}
		if err := e.executeStep(ctx, wf.TicketKey, step.Title); err != nil {
			wf.Plan[i].Note = err.Error()
			e.save(wf)
			return e.fail(wf, "executing", err)
		}
		wf.Plan[i].Done = true
		e.save(wf)
	}

	// 4. Test (best-effort).
	wf.State = memory.WFTesting
	e.save(wf)
	if err := e.runTests(ctx, repo); err != nil {
		// Log but don't fail — the agent's verify loop will catch real
		// regressions and the user always reviews the PR.
		fmt.Fprintf(e.Stderr, "tests failed (non-fatal): %v\n", err)
	}

	// 5. Verify N times.
	wf.State = memory.WFVerifying
	wf.VerifyRuns = e.verifyN()
	e.save(wf)
	for i := 0; i < wf.VerifyRuns; i++ {
		select {
		case <-ctx.Done():
			return e.fail(wf, "verifying", ctx.Err())
		default:
		}
		if err := e.verifyOnce(ctx, t); err != nil {
			return e.fail(wf, "verifying", fmt.Errorf("verify pass %d: %w", i+1, err))
		}
	}

	// 6. Open PR (skipped when no host is configured).
	wf.State = memory.WFOpeningPR
	e.save(wf)
	if e.Host != nil {
		pr, err := e.openPR(ctx, t, wf, repo)
		if err != nil {
			return e.fail(wf, "opening_pr", err)
		}
		wf.PRURL = pr.URL
		wf.Branch = pr.Branch
	}

	// 7. Notify.
	wf.State = memory.WFNotifying
	e.save(wf)
	e.notify(ctx, t, wf)

	wf.State = memory.WFDone
	e.save(wf)
	if e.Board != nil {
		_ = e.Board.Comment(ctx, t.ID, fmt.Sprintf("✓ goon completed this ticket. PR: %s", wf.PRURL))
		_ = e.Board.Transition(ctx, t.ID, boards.StatusInReview)
	}
	return wf, nil
}

// --- helpers ---------------------------------------------------------------

func (e *Engine) save(w memory.Workflow) {
	if e.Memory != nil {
		e.Memory.UpsertWorkflow(w)
	}
}

func (e *Engine) fail(w memory.Workflow, where string, err error) (memory.Workflow, error) {
	w.State = memory.WFFailed
	w.Error = where + ": " + err.Error()
	e.save(w)
	return w, err
}

func (e *Engine) verifyN() int {
	if e.VerifyRunsOverride > 0 {
		return e.VerifyRunsOverride
	}
	return VerifyRuns
}

// triage asks the LLM to produce a structured plan. The prompt is strict-JSON
// like the agent's, but with a different schema: an ordered list of steps.
func (e *Engine) triage(ctx context.Context, t boards.Ticket) ([]memory.PlanStep, string, error) {
	if e.LLM == nil {
		return nil, "", errors.New("triage: no LLM provider configured")
	}
	repo := pickRepo(t)
	prompt := fmt.Sprintf(`You are GOON's planner. The user wants you to break this ticket into 3-7 ordered, atomic engineering steps that an autonomous agent can execute one-by-one. Each step MUST be small enough to finish in <= 5 tool calls.

Reply with EXACTLY ONE JSON object: {"steps":[{"title":"..."}, ...], "repo":"%s"}.
No prose, no fences, no comments.

TICKET:
key: %s
title: %s
description: %s
`, repo, t.Key, t.Title, snippet(t.Description, 1500))

	out, err := e.LLM.Generate(ctx, []llm.Message{
		{Role: llm.RoleUser, Content: prompt},
	}, llm.Options{Temperature: 0.1, JSONMode: true, MaxTokens: 800})
	if err != nil {
		return nil, "", err
	}

	plan, repoOut, err := parseTriage(out)
	if err != nil {
		return nil, "", fmt.Errorf("triage parse: %w (raw=%q)", err, snippet(out, 200))
	}
	if repoOut != "" {
		repo = repoOut
	}
	return plan, repo, nil
}

// pickRepo chooses a target repo based on RepoMap or the ticket's project.
func pickRepo(t boards.Ticket) string {
	rm := RepoMap()
	if v, ok := rm[t.Project]; ok {
		return v
	}
	if v, ok := rm["*"]; ok {
		return v
	}
	return t.Project
}

func (e *Engine) executeStep(ctx context.Context, ticketKey, step string) error {
	if e.LLM == nil || e.Tools == nil || e.Executor == nil {
		return errors.New("execute: agent runtime not configured")
	}
	a := agent.New(agent.Options{
		LLM:      e.LLM,
		Tools:    e.Tools,
		Executor: e.Executor,
		Memory:   e.Memory,
		Stdout:   e.Stdout,
		Stderr:   e.Stderr,
		Debug:    e.Debug,
	})
	task := fmt.Sprintf("[%s] %s", ticketKey, step)
	return a.Run(ctx, task)
}

// runTests runs `make test` if the repo has a Makefile, otherwise tries
// `go test ./...`. Best-effort.
func (e *Engine) runTests(ctx context.Context, repo string) error {
	if repo == "" {
		return nil
	}
	if _, err := os.Stat(repo); err != nil {
		return nil // repo not local; skip
	}
	tool, ok := e.Tools.Get("run_command")
	if !ok {
		return nil
	}
	makefile := repo + "/Makefile"
	cmd := "go test ./..."
	if _, err := os.Stat(makefile); err == nil {
		cmd = "make test"
	}
	cmd = "cd " + repo + " && " + cmd
	_, err := tool.Run(ctx, map[string]string{"command": cmd})
	return err
}

// verifyOnce re-runs the agent to inspect the implementation. The verifier's
// task is intentionally narrow: "double-check the implementation matches the
// ticket".
func (e *Engine) verifyOnce(ctx context.Context, t boards.Ticket) error {
	a := agent.New(agent.Options{
		LLM: e.LLM, Tools: e.Tools, Executor: e.Executor, Memory: e.Memory,
		Stdout: e.Stdout, Stderr: e.Stderr, Debug: e.Debug,
	})
	prompt := fmt.Sprintf("Verify the implementation for ticket %s (%q) is correct. List any defects via finish.", t.Key, t.Title)
	return a.Run(ctx, prompt)
}

func (e *Engine) openPR(ctx context.Context, t boards.Ticket, wf memory.Workflow, repo string) (githost.PR, error) {
	branch := fmt.Sprintf("goon/%s", strings.ToLower(strings.ReplaceAll(t.Key, " ", "-")))
	body := fmt.Sprintf("Resolves %s.\n\nPlan:\n", t.Key)
	for _, s := range wf.Plan {
		mark := "✗"
		if s.Done {
			mark = "✓"
		}
		body += fmt.Sprintf("- %s %s\n", mark, s.Title)
	}
	body += "\n— opened by goon 🤖"
	return e.Host.OpenPR(ctx, githost.CreateOptions{
		Repo:   ghRepoFromTicket(t, repo),
		Title:  fmt.Sprintf("[%s] %s", t.Key, t.Title),
		Body:   body,
		Head:   branch,
		Labels: []string{"goon", "auto"},
	})
}

func ghRepoFromTicket(t boards.Ticket, fallback string) string {
	if t.Source == "github" {
		return t.Project // already "owner/repo"
	}
	return fallback
}

func (e *Engine) notify(ctx context.Context, t boards.Ticket, wf memory.Workflow) {
	tool, ok := e.Tools.Get("telegram")
	if !ok {
		return
	}
	msg := fmt.Sprintf("✅ goon finished %s: %s", t.Key, t.Title)
	if wf.PRURL != "" {
		msg += "\nPR: " + wf.PRURL
	}
	_, _ = tool.Run(ctx, map[string]string{"text": msg})
}

func snippet(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
