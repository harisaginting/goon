package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/template"

	"github.com/harisaginting/goon/internal/agent"
	"github.com/harisaginting/goon/internal/boards"
	"github.com/harisaginting/goon/internal/executor"
	"github.com/harisaginting/goon/internal/llm"
	"github.com/harisaginting/goon/internal/memory"
	"github.com/harisaginting/goon/internal/tools"
)

// StageRunner executes a declarative pipeline (cfg.Stages) for a single
// ticket. It maintains a state map keyed by stage name (or stage.Output
// override) so later stages can reference earlier outputs via templates:
//
//	{{.Stages.triage.steps}}    → output of the "triage" stage
//	{{.Title}} / {{.Key}} / …    → ticket fields (HookCtx)
//
// A stage's parsed output is whatever the stage produced:
//   - llm   stage with json_mode=true: the parsed JSON object (map[string]any)
//   - llm   stage with json_mode=false: the raw string
//   - agent stage: nil (the agent returns no structured value)
//
// Hooks fire at the equivalent boundaries when a stage's name matches a
// known phase (triage, execute, test, verify, pr).
type StageRunner struct {
	LLM      llm.Provider
	Tools    *tools.Registry
	Executor *executor.Executor
	Memory   *memory.Memory
	Stdout   io.Writer
	Stderr   io.Writer
	Debug    bool
}

// StageState is the data block visible to stage templates. It carries the
// HookCtx fields (Key/Title/Repo/Branch/...) plus a Stages map of every
// completed stage's parsed output.
type StageState struct {
	HookCtx
	Stages  map[string]any
	Item    any // populated when iterating a list (reserved for future for_each)
	Attempt int // populated when Repeat > 1: 1-based pass index
	Ask     any // output of the most-recent ask_stage sub-call
}

// On-error policies.
const (
	OnErrorFail     = "fail"     // default; abort the workflow
	OnErrorContinue = "continue" // log and move on as if the stage produced nil
	OnErrorWarn     = "warn"     // alias for continue but with a louder log line
)

// RunStages executes the pipeline defined by cfg.Stages using a named-stage
// state machine. Non-linear routing (on_next, on_reject, ask_stage, loops) is
// supported alongside the original linear / repeat / if / on_error semantics.
func (r *StageRunner) RunStages(ctx context.Context, cfg WorkflowConfig, t boards.Ticket, branch, repo string) (*StageState, error) {
	state := &StageState{
		HookCtx: FromTicket(t, repo, branch, nil),
		Stages:  map[string]any{},
	}
	if len(cfg.Stages) == 0 {
		return state, errors.New("no stages defined")
	}
	if err := validateStages(cfg.Stages); err != nil {
		return state, err
	}

	// Build name→index and name→stage maps for O(1) lookup.
	stageIdx := make(map[string]int, len(cfg.Stages))
	stageMap := make(map[string]StageConfig, len(cfg.Stages))
	for i, s := range cfg.Stages {
		stageIdx[s.Name] = i
		stageMap[s.Name] = s
	}

	// loopCounts tracks times a (rejecting-stage → target) arc has fired.
	loopCounts := map[string]int{}
	total := len(cfg.Stages)
	current := cfg.Stages[0].Name

	for current != "" {
		select {
		case <-ctx.Done():
			return state, ctx.Err()
		default:
		}

		s, ok := stageMap[current]
		if !ok {
			return state, fmt.Errorf("stage %q not found in pipeline", current)
		}
		idx := stageIdx[current]

		// ── ask sub-call ─────────────────────────────────────────────────
		if s.AskStage != "" {
			ask, ok2 := stageMap[s.AskStage]
			if !ok2 {
				return state, fmt.Errorf("stage %s: ask_stage %q not found", s.Name, s.AskStage)
			}
			r.logf("  ↪ ask %s\n", ask.Name)
			askOut, err := r.runOne(ctx, ask, state)
			if err != nil {
				return state, fmt.Errorf("ask_stage %s: %w", ask.Name, err)
			}
			state.Ask = askOut
		}

		// ── conditional skip ─────────────────────────────────────────────
		if s.If != "" {
			cond, err := renderTemplate("if["+s.Name+"]", s.If, state)
			if err != nil {
				return state, fmt.Errorf("stage %s: bad if template: %w", s.Name, err)
			}
			if isFalsy(cond) {
				r.logf("⏭  skip %d/%d %s (if=%q)\n", idx+1, total, s.Name, strings.TrimSpace(cond))
				current = r.resolveNext(cfg.Stages, s, idx)
				continue
			}
		}

		// ── run (with repeat) ────────────────────────────────────────────
		repeat := s.Repeat
		if repeat <= 0 {
			repeat = 1
		}
		var lastOut any
		var stageErr error
		for pass := 1; pass <= repeat; pass++ {
			state.Attempt = pass
			r.logf("→ stage %d/%d %s (%s) pass %d/%d\n", idx+1, total, s.Name, s.Type, pass, repeat)
			out, err := r.runOne(ctx, s, state)
			if err != nil {
				stageErr = fmt.Errorf("stage %s pass %d/%d: %w", s.Name, pass, repeat, err)
				break
			}
			lastOut = out
		}
		state.Attempt = 0

		// ── on_error (execution errors, not reject) ──────────────────────
		if stageErr != nil {
			policy := strings.ToLower(strings.TrimSpace(s.OnError))
			if policy == "" {
				policy = OnErrorFail
			}
			switch policy {
			case OnErrorFail:
				return state, stageErr
			case OnErrorContinue:
				r.warnf("stage %s failed (continue): %v\n", s.Name, stageErr)
			case OnErrorWarn:
				r.warnf("stage %s failed (warn): %v\n", s.Name, stageErr)
			default:
				return state, fmt.Errorf("stage %s: unknown on_error policy %q", s.Name, policy)
			}
		}

		// Store output.
		key := s.Output
		if key == "" {
			key = s.Name
		}
		state.Stages[key] = lastOut

		// ── reject_if ────────────────────────────────────────────────────
		rejected := false
		if s.RejectIf != "" {
			cond, err := renderTemplate("reject_if["+s.Name+"]", s.RejectIf, state)
			if err != nil {
				return state, fmt.Errorf("stage %s: bad reject_if template: %w", s.Name, err)
			}
			rejected = !isFalsy(cond)
		}

		// ── routing ──────────────────────────────────────────────────────
		if rejected {
			target := strings.TrimSpace(s.OnReject)
			if target == "" || target == "end" {
				return state, fmt.Errorf("stage %s rejected (reject_if=%q)", s.Name, s.RejectIf)
			}
			loopKey := s.Name + "->" + target
			maxLoops := s.MaxLoops
			if maxLoops <= 0 {
				maxLoops = 3
			}
			loopCounts[loopKey]++
			if loopCounts[loopKey] > maxLoops {
				return state, fmt.Errorf("stage %s: max loops (%d) reached on reject path to %q", s.Name, maxLoops, target)
			}
			r.logf("↩ stage %s rejected (loop %d/%d) → %s\n", s.Name, loopCounts[loopKey], maxLoops, target)
			current = target
		} else {
			current = r.resolveNext(cfg.Stages, s, idx)
		}
	}
	return state, nil
}

// resolveNext returns the name of the stage to execute after s succeeds.
// on_next wins; otherwise falls back to the next stage in array order.
// Returns "" (done) when s is the last stage or on_next is "end".
func (r *StageRunner) resolveNext(stages []StageConfig, s StageConfig, idx int) string {
	if n := strings.TrimSpace(s.OnNext); n != "" {
		if n == "end" {
			return ""
		}
		return n
	}
	if idx+1 < len(stages) {
		return stages[idx+1].Name
	}
	return ""
}

// runOne dispatches to the per-type executor.
func (r *StageRunner) runOne(ctx context.Context, s StageConfig, state *StageState) (any, error) {
	switch strings.ToLower(strings.TrimSpace(s.Type)) {
	case StageTypeLLM:
		return r.runLLM(ctx, s, state)
	case StageTypeAgent:
		return r.runAgent(ctx, s, state)
	case StageTypeNotify:
		return r.runNotify(ctx, s, state)
	case StageTypeHTTP:
		return r.runHTTP(ctx, s, state)
	default:
		return nil, fmt.Errorf("unknown stage type %q (want %s|%s|%s|%s)",
			s.Type, StageTypeLLM, StageTypeAgent, StageTypeNotify, StageTypeHTTP)
	}
}

// runNotify sends a message via the registry's "telegram" tool — the same
// channel the built-in notify phase uses. The rendered message is returned
// (and stored under .Stages.<name>) so later stages can reference it.
//
// If no telegram tool is registered the stage errors, naming the env vars to
// set. Make it optional with on_error: continue.
func (r *StageRunner) runNotify(ctx context.Context, s StageConfig, state *StageState) (any, error) {
	if r.Tools == nil {
		return nil, errors.New("notify stage: tools registry not configured")
	}
	tool, ok := r.Tools.Get("telegram")
	if !ok {
		return nil, errors.New("notify stage: no 'telegram' tool registered (set TELEGRAM_BOT_TOKEN + a chat); use on_error: continue to make it optional")
	}
	if strings.TrimSpace(s.Message) == "" {
		return nil, errors.New("notify stage: message is required")
	}
	msg, err := renderTemplate("message["+s.Name+"]", s.Message, state)
	if err != nil {
		return nil, err
	}
	if _, err := tool.Run(ctx, map[string]string{"text": msg}); err != nil {
		return nil, fmt.Errorf("notify: %w", err)
	}
	return msg, nil
}

// runHTTP fetches an https URL (GET) via the registry's "fetch_url" tool and
// returns the response body as a string, stored under .Stages.<name> so a
// later llm/agent stage can reference it. For POST/webhooks, use an agent
// stage (or a future command stage) — fetch_url is GET-only by design.
func (r *StageRunner) runHTTP(ctx context.Context, s StageConfig, state *StageState) (any, error) {
	if r.Tools == nil {
		return nil, errors.New("http stage: tools registry not configured")
	}
	tool, ok := r.Tools.Get("fetch_url")
	if !ok {
		return nil, errors.New("http stage: no 'fetch_url' tool registered")
	}
	if strings.TrimSpace(s.URL) == "" {
		return nil, errors.New("http stage: url is required")
	}
	rawURL, err := renderTemplate("url["+s.Name+"]", s.URL, state)
	if err != nil {
		return nil, err
	}
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, errors.New("http stage: url rendered empty")
	}
	res, err := tool.Run(ctx, map[string]string{"url": rawURL})
	if err != nil {
		return nil, fmt.Errorf("http fetch: %w", err)
	}
	if res.Err != nil {
		return nil, fmt.Errorf("http fetch: %w", res.Err)
	}
	return strings.TrimSpace(res.Stdout), nil
}

// runLLM renders the prompt template, calls the LLM, and (if json_mode)
// parses the response as JSON. The parsed value (or raw string) is what
// later stages see under .Stages.<name>.
func (r *StageRunner) runLLM(ctx context.Context, s StageConfig, state *StageState) (any, error) {
	if r.LLM == nil && s.Provider == "" && s.Model == "" {
		return nil, errors.New("llm stage: no LLM provider configured")
	}
	// Per-stage model override: build a fresh provider when provider or model
	// differs from the runner's default. Falls back to r.LLM when both empty.
	stageLLM := r.LLM
	if s.Provider != "" || s.Model != "" {
		p, err := llm.NewWithOverrides(s.Provider, s.Model)
		if err != nil {
			return nil, fmt.Errorf("llm stage %q: cannot build per-stage provider: %w", s.Name, err)
		}
		stageLLM = p
	}
	if strings.TrimSpace(s.Prompt) == "" {
		return nil, errors.New("llm stage: prompt is required")
	}
	prompt, err := renderTemplate("prompt["+s.Name+"]", s.Prompt, state)
	if err != nil {
		return nil, err
	}
	var msgs []llm.Message
	if sys := strings.TrimSpace(s.System); sys != "" {
		rendered, err := renderTemplate("system["+s.Name+"]", sys, state)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: rendered})
	}
	msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: prompt})

	opts := llm.Options{
		Temperature: s.Temperature,
		MaxTokens:   s.MaxTokens,
		JSONMode:    s.JSONMode,
	}
	out, err := stageLLM.Generate(ctx, msgs, opts)
	if err != nil {
		return nil, err
	}
	if !s.JSONMode {
		return strings.TrimSpace(out), nil
	}
	chunk, err := extractJSONObject(out)
	if err != nil {
		return nil, fmt.Errorf("parse JSON: %w (raw=%q)", err, snippet(out, 200))
	}
	var parsed any
	if err := json.Unmarshal([]byte(chunk), &parsed); err != nil {
		return nil, fmt.Errorf("decode JSON: %w (raw=%q)", err, snippet(chunk, 200))
	}
	return parsed, nil
}

// runAgent renders the task template and runs goon's agent loop. The agent
// returns no structured value, so the stored state value is the rendered
// task string (so later stages can reference what was asked, even if the
// agent itself produced no JSON).
func (r *StageRunner) runAgent(ctx context.Context, s StageConfig, state *StageState) (any, error) {
	if r.Tools == nil || r.Executor == nil {
		return nil, errors.New("agent stage: agent runtime not configured (Tools/Executor required)")
	}
	if r.LLM == nil && s.Provider == "" && s.Model == "" {
		return nil, errors.New("agent stage: no LLM provider configured")
	}
	if strings.TrimSpace(s.Task) == "" {
		return nil, errors.New("agent stage: task is required")
	}
	task, err := renderTemplate("task["+s.Name+"]", s.Task, state)
	if err != nil {
		return nil, err
	}
	stageLLM := r.LLM
	if s.Provider != "" || s.Model != "" {
		p, err := llm.NewWithOverrides(s.Provider, s.Model)
		if err != nil {
			return nil, fmt.Errorf("agent stage %q: cannot build per-stage provider: %w", s.Name, err)
		}
		stageLLM = p
	}
	a := agent.New(agent.Options{
		LLM:      stageLLM,
		Tools:    r.Tools,
		Executor: r.Executor,
		Memory:   r.Memory,
		Stdout:   r.Stdout,
		Stderr:   r.Stderr,
		Debug:    r.Debug,
	})
	_ = s.MaxSteps // reserved; agent.MaxSteps is a package-level knob today
	if err := a.Run(ctx, task); err != nil {
		return task, fmt.Errorf("agent loop: %w", err)
	}
	return task, nil
}

// validateStages enforces uniqueness of stage names and presence of required
// per-type fields.
func validateStages(stages []StageConfig) error {
	seen := map[string]bool{}
	for i, s := range stages {
		name := strings.TrimSpace(s.Name)
		if name == "" {
			return fmt.Errorf("stage[%d]: name is required", i)
		}
		if seen[name] {
			return fmt.Errorf("stage[%d]: duplicate name %q", i, name)
		}
		seen[name] = true
		switch strings.ToLower(strings.TrimSpace(s.Type)) {
		case StageTypeLLM:
			if strings.TrimSpace(s.Prompt) == "" {
				return fmt.Errorf("stage %s: prompt is required for type=llm", name)
			}
		case StageTypeAgent:
			if strings.TrimSpace(s.Task) == "" {
				return fmt.Errorf("stage %s: task is required for type=agent", name)
			}
		case StageTypeNotify:
			if strings.TrimSpace(s.Message) == "" {
				return fmt.Errorf("stage %s: message is required for type=notify", name)
			}
		case StageTypeHTTP:
			if strings.TrimSpace(s.URL) == "" {
				return fmt.Errorf("stage %s: url is required for type=http", name)
			}
		default:
			return fmt.Errorf("stage %s: unknown type %q (want %s|%s|%s|%s)",
				name, s.Type, StageTypeLLM, StageTypeAgent, StageTypeNotify, StageTypeHTTP)
		}
	}
	return nil
}

// templateFuncs is the FuncMap available to every stage template. Kept
// deliberately small so the JSON config stays approachable for non-coders.
//
//	{{json .Stages.brief}}              — marshal a value back to JSON (handy
//	                                       when piping a prior stage's parsed
//	                                       output back into the next prompt)
//	{{get .Stages.x "field"}}           — safe map lookup that returns "" when
//	                                       the key is absent or the chain is nil
//	{{default "fallback" .Stages.x.y}}  — first non-empty value
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
		// Treat empty strings as "absent" too — most templates pass strings.
		if s, ok := v.(string); ok && s == "" {
			return fallback
		}
		return v
	},
}

// renderTemplate runs a Go text/template with the StageState as data and the
// shared templateFuncs registered.
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
// "" / "false" / "no" / "0" all skip the stage. Anything else runs it.
func isFalsy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "false", "no", "0", "off":
		return true
	}
	return false
}

func (r *StageRunner) logf(format string, args ...any) {
	if r.Stdout == nil {
		return
	}
	fmt.Fprintf(r.Stdout, format, args...)
}

func (r *StageRunner) warnf(format string, args ...any) {
	if r.Stderr == nil {
		return
	}
	fmt.Fprintf(r.Stderr, format, args...)
}
