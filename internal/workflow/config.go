package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/harisaginting/goon/internal/logx"
	"github.com/harisaginting/goon/internal/storage"
)

// WorkflowConfig is the user-customizable description of how the workflow
// runs each ticket. Loaded from JSON; every field is optional and defaults
// to the values returned by DefaultConfig().
//
// Hook keys (each value is a list of shell commands run via sh -c, with the
// ticket exported as env vars TICKET_KEY, TICKET_TITLE, TICKET_URL,
// TICKET_PROJECT, REPO, BRANCH):
//
//	before_triage    — before the planner LLM call
//	before_execute   — after triage, before plan execution starts
//	after_execute    — after every plan step has finished
//	before_test      — before the test command runs
//	after_test       — after tests pass (skipped if tests failed)
//	before_verify    — before the first verification pass
//	after_verify     — after all verification passes
//	before_pr        — right before the PR is opened (great for fmt/lint)
//	after_pr         — after the PR is opened
//	on_failure       — when any phase fails (best-effort)
//
// Each hook list is run sequentially, stopping at the first error. A failed
// hook fails the whole workflow phase (except on_failure, which is best-
// effort). Commands still go through goon's safety validator, so you can't
// accidentally rm -rf / from a hook.
//
// Templates available in PRTitleTemplate and PRBodyTemplate:
//
//	{{.Key}}     ticket key (e.g. "ENG-123")
//	{{.Title}}   ticket title
//	{{.URL}}     ticket URL (Jira/GitHub)
//	{{.Source}}  "jira" | "github"
//	{{.Project}} board project key (e.g. "ENG" or "owner/repo")
//	{{.Branch}}  branch name goon pushed
//	{{.Repo}}    local repo path
//	{{.Plan}}    []PlanStep with .Title and .Done
type WorkflowConfig struct {
	Version int `json:"version,omitempty"`

	// Name identifies which workflow this file represents. Surfaced at startup
	// (`goon start` and `goon workflow show`) and on every per-ticket
	// `workflow.start` log line so operators can tell at a glance which
	// pipeline is in use. Defaults to "default" when omitted.
	Name string `json:"name,omitempty"`

	// Description is an optional human-readable summary of what this
	// workflow does. Printed by `goon workflow show`.
	Description string `json:"description,omitempty"`

	// BranchPrefix is prepended to the lowercased ticket key. Default "goon/".
	BranchPrefix string `json:"branch_prefix,omitempty"`

	// TestCommand overrides the auto-detected test command. Default "" =
	// auto-detect ("make test" if Makefile, "go test ./..." otherwise).
	TestCommand string `json:"test_command,omitempty"`

	// VerifyRuns overrides GOON_VERIFY_RUNS / the package default. 0 = inherit.
	VerifyRuns int `json:"verify_runs,omitempty"`

	// AutoApprove, when true, skips the user-approval gates (confirm_repo,
	// approve_plan) so the daemon runs fully unattended. Useful for trusted
	// pipelines or CI-driven runs. Env var GOON_AUTO_APPROVE=1 also works
	// (env wins over file). Default false — gates fire and the workflow
	// pauses until the user replies via `goon train` or the web UI.
	AutoApprove bool `json:"auto_approve,omitempty"`

	// PRTitleTemplate / PRBodyTemplate are Go text/template strings.
	PRTitleTemplate string `json:"pr_title_template,omitempty"`
	PRBodyTemplate  string `json:"pr_body_template,omitempty"`

	// Labels added to the PR (in addition to the built-in "goon", "auto").
	ExtraLabels []string `json:"extra_labels,omitempty"`

	// Hooks: phase name -> list of shell commands.
	Hooks map[string][]string `json:"hooks,omitempty"`

	// Stages is the role-graph goon runs for each ticket: a set of typed nodes
	// (analyst | executor | reviewer | loop | notify — see StageConfig) wired by
	// action. This is goon's DEFAULT pipeline — LoadConfig injects
	// BuiltinRoleGraph() when a config omits stages, so the graph runs even with
	// an empty or stage-less workflow.json. Nodes share a state map and can
	// reference earlier outputs via templates ({{.Stages.NAME.field}}).
	//
	// (The legacy linear phase pipeline still runs when an Engine is handed an
	// explicit stage-less Config — used by tests and back-compat callers.)
	Stages []StageConfig `json:"stages,omitempty"`

	// Trigger decides what starts this workflow: "board" (ticket-driven — the
	// software-factory default) or "schedule" (an automation fired by the
	// cron/interval in Trigger). Empty/absent = board.
	Trigger Trigger `json:"trigger,omitempty"`

	// Enabled lets an automation be paused without deleting it. A nil/absent
	// value means enabled, so existing board pipelines keep running.
	Enabled *bool `json:"enabled,omitempty"`

	// Layout carries editor node positions (keyed by node name, plus the
	// "__start"/"__finish" lifecycle markers) so a SHARED workflow.json opens
	// with the same canvas view for whoever loads it. Cosmetic only — the
	// engine ignores it.
	Layout map[string]Pos `json:"layout,omitempty"`
}

// Pos is an editor canvas position in world coordinates.
type Pos struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// Trigger configures what fires a workflow.
//
//	board    — the daemon polls a ticket board and runs the graph per ticket.
//	schedule — the daemon's scheduler runs the graph on Cron / Every.
//	manual   — only runs on an explicit "run now".
type Trigger struct {
	Type  string `json:"type,omitempty"`  // "board" (default) | "schedule" | "manual"
	Cron  string `json:"cron,omitempty"`  // 5-field cron, when Type=="schedule"
	Every string `json:"every,omitempty"` // simple interval alt: "30m" | "1h" | "24h"
}

// IsScheduled reports whether this workflow is a scheduled automation.
func (c WorkflowConfig) IsScheduled() bool {
	return strings.EqualFold(strings.TrimSpace(c.Trigger.Type), "schedule")
}

// IsEnabled reports whether the workflow should run (nil Enabled == enabled).
func (c WorkflowConfig) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// Role constants — the node "roles" in a goon role-graph. A graph is a set
// of typed nodes wired by ACTION (see Action* below): each role runs, emits
// one action, and routing follows that action's target list (fan-out
// supported). Keep in sync with the dispatch switch in graph.go (runNode)
// and the web editor's TYPES metadata.
//
//	analyst  — ask-only knowledge node. Other nodes `ask` it a question; it
//	           answers (optionally fetching `urls` first to augment goon's
//	           knowledge). It never routes forward on its own. Action: answer.
//	executor — does the technical work: code, run commands, open the PR. Runs
//	           goon's autonomous agent loop inside the assigned repo. Actions:
//	           next (done → on_next) | ask (consult the analyst, then re-run).
//	reviewer — judges the executor's work. mode=human pauses and shows a person
//	           a change summary to approve/reject; mode=llm decides automatically.
//	           Actions: approve (→ on_approve, fan-out) | reject (→ on_reject,
//	           usually a loop) | ask (consult the analyst, then re-run).
//	loop     — pure routing node: bounded loop-back (no model call).
//	notify   — send a message via the 'telegram' tool.
const (
	RoleAnalyst  = "analyst"
	RoleExecutor = "executor"
	RoleReviewer = "reviewer"
	RoleLoop     = "loop"
	RoleNotify   = "notify"
)

// Action constants — what a node emits after it runs. Routing keys off the
// action; each action maps to a StringList of targets (so any action can
// fan out to several nodes that then run sequentially, sharing state).
const (
	ActionNext    = "next"    // executor finished → follow on_next
	ActionAsk     = "ask"     // node needs the analyst → consult it, then re-run this node
	ActionApprove = "approve" // reviewer approved → follow on_approve (fan-out)
	ActionReject  = "reject"  // reviewer rejected → follow on_reject (usually a loop)
	ActionAnswer  = "answer"  // analyst produced an answer (terminal for that node)
)

// Reviewer modes (StageConfig.Mode).
const (
	ReviewerModeHuman = "human" // pause and ask a person to approve/reject (default)
	ReviewerModeLLM   = "llm"   // an automated model decides approve/reject
)

// Executor built-in capabilities (StageConfig.Do) — performed with goon's own
// machinery (git host, PR templates) instead of a freeform agent task.
const (
	DoOpenPR = "open_pr" // open/update the PR for the workflow's repo + branch
)

// StringList unmarshals from either a JSON string ("a") or a JSON array
// (["a","b"]) so routing fields stay backward compatible while gaining
// fan-out support. It marshals back to a bare string when it holds
// exactly one element, keeping existing workflow.json files stable.
type StringList []string

func (l *StringList) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		*l = nil
		return nil
	}
	if strings.HasPrefix(s, "[") {
		var a []string
		if err := json.Unmarshal(b, &a); err != nil {
			return err
		}
		*l = a
		return nil
	}
	var one string
	if err := json.Unmarshal(b, &one); err != nil {
		return err
	}
	if strings.TrimSpace(one) == "" {
		*l = nil
	} else {
		*l = []string{one}
	}
	return nil
}

func (l StringList) MarshalJSON() ([]byte, error) {
	if len(l) == 1 {
		return json.Marshal(l[0])
	}
	return json.Marshal([]string(l))
}

// StageConfig declares one node in a role-graph.
//
// Common fields:
//
//	name      — unique identifier; later nodes reference output as {{.Stages.NAME.…}}
//	type      — "analyst" | "executor" | "reviewer" | "loop" | "notify"
//	if        — optional Go-template expression; node skipped when it renders to "", "false", "no", "0"
//	repeat    — run the node this many times (1 if omitted). Useful for verify-style passes.
//	on_error  — "fail" (default) | "continue" | "warn"
//
// Role-specific fields:
//
//	analyst  : prompt, system, json_mode, temperature, max_tokens, output,
//	           urls — a list of https URLs fetched (GET) before answering so the
//	           analyst can ground its reply in fresh, project-specific knowledge.
//	executor : task, max_steps, do (built-in capability, e.g. "open_pr").
//	reviewer : task (or prompt), mode ("human" default | "llm").
//	notify   : message — text sent via the 'telegram' tool. The rendered message
//	           is also stored under .Stages.<name> so later nodes can reference it.
type StageConfig struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	If      string `json:"if,omitempty"`
	Repeat  int    `json:"repeat,omitempty"`
	OnError string `json:"on_error,omitempty"`

	// Mode selects a reviewer's decision-maker: "human" (pause, show a change
	// summary, wait for approve/reject) or "llm" (an automated model decides).
	// Empty defaults to "human" for a reviewer. Ignored by other roles.
	Mode string `json:"mode,omitempty"`

	// Do names a built-in executor capability performed with goon's own
	// machinery instead of a freeform agent task. Currently: "open_pr". When
	// set, `task` is ignored. Ignored by non-executor roles.
	Do string `json:"do,omitempty"`

	// ── Routing (action → targets) ─────────────────────────────────────────────
	//
	// By default nodes execute in array order. Routing fields wire the graph by
	// ACTION — each maps the named action to the node(s) to run next. Every
	// target field is a StringList: a single name OR an array for fan-out
	// (branches run sequentially, sharing state; the first branch's whole chain
	// completes before the next starts). "end" terminates that branch.
	//
	//   on_next    — executor emitted `next` (or any non-routing node finished).
	//   on_approve — reviewer emitted `approve`. Fan-out lives here, e.g.
	//                ["open_pr","notify"].
	//   on_reject  — reviewer emitted `reject` (or a node failed reject_if).
	//                Usually points at a loop node that sends flow back.
	//   ask        — name of the analyst node to consult when this node emits
	//                `ask`. The analyst answers (its reply lands in .Ask) and
	//                THIS node is re-run with that knowledge, bounded by max_loops.
	//   reject_if  — advanced: a Go-template expression evaluated after the node
	//                runs; truthy ("" / "false" / "no" / "0" are falsy) forces a
	//                reject regardless of role, routing via on_reject.
	//   max_loops  — for normal nodes: max times on_reject / ask can loop back
	//                before the graph hard-fails (default 3). For type=loop:
	//                maximum iterations of the loop body.
	//   on_done    — type=loop only: node to continue with once max_loops
	//                iterations are spent. Empty = next in array order.

	OnNext    StringList `json:"on_next,omitempty"`
	OnApprove StringList `json:"on_approve,omitempty"`
	OnReject  string     `json:"on_reject,omitempty"`
	Ask       string     `json:"ask,omitempty"`
	RejectIf  string     `json:"reject_if,omitempty"`
	MaxLoops  int        `json:"max_loops,omitempty"`
	OnDone    string     `json:"on_done,omitempty"`

	// Model override — leave empty to use the process-wide default.
	// Provider is the provider name (openai | anthropic | gemini | ollama | mock).
	// Model is the model string (e.g. "gpt-4o", "claude-opus-4-5").
	// When only Model is set, the current provider is used with that model.
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`

	// analyst / reviewer(llm)
	Prompt      string     `json:"prompt,omitempty"`
	System      string     `json:"system,omitempty"`
	JSONMode    bool       `json:"json_mode,omitempty"`
	Temperature float64    `json:"temperature,omitempty"`
	MaxTokens   int        `json:"max_tokens,omitempty"`
	Output      string     `json:"output,omitempty"` // override key under .Stages (defaults to Name)
	URLs        StringList `json:"urls,omitempty"`   // analyst: pages fetched to augment knowledge

	// executor / reviewer
	Task     string `json:"task,omitempty"`
	MaxSteps int    `json:"max_steps,omitempty"`

	// notify
	Message string `json:"message,omitempty"`
}

// Hook phase names. Keep in sync with the engine.
const (
	HookBeforeTriage  = "before_triage"
	HookBeforeExecute = "before_execute"
	HookAfterExecute  = "after_execute"
	HookBeforeTest    = "before_test"
	HookAfterTest     = "after_test"
	HookBeforeVerify  = "before_verify"
	HookAfterVerify   = "after_verify"
	HookBeforePR      = "before_pr"
	HookAfterPR       = "after_pr"
	HookOnFailure     = "on_failure"
)

// AllHooks lists every supported hook name (handy for `goon workflow show`).
var AllHooks = []string{
	HookBeforeTriage, HookBeforeExecute, HookAfterExecute,
	HookBeforeTest, HookAfterTest,
	HookBeforeVerify, HookAfterVerify,
	HookBeforePR, HookAfterPR,
	HookOnFailure,
}

// DefaultConfig returns the built-in workflow shape — the source of truth
// for what `goon workflow init` writes and what every loaded config is
// merged on top of.
//
// The 9 phases are: triage → confirm_repo → approve_plan → execute → test
// → verify → update_memory → open_pr → notify. The two gates pause the
// workflow for user approval; auto_approve=true (or env GOON_AUTO_APPROVE)
// skips them for unattended runs.
func DefaultConfig() WorkflowConfig {
	return WorkflowConfig{
		Version:         1,
		Name:            "default",
		Description:     "Goon's all-purpose pipeline: triage → confirm_repo (gate) → approve_plan (gate) → execute → test → verify → update_memory → open_pr → notify. Set auto_approve=true to skip the gates for unattended runs.",
		BranchPrefix:    "goon/",
		VerifyRuns:      0, // 0 = inherit package var (default 3, env GOON_VERIFY_RUNS)
		AutoApprove:     false,
		PRTitleTemplate: "[{{.Key}}] {{.Title}}",
		PRBodyTemplate: `Resolves {{.Key}}.

## Plan
{{range .Plan}}- {{if .Done}}✓{{else}}✗{{end}} {{.Title}}
{{end}}

Branch: ` + "`{{.Branch}}`" + `
Project: ` + "`{{.Project}}`" + `

— opened autonomously by goon 🤖`,
		ExtraLabels: []string{"goon", "auto"},
		Hooks: map[string][]string{
			HookBeforeTriage:  nil,
			HookBeforeExecute: nil,
			HookAfterExecute:  nil,
			HookBeforeTest:    nil,
			HookAfterTest:     nil,
			HookBeforeVerify:  nil,
			HookAfterVerify:   nil,
			HookBeforePR:      nil,
			HookAfterPR:       nil,
			HookOnFailure:     nil,
		},
	}
}

// BuiltinRoleGraph returns goon's DEFAULT role-graph — the autonomous
// "software house" flow used when a config declares no stages of its own:
//
//	execute (executor)            do the work in the ticket's assigned repo
//	   → review (reviewer/human)  show a person a change summary; approve or reject
//	        approve → open_pr (executor) + notify   ← fan-out
//	        reject  → rework (loop ×3) → execute     ← bounded retry
//
// There is no repo gate (the ticket arrives already assigned to its repo) and
// no plan gate — the reviewer is the single human checkpoint, and it judges the
// finished change, not a guess. Set auto_approve=true (or GOON_AUTO_APPROVE=1)
// to let the reviewer approve automatically for fully-unattended runs.
//
// An analyst node can be added (and wired via `ask`) to give the executor or
// reviewer an on-demand knowledge oracle; it's omitted here to keep the default
// minimal.
func BuiltinRoleGraph() []StageConfig {
	return []StageConfig{
		{
			Name: "execute", Type: RoleExecutor,
			// Empty task → the executor plans (triage) and implements the plan
			// in the assigned repo using goon's agent loop.
			OnNext: StringList{"review"},
		},
		{
			Name: "review", Type: RoleReviewer, Mode: ReviewerModeHuman,
			Task:      "Review the executor's changes for {{.Key}} ({{.Title}}): correctness, regressions, missing edge cases.",
			OnApprove: StringList{"open_pr", "notify"},
			OnReject:  "rework",
			MaxLoops:  3,
		},
		{
			Name: "rework", Type: RoleLoop, MaxLoops: 3,
			OnNext: StringList{"execute"}, OnDone: "end",
		},
		{
			Name: "open_pr", Type: RoleExecutor, Do: DoOpenPR,
			OnNext: StringList{"end"},
		},
		{
			Name: "notify", Type: RoleNotify,
			Message: "✅ {{.Key}} approved — {{.Title}}{{if .PRURL}}\nPR: {{.PRURL}}{{end}}",
		},
	}
}

// Hook returns the configured commands for a phase, or nil if none.
func (c WorkflowConfig) Hook(name string) []string {
	if c.Hooks == nil {
		return nil
	}
	return c.Hooks[name]
}

// injectDefaultGraph makes the role-graph goon's default pipeline: when a
// loaded config declares no stages of its own, fill in BuiltinRoleGraph() so
// the daemon runs the graph even from an empty or stage-less workflow.json. A
// config that DOES declare stages is left untouched (the user's graph wins).
//
// This is intentionally applied in LoadConfig (not DefaultConfig) so the
// legacy linear engine still runs when an Engine is handed an explicit
// stage-less Config — the path the workflow_test.go suite exercises.
func injectDefaultGraph(cfg *WorkflowConfig) {
	if len(cfg.Stages) == 0 {
		cfg.Stages = BuiltinRoleGraph()
	}
}

// LoadConfig finds and loads the workflow config. Resolution order, first
// match wins:
//
//  1. $GOON_WORKFLOW_FILE
//  2. ./workflow.json                   (repo root — the new default)
//  3. <repoDir>/workflow.json           (when repoDir != "" and != ".")
//  4. <repoDir>/.goon/workflow.json     (legacy per-repo location)
//  5. ./.goon/workflow.json             (legacy current-dir location)
//  6. $XDG_CONFIG_HOME/goon/workflow.json
//  7. ~/.config/goon/workflow.json
//  8. ~/.goon/workflow.json
//
// The legacy ~/.goon and .goon/ paths are kept for backwards compat —
// people who set them up before still work — but new installs land their
// workflow at the project root, so it's grep-able and version-controllable
// next to the code.
//
// Returns DefaultConfig() if none found. The found values are merged on top
// of the defaults so partial files are valid.
func LoadConfig(repoDir string) (WorkflowConfig, string, error) {
	cfg := DefaultConfig()
	for _, p := range candidatePaths(repoDir) {
		if p == "" {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return cfg, p, fmt.Errorf("read %s: %w", p, err)
		}
		var got WorkflowConfig
		if err := json.Unmarshal(data, &got); err != nil {
			return cfg, p, fmt.Errorf("parse %s: %w", p, err)
		}
		merge(&cfg, got)
		abs, err := filepath.Abs(p)
		if err != nil {
			abs = p
		}
		injectDefaultGraph(&cfg)
		return cfg, abs, nil
	}
	injectDefaultGraph(&cfg)
	return cfg, "", nil
}

// candidatePaths returns the filesystem locations LoadConfig consults in
// order. Returned paths may not exist.
//
// Order is intentional: ./workflow.json (repo root) wins over every legacy
// location because that's where new installs put it. Legacy paths
// (.goon/workflow.json, ~/.config/goon/, ~/.goon/) are kept so older
// setups keep working — but they're tried last.
func candidatePaths(repoDir string) []string {
	out := []string{}
	if v := strings.TrimSpace(os.Getenv("GOON_WORKFLOW_FILE")); v != "" {
		out = append(out, v)
	}
	// Repo-root workflow.json — the canonical new default.
	out = append(out, "workflow.json")
	if rd := strings.TrimSpace(repoDir); rd != "" && rd != "." {
		out = append(out, filepath.Join(rd, "workflow.json"))
		out = append(out, filepath.Join(rd, ".goon", "workflow.json"))
	}
	// Legacy locations, last-resort. New installs shouldn't write here.
	out = append(out, filepath.Join(".goon", "workflow.json"))
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		out = append(out, filepath.Join(xdg, "goon", "workflow.json"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out,
			filepath.Join(home, ".config", "goon", "workflow.json"),
			filepath.Join(home, ".goon", "workflow.json"),
		)
	}
	return out
}

// Announce loads the workflow config and writes a one-line banner to w
// telling the operator which workflow is in use and where it came from.
// It also emits a structured `workflow.loaded` log line via logx so the
// same information is captured to disk.
//
// Returns the resolved config and the source path (empty when defaults
// were used) so callers can pass them onward without re-reading the file.
//
// This is called from `goon start` so the very first thing a user sees on
// the daemon's stdout is the active workflow name.
func Announce(repoDir string, w io.Writer) (WorkflowConfig, string) {
	cfg, source, err := LoadConfig(repoDir)
	name := cfg.Name
	if name == "" {
		name = "default"
		cfg.Name = name
	}
	if err != nil {
		fmt.Fprintf(w, "→ workflow: %q (config error: %v — falling back to defaults)\n", name, err)
		logx.Warn("workflow.loaded", "name", name, "source", source, "error", err.Error())
		return cfg, source
	}
	switch {
	case source == "":
		fmt.Fprintf(w, "→ workflow: %q (built-in defaults — `goon workflow init` to customize)\n", name)
	case len(cfg.Stages) > 0:
		fmt.Fprintf(w, "→ workflow: %q — %d stage(s) — %s\n", name, len(cfg.Stages), source)
	default:
		fmt.Fprintf(w, "→ workflow: %q — %s\n", name, source)
	}
	if cfg.Description != "" {
		fmt.Fprintf(w, "  %s\n", cfg.Description)
	}
	logx.Info("workflow.loaded",
		"name", name,
		"source", source,
		"stages", len(cfg.Stages),
		"hooks", len(cfg.Hooks),
	)
	return cfg, source
}

// ── Automations (scheduled workflows) ──────────────────────────────────────
//
// Beyond the main board pipeline (./workflow.json), goon runs any number of
// scheduled automations — each one a role-graph with a schedule Trigger,
// stored as its own JSON under storage/workflows/. The daemon's scheduler
// loads them and fires the due ones.

// LoadedWorkflow pairs a parsed config with the file it came from.
type LoadedWorkflow struct {
	Config WorkflowConfig
	Path   string
}

// AutomationsDir is where scheduled automations live — one JSON per workflow.
func AutomationsDir() string { return storage.Path("workflows") }

// LoadAutomations reads every automation JSON under AutomationsDir(), sorted by
// file name for stable order. A bad/unparseable file is skipped (one broken
// automation must not break the rest). Stages are NOT injected here — an
// automation with no stages is simply inert.
func LoadAutomations() []LoadedWorkflow {
	entries, err := os.ReadDir(AutomationsDir())
	if err != nil {
		return nil
	}
	out := []LoadedWorkflow{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
			continue
		}
		p := filepath.Join(AutomationsDir(), e.Name())
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var cfg WorkflowConfig
		if err := json.Unmarshal(data, &cfg); err != nil {
			logx.Warn("automation.parse_error", "path", p, "error", err.Error())
			continue
		}
		if strings.TrimSpace(cfg.Name) == "" {
			cfg.Name = strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		}
		out = append(out, LoadedWorkflow{Config: cfg, Path: p})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// AutomationSlug turns a workflow name into a safe filename stem.
func AutomationSlug(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ' || r == '.':
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "automation"
	}
	return out
}

// SaveAutomation writes (or overwrites) one automation JSON and returns its
// path. It validates the config first so the scheduler never loads a config
// the daemon would reject.
func SaveAutomation(cfg WorkflowConfig) (string, error) {
	if strings.TrimSpace(cfg.Name) == "" {
		return "", errors.New("automation: name is required")
	}
	if err := cfg.Validate(); err != nil {
		return "", err
	}
	if err := os.MkdirAll(AutomationsDir(), 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(AutomationsDir(), AutomationSlug(cfg.Name)+".json")
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(p, append(data, '\n'), 0o644); err != nil {
		return "", err
	}
	return p, nil
}

// DeleteAutomation removes one automation JSON by name.
func DeleteAutomation(name string) error {
	return os.Remove(filepath.Join(AutomationsDir(), AutomationSlug(name)+".json"))
}

// DefaultConfigFilePath returns the canonical path `goon workflow init`
// writes to. The new default is ./workflow.json — repo-local, easy to
// commit alongside the project. $GOON_WORKFLOW_FILE overrides if set so
// users can keep a custom location pinned.
func DefaultConfigFilePath() string {
	if v := strings.TrimSpace(os.Getenv("GOON_WORKFLOW_FILE")); v != "" {
		return v
	}
	abs, err := filepath.Abs("workflow.json")
	if err != nil {
		return "workflow.json"
	}
	return abs
}

// SaveDefault writes a comprehensive starter workflow.json. Used by
// `goon workflow init`. Refuses to overwrite an existing file (notes are
// precious — never blow away a user's customizations).
//
// The written file is intentionally richer than DefaultConfig() — every
// hook is pre-filled with a benign, self-documenting `echo` command so a
// new user can read the file top-to-bottom and learn what each hook does
// without consulting the docs. Replace, comment-out (by deleting the
// array entry), or leave as-is.
//
// For preset variants (declarative stages, marketing brief, sales lead,
// fully unattended), see examples/workflows/.
func SaveDefault(path string) error {
	if path == "" {
		return errors.New("save default: empty path")
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("refusing to overwrite existing %s", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	cfg := starterConfig()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// starterConfig returns the kitchen-sink config that `goon workflow init`
// writes. It builds on DefaultConfig() but pre-fills every hook with a
// benign echo command so a first-time user can read the JSON
// top-to-bottom and learn the hook surface without bouncing to docs.
//
// The starter intentionally:
//   - sets verify_runs=3 explicitly (matches the package default, but
//     having it in the file makes the override point obvious)
//   - includes every hook key, even unused ones, with empty arrays
//   - sets auto_approve=false explicitly (so the gates fire by default
//     and the user opts in to unattended mode by flipping it to true)
//   - sticks to ASCII shell commands that work on POSIX and `cmd /C`
//     on Windows (where the safety.ShellCommand helper picks the
//     right interpreter).
func starterConfig() WorkflowConfig {
	cfg := DefaultConfig()
	cfg.VerifyRuns = 3
	// `goon workflow init` writes the default role-graph so the file matches
	// what goon actually runs (LoadConfig injects the same graph when stages
	// are omitted). The reviewer is the single human gate; flip auto_approve to
	// let it approve automatically.
	cfg.Description = "goon's default role-graph: execute → reviewer (human approval) → on approve fan out to open_pr + notify; on reject loop back through rework. The reviewer node is the only human checkpoint — set auto_approve=true (or env GOON_AUTO_APPROVE=1) to let the reviewer approve automatically for unattended runs."
	cfg.Stages = BuiltinRoleGraph()
	cfg.Hooks = map[string][]string{
		HookBeforeTriage: {
			"echo \"→ goon picked up {{.Key}} — {{.Title}}\"",
		},
		HookBeforeExecute: {},
		HookAfterExecute: {
			"echo \"✓ all plan steps completed for {{.Key}}\"",
		},
		HookBeforeTest:   {},
		HookAfterTest:    {},
		HookBeforeVerify: {},
		HookAfterVerify:  {},
		HookBeforePR: {
			"echo \"→ opening PR for {{.Key}} on branch {{.Branch}}\"",
		},
		HookAfterPR: {
			"echo \"✓ PR opened for {{.Key}}\"",
		},
		HookOnFailure: {
			"echo \"workflow failed for {{.Key}} — see goon logs --tail=50\" >&2",
		},
	}
	return cfg
}

// merge overlays partial onto base, only touching fields that are non-zero
// in partial.
func merge(base *WorkflowConfig, partial WorkflowConfig) {
	if partial.Version != 0 {
		base.Version = partial.Version
	}
	if partial.Name != "" {
		base.Name = partial.Name
	}
	if partial.Description != "" {
		base.Description = partial.Description
	}
	if partial.BranchPrefix != "" {
		base.BranchPrefix = partial.BranchPrefix
	}
	if partial.TestCommand != "" {
		base.TestCommand = partial.TestCommand
	}
	if partial.VerifyRuns != 0 {
		base.VerifyRuns = partial.VerifyRuns
	}
	if partial.AutoApprove {
		base.AutoApprove = partial.AutoApprove
	}
	if partial.PRTitleTemplate != "" {
		base.PRTitleTemplate = partial.PRTitleTemplate
	}
	if partial.PRBodyTemplate != "" {
		base.PRBodyTemplate = partial.PRBodyTemplate
	}
	if len(partial.ExtraLabels) > 0 {
		base.ExtraLabels = partial.ExtraLabels
	}
	if len(partial.Hooks) > 0 {
		if base.Hooks == nil {
			base.Hooks = map[string][]string{}
		}
		for k, v := range partial.Hooks {
			base.Hooks[k] = v
		}
	}
	if len(partial.Stages) > 0 {
		// Stages are an all-or-nothing override: a partial file with `stages`
		// set replaces the built-in pipeline entirely. Preserves intuitive
		// semantics — you don't accidentally inherit a 7-phase eng pipeline
		// when you wrote a 3-stage marketing pipeline.
		base.Stages = append([]StageConfig(nil), partial.Stages...)
	}
}
