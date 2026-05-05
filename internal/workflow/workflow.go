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

	"github.com/harisaginting/goon/internal/agent"
	"github.com/harisaginting/goon/internal/boards"
	"github.com/harisaginting/goon/internal/executor"
	"github.com/harisaginting/goon/internal/githost"
	"github.com/harisaginting/goon/internal/llm"
	"github.com/harisaginting/goon/internal/logx"
	"github.com/harisaginting/goon/internal/memory"
	"github.com/harisaginting/goon/internal/safety"
	"github.com/harisaginting/goon/internal/tools"
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

	// Config — when zero, LoadConfig is consulted lazily inside Run().
	// Provide explicitly to skip on-disk config loading (useful in tests).
	Config WorkflowConfig
}

// Run runs the workflow for one ticket. It returns the final Workflow record.
// Any error is also recorded inside the Workflow.
//
// At every phase boundary we look up user-defined hook commands from the
// loaded WorkflowConfig and run them sequentially. A failed hook fails the
// phase (except on_failure, which is best-effort).
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
	logx.Info("workflow.start", "wf", wf.ID, "ticket", t.Key, "title", t.Title)
	defer func() {
		logx.Info("workflow.end", "wf", wf.ID, "ticket", t.Key,
			"state", string(wf.State), "pr_url", wf.PRURL,
			"duration_ms", time.Since(wf.StartedAt).Milliseconds())
	}()

	// Resolve config: explicit Config wins; otherwise load from disk.
	cfg := e.Config
	if cfg.Version == 0 {
		loaded, _, _ := LoadConfig("")
		cfg = loaded
	}
	hr := &HookRunner{
		Stdout: e.Stdout, Stderr: e.Stderr,
		Validator: safety.Default(),
	}

	branch := branchName(cfg.BranchPrefix, t.Key)
	wf.Branch = branch

	// --- Declarative stages mode ---------------------------------------------
	// When the user provides cfg.Stages, the built-in 7-phase pipeline is
	// replaced wholesale with their declared sequence. Hooks (before_triage,
	// on_failure, etc.) and PR/notify still fire so existing config keys keep
	// working; the *body* of the workflow is now whatever the user wrote.
	if len(cfg.Stages) > 0 {
		return e.runStages(ctx, wf, t, cfg, hr, branch)
	}

	// before_triage hook
	hctx := FromTicket(t, "", branch, nil)
	if err := hr.Run(ctx, HookBeforeTriage, cfg.Hook(HookBeforeTriage), hctx); err != nil {
		e.runFailureHook(ctx, hr, cfg, hctx, err)
		return e.fail(wf, "before_triage", err)
	}

	// 1. Triage.
	plan, repo, err := e.triage(ctx, t)
	if err != nil {
		e.runFailureHook(ctx, hr, cfg, hctx, err)
		return e.fail(wf, "triage", err)
	}
	wf.Repo = repo
	wf.Plan = plan
	wf.State = memory.WFPlanning
	e.save(wf)

	// before_execute hook
	hctx = FromTicket(t, repo, branch, plan)
	if err := hr.Run(ctx, HookBeforeExecute, cfg.Hook(HookBeforeExecute), hctx); err != nil {
		e.runFailureHook(ctx, hr, cfg, hctx, err)
		return e.fail(wf, "before_execute", err)
	}

	wf.State = memory.WFExecuting
	e.save(wf)

	// 3. Execute each plan step.
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
			e.runFailureHook(ctx, hr, cfg, hctx, err)
			return e.fail(wf, "executing", err)
		}
		wf.Plan[i].Done = true
		e.save(wf)
	}

	// after_execute hook
	hctx = FromTicket(t, repo, branch, wf.Plan)
	if err := hr.Run(ctx, HookAfterExecute, cfg.Hook(HookAfterExecute), hctx); err != nil {
		e.runFailureHook(ctx, hr, cfg, hctx, err)
		return e.fail(wf, "after_execute", err)
	}

	// 4. Test phase.
	wf.State = memory.WFTesting
	e.save(wf)
	if err := hr.Run(ctx, HookBeforeTest, cfg.Hook(HookBeforeTest), hctx); err != nil {
		e.runFailureHook(ctx, hr, cfg, hctx, err)
		return e.fail(wf, "before_test", err)
	}
	testErr := e.runTests(ctx, repo, cfg.TestCommand)
	if testErr != nil {
		// Tests failing is logged but not fatal — the verify pass catches real regressions.
		fmt.Fprintf(e.Stderr, "tests failed (non-fatal): %v\n", testErr)
	} else {
		if err := hr.Run(ctx, HookAfterTest, cfg.Hook(HookAfterTest), hctx); err != nil {
			e.runFailureHook(ctx, hr, cfg, hctx, err)
			return e.fail(wf, "after_test", err)
		}
	}

	// 5. Verify N times.
	wf.State = memory.WFVerifying
	wf.VerifyRuns = e.verifyN(cfg)
	e.save(wf)
	if err := hr.Run(ctx, HookBeforeVerify, cfg.Hook(HookBeforeVerify), hctx); err != nil {
		e.runFailureHook(ctx, hr, cfg, hctx, err)
		return e.fail(wf, "before_verify", err)
	}
	for i := 0; i < wf.VerifyRuns; i++ {
		select {
		case <-ctx.Done():
			return e.fail(wf, "verifying", ctx.Err())
		default:
		}
		if err := e.verifyOnce(ctx, t); err != nil {
			e.runFailureHook(ctx, hr, cfg, hctx, err)
			return e.fail(wf, "verifying", fmt.Errorf("verify pass %d: %w", i+1, err))
		}
	}
	if err := hr.Run(ctx, HookAfterVerify, cfg.Hook(HookAfterVerify), hctx); err != nil {
		e.runFailureHook(ctx, hr, cfg, hctx, err)
		return e.fail(wf, "after_verify", err)
	}

	// 6. Open PR.
	wf.State = memory.WFOpeningPR
	e.save(wf)
	if err := hr.Run(ctx, HookBeforePR, cfg.Hook(HookBeforePR), hctx); err != nil {
		e.runFailureHook(ctx, hr, cfg, hctx, err)
		return e.fail(wf, "before_pr", err)
	}
	if e.Host != nil {
		pr, err := e.openPR(ctx, t, wf, repo, cfg)
		if err != nil {
			e.runFailureHook(ctx, hr, cfg, hctx, err)
			return e.fail(wf, "opening_pr", err)
		}
		wf.PRURL = pr.URL
		wf.Branch = pr.Branch
	}
	if err := hr.Run(ctx, HookAfterPR, cfg.Hook(HookAfterPR), hctx); err != nil {
		// after_pr failure is non-fatal — PR is already up
		fmt.Fprintf(e.Stderr, "after_pr hook failed (non-fatal): %v\n", err)
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

// runFailureHook fires the on_failure hook list as a best-effort. Errors are
// logged but never propagate further.
func (e *Engine) runFailureHook(ctx context.Context, hr *HookRunner, cfg WorkflowConfig, hctx HookCtx, cause error) {
	cmds := cfg.Hook(HookOnFailure)
	if len(cmds) == 0 {
		return
	}
	hctxCopy := hctx
	if hctxCopy.Title == "" {
		hctxCopy.Title = cause.Error()
	}
	if err := hr.Run(ctx, HookOnFailure, cmds, hctxCopy); err != nil {
		fmt.Fprintf(e.Stderr, "on_failure hook itself failed (best-effort): %v\n", err)
	}
}

// branchName turns "ENG-123" into "goon/eng-123" using the configured prefix.
func branchName(prefix, key string) string {
	if prefix == "" {
		prefix = "goon/"
	}
	if !strings.HasSuffix(prefix, "/") && !strings.HasSuffix(prefix, "-") && !strings.HasSuffix(prefix, "_") {
		prefix += "/"
	}
	return prefix + strings.ToLower(strings.ReplaceAll(key, " ", "-"))
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

func (e *Engine) verifyN(cfg WorkflowConfig) int {
	if e.VerifyRunsOverride > 0 {
		return e.VerifyRunsOverride
	}
	if cfg.VerifyRuns > 0 {
		return cfg.VerifyRuns
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

// runTests runs the workflow-config TestCommand if set, else auto-detects
// (make test if Makefile exists, otherwise go test ./...). Best-effort.
func (e *Engine) runTests(ctx context.Context, repo, override string) error {
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
	cmd := strings.TrimSpace(override)
	if cmd == "" {
		makefile := repo + "/Makefile"
		cmd = "go test ./..."
		if _, err := os.Stat(makefile); err == nil {
			cmd = "make test"
		}
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

func (e *Engine) openPR(ctx context.Context, t boards.Ticket, wf memory.Workflow, repo string, cfg WorkflowConfig) (githost.PR, error) {
	branch := branchName(cfg.BranchPrefix, t.Key)

	hctx := FromTicket(t, repo, branch, wf.Plan)

	title, err := RenderTemplate("pr_title", cfg.PRTitleTemplate, hctx)
	if err != nil || title == "" {
		title = fmt.Sprintf("[%s] %s", t.Key, t.Title)
	}
	body, err := RenderTemplate("pr_body", cfg.PRBodyTemplate, hctx)
	if err != nil || body == "" {
		// Fall back to the historical body if the template fails.
		body = fmt.Sprintf("Resolves %s.\n\nPlan:\n", t.Key)
		for _, s := range wf.Plan {
			mark := "✗"
			if s.Done {
				mark = "✓"
			}
			body += fmt.Sprintf("- %s %s\n", mark, s.Title)
		}
		body += "\n— opened by goon 🤖"
	}

	labels := append([]string{"goon", "auto"}, cfg.ExtraLabels...)
	return e.Host.OpenPR(ctx, githost.CreateOptions{
		Repo:   ghRepoFromTicket(t, repo),
		Title:  title,
		Body:   body,
		Head:   branch,
		Labels: labels,
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

// runStages drives the user-defined pipeline declared in cfg.Stages. The
// built-in phases are bypassed entirely. Hooks still fire at the equivalent
// boundaries (before/after each named stage), the PR is opened if a stage
// is named "pr" — or always at the end when cfg.PRTitleTemplate is set —
// and the notify step runs at the very end if a notify channel is wired.
func (e *Engine) runStages(ctx context.Context, wf memory.Workflow, t boards.Ticket, cfg WorkflowConfig, hr *HookRunner, branch string) (memory.Workflow, error) {
	repo := pickRepo(t)
	wf.Repo = repo
	wf.State = memory.WFExecuting
	e.save(wf)

	hctx := FromTicket(t, repo, branch, nil)

	// before_triage fires once at the start of the declarative pipeline so
	// users with existing hook configs get the same setup behavior they had.
	if err := hr.Run(ctx, HookBeforeTriage, cfg.Hook(HookBeforeTriage), hctx); err != nil {
		e.runFailureHook(ctx, hr, cfg, hctx, err)
		return e.fail(wf, "before_triage", err)
	}

	runner := &StageRunner{
		LLM:      e.LLM,
		Tools:    e.Tools,
		Executor: e.Executor,
		Memory:   e.Memory,
		Stdout:   e.Stdout,
		Stderr:   e.Stderr,
		Debug:    e.Debug,
	}
	state, err := runner.RunStages(ctx, cfg, t, branch, repo)
	if err != nil {
		e.runFailureHook(ctx, hr, cfg, hctx, err)
		return e.fail(wf, "stages", err)
	}

	// Mirror any "steps" output into wf.Plan so the web UI / status command
	// keep showing per-step progress for stages whose JSON returned a list.
	if plan := planFromState(state); len(plan) > 0 {
		wf.Plan = plan
		e.save(wf)
	}

	// after_execute fires after every declared stage has run.
	hctx = FromTicket(t, repo, branch, wf.Plan)
	if err := hr.Run(ctx, HookAfterExecute, cfg.Hook(HookAfterExecute), hctx); err != nil {
		e.runFailureHook(ctx, hr, cfg, hctx, err)
		return e.fail(wf, "after_execute", err)
	}

	// PR + notify still run if the user has them configured. They're optional
	// for non-engineering pipelines (marketing, sales, ops) — skipping is
	// triggered by the absence of e.Host or by setting pr_title_template:""
	// AND no GitHub/GitLab tokens.
	if e.Host != nil && (cfg.PRTitleTemplate != "" || cfg.PRBodyTemplate != "") {
		wf.State = memory.WFOpeningPR
		e.save(wf)
		if err := hr.Run(ctx, HookBeforePR, cfg.Hook(HookBeforePR), hctx); err != nil {
			e.runFailureHook(ctx, hr, cfg, hctx, err)
			return e.fail(wf, "before_pr", err)
		}
		pr, err := e.openPR(ctx, t, wf, repo, cfg)
		if err != nil {
			e.runFailureHook(ctx, hr, cfg, hctx, err)
			return e.fail(wf, "opening_pr", err)
		}
		wf.PRURL = pr.URL
		wf.Branch = pr.Branch
		if err := hr.Run(ctx, HookAfterPR, cfg.Hook(HookAfterPR), hctx); err != nil {
			fmt.Fprintf(e.Stderr, "after_pr hook failed (non-fatal): %v\n", err)
		}
	}

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

// planFromState scans the StageRunner state for any output named "plan" or
// containing a "steps" key, and converts it to []memory.PlanStep so the web
// UI / `goon status` keep showing per-step progress under declarative mode.
func planFromState(state *StageState) []memory.PlanStep {
	if state == nil {
		return nil
	}
	for _, key := range []string{"plan", "triage", "steps"} {
		if v, ok := state.Stages[key]; ok {
			if plan := planFromValue(v); len(plan) > 0 {
				return plan
			}
		}
	}
	return nil
}

func planFromValue(v any) []memory.PlanStep {
	switch x := v.(type) {
	case map[string]any:
		if steps, ok := x["steps"].([]any); ok {
			return planFromList(steps)
		}
	case []any:
		return planFromList(x)
	}
	return nil
}

func planFromList(items []any) []memory.PlanStep {
	out := make([]memory.PlanStep, 0, len(items))
	for i, it := range items {
		switch x := it.(type) {
		case string:
			s := strings.TrimSpace(x)
			if s == "" {
				continue
			}
			out = append(out, memory.PlanStep{Index: i, Title: s, Done: true})
		case map[string]any:
			title, _ := x["title"].(string)
			title = strings.TrimSpace(title)
			if title == "" {
				continue
			}
			out = append(out, memory.PlanStep{Index: i, Title: title, Done: true})
		}
	}
	return out
}
