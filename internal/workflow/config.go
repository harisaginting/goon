package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/harisaginting/goon/internal/logx"
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

	// Stages, when non-empty, REPLACES the built-in 7-phase pipeline with a
	// user-defined sequence. Each stage is one of a small set of typed steps
	// (llm, agent — see StageConfig). Stages run in order, share a state map,
	// and can reference earlier outputs via templates ({{.Stages.NAME.field}}).
	//
	// Hooks (before_*/after_*) still fire at the equivalent boundaries when a
	// stage's name matches a known phase (triage, execute, test, verify, pr).
	Stages []StageConfig `json:"stages,omitempty"`
}

// Stage type constants. Keep in sync with the dispatch switch in
// runner.go (runOne) and the web editor's TYPES metadata.
const (
	StageTypeLLM    = "llm"    // one model call (prompt → text/JSON)
	StageTypeAgent  = "agent"  // goon's autonomous tool-using loop
	StageTypeNotify = "notify" // send a message via the 'telegram' tool
	StageTypeHTTP   = "http"   // GET an https URL via the 'fetch_url' tool
)

// StageConfig declares one step in a user-defined pipeline.
//
// Common fields:
//
//	name      — unique identifier; later stages reference output as {{.Stages.NAME.…}}
//	type      — "llm" | "agent" | "notify" | "http"
//	if        — optional Go-template expression; stage skipped when it renders to "", "false", "no", "0"
//	repeat    — run the stage this many times (1 if omitted). Useful for verify-style passes.
//	on_error  — "fail" (default) | "continue" | "warn"
//
// Type-specific fields:
//
//	llm    : prompt, system, json_mode, temperature, max_tokens, output (named key)
//	agent  : task, max_steps
//	notify : message — text sent via the 'telegram' tool. The rendered message
//	         is also stored under .Stages.<name> so later stages can reference it.
//	http   : url — an https URL fetched (GET) via the 'fetch_url' tool. The
//	         response body is stored under .Stages.<name> as a string.
type StageConfig struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	If      string `json:"if,omitempty"`
	Repeat  int    `json:"repeat,omitempty"`
	OnError string `json:"on_error,omitempty"`

	// ── Routing ────────────────────────────────────────────────────────────────
	//
	// By default stages execute in array order. Setting these fields enables
	// non-linear pipelines: conditional branches, sub-calls, and loops.
	//
	//   on_next   — stage name to go to after a successful run. "end" finishes
	//               the pipeline. Empty = next stage in array order.
	//   reject_if — Go-template expression evaluated against StageState after
	//               the stage runs. When it renders to a truthy value (anything
	//               except "", "false", "no", "0") the stage is REJECTED and
	//               routing follows on_reject instead of on_next.
	//   on_reject — stage name to jump to on rejection. "end" aborts the
	//               pipeline with an error. Empty = "end".
	//   ask_stage — name of a helper stage to run as an inline sub-call BEFORE
	//               this stage executes. Its output is available as .Ask in
	//               the current stage's templates.
	//   max_loops — max number of times on_reject can loop back to an earlier
	//               stage before the pipeline hard-fails. Default 3.

	OnNext   string `json:"on_next,omitempty"`
	RejectIf string `json:"reject_if,omitempty"`
	OnReject string `json:"on_reject,omitempty"`
	AskStage string `json:"ask_stage,omitempty"`
	MaxLoops int    `json:"max_loops,omitempty"`

	// Model override — leave empty to use the process-wide default.
	// Provider is the provider name (openai | anthropic | gemini | ollama | mock).
	// Model is the model string (e.g. "gpt-4o", "claude-opus-4-5").
	// When only Model is set, the current provider is used with that model.
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`

	// llm
	Prompt      string  `json:"prompt,omitempty"`
	System      string  `json:"system,omitempty"`
	JSONMode    bool    `json:"json_mode,omitempty"`
	Temperature float64 `json:"temperature,omitempty"`
	MaxTokens   int     `json:"max_tokens,omitempty"`
	Output      string  `json:"output,omitempty"` // override key under .Stages (defaults to Name)

	// agent
	Task     string `json:"task,omitempty"`
	MaxSteps int    `json:"max_steps,omitempty"`

	// notify
	Message string `json:"message,omitempty"`

	// http
	URL string `json:"url,omitempty"`
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

// Hook returns the configured commands for a phase, or nil if none.
func (c WorkflowConfig) Hook(name string) []string {
	if c.Hooks == nil {
		return nil
	}
	return c.Hooks[name]
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
		return cfg, abs, nil
	}
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
