package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

	// BranchPrefix is prepended to the lowercased ticket key. Default "goon/".
	BranchPrefix string `json:"branch_prefix,omitempty"`

	// TestCommand overrides the auto-detected test command. Default "" =
	// auto-detect ("make test" if Makefile, "go test ./..." otherwise).
	TestCommand string `json:"test_command,omitempty"`

	// VerifyRuns overrides GOON_VERIFY_RUNS / the package default. 0 = inherit.
	VerifyRuns int `json:"verify_runs,omitempty"`

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

// Stage type constants. Keep in sync with internal/workflow/stages.go.
const (
	StageTypeLLM   = "llm"
	StageTypeAgent = "agent"
)

// StageConfig declares one step in a user-defined pipeline.
//
// Common fields:
//
//	name      — unique identifier; later stages reference output as {{.Stages.NAME.…}}
//	type      — "llm" | "agent"
//	if        — optional Go-template expression; stage skipped when it renders to "", "false", "no", "0"
//	repeat    — run the stage this many times (1 if omitted). Useful for verify-style passes.
//	on_error  — "fail" (default) | "continue" | "warn"
//
// Type-specific fields:
//
//	llm  : prompt, system, json_mode, temperature, max_tokens, output (named key)
//	agent: task, max_steps
type StageConfig struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	If      string `json:"if,omitempty"`
	Repeat  int    `json:"repeat,omitempty"`
	OnError string `json:"on_error,omitempty"`

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

// DefaultConfig returns the built-in workflow shape.
func DefaultConfig() WorkflowConfig {
	return WorkflowConfig{
		Version:         1,
		BranchPrefix:    "goon/",
		PRTitleTemplate: "[{{.Key}}] {{.Title}}",
		PRBodyTemplate: `Resolves {{.Key}}.

Plan:
{{range .Plan}}- {{if .Done}}✓{{else}}✗{{end}} {{.Title}}
{{end}}
— opened by goon 🤖`,
		ExtraLabels: nil,
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
//  2. <repoDir>/.goon/workflow.json     (when repoDir != "")
//  3. ./.goon/workflow.json
//  4. $XDG_CONFIG_HOME/goon/workflow.json
//  5. ~/.config/goon/workflow.json
//  6. ~/.goon/workflow.json
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
		return cfg, p, nil
	}
	return cfg, "", nil
}

// candidatePaths returns the filesystem locations LoadConfig consults in
// order. Returned paths may not exist.
func candidatePaths(repoDir string) []string {
	out := []string{}
	if v := strings.TrimSpace(os.Getenv("GOON_WORKFLOW_FILE")); v != "" {
		out = append(out, v)
	}
	if strings.TrimSpace(repoDir) != "" {
		out = append(out, filepath.Join(repoDir, ".goon", "workflow.json"))
	}
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

// DefaultConfigFilePath returns the canonical path goon writes to with
// `goon workflow init`.
func DefaultConfigFilePath() string {
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		return filepath.Join(xdg, "goon", "workflow.json")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "goon", "workflow.json")
	}
	return ".goon/workflow.json"
}

// SaveDefault writes the DefaultConfig to path (creating parent dirs). Used
// by `goon workflow init`. Refuses to overwrite an existing file.
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
	cfg := DefaultConfig()
	// Seed all hook keys with empty arrays so the JSON shape is obvious.
	for _, h := range AllHooks {
		if cfg.Hooks[h] == nil {
			cfg.Hooks[h] = []string{}
		}
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// merge overlays partial onto base, only touching fields that are non-zero
// in partial.
func merge(base *WorkflowConfig, partial WorkflowConfig) {
	if partial.Version != 0 {
		base.Version = partial.Version
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
