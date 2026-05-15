// Package workflow implements goon's autonomous engineering pipeline.
//
//	Triage         — classify the ticket, propose a target repo + plan
//	ConfirmRepo    — gate: ask the user to confirm/override the repo
//	ApprovePlan    — gate: ask the user to approve the work + test plan
//	Execute        — run the agent loop on each plan step
//	Test           — run the repo's test command (best-effort)
//	Verify         — re-run the agent N times to double-check the work
//	UpdateMemory   — distil learnings into the markdown notes store (PINNED.md / topic notes)
//	OpenPR         — push the branch and create a PR / MR
//	Notify         — Telegram message with a link to the PR
//
// The pipeline is a resumable state machine. ConfirmRepo and ApprovePlan
// queue questions in memory and pause the workflow (state =
// WFAwaitingApproval). The daemon picks the workflow up again on a later
// tick once the user replies via `goon train` or the web UI.
//
// Set workflow.json `auto_approve: true` (or env GOON_AUTO_APPROVE=1) to
// skip the gates entirely for fully unattended runs.
//
// Each phase is a focused LLM call with strict-JSON output, run on top of
// the existing agent / executor / safety layers. Workflow state is persisted
// to memory after every phase so a crash mid-flight leaves a recoverable
// trail.
package workflow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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

// WorkspaceDir returns the configured GOON_WORKSPACE_DIR — a parent
// directory that holds multiple git repos as immediate children.
// When set, the confirm_repo gate enumerates the workspace's repos
// and presents them as a numbered list instead of asking the user to
// type a path. Empty when unset.
func WorkspaceDir() string {
	return strings.TrimSpace(os.Getenv("GOON_WORKSPACE_DIR"))
}

// DiscoverWorkspaceRepos returns every immediate subdirectory of
// GOON_WORKSPACE_DIR that contains a .git entry (file or dir — so
// both standard checkouts and git worktrees count). The returned
// paths are absolute. Empty slice when the workspace is unset or
// unreadable; we never error out — the gate falls back to "type a
// path" mode in that case.
//
// Sorted alphabetically so the numbered list is stable across calls;
// users build muscle memory for "option 1 is always X".
func DiscoverWorkspaceRepos() []string {
	dir := WorkspaceDir()
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Skip hidden directories (.git, .vscode, etc.) — the workspace
		// root might itself BE a project containing tools alongside
		// repos and we don't want to offer ".cache" as a candidate.
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		full := filepath.Join(dir, e.Name())
		// Treat a directory as a repo if it has a .git entry — works
		// for both normal clones (dir) and git-worktree (file).
		if _, err := os.Stat(filepath.Join(full, ".git")); err != nil {
			continue
		}
		out = append(out, full)
	}
	sort.Strings(out)
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

	// AutoApprove, when true, skips every user-approval gate. Tests use
	// this to keep the existing happy-path assertions; in production the
	// equivalent knob is cfg.AutoApprove or env var GOON_AUTO_APPROVE.
	AutoApprove bool
}

// errPaused is the sentinel a phase returns when it asks the user a question
// and needs to wait for an answer. Run() catches it and returns the workflow
// in WFAwaitingApproval state without firing the on_failure hook.
var errPaused = errors.New("workflow paused: awaiting user answer")

// errReplan is the sentinel phaseApprovePlan returns after the user
// rejected a plan with feedback. Run() catches it and rewinds to
// phaseTriage in the SAME Run() call so the new plan is generated and
// the next approve_plan question is asked atomically — without a
// second daemon tick. Without this, a rejected plan would set
// Stage=triage on disk and the test/single-call user surface would
// see no new pending question until the daemon polled again.
var errReplan = errors.New("workflow replan: re-entering triage with feedback")

// phase is one entry in the built-in pipeline.
type phase struct {
	name string
	fn   func(ctx context.Context, wf *memory.Workflow, t boards.Ticket, p *phaseCtx) error
}

// phaseCtx is shared state for the duration of a single Run() call. It
// carries the resolved WorkflowConfig, the hook runner, the branch name and
// the auto-approve flag so phase functions don't need a long arg list.
type phaseCtx struct {
	cfg         WorkflowConfig
	hr          *HookRunner
	branch      string
	autoApprove bool
}

// Run runs (or resumes) the workflow for one ticket. It returns the final
// Workflow record. Any error is also recorded inside the Workflow.
//
// The pipeline is a resumable state machine:
//
//	triage → confirm_repo → approve_plan → execute → test → verify →
//	  update_memory → open_pr → notify
//
// confirm_repo and approve_plan use ask_user-style gates (queue a Question
// in memory, set wf.State=WFAwaitingApproval, and return — the daemon picks
// the workflow up again on a later tick once the user replies). update_memory
// runs an agent task that distils what the workflow learned into the markdown
// notes store (PINNED.md / topic notes). Set cfg.AutoApprove or env var
// GOON_AUTO_APPROVE=1 to skip the gates entirely for unattended runs.
//
// Hooks fire at the same boundaries as before so existing workflow.json
// configs keep working unchanged.
func (e *Engine) Run(ctx context.Context, t boards.Ticket) (memory.Workflow, error) {
	cfg := e.resolveConfig()
	wfName := cfg.Name
	if wfName == "" {
		wfName = "default"
	}
	branch := branchName(cfg.BranchPrefix, t.Key)

	// Resume if we already have an open workflow for this ticket; otherwise
	// initialise a fresh record.
	wf, resuming := e.openOrInitWorkflow(t, branch)
	e.save(wf)

	logx.Info("workflow.start",
		"wf", wf.ID, "name", wfName,
		"ticket", t.Key, "title", t.Title,
		"stages", len(cfg.Stages), "stage", wf.Stage,
		"resuming", resuming,
	)
	if e.Stdout != nil {
		if resuming {
			fmt.Fprintf(e.Stdout, "[workflow] %s resuming %s at %s — %q\n", wfName, t.Key, wf.Stage, t.Title)
		} else {
			fmt.Fprintf(e.Stdout, "[workflow] %s started for %s — %q\n", wfName, t.Key, t.Title)
		}
	}
	defer func() {
		logx.Info("workflow.end", "wf", wf.ID, "name", wfName, "ticket", t.Key,
			"state", string(wf.State), "stage", wf.Stage, "pr_url", wf.PRURL,
			"duration_ms", time.Since(wf.StartedAt).Milliseconds())
	}()

	hr := &HookRunner{
		Stdout: e.Stdout, Stderr: e.Stderr,
		Validator: safety.Default(),
	}

	// --- Declarative stages mode ---------------------------------------------
	// When the user provides cfg.Stages, the built-in pipeline is replaced
	// wholesale with their declared sequence. Approval gates do NOT fire here
	// — declarative pipelines are an explicit opt-in to a fully-custom flow.
	if len(cfg.Stages) > 0 {
		return e.runStages(ctx, wf, t, cfg, hr, branch)
	}

	pctx := &phaseCtx{
		cfg:         cfg,
		hr:          hr,
		branch:      branch,
		autoApprove: e.isAutoApprove(cfg),
	}

	phases := []phase{
		{name: "triage", fn: e.phaseTriage},
		{name: "confirm_repo", fn: e.phaseConfirmRepo},
		{name: "approve_plan", fn: e.phaseApprovePlan},
		{name: "execute", fn: e.phaseExecute},
		{name: "test", fn: e.phaseTest},
		{name: "verify", fn: e.phaseVerify},
		{name: "update_memory", fn: e.phaseUpdateMemory},
		{name: "open_pr", fn: e.phaseOpenPR},
		{name: "notify", fn: e.phaseNotify},
	}
	start := indexOfPhase(phases, wf.Stage)
	if start < 0 {
		start = 0
	}

	// Cap re-plan iterations defensively at the loop level too, even
	// though phaseApprovePlan tracks its own counter. A misbehaving
	// LLM that always produces the same plan + a user always
	// rejecting + the in-call rewind would otherwise spin forever.
	const loopReplanCap = 16
	loopReplans := 0
	for i := start; i < len(phases); i++ {
		select {
		case <-ctx.Done():
			return e.fail(wf, phases[i].name, ctx.Err())
		default:
		}
		wf.Stage = phases[i].name
		e.save(wf)
		err := phases[i].fn(ctx, &wf, t, pctx)
		if err == nil {
			continue
		}
		if errors.Is(err, errPaused) {
			// Gate paused us; state already saved by the gate.
			return wf, nil
		}
		if errors.Is(err, errReplan) {
			// User rejected the plan with feedback. Rewind to
			// triage in this same Run call so the new plan is
			// generated and the next approve_plan question is
			// asked atomically.
			loopReplans++
			if loopReplans > loopReplanCap {
				return e.fail(wf, phases[i].name, fmt.Errorf("re-plan loop did not stabilize after %d iterations", loopReplanCap))
			}
			i = indexOfPhase(phases, "triage") - 1 // -1 because the for-loop's i++ runs next
			continue
		}
		hctx := FromTicketMulti(t, wf.Repo, wf.Repos, branch, wf.Plan)
		e.runFailureHook(ctx, hr, cfg, hctx, err)
		return e.fail(wf, phases[i].name, err)
	}

	wf.Stage = "done"
	wf.State = memory.WFDone
	wf.PendingQuestionID = ""
	e.save(wf)
	if e.Board != nil {
		_ = e.Board.Comment(ctx, t.ID, fmt.Sprintf("✓ goon completed this ticket. PR: %s", wf.PRURL))
		_ = e.Board.Transition(ctx, t.ID, boards.StatusInReview)
	}
	return wf, nil
}

// openOrInitWorkflow looks up an open workflow for the ticket and returns it
// (with resuming=true). If none exists it returns a fresh Workflow record
// pre-stamped with the ticket fields and starting stage.
func (e *Engine) openOrInitWorkflow(t boards.Ticket, branch string) (memory.Workflow, bool) {
	if e.Memory != nil {
		if existing, ok := e.Memory.OpenWorkflowFor(t.ID); ok {
			if existing.Branch == "" {
				existing.Branch = branch
			}
			return existing, true
		}
	}
	return memory.Workflow{
		ID:        fmt.Sprintf("wf-%d", time.Now().UnixNano()),
		TicketID:  t.ID,
		TicketKey: t.Key,
		Title:     t.Title,
		StartedAt: time.Now(),
		State:     memory.WFTriaging,
		Stage:     "triage",
		Branch:    branch,
		Approvals: map[string]string{},
	}, false
}

// resolveConfig returns the explicit Engine.Config when set, otherwise loads
// from disk via LoadConfig. Mirrors the inline behavior of the old Run().
func (e *Engine) resolveConfig() WorkflowConfig {
	cfg := e.Config
	if cfg.Version == 0 {
		loaded, _, _ := LoadConfig("")
		cfg = loaded
	}
	return cfg
}

// isAutoApprove reports whether approval gates should be skipped for this
// run. Precedence: Engine field → cfg.AutoApprove → GOON_AUTO_APPROVE env.
func (e *Engine) isAutoApprove(cfg WorkflowConfig) bool {
	if e.AutoApprove {
		return true
	}
	if cfg.AutoApprove {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GOON_AUTO_APPROVE"))) {
	case "1", "true", "yes", "y", "on":
		return true
	}
	return false
}

// indexOfPhase returns the index of the phase named stage, or -1 when not
// found. An empty stage string returns 0 so a fresh workflow starts at the
// top of the pipeline.
func indexOfPhase(phases []phase, stage string) int {
	if stage == "" {
		return 0
	}
	for i, p := range phases {
		if p.name == stage {
			return i
		}
	}
	return -1
}

// gate queues a question for the user and returns (answer, true, nil) if the
// answer is already on file, or ("", false, nil) when the workflow needs to
// pause and wait. Callers should bail out with errPaused when ready=false.
//
// On a positive answer the workflow's PendingQuestionID is cleared so the
// daemon's ResumableWorkflow() doesn't pick it up again.
func (e *Engine) gate(ctx context.Context, wf *memory.Workflow, t boards.Ticket, stage, question string) (string, bool, error) {
	_ = ctx
	if e.Memory == nil {
		// No memory backend → can't queue/resume. Treat as auto-approved so
		// callers using memory.Disabled() (one-shot agent) don't deadlock.
		return "auto:no-memory", true, nil
	}
	if ans, ok := e.Memory.FindAnswer(t.ID, question); ok {
		// Resume path: the answer is here; clear the pending marker.
		wf.PendingQuestionID = ""
		return ans, true, nil
	}
	qid := e.Memory.AskQuestion(memory.Question{
		TicketID: t.ID, WorkflowID: wf.ID, Question: question,
	})
	wf.State = memory.WFAwaitingApproval
	wf.Stage = stage
	wf.PendingQuestionID = qid
	e.save(*wf)
	if e.Stdout != nil {
		fmt.Fprintf(e.Stdout,
			"[workflow] paused at %s — awaiting %s (run `goon train` to answer)\n",
			stage, qid)
	}
	return "", false, nil
}

// --- phase implementations ---------------------------------------------------

func (e *Engine) phaseTriage(ctx context.Context, wf *memory.Workflow, t boards.Ticket, p *phaseCtx) error {
	if len(wf.Plan) > 0 {
		// Already triaged on a previous run — skip on resume.
		return nil
	}
	hctx := FromTicket(t, "", p.branch, nil)
	if err := p.hr.Run(ctx, HookBeforeTriage, p.cfg.Hook(HookBeforeTriage), hctx); err != nil {
		return err
	}
	wf.State = memory.WFTriaging
	e.save(*wf)
	// Carry rejection feedback forward — when the user rejected the
	// previous plan with text, that text becomes context for the next
	// triage so the model knows what to change.
	feedback := ""
	if wf.Approvals != nil {
		feedback = wf.Approvals["replan_feedback"]
	}
	plan, repo, err := e.triageWithFeedback(ctx, t, feedback)
	if err != nil {
		return err
	}
	wf.Plan = plan
	wf.Repo = repo
	wf.State = memory.WFPlanning
	// Clear feedback so the gate's "approval already recorded" check
	// for approve_plan doesn't get poisoned. We keep replan_count.
	if wf.Approvals != nil {
		delete(wf.Approvals, "replan_feedback")
		// Approval value for approve_plan must be cleared too —
		// otherwise approvalAnswer() short-circuits the gate on
		// the next call.
		delete(wf.Approvals, "approve_plan")
	}
	e.save(*wf)
	return nil
}

func (e *Engine) phaseConfirmRepo(ctx context.Context, wf *memory.Workflow, t boards.Ticket, p *phaseCtx) error {
	// Auto-approve still records the resolved repo so the choice
	// persists into Memory.RepoChoices for future tickets.
	if p.autoApprove {
		ensureApproval(wf, "confirm_repo", "auto:approved")
		e.rememberRepo(t.Project, wf.Repo)
		return nil
	}
	if existing := approvalAnswer(wf, "confirm_repo"); existing != "" {
		return nil
	}
	// If we already learned a repo for this project, skip the gate
	// entirely. Gives the user the satisfaction of "I confirmed this
	// once, don't ask me again." They can break the cache via
	// `goon repo forget <project>` (see cmd/repo.go).
	if learned, ok := e.lookupLearnedRepo(t.Project); ok {
		wf.Repo = learned
		ensureApproval(wf, "confirm_repo", "auto:remembered")
		wf.State = memory.WFPlanning
		e.save(*wf)
		if e.Stdout != nil {
			fmt.Fprintf(e.Stdout, "[workflow] using remembered repo %q for project %q\n", learned, t.Project)
		}
		return nil
	}
	suggested := wf.Repo
	if suggested == "" {
		suggested = e.pickRepoForTicket(t)
		wf.Repo = suggested
	}
	// Build the candidate list from BOTH the local workspace and the
	// configured git host. The user picks one or more by number; the
	// menu uses stable indexing so "Pick 2" always points at the same
	// repo within a single question lifetime.
	candidates := e.buildRepoCandidates(ctx, t)
	q := buildRepoGateQuestion(t, suggested, candidates)
	ans, ready, err := e.gate(ctx, wf, t, "confirm_repo", q)
	if err != nil {
		return err
	}
	if !ready {
		return errPaused
	}
	switch {
	case isYes(ans):
		// accept the suggestion as-is
	default:
		// Try multi-pick first ("1,3,5" or "1 3 5"); falls back to
		// a single number, then change=<path>, then rejection.
		if picks, ok := pickWorkspaceReposMulti(ans, candidates); ok {
			if len(picks) > 0 {
				wf.Repo = picks[0]
				wf.Repos = picks
			}
		} else if newRepo, ok := parseRepoChange(ans); ok {
			wf.Repo = newRepo
			wf.Repos = []string{newRepo}
		} else {
			return fmt.Errorf("user rejected repo: %s", ans)
		}
	}
	if len(wf.Repos) == 0 && wf.Repo != "" {
		wf.Repos = []string{wf.Repo}
	}
	ensureApproval(wf, "confirm_repo", ans)
	// Remember the primary pick per project so subsequent tickets
	// from the same project skip the gate. Multi-repo workflows
	// still get full visibility via wf.Repos.
	e.rememberRepo(t.Project, wf.Repo)
	wf.State = memory.WFPlanning
	e.save(*wf)
	return nil
}

// repoCandidate is one entry in the confirm_repo numbered menu —
// either a local workspace clone or a remote git-host repo. We carry
// a flag so the prompt can label it ("/path/to/repo" vs "owner/name
// (remote)") and so callers know whether to clone before executing.
type repoCandidate struct {
	Label    string // human-readable name for the menu
	Value    string // what gets stored in wf.Repo / wf.Repos
	IsRemote bool   // false = local workspace clone; true = host slug
}

// buildRepoCandidates merges the local workspace + the git host's
// repo list (when reachable) into a single deduped, sorted slice.
// We swallow host errors (network down, token missing) — the gate
// stays usable from the workspace alone in that case.
func (e *Engine) buildRepoCandidates(ctx context.Context, t boards.Ticket) []repoCandidate {
	out := []repoCandidate{}

	// Local workspace first — they're already on disk, safe to act on.
	for _, p := range DiscoverWorkspaceRepos() {
		out = append(out, repoCandidate{
			Label: filepath.Base(p),
			Value: p,
		})
	}

	// Git host repos — only if the host implements RepoLister.
	if e.Host != nil {
		if lister, ok := e.Host.(githost.RepoLister); ok {
			lsCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			if repos, err := lister.ListRepos(lsCtx); err == nil {
				// De-dupe: if a workspace clone matches a remote
				// slug by basename, prefer the local entry.
				localBases := map[string]bool{}
				for _, c := range out {
					localBases[strings.ToLower(c.Label)] = true
				}
				for _, r := range repos {
					if r.Slug == "" {
						continue
					}
					base := r.Slug
					if i := strings.LastIndexByte(base, '/'); i >= 0 {
						base = base[i+1:]
					}
					if localBases[strings.ToLower(base)] {
						continue
					}
					out = append(out, repoCandidate{
						Label:    r.Slug,
						Value:    r.Slug,
						IsRemote: true,
					})
				}
			} else if e.Stderr != nil {
				fmt.Fprintf(e.Stderr, "[workflow] list repos: %v (continuing with workspace + memory only)\n", err)
			}
		}
	}
	return out
}

// buildRepoGateQuestion composes the confirm_repo prompt. When the
// candidate list is non-empty we render it as a numbered menu so the
// user can pick by number(s) instead of typing a path. Remote repos
// are tagged "(remote)" so the user knows they're picking a slug,
// not a checkout path.
func buildRepoGateQuestion(t boards.Ticket, suggested string, candidates []repoCandidate) string {
	if len(candidates) == 0 {
		return fmt.Sprintf("Confirm repo for %s — %q\nSuggested: %s\nReply: yes / change=<path> / no",
			t.Key, t.Title, suggested)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Confirm repo for %s — %q\nSuggested: %s\n\nAvailable repos:\n",
		t.Key, t.Title, suggested)
	for i, c := range candidates {
		marker := " "
		if c.Value == suggested {
			marker = "*"
		}
		tag := ""
		if c.IsRemote {
			tag = " (remote)"
		}
		fmt.Fprintf(&sb, " %s %d. %s%s\n", marker, i+1, c.Label, tag)
	}
	sb.WriteString("\nReply: <n> or <n>,<n>,<n> (one or more numbers — first is primary)  |  yes (accept suggested)  |  change=<path>  |  no")
	return sb.String()
}

// pickWorkspaceReposMulti parses an answer that contains one or more
// numbers (separated by comma, space, or "+"). Returns the resolved
// repo values in the same order the user picked, deduplicated. ok=false
// when the input isn't numeric-only or every number is out of range —
// so the caller can fall through to other parsing modes.
func pickWorkspaceReposMulti(ans string, candidates []repoCandidate) ([]string, bool) {
	if len(candidates) == 0 {
		return nil, false
	}
	// Replace common separators with commas, then split.
	s := strings.NewReplacer(" ", ",", "\t", ",", "+", ",", ";", ",").Replace(strings.TrimSpace(ans))
	if s == "" {
		return nil, false
	}
	seen := map[string]bool{}
	picks := []string{}
	for _, tok := range strings.Split(s, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		n, err := strconv.Atoi(tok)
		if err != nil {
			return nil, false // a non-numeric token disqualifies the whole answer
		}
		if n < 1 || n > len(candidates) {
			return nil, false
		}
		v := candidates[n-1].Value
		if seen[v] {
			continue
		}
		seen[v] = true
		picks = append(picks, v)
	}
	if len(picks) == 0 {
		return nil, false
	}
	return picks, true
}

// pickWorkspaceRepo is the legacy single-pick form. Kept so existing
// tests pass; the live confirm_repo gate now uses the multi-pick
// variant. Returns the first match (or false on no/invalid input).
func pickWorkspaceRepo(ans string, wsRepos []string) (string, bool) {
	if len(wsRepos) == 0 {
		return "", false
	}
	n, err := strconv.Atoi(strings.TrimSpace(ans))
	if err != nil {
		return "", false
	}
	if n < 1 || n > len(wsRepos) {
		return "", false
	}
	return wsRepos[n-1], true
}

// pickRepoForTicket resolves a repo path for the ticket using the
// priority ladder:
//
//  1. GOON_REPO_MAP exact match on project key (operator-explicit)
//  2. Memory.RepoChoices learned mapping (user-confirmed once before)
//  3. GOON_REPO_MAP "*" wildcard fallback
//  4. ticket.Project as a literal (last resort — usually not a real path)
//
// Operator-explicit env wins over learned because admins set env for
// security; learned wins over the wildcard so a single confirmation
// overrides a vague catch-all.
func (e *Engine) pickRepoForTicket(t boards.Ticket) string {
	rm := RepoMap()
	if v, ok := rm[t.Project]; ok && v != "" {
		return v
	}
	if e.Memory != nil {
		if v, ok := e.Memory.LookupRepoChoice(t.Project); ok && v != "" {
			return v
		}
	}
	if v, ok := rm["*"]; ok && v != "" {
		return v
	}
	return t.Project
}

// lookupLearnedRepo returns the persisted choice without consulting
// env. Used by phaseConfirmRepo to short-circuit the gate when we've
// already had this conversation. Separate from pickRepoForTicket so
// the env-override path stays explicit.
func (e *Engine) lookupLearnedRepo(project string) (string, bool) {
	if e.Memory == nil {
		return "", false
	}
	// Env-explicit mapping always wins, even over memory — admins
	// expect their env to be authoritative.
	if v, ok := RepoMap()[project]; ok && v != "" {
		return v, true
	}
	return e.Memory.LookupRepoChoice(project)
}

// rememberRepo persists project→repo to Memory.RepoChoices when both
// values are concrete. Best-effort: silently no-ops on missing memory
// or empty inputs.
func (e *Engine) rememberRepo(project, repo string) {
	if e.Memory == nil {
		return
	}
	e.Memory.RecordRepoChoice(project, repo)
}

func (e *Engine) phaseApprovePlan(ctx context.Context, wf *memory.Workflow, t boards.Ticket, p *phaseCtx) error {
	if p.autoApprove {
		ensureApproval(wf, "approve_plan", "auto:approved")
		return nil
	}
	if existing := approvalAnswer(wf, "approve_plan"); existing != "" {
		return nil
	}
	// Include the replan_count in the question text. Without this,
	// FindAnswer (which matches by ticket+question text) would return
	// the previous "no" and auto-reject every regenerated plan
	// without ever asking the user — burning the maxRePlans budget
	// in a single tick.
	replanCount := 0
	if wf.Approvals != nil {
		fmt.Sscanf(wf.Approvals["replan_count"], "%d", &replanCount)
	}
	header := "Approve work + test plan for " + t.Key + "?"
	if replanCount > 0 {
		header = fmt.Sprintf("Approve REVISED plan (attempt %d) for %s?", replanCount+1, t.Key)
	}
	q := header + "\n" +
		formatPlanForApproval(wf.Plan) +
		"Reply: yes / no / <feedback to re-plan with>"
	ans, ready, err := e.gate(ctx, wf, t, "approve_plan", q)
	if err != nil {
		return err
	}
	if !ready {
		return errPaused
	}
	if !isYes(ans) {
		// Treat any non-yes answer as "re-plan with this feedback"
		// instead of WFFailed. The old behaviour killed the workflow,
		// daemon picked the same ticket up again, and the user faced
		// the same plan with no way to influence it. Now we discard
		// the plan, store the feedback for the next triage, and
		// re-enter the pipeline at triage. Capped to avoid an
		// infinite re-plan loop.
		const maxRePlans = 3
		if wf.Approvals == nil {
			wf.Approvals = map[string]string{}
		}
		count := 0
		fmt.Sscanf(wf.Approvals["replan_count"], "%d", &count)
		count++
		if count > maxRePlans {
			// Persist the final count + last feedback before failing
			// so the on-disk record matches the error message and
			// post-mortem readers see the actual rejection that
			// blew the budget.
			wf.Approvals["replan_count"] = fmt.Sprintf("%d", count)
			wf.Approvals["replan_feedback"] = ans
			e.save(*wf)
			return fmt.Errorf("plan rejected %d times — giving up: %s", count, ans)
		}
		wf.Approvals["replan_count"] = fmt.Sprintf("%d", count)
		wf.Approvals["replan_feedback"] = ans
		wf.Plan = nil
		wf.Stage = "triage"
		wf.State = memory.WFTriaging
		// Clear the stale PendingQuestionID — the previous question is
		// now answered, and leaving it set causes the web UI's
		// workflow card to render "⏸ awaiting q-X" for a workflow
		// that's actually mid-triage.
		wf.PendingQuestionID = ""
		e.save(*wf)
		if e.Stdout != nil {
			fmt.Fprintf(e.Stdout, "[workflow] plan rejected (%d/%d) — re-planning with feedback: %q\n", count, maxRePlans, ans)
		}
		// Return errReplan so Run() rewinds to phaseTriage in this
		// same call, generates the new plan, and asks the next
		// approve_plan question — all atomically, no daemon round-
		// trip needed. The user (or test) sees a new pending
		// question after one Run, not two.
		return errReplan
	}
	ensureApproval(wf, "approve_plan", ans)
	// Clear the "awaiting" state on resume — the next phase (execute) sets
	// its own state but we don't want a brief window where Stage="execute"
	// but State=WFAwaitingApproval if the user runs `goon status`.
	wf.State = memory.WFPlanning
	e.save(*wf)
	return nil
}

func (e *Engine) phaseExecute(ctx context.Context, wf *memory.Workflow, t boards.Ticket, p *phaseCtx) error {
	hctx := FromTicketMulti(t, wf.Repo, wf.Repos, p.branch, wf.Plan)
	allDone := true
	anyDone := false
	for _, s := range wf.Plan {
		if s.Done {
			anyDone = true
		} else {
			allDone = false
		}
	}
	if allDone && len(wf.Plan) > 0 {
		return nil
	}
	if !anyDone {
		if err := p.hr.Run(ctx, HookBeforeExecute, p.cfg.Hook(HookBeforeExecute), hctx); err != nil {
			return err
		}
	}
	wf.State = memory.WFExecuting
	e.save(*wf)
	for i := range wf.Plan {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if wf.Plan[i].Done {
			continue
		}
		if err := e.executeStep(ctx, wf.TicketKey, wf.Plan[i].Title); err != nil {
			wf.Plan[i].Note = err.Error()
			e.save(*wf)
			return err
		}
		wf.Plan[i].Done = true
		e.save(*wf)
	}
	if err := p.hr.Run(ctx, HookAfterExecute, p.cfg.Hook(HookAfterExecute), hctx); err != nil {
		return err
	}
	return nil
}

func (e *Engine) phaseTest(ctx context.Context, wf *memory.Workflow, t boards.Ticket, p *phaseCtx) error {
	hctx := FromTicketMulti(t, wf.Repo, wf.Repos, p.branch, wf.Plan)
	wf.State = memory.WFTesting
	e.save(*wf)
	if err := p.hr.Run(ctx, HookBeforeTest, p.cfg.Hook(HookBeforeTest), hctx); err != nil {
		return err
	}
	testErr := e.runTests(ctx, wf.Repo, p.cfg.TestCommand)
	if testErr != nil {
		// Tests failing is logged but not fatal — verify catches real regressions.
		if e.Stderr != nil {
			fmt.Fprintf(e.Stderr, "tests failed (non-fatal): %v\n", testErr)
		}
		return nil
	}
	if err := p.hr.Run(ctx, HookAfterTest, p.cfg.Hook(HookAfterTest), hctx); err != nil {
		return err
	}
	return nil
}

func (e *Engine) phaseVerify(ctx context.Context, wf *memory.Workflow, t boards.Ticket, p *phaseCtx) error {
	hctx := FromTicketMulti(t, wf.Repo, wf.Repos, p.branch, wf.Plan)
	wf.State = memory.WFVerifying
	if wf.VerifyRuns == 0 {
		wf.VerifyRuns = e.verifyN(p.cfg)
	}
	e.save(*wf)
	if err := p.hr.Run(ctx, HookBeforeVerify, p.cfg.Hook(HookBeforeVerify), hctx); err != nil {
		return err
	}
	for i := 0; i < wf.VerifyRuns; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err := e.verifyOnce(ctx, t); err != nil {
			return fmt.Errorf("verify pass %d: %w", i+1, err)
		}
	}
	if err := p.hr.Run(ctx, HookAfterVerify, p.cfg.Hook(HookAfterVerify), hctx); err != nil {
		return err
	}
	return nil
}

// phaseUpdateMemory runs a focused agent task that asks the LLM to distil
// what it learned during execution into persistent markdown notes. This is
// where goon's "active memory" gets steady-state updates: conventions
// discovered, file layouts learned, names that matter, gotchas to avoid.
//
// Failures here are non-fatal — the workflow continues to PR even if the
// memory update misbehaves, because losing a learning is preferable to
// blocking a finished ticket.
func (e *Engine) phaseUpdateMemory(ctx context.Context, wf *memory.Workflow, t boards.Ticket, p *phaseCtx) error {
	_ = p
	wf.State = memory.WFUpdatingMemory
	e.save(*wf)
	if e.LLM == nil || e.Tools == nil || e.Executor == nil {
		// No agent runtime — silently skip.
		return nil
	}
	a := agent.New(agent.Options{
		LLM: e.LLM, Tools: e.Tools, Executor: e.Executor, Memory: e.Memory,
		Stdout: e.Stdout, Stderr: e.Stderr, Debug: e.Debug,
	})
	task := fmt.Sprintf(`Reflect on what you just learned implementing %s — %q.
Use memory_append (preferred) or memory_write to capture durable knowledge:
- conventions discovered
- file layouts and names that matter
- bugs avoided / gotchas
- API/CLI quirks worth remembering
One topic per .md file (kebab-case names). Anything broadly invariant goes in PINNED.md.
Skip notes that are trivial or only apply to this single ticket. Call finish when done.`, t.Key, t.Title)
	if err := a.Run(ctx, task); err != nil {
		if e.Stderr != nil {
			fmt.Fprintf(e.Stderr, "memory update failed (non-fatal): %v\n", err)
		}
	}
	return nil
}

func (e *Engine) phaseOpenPR(ctx context.Context, wf *memory.Workflow, t boards.Ticket, p *phaseCtx) error {
	if e.Host == nil {
		return nil
	}
	hctx := FromTicketMulti(t, wf.Repo, wf.Repos, p.branch, wf.Plan)
	wf.State = memory.WFOpeningPR
	e.save(*wf)
	if err := p.hr.Run(ctx, HookBeforePR, p.cfg.Hook(HookBeforePR), hctx); err != nil {
		return err
	}
	pr, err := e.openPR(ctx, t, *wf, wf.Repo, p.cfg)
	if err != nil {
		return err
	}
	wf.PRURL = pr.URL
	wf.Branch = pr.Branch
	e.save(*wf)
	if err := p.hr.Run(ctx, HookAfterPR, p.cfg.Hook(HookAfterPR), hctx); err != nil {
		// after_pr failure is non-fatal — PR is already up.
		if e.Stderr != nil {
			fmt.Fprintf(e.Stderr, "after_pr hook failed (non-fatal): %v\n", err)
		}
	}
	return nil
}

func (e *Engine) phaseNotify(ctx context.Context, wf *memory.Workflow, t boards.Ticket, p *phaseCtx) error {
	_ = p
	wf.State = memory.WFNotifying
	e.save(*wf)
	e.notify(ctx, t, *wf)
	return nil
}

// approvalAnswer returns the recorded answer for stage, or "" if none.
func approvalAnswer(wf *memory.Workflow, stage string) string {
	if wf == nil || wf.Approvals == nil {
		return ""
	}
	return wf.Approvals[stage]
}

// ensureApproval records ans under stage in wf.Approvals (creating the map
// if needed). Used by both auto-approve and the post-gate happy path.
func ensureApproval(wf *memory.Workflow, stage, ans string) {
	if wf == nil {
		return
	}
	if wf.Approvals == nil {
		wf.Approvals = map[string]string{}
	}
	wf.Approvals[stage] = ans
}

// parseRepoChange recognises an answer like "change=/path/to/repo",
// "repo=/path", "use=/path", or a bare path with a slash/dot/colon and
// returns (path, true). Plain "yes" or "no" returns ("", false).
func parseRepoChange(ans string) (string, bool) {
	s := strings.TrimSpace(ans)
	for _, prefix := range []string{"change=", "repo=", "use=", "path="} {
		if strings.HasPrefix(s, prefix) {
			s = strings.TrimSpace(strings.TrimPrefix(s, prefix))
			break
		}
	}
	if s == "" || isYes(s) {
		return "", false
	}
	if strings.EqualFold(s, "no") || strings.EqualFold(s, "n") || strings.EqualFold(s, "reject") {
		return "", false
	}
	if strings.ContainsAny(s, " \t\n") {
		return "", false
	}
	// Heuristic: path-like answers contain at least one of / . : ~
	if !strings.ContainsAny(s, "/.:~") {
		return "", false
	}
	return s, true
}

// isYes treats any of yes/y/ok/approve/lgtm/etc. (case-insensitive) — and
// any "auto:..." marker — as approval.
func isYes(s string) bool {
	low := strings.ToLower(strings.TrimSpace(s))
	switch low {
	case "y", "yes", "ok", "approve", "approved", "confirm", "confirmed", "lgtm", "go", "ship":
		return true
	}
	// "yes:edited" / "yes:edited-plan" / etc. — the web plan editor
	// records these so audit logs distinguish user-edited approvals
	// from a plain yes. Same downstream behaviour.
	return strings.HasPrefix(low, "auto:") || strings.HasPrefix(low, "yes:")
}

// formatPlanForApproval renders a numbered list of plan steps for the user
// to review at the approve_plan gate.
func formatPlanForApproval(plan []memory.PlanStep) string {
	var b strings.Builder
	for i, s := range plan {
		fmt.Fprintf(&b, "  %d. %s\n", i+1, s.Title)
	}
	return b.String()
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

// branchName composes the goon-owned branch for a ticket.
// branchName composes the goon-owned branch name for a ticket. The
// canonical format is "goon/<TICKET-KEY>" where the key is preserved
// verbatim except for git-disallowed characters which become dashes.
//
// We deliberately do NOT lowercase the key — Jira ticket codes are
// conventionally uppercase ("EB-4795") and users want to recognize
// them on the branch list at a glance. Git branch names ARE
// case-sensitive but most modern hosts (GitHub, GitLab, Bitbucket
// Cloud) preserve case correctly. If a workflow.json overrides
// BranchPrefix, the same sanitization applies to whatever they pick.
func branchName(prefix, key string) string {
	if prefix == "" {
		prefix = "goon/"
	}
	if !strings.HasSuffix(prefix, "/") && !strings.HasSuffix(prefix, "-") && !strings.HasSuffix(prefix, "_") {
		prefix += "/"
	}
	return prefix + sanitizeBranchSegment(key)
}

// sanitizeBranchSegment replaces git-disallowed characters with "-"
// while preserving case. Allows ASCII letters, digits, "-", "_", "."
// — every other rune (spaces, "#", "/" inside the segment, unicode
// punctuation) collapses to a dash. Repeated dashes get squashed and
// edge dashes/dots/underscores get trimmed.
//
// Empty input becomes "unknown" so we never produce a refspec like
// "goon/" with nothing after it (git rejects that).
func sanitizeBranchSegment(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := b.String()
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	out = strings.Trim(out, "-_.")
	if out == "" {
		return "unknown"
	}
	return out
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

// triage is the legacy zero-feedback entry point — kept so older callers
// keep compiling. New code should call triageWithFeedback so the user's
// previous-rejection feedback (if any) is woven into the prompt.
func (e *Engine) triage(ctx context.Context, t boards.Ticket) ([]memory.PlanStep, string, error) {
	return e.triageWithFeedback(ctx, t, "")
}

// triageWithFeedback asks the LLM to produce a structured plan. When
// feedback is non-empty it's woven into the prompt under a REJECTED:
// section so the model knows what the previous plan got wrong and can
// produce a different one.
func (e *Engine) triageWithFeedback(ctx context.Context, t boards.Ticket, feedback string) ([]memory.PlanStep, string, error) {
	if e.LLM == nil {
		return nil, "", errors.New("triage: no LLM provider configured")
	}
	// Use the memory-aware picker so the LLM sees a sensible default
	// in the prompt (env > learned > wildcard > project literal).
	repo := e.pickRepoForTicket(t)
	feedbackBlock := ""
	if strings.TrimSpace(feedback) != "" {
		feedbackBlock = fmt.Sprintf(`
PREVIOUS PLAN WAS REJECTED. The user said:
  %q
Produce a different plan that addresses the feedback.
`, feedback)
	}
	prompt := fmt.Sprintf(`You are GOON's planner. The user wants you to break this ticket into 3-7 ordered, atomic engineering steps that an autonomous agent can execute one-by-one. Each step MUST be small enough to finish in <= 5 tool calls.
%s
Reply with EXACTLY ONE JSON object: {"steps":[{"title":"..."}, ...], "repo":"%s"}.
No prose, no fences, no comments.

TICKET:
key: %s
title: %s
description: %s
`, feedbackBlock, repo, t.Key, t.Title, snippet(t.Description, 1500))

	out, err := e.LLM.Generate(ctx, []llm.Message{
		{Role: llm.RoleUser, Content: prompt},
	}, llm.Options{
		Temperature: 0.1,
		JSONMode:    true,
		// 4096 (was 800) — Gemini 2.5 spends invisible thinking
		// tokens before producing the JSON plan; with the lower
		// cap the plan got truncated mid-string and parseTriage
		// rejected it as unterminated JSON. The OpenAI/Anthropic
		// adapters happily ignore the higher cap; only providers
		// that bill per output token feel it, and a plan rarely
		// exceeds ~600 tokens of actual JSON.
		MaxTokens: 4096,
	})
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
	// Declarative stages also benefit from the learned repo cache.
	repo := e.pickRepoForTicket(t)
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
	if e.Board != nil && wf.PRURL != "" {
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
