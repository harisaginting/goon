package workflow

// graph.go implements goon's role-graph executor — the engine behind the
// default pipeline and any user-defined cfg.Stages. It replaces the old linear
// StageRunner: nodes have ROLES (analyst | executor | reviewer | loop | notify),
// each emits an ACTION (next | ask | approve | reject | answer), and routing
// follows that action's target list (fan-out supported). The traversal runs as
// an Engine method so it can reuse the Engine's plumbing — triage, repo
// resolution/cloning, the agent loop in the repo, PR opening, hooks, Telegram
// notify — and, crucially, PAUSE at a human reviewer via the same gate/resume
// mechanism the linear pipeline uses (wf.Stage + PendingQuestionID).
//
// Resume: a human reviewer queues a question and returns errPaused; Engine.Run
// returns the workflow in WFAwaitingApproval. On a later tick the daemon's
// ResumableWorkflow picks it up, Engine.Run re-enters runGraph, and the walk
// resumes AT wf.Stage (the reviewer node) — earlier nodes are not re-run.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"text/template"

	"github.com/harisaginting/goon/internal/agent"
	"github.com/harisaginting/goon/internal/boards"
	"github.com/harisaginting/goon/internal/learnings"
	"github.com/harisaginting/goon/internal/llm"
	"github.com/harisaginting/goon/internal/memory"
	"github.com/harisaginting/goon/internal/repository"
	"github.com/harisaginting/goon/internal/tools"
)

// StageState is the data block visible to node templates. It carries the
// HookCtx fields (Key/Title/Repo/Branch/...) plus a Stages map of every
// completed node's parsed output, and a few durable mirrors (PRURL, Note) so
// templates like a notify message can reference them after the fact.
type StageState struct {
	HookCtx
	Stages      map[string]any // node name (or Output) → parsed output
	Item        any            // reserved for future for_each iteration
	Attempt     int            // 1-based pass index when Repeat > 1
	Ask         any            // the analyst's most-recent answer (for the re-running node)
	AskQuestion string         // the question being put to the analyst
	PRURL       string         // mirror of wf.PRURL for templates
	Note        string         // mirror of wf.Note (last executor result)
}

// On-error policies (StageConfig.OnError).
const (
	OnErrorFail     = "fail"     // default; abort the workflow
	OnErrorContinue = "continue" // log and move on as if the node emitted next
	OnErrorWarn     = "warn"     // alias for continue but with a louder log line
)

// graphCtx is shared state for the duration of a single runGraph call.
type graphCtx struct {
	cfg         WorkflowConfig
	hr          *HookRunner
	branch      string
	autoApprove bool
}

// runGraph executes the role-graph (cfg.Stages) for one ticket. It is the
// stages-mode entry point from Engine.Run, replacing the old runStages.
func (e *Engine) runGraph(ctx context.Context, wf memory.Workflow, t boards.Ticket, cfg WorkflowConfig, hr *HookRunner, branch string) (memory.Workflow, error) {
	if err := validateStages(cfg.Stages); err != nil {
		return e.fail(wf, "validate", err)
	}
	gc := &graphCtx{cfg: cfg, hr: hr, branch: branch, autoApprove: e.isAutoApprove(cfg)}

	stageMap := make(map[string]StageConfig, len(cfg.Stages))
	for _, s := range cfg.Stages {
		stageMap[s.Name] = s
	}

	state := &StageState{
		HookCtx: FromTicketMulti(t, wf.Repo, wf.Repos, branch, wf.Plan),
		Stages:  map[string]any{},
		PRURL:   wf.PRURL,
		Note:    wf.Note,
	}

	// before_triage fires once at the very start of a fresh run.
	fresh := len(wf.Plan) == 0 && (wf.Stage == "" || wf.Stage == "triage")
	if fresh {
		if err := hr.Run(ctx, HookBeforeTriage, cfg.Hook(HookBeforeTriage), state.HookCtx); err != nil {
			e.runFailureHook(ctx, hr, cfg, state.HookCtx, err)
			return e.fail(wf, "before_triage", err)
		}
	}

	// Entry node: resume at wf.Stage when it names a node (and isn't the
	// terminal "done"); otherwise start at the first node.
	entry := cfg.Stages[0].Name
	if wf.Stage != "" && wf.Stage != "done" {
		if _, ok := stageMap[wf.Stage]; ok {
			entry = wf.Stage
		}
	}

	queue := []string{entry}
	loops := map[string]int{}  // reject arcs "from->to"
	asks := map[string]int{}   // ask rounds per node
	iters := map[string]int{}  // loop-node iterations
	visits := map[string]int{} // hard per-node visit cap
	const maxVisits = 50

	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if name == "" || name == "end" {
			continue
		}
		select {
		case <-ctx.Done():
			return e.fail(wf, name, ctx.Err())
		default:
		}
		s, ok := stageMap[name]
		if !ok {
			return e.fail(wf, name, fmt.Errorf("graph: node %q not found", name))
		}
		visits[name]++
		if visits[name] > maxVisits {
			return e.fail(wf, name, fmt.Errorf("node %s exceeded %d visits — cyclic graph without a loop guard (add a loop node)", name, maxVisits))
		}

		// ── loop routing node (pure routing, no model call) ───────────────
		if strings.EqualFold(strings.TrimSpace(s.Type), RoleLoop) {
			max := s.MaxLoops
			if max <= 0 {
				max = 3
			}
			iters[name]++
			state.Stages[stageKey(s)] = map[string]any{"iteration": iters[name], "max": max}
			if iters[name] <= max && len(targetsOf(s.OnNext)) > 0 {
				e.graphLogf("⟳ loop %s iteration %d/%d\n", name, iters[name], max)
				queue = append(targetsOf(s.OnNext), queue...)
			} else {
				exit := strings.TrimSpace(s.OnDone)
				e.graphLogf("⟳ loop %s done after %d iteration(s) → %s\n", name, iters[name]-1, orWord(exit, "end"))
				if exit != "" && exit != "end" {
					queue = append([]string{exit}, queue...)
				}
			}
			continue
		}

		// ── conditional skip ──────────────────────────────────────────────
		if s.If != "" {
			cond, err := renderTemplate("if["+name+"]", s.If, state)
			if err != nil {
				return e.fail(wf, name, err)
			}
			if isFalsy(cond) {
				e.graphLogf("⏭  skip %s (if=%q)\n", name, strings.TrimSpace(cond))
				queue = append(nextTargets(cfg.Stages, s), queue...)
				continue
			}
		}

		// Mark the current node and persist so a pause resumes exactly here.
		wf.Stage = name
		e.save(wf)

		// ── run (with repeat) ─────────────────────────────────────────────
		repeat := s.Repeat
		if repeat <= 0 {
			repeat = 1
		}
		var out any
		var action string
		var rerr error
		for pass := 1; pass <= repeat; pass++ {
			state.Attempt = pass
			out, action, rerr = e.runNode(ctx, s, state, &wf, t, gc)
			if rerr != nil {
				break
			}
		}
		state.Attempt = 0

		// Pause (human reviewer awaiting an answer): state already saved by
		// the gate. Return cleanly; the daemon resumes us next tick.
		if errors.Is(rerr, errPaused) {
			return wf, nil
		}

		// ── on_error ──────────────────────────────────────────────────────
		if rerr != nil {
			switch strings.ToLower(strings.TrimSpace(s.OnError)) {
			case OnErrorContinue, OnErrorWarn:
				e.graphWarnf("node %s failed (%s): %v\n", name, s.OnError, rerr)
				action = ActionNext
			default:
				e.runFailureHook(ctx, hr, cfg, state.HookCtx, rerr)
				return e.fail(wf, name, rerr)
			}
		}

		// Store output + refresh durable mirrors for downstream templates.
		state.Stages[stageKey(s)] = out
		state.PRURL = wf.PRURL
		state.Note = wf.Note
		state.Repo = wf.Repo
		state.Repos = wf.Repos
		state.Plan = wf.Plan

		// reject_if override (advanced; ask short-circuits it).
		if s.RejectIf != "" && action != ActionAsk {
			cond, err := renderTemplate("reject_if["+name+"]", s.RejectIf, state)
			if err != nil {
				return e.fail(wf, name, err)
			}
			if !isFalsy(cond) {
				action = ActionReject
			}
		}

		// ── route by action ───────────────────────────────────────────────
		switch action {
		case ActionAsk:
			target := strings.TrimSpace(s.Ask)
			if target == "" {
				return e.fail(wf, name, fmt.Errorf("node %s emitted ask but has no `ask` analyst wired", name))
			}
			max := s.MaxLoops
			if max <= 0 {
				max = 3
			}
			asks[name]++
			if asks[name] > max {
				return e.fail(wf, name, fmt.Errorf("node %s: exceeded %d ask rounds", name, max))
			}
			ans, err := e.consultAnalyst(ctx, stageMap, target, state)
			if err != nil {
				return e.fail(wf, name, err)
			}
			state.Ask = ans
			queue = append([]string{name}, queue...) // re-run this node with the answer
		case ActionApprove:
			queue = append(targetsOf(s.OnApprove), queue...)
		case ActionReject:
			tgt := strings.TrimSpace(s.OnReject)
			if tgt == "" || tgt == "end" {
				return e.fail(wf, name, fmt.Errorf("node %s rejected but has no on_reject target", name))
			}
			max := s.MaxLoops
			if max <= 0 {
				max = 3
			}
			key := name + "->" + tgt
			loops[key]++
			if loops[key] > max {
				return e.fail(wf, name, fmt.Errorf("node %s: max reject loops (%d) to %q reached", name, max, tgt))
			}
			e.graphLogf("↩ %s rejected (loop %d/%d) → %s\n", name, loops[key], max, tgt)
			queue = append([]string{tgt}, queue...)
		default: // ActionNext / ActionAnswer / ""
			queue = append(nextTargets(cfg.Stages, s), queue...)
		}
	}

	// Distil learnings from the run (HISTORY.md + a short distillation pass),
	// mirroring the linear pipeline's update_memory phase. Best-effort.
	wf.State = memory.WFUpdatingMemory
	e.save(wf)
	_ = learnings.Capture(ctx, learnings.Options{
		Task:     fmt.Sprintf("[%s] %s", t.Key, t.Title),
		Outcome:  "ok",
		LLM:      e.LLM,
		Tools:    e.Tools,
		Executor: e.Executor,
		Memory:   e.Memory,
		Stdout:   e.Stdout,
		Stderr:   e.Stderr,
		Debug:    e.Debug,
	})

	// Graph finished — mark done and (for code work) hand the ticket back.
	wf.Stage = "done"
	wf.State = memory.WFDone
	wf.PendingQuestionID = ""
	e.save(wf)
	// Board hand-off only for real board tickets — a scheduled automation job
	// has no ticket to comment on / transition.
	if e.Board != nil && t.Source != "schedule" {
		msg := "✓ goon completed this ticket."
		if wf.PRURL != "" {
			msg += " PR: " + wf.PRURL
		} else if !memory.WorkflowNeedsRepo(wf) {
			msg += " (no code changes — see goon's memory notes for the outcome)"
		}
		_ = e.Board.Comment(ctx, t.ID, msg)
		_ = e.Board.Transition(ctx, t.ID, boards.StatusInReview)
	}
	return wf, nil
}

// runNode dispatches to the per-role executor and returns the node's output
// plus the action it emitted.
func (e *Engine) runNode(ctx context.Context, s StageConfig, state *StageState, wf *memory.Workflow, t boards.Ticket, gc *graphCtx) (any, string, error) {
	switch strings.ToLower(strings.TrimSpace(s.Type)) {
	case RoleAnalyst:
		out, err := e.runAnalystNode(ctx, s, state)
		return out, ActionAnswer, err
	case RoleExecutor:
		return e.runExecutorNode(ctx, s, state, wf, t, gc)
	case RoleReviewer:
		return e.runReviewerNode(ctx, s, state, wf, t, gc)
	case RoleNotify:
		out, err := e.runNotifyNode(ctx, s, state)
		return out, ActionNext, err
	default:
		return nil, "", fmt.Errorf("unknown role %q for node %s", s.Type, s.Name)
	}
}

// runAnalystNode answers a question (state.AskQuestion). It first fetches any
// configured URLs to ground the answer in fresh, project-specific knowledge,
// then calls the model. The analyst never routes forward on its own — it is
// reached only via another node's `ask`.
func (e *Engine) runAnalystNode(ctx context.Context, s StageConfig, state *StageState) (any, error) {
	prov := e.LLM
	if s.Provider != "" || s.Model != "" {
		p, err := llm.NewWithOverrides(s.Provider, s.Model)
		if err != nil {
			return nil, fmt.Errorf("analyst %q: %w", s.Name, err)
		}
		prov = p
	}
	if prov == nil {
		return nil, errors.New("analyst: no LLM provider configured")
	}
	var kb strings.Builder
	for _, u := range s.URLs {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		body, err := e.fetchURL(ctx, u)
		if err != nil {
			e.graphWarnf("analyst %s: fetch %s failed: %v\n", s.Name, u, err)
			continue
		}
		if body = strings.TrimSpace(body); body != "" {
			kb.WriteString("\n# Source: " + u + "\n" + snippet(body, 4000) + "\n")
		}
	}
	prompt := strings.TrimSpace(s.Prompt)
	if prompt == "" {
		prompt = "Answer the question precisely, using the repository context and any reference material provided.\n\nQuestion:\n{{.AskQuestion}}"
	}
	rendered, err := renderTemplate("prompt["+s.Name+"]", prompt, state)
	if err != nil {
		return nil, err
	}
	if kb.Len() > 0 {
		rendered = "Reference material you fetched:\n" + kb.String() + "\n\n" + rendered
	}
	var msgs []llm.Message
	if sys := strings.TrimSpace(s.System); sys != "" {
		r, err := renderTemplate("system["+s.Name+"]", sys, state)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: r})
	}
	msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: rendered})
	out, err := prov.Generate(ctx, msgs, llm.Options{Temperature: s.Temperature, MaxTokens: s.MaxTokens, JSONMode: s.JSONMode})
	if err != nil {
		return nil, err
	}
	if s.JSONMode {
		if chunk, e2 := extractJSONObject(out); e2 == nil {
			var v any
			if json.Unmarshal([]byte(chunk), &v) == nil {
				return v, nil
			}
		}
	}
	return strings.TrimSpace(out), nil
}

// runExecutorNode does the technical work. Three shapes:
//   - do: "open_pr"  → open the PR with goon's git-host machinery (no agent).
//   - task set       → run the agent loop on that task (in the repo if set).
//   - task empty     → the DEFAULT executor: triage the ticket into a plan,
//     resolve the assigned repo, then run the agent on each plan step.
//
// Emits ActionAsk when an agent task finishes with a line "ASK: <question>"
// and the node has an `ask` analyst wired; otherwise ActionNext.
func (e *Engine) runExecutorNode(ctx context.Context, s StageConfig, state *StageState, wf *memory.Workflow, t boards.Ticket, gc *graphCtx) (any, string, error) {
	if strings.TrimSpace(s.Do) == DoOpenPR {
		return e.execOpenPR(ctx, wf, t, gc)
	}
	if e.LLM == nil && s.Provider == "" && s.Model == "" {
		return nil, "", errors.New("executor: no LLM provider configured")
	}

	// Explicit-task executor (team graphs: architect, engineer, …).
	if strings.TrimSpace(s.Task) != "" {
		// A team graph wires several executors with explicit tasks and no
		// triage step, so nothing has resolved the repo yet. Ensure the
		// assigned repo is checked out so the agent edits the right tree.
		// Best-effort: a non-code pipeline (no repo) just runs in the working
		// dir rather than failing.
		if memory.WorkflowNeedsRepo(*wf) && !isLocalCheckout(wf.Repo) {
			if err := e.resolveRepoForGraph(ctx, wf, t); err != nil {
				e.graphWarnf("executor %s: no repo checkout (%v); running in the working dir\n", s.Name, err)
			} else {
				e.save(*wf)
			}
		}
		task, err := renderTemplate("task["+s.Name+"]", s.Task, state)
		if err != nil {
			return nil, "", err
		}
		res, err := e.runAgentForNode(ctx, s, wf.Repo, task)
		if err != nil {
			return res, "", err
		}
		if q, ok := parseAskSignal(res); ok && strings.TrimSpace(s.Ask) != "" {
			state.AskQuestion = q
			return res, ActionAsk, nil
		}
		if r := strings.TrimSpace(res); r != "" {
			wf.Note = r
		}
		return res, ActionNext, nil
	}

	// Default executor: triage (once) → resolve repo → execute the plan.
	if len(wf.Plan) == 0 {
		feedback := ""
		if wf.Approvals != nil {
			feedback = wf.Approvals["replan_feedback"]
		}
		tr, err := e.triageWithFeedback(ctx, t, feedback)
		if err != nil {
			return nil, "", err
		}
		wf.Plan = tr.Plan
		needs := tr.NeedsRepo
		wf.NeedsRepo = &needs
		wf.Repo = tr.Repo
		wf.Repos = tr.Repos
		if err := e.resolveRepoForGraph(ctx, wf, t); err != nil {
			return nil, "", err
		}
		if wf.Approvals != nil {
			delete(wf.Approvals, "replan_feedback")
		}
		wf.State = memory.WFPlanning
		e.save(*wf)
		e.graphLogf("[graph] %s planned %d step(s); repo=%s needs_repo=%v\n", t.Key, len(wf.Plan), wf.Repo, needs)
	}

	hctx := FromTicketMulti(t, wf.Repo, wf.Repos, gc.branch, wf.Plan)
	if err := gc.hr.Run(ctx, HookBeforeExecute, gc.cfg.Hook(HookBeforeExecute), hctx); err != nil {
		return nil, "", err
	}
	wf.State = memory.WFExecuting
	e.save(*wf)
	for i := range wf.Plan {
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		default:
		}
		if wf.Plan[i].Done {
			continue
		}
		stepCtx, cancel := context.WithTimeout(ctx, executeStepTimeout())
		res, err := e.executeStep(stepCtx, wf.Repo, wf.TicketKey, sanitizePlanStep(wf.Plan[i].Title))
		cancel()
		if err != nil {
			wf.Plan[i].Note = err.Error()
			e.save(*wf)
			return nil, "", err
		}
		wf.Plan[i].Done = true
		if r := strings.TrimSpace(res); r != "" {
			wf.Note = r
		}
		e.save(*wf)
	}
	if err := gc.hr.Run(ctx, HookAfterExecute, gc.cfg.Hook(HookAfterExecute), hctx); err != nil {
		return nil, "", err
	}
	return wf.Note, ActionNext, nil
}

// execOpenPR opens/updates the PR for the workflow's repo + branch using
// goon's git-host machinery + PR templates. No-op (emit next) for non-code
// tickets or when no git host is configured.
func (e *Engine) execOpenPR(ctx context.Context, wf *memory.Workflow, t boards.Ticket, gc *graphCtx) (any, string, error) {
	if e.Host == nil {
		return "", ActionNext, nil
	}
	if !memory.WorkflowNeedsRepo(*wf) || strings.TrimSpace(wf.Repo) == "" {
		e.graphLogf("[graph] %s — no repo, skipping open_pr\n", t.Key)
		return "", ActionNext, nil
	}
	hctx := FromTicketMulti(t, wf.Repo, wf.Repos, gc.branch, wf.Plan)
	wf.State = memory.WFOpeningPR
	e.save(*wf)
	if err := gc.hr.Run(ctx, HookBeforePR, gc.cfg.Hook(HookBeforePR), hctx); err != nil {
		return nil, "", err
	}
	pr, err := e.openPR(ctx, t, *wf, wf.Repo, gc.cfg)
	if err != nil {
		return nil, "", err
	}
	wf.PRURL = pr.URL
	wf.Branch = pr.Branch
	e.save(*wf)
	if err := gc.hr.Run(ctx, HookAfterPR, gc.cfg.Hook(HookAfterPR), hctx); err != nil {
		e.graphWarnf("after_pr hook failed (non-fatal): %v\n", err)
	}
	return pr.URL, ActionNext, nil
}

// runReviewerNode judges the executor's work. mode=human pauses and shows a
// person a change summary (approve/reject/feedback); mode=llm lets a model
// decide. Non-code tickets and auto-approve runs approve straight through.
func (e *Engine) runReviewerNode(ctx context.Context, s StageConfig, state *StageState, wf *memory.Workflow, t boards.Ticket, gc *graphCtx) (any, string, error) {
	if !memory.WorkflowNeedsRepo(*wf) {
		return "no code changes to review", ActionApprove, nil
	}
	if gc.autoApprove {
		return "auto-approved (auto_approve)", ActionApprove, nil
	}
	mode := strings.ToLower(strings.TrimSpace(s.Mode))
	if mode == "" {
		mode = ReviewerModeHuman
	}

	if mode == ReviewerModeLLM {
		dec, reason, question, err := e.llmReview(ctx, s, state, wf, t)
		if err != nil {
			return nil, "", err
		}
		switch dec {
		case "approve":
			return reason, ActionApprove, nil
		case "ask":
			if strings.TrimSpace(s.Ask) != "" {
				state.AskQuestion = question
				return reason, ActionAsk, nil
			}
			fallthrough
		default:
			e.rejectWithFeedback(wf, reason)
			return reason, ActionReject, nil
		}
	}

	// Human mode. On a fresh entry, build + post the summary; on resume the
	// question already exists, so resolve it by ID (no rebuild).
	if wf.PendingQuestionID == "" {
		summary := e.changeSummary(ctx, *wf, t)
		round := 0
		if wf.Approvals != nil {
			fmt.Sscanf(wf.Approvals["review_round"], "%d", &round)
		}
		round++
		if wf.Approvals == nil {
			wf.Approvals = map[string]string{}
		}
		wf.Approvals["review_round"] = fmt.Sprintf("%d", round)
		q := fmt.Sprintf("Review #%d — approve the change for %s (%s)?\n\n%s\n\n%s\nReply: approve / reject / <feedback to rework with>",
			round, t.Key, t.Title, summary, formatPlanForApproval(wf.Plan))
		ans, ready, err := e.gate(ctx, wf, t, s.Name, q)
		if err != nil {
			return nil, "", err
		}
		if !ready {
			return nil, "", errPaused
		}
		return e.applyReviewAnswer(ans, s, state, wf)
	}
	ans, ready, err := e.gate(ctx, wf, t, s.Name, "")
	if err != nil {
		return nil, "", err
	}
	if !ready {
		return nil, "", errPaused
	}
	return e.applyReviewAnswer(ans, s, state, wf)
}

// applyReviewAnswer maps a human reviewer's reply to a routing action.
func (e *Engine) applyReviewAnswer(ans string, s StageConfig, state *StageState, wf *memory.Workflow) (any, string, error) {
	low := strings.ToLower(strings.TrimSpace(ans))
	switch {
	case isApproveWord(low):
		return ans, ActionApprove, nil
	case strings.HasPrefix(low, "ask:") && strings.TrimSpace(s.Ask) != "":
		state.AskQuestion = strings.TrimSpace(ans[len("ask:"):])
		return ans, ActionAsk, nil
	default:
		e.rejectWithFeedback(wf, ans)
		return ans, ActionReject, nil
	}
}

// rejectWithFeedback records the reviewer's feedback and clears the plan so the
// executor re-plans (with that feedback) on the rework loop.
func (e *Engine) rejectWithFeedback(wf *memory.Workflow, feedback string) {
	if wf.Approvals == nil {
		wf.Approvals = map[string]string{}
	}
	wf.Approvals["replan_feedback"] = strings.TrimSpace(feedback)
	wf.Plan = nil
}

// llmReview runs an automated reviewer over the change summary and parses a
// strict-JSON decision.
func (e *Engine) llmReview(ctx context.Context, s StageConfig, state *StageState, wf *memory.Workflow, t boards.Ticket) (decision, reason, question string, err error) {
	summary := e.changeSummary(ctx, *wf, t)
	instr := "You are a strict code reviewer. Decide whether to approve the change, reject it (with a reason), or ask the analyst a question.\nReply EXACTLY one JSON object: {\"decision\":\"approve|reject|ask\",\"reason\":\"...\",\"question\":\"... (only when decision=ask)\"}.\nNo prose, no fences."
	reviewCtx := "Ticket: " + t.Key + " — " + t.Title
	if strings.TrimSpace(s.Task) != "" {
		if tr, e2 := renderTemplate("task["+s.Name+"]", s.Task, state); e2 == nil {
			reviewCtx = tr
		}
	}
	prompt := instr + "\n\n" + reviewCtx + "\n\nCHANGE UNDER REVIEW:\n" + summary
	// On a re-run after an ask, fold the analyst's answer into the prompt so
	// the reviewer decides with that knowledge.
	if state.Ask != nil {
		if a := strings.TrimSpace(fmt.Sprint(state.Ask)); a != "" {
			prompt += "\n\nANALYST ANSWER to your earlier question:\n" + a
		}
	}
	out, err := e.llmFor(llm.RoleReview).Generate(ctx, []llm.Message{{Role: llm.RoleUser, Content: prompt}},
		llm.Options{Temperature: 0.1, JSONMode: true, MaxTokens: 800})
	if err != nil {
		return "", "", "", err
	}
	chunk, err := extractJSONObject(out)
	if err != nil {
		return "", "", "", fmt.Errorf("reviewer JSON: %w (raw=%q)", err, snippet(out, 160))
	}
	var r struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
		Question string `json:"question"`
	}
	if err := json.Unmarshal([]byte(chunk), &r); err != nil {
		return "", "", "", fmt.Errorf("reviewer JSON decode: %w", err)
	}
	d := strings.ToLower(strings.TrimSpace(r.Decision))
	if d != "approve" && d != "reject" && d != "ask" {
		d = "reject"
	}
	return d, strings.TrimSpace(r.Reason), strings.TrimSpace(r.Question), nil
}

// changeSummary renders a concise, human-readable summary of the executor's
// work (the git diff of the assigned repo), produced by the review-role model.
// Falls back to the raw diff when no model is available.
func (e *Engine) changeSummary(ctx context.Context, wf memory.Workflow, t boards.Ticket) string {
	repo := strings.TrimSpace(wf.Repo)
	if repo == "" || !isLocalCheckout(repo) {
		return "(no local repo checkout — unable to show a diff)"
	}
	if e.Tools == nil {
		return "(diff unavailable — no tools registry)"
	}
	tool, ok := e.Tools.Get("run_command")
	if !ok {
		return "(diff unavailable — run_command tool not registered)"
	}
	get := func(sub string) string {
		res, err := tool.Run(ctx, map[string]string{"command": "cd " + shellQuoteArg(repo) + " && " + sub})
		if err != nil {
			return ""
		}
		return strings.TrimSpace(res.Stdout)
	}
	// Capture both uncommitted edits and any commits the executor made on the
	// branch, so the reviewer sees the change regardless of whether the agent
	// committed.
	stat := get("git --no-pager diff --stat HEAD")
	diff := get("git --no-pager diff HEAD")
	if diff == "" {
		diff = get("git --no-pager diff")
	}
	recent := get("git --no-pager log --oneline -8")
	lastStat := get("git --no-pager show --stat --oneline HEAD")
	var b strings.Builder
	if stat != "" {
		b.WriteString("WORKING-TREE CHANGES (vs HEAD):\n" + stat + "\n\n")
	}
	if diff != "" {
		b.WriteString(diff + "\n\n")
	}
	if recent != "" {
		b.WriteString("RECENT COMMITS:\n" + recent + "\n\n")
	}
	if stat == "" && diff == "" && lastStat != "" {
		b.WriteString("LAST COMMIT:\n" + lastStat + "\n")
	}
	combined := strings.TrimSpace(b.String())
	if combined == "" {
		return "(no changes detected in " + repo + ")"
	}
	combined = snippet(combined, 12000)
	if e.LLM == nil {
		return combined
	}
	prompt := "Summarize this code change for a human reviewer in 4-8 short lines: what it does, any risks or gaps, and whether it looks complete. End with a line 'FILES: <changed files>'.\n\nPLAN:\n" +
		formatPlanForApproval(wf.Plan) + "\nDIFF:\n" + combined
	out, err := e.llmFor(llm.RoleReview).Generate(ctx, []llm.Message{{Role: llm.RoleUser, Content: prompt}},
		llm.Options{Temperature: 0.2, MaxTokens: 600})
	if err != nil || strings.TrimSpace(out) == "" {
		return combined
	}
	return strings.TrimSpace(out)
}

// runNotifyNode sends a Telegram message. Missing channel is a no-op (it does
// not fail the graph) so a notify node is safe without Telegram configured.
func (e *Engine) runNotifyNode(ctx context.Context, s StageConfig, state *StageState) (any, error) {
	if strings.TrimSpace(s.Message) == "" {
		return nil, errors.New("notify: message is required")
	}
	msg, err := renderTemplate("message["+s.Name+"]", s.Message, state)
	if err != nil {
		return nil, err
	}
	if e.Tools == nil {
		return msg, nil
	}
	tool, ok := e.Tools.Get("telegram")
	if !ok {
		return msg, nil
	}
	if _, err := tool.Run(ctx, map[string]string{"text": msg}); err != nil {
		return nil, fmt.Errorf("notify: %w", err)
	}
	return msg, nil
}

// runAgentForNode runs goon's agent loop for an explicit-task executor node,
// inside the repo when one is set so file/command tools hit the right tree.
func (e *Engine) runAgentForNode(ctx context.Context, s StageConfig, repo, task string) (string, error) {
	prov := e.llmFor(llm.RoleCode)
	if s.Provider != "" || s.Model != "" {
		p, err := llm.NewWithOverrides(s.Provider, s.Model)
		if err != nil {
			return "", fmt.Errorf("executor %q: %w", s.Name, err)
		}
		prov = p
	}
	if isLocalCheckout(repo) {
		ctx = tools.WithWorkDir(ctx, repo)
	}
	a := agent.New(agent.Options{
		LLM: prov, Tools: e.Tools, Executor: e.Executor, Memory: e.Memory,
		Stdout: e.Stdout, Stderr: e.Stderr, Debug: e.Debug,
	})
	err := a.Run(ctx, task)
	return a.Result(), err
}

// resolveRepoForGraph fills wf.Repo/wf.Repos with a LOCAL checkout. Order:
// pre-assigned repos (the "ticket already assigned to a repo" model) win; then
// triage's suggestions, cloning remote-only ones. Non-code tickets clear the
// repo. Errors when nothing resolves so the executor doesn't run against
// nothing.
func (e *Engine) resolveRepoForGraph(ctx context.Context, wf *memory.Workflow, t boards.Ticket) error {
	if !memory.WorkflowNeedsRepo(*wf) {
		wf.Repo = ""
		wf.Repos = nil
		return nil
	}
	if e.Memory != nil {
		if assigned, ok := e.Memory.AssignedRepos(t.ID); ok {
			var locals []string
			for _, r := range assigned {
				switch {
				case isLocalCheckout(r):
					locals = append(locals, r)
				default:
					if ent, ok := repository.Lookup(r); ok && isLocalCheckout(ent.Local) {
						locals = append(locals, ent.Local)
					}
				}
			}
			if len(locals) > 0 {
				wf.Repo = locals[0]
				wf.Repos = locals
				return nil
			}
		}
	}
	if isLocalCheckout(wf.Repo) {
		return nil
	}
	cands := e.buildRepoCandidates(ctx, t)
	if len(wf.Repos) == 0 && len(cands) == 1 {
		wf.Repo = cands[0].Value
		wf.Repos = []string{cands[0].Value}
	}
	if len(wf.Repos) > 0 {
		if err := e.ensureReposCloned(ctx, wf, cands); err != nil {
			return err
		}
	}
	if !isLocalCheckout(wf.Repo) {
		return fmt.Errorf("no local repo resolved for %s — assign it on the Tickets tab or add it to REPOSITORY.md", t.Key)
	}
	return nil
}

// consultAnalyst runs the named analyst node as an inline sub-call and returns
// its answer (stored by the caller in state.Ask for the re-running node).
func (e *Engine) consultAnalyst(ctx context.Context, stageMap map[string]StageConfig, name string, state *StageState) (any, error) {
	a, ok := stageMap[name]
	if !ok {
		return nil, fmt.Errorf("ask target %q not found", name)
	}
	if strings.ToLower(strings.TrimSpace(a.Type)) != RoleAnalyst {
		return nil, fmt.Errorf("ask target %q is not an analyst", name)
	}
	e.graphLogf("  ↪ ask %s\n", name)
	return e.runAnalystNode(ctx, a, state)
}

// fetchURL GETs a URL via the registry's fetch_url tool (used by analysts).
func (e *Engine) fetchURL(ctx context.Context, url string) (string, error) {
	if e.Tools == nil {
		return "", errors.New("no tools registry")
	}
	tool, ok := e.Tools.Get("fetch_url")
	if !ok {
		return "", errors.New("no fetch_url tool registered")
	}
	res, err := tool.Run(ctx, map[string]string{"url": url})
	if err != nil {
		return "", err
	}
	if res.Err != nil {
		return "", res.Err
	}
	return res.Stdout, nil
}

func (e *Engine) graphLogf(format string, args ...any) {
	if e.Stdout != nil {
		fmt.Fprintf(e.Stdout, format, args...)
	}
}

func (e *Engine) graphWarnf(format string, args ...any) {
	if e.Stderr != nil {
		fmt.Fprintf(e.Stderr, format, args...)
	}
}

// ── routing + template helpers ──────────────────────────────────────────────

// stageKey is the state key a node writes its output under (Output override or
// Name).
func stageKey(s StageConfig) string {
	if s.Output != "" {
		return s.Output
	}
	return s.Name
}

// nextTargets returns the nodes to run after a `next`/default action: on_next
// when set, else the next NON-ANALYST node in array order. Analyst nodes are
// consultation sidecars reached only via `ask`, so they are never the implicit
// "next step" — skipping them stops a trailing analyst (e.g. a Product Owner
// placed last) from being run as a phantom step after a notify/executor that
// has no explicit on_next. A node with nothing flow-worthy after it ends its
// branch (returns nil → treated as end).
func nextTargets(stages []StageConfig, s StageConfig) []string {
	if len(s.OnNext) > 0 {
		return targetsOf(s.OnNext)
	}
	for i, st := range stages {
		if st.Name == s.Name {
			for j := i + 1; j < len(stages); j++ {
				if stages[j].Type == RoleAnalyst {
					continue
				}
				return []string{stages[j].Name}
			}
			break
		}
	}
	return nil
}

// targetsOf trims a routing list and drops blanks + "end" terminators.
func targetsOf(l StringList) []string {
	out := make([]string, 0, len(l))
	for _, n := range l {
		n = strings.TrimSpace(n)
		if n == "" || n == "end" {
			continue
		}
		out = append(out, n)
	}
	return out
}

func orWord(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func isApproveWord(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "approve", "approved", "yes", "y", "lgtm", "ok", "okay", "ship", "ship it", "accept", "accepted", "👍":
		return true
	}
	return false
}

// parseAskSignal recognises an executor agent finishing with "ASK: <question>".
func parseAskSignal(res string) (string, bool) {
	t := strings.TrimSpace(res)
	if len(t) >= 4 && strings.EqualFold(t[:4], "ASK:") {
		return strings.TrimSpace(t[4:]), true
	}
	return "", false
}

// validateStages enforces unique names, valid roles, required per-role fields,
// and that every `ask` target points at an analyst. Shared by the web save
// handler (via WorkflowConfig.Validate) and runGraph.
func validateStages(stages []StageConfig) error {
	if len(stages) == 0 {
		return nil
	}
	seen := map[string]bool{}
	typeByName := map[string]string{}
	for i, s := range stages {
		name := strings.TrimSpace(s.Name)
		if name == "" {
			return fmt.Errorf("stage[%d]: name is required", i)
		}
		if seen[name] {
			return fmt.Errorf("stage[%d]: duplicate name %q", i, name)
		}
		seen[name] = true
		typeByName[name] = strings.ToLower(strings.TrimSpace(s.Type))
	}
	for _, s := range stages {
		name := strings.TrimSpace(s.Name)
		switch strings.ToLower(strings.TrimSpace(s.Type)) {
		case RoleAnalyst:
			// Reachable only via ask; prompt optional (sensible default).
		case RoleExecutor:
			if s.Do != "" && s.Do != DoOpenPR {
				return fmt.Errorf("stage %s: unknown executor do=%q (want %q)", name, s.Do, DoOpenPR)
			}
		case RoleReviewer:
			if m := strings.ToLower(strings.TrimSpace(s.Mode)); m != "" && m != ReviewerModeHuman && m != ReviewerModeLLM {
				return fmt.Errorf("stage %s: reviewer mode must be %q or %q", name, ReviewerModeHuman, ReviewerModeLLM)
			}
			if len(targetsOf(s.OnApprove)) == 0 && strings.TrimSpace(s.OnReject) == "" {
				return fmt.Errorf("stage %s: reviewer needs on_approve and/or on_reject wired", name)
			}
		case RoleNotify:
			if strings.TrimSpace(s.Message) == "" {
				return fmt.Errorf("stage %s: message is required for type=notify", name)
			}
		case RoleLoop:
			if len(targetsOf(s.OnNext)) == 0 && strings.TrimSpace(s.OnDone) == "" {
				return fmt.Errorf("stage %s: loop needs on_next (the loop target) or on_done (the exit)", name)
			}
		case "llm", "agent", "http":
			return fmt.Errorf("stage %s: type %q was removed — use %q (was llm), %q (was agent), or an analyst with `urls` (was http)",
				name, s.Type, RoleAnalyst, RoleExecutor)
		default:
			return fmt.Errorf("stage %s: unknown type %q (want %s|%s|%s|%s|%s)",
				name, s.Type, RoleAnalyst, RoleExecutor, RoleReviewer, RoleLoop, RoleNotify)
		}
		if a := strings.TrimSpace(s.Ask); a != "" {
			at, ok := typeByName[a]
			if !ok {
				return fmt.Errorf("stage %s: ask target %q not found", name, a)
			}
			if at != RoleAnalyst {
				return fmt.Errorf("stage %s: ask target %q must be an analyst (it is %q)", name, a, at)
			}
		}
	}
	return nil
}

// templateFuncs is the small FuncMap available to every node template.
var templateFuncs = template.FuncMap{
	"json": func(v any) string {
		if v == nil {
			return ""
		}
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(b)
	},
	"get": func(m any, key string) any {
		if mm, ok := m.(map[string]any); ok {
			return mm[key]
		}
		return nil
	},
	"default": func(fallback any, v any) any {
		if v == nil {
			return fallback
		}
		if s, ok := v.(string); ok && s == "" {
			return fallback
		}
		return v
	},
}

// renderTemplate runs a Go text/template with the StageState as data.
func renderTemplate(name, raw string, state *StageState) (string, error) {
	if !strings.Contains(raw, "{{") {
		return raw, nil
	}
	tpl, err := template.New(name).Funcs(templateFuncs).Option("missingkey=zero").Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", name, err)
	}
	var b bytes.Buffer
	if err := tpl.Execute(&b, state); err != nil {
		return "", fmt.Errorf("render %s: %w", name, err)
	}
	return b.String(), nil
}

// isFalsy applies a permissive bool reading on rendered template output.
func isFalsy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "false", "no", "0", "off":
		return true
	}
	return false
}
