package workflow

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/harisaginting/goon/internal/boards"
	"github.com/harisaginting/goon/internal/executor"
	"github.com/harisaginting/goon/internal/llm"
	"github.com/harisaginting/goon/internal/memory"
	"github.com/harisaginting/goon/internal/safety"
	"github.com/harisaginting/goon/internal/tools"
)

func newRunner(t *testing.T, replies []string) (*StageRunner, *llm.Mock, *bytes.Buffer) {
	t.Helper()
	mock := llm.NewMock(replies)
	var out bytes.Buffer
	exec := executor.New(executor.Options{
		Mode:      executor.ModeAuto,
		Validator: safety.Default(),
		Stdin:     strings.NewReader(""),
		Stdout:    &out,
		Stderr:    &out,
	})
	r := &StageRunner{
		LLM:      mock,
		Tools:    tools.DefaultRegistry(),
		Executor: exec,
		Memory:   memory.Disabled(),
		Stdout:   &out,
		Stderr:   &out,
	}
	return r, mock, &out
}

func ticketFixture() boards.Ticket {
	return boards.Ticket{
		ID: "o/r#1", Source: "github", Key: "ENG-1",
		Title: "Add login", Description: "OAuth endpoint", Project: "o/r",
	}
}

// TestStageRunner_LLMThenAgent verifies a 2-stage pipeline: an llm stage
// produces JSON, then an agent stage references it via {{.Stages.…}}.
func TestStageRunner_LLMThenAgent(t *testing.T) {
	replies := []string{
		// stage 1: triage llm reply (json_mode)
		`{"steps":[{"title":"add OAuth handler"},{"title":"wire route"}],"repo":"o/r"}`,
		// stage 2: agent loop step → finish
		`{"tool":"finish","args":{"message":"done"}}`,
	}
	r, mock, out := newRunner(t, replies)

	cfg := WorkflowConfig{
		Stages: []StageConfig{
			{
				Name:     "triage",
				Type:     StageTypeLLM,
				Prompt:   `Plan ticket {{.Key}} ({{.Title}}). Reply JSON {"steps":[{"title":"…"}]}`,
				JSONMode: true,
			},
			{
				Name: "execute",
				Type: StageTypeAgent,
				Task: `Implement first step: {{(index (index .Stages.triage.steps 0) "title")}}`,
			},
		},
	}
	state, err := r.RunStages(context.Background(), cfg, ticketFixture(), "goon/eng-1", "")
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	if mock.Calls != 2 {
		t.Fatalf("want 2 LLM calls, got %d", mock.Calls)
	}
	triage, ok := state.Stages["triage"].(map[string]any)
	if !ok {
		t.Fatalf("triage output not parsed as JSON object: %T", state.Stages["triage"])
	}
	steps, ok := triage["steps"].([]any)
	if !ok || len(steps) != 2 {
		t.Fatalf("expected 2 steps, got %#v", triage["steps"])
	}
	first, _ := steps[0].(map[string]any)
	if first["title"] != "add OAuth handler" {
		t.Errorf("first step title = %v", first["title"])
	}
	// agent stage stores the rendered task string
	if v, hit := state.Stages["execute"]; !hit {
		t.Errorf("execute stage not found in state")
	} else if _, ok := v.(string); !ok {
		t.Errorf("execute stage state = %T (want string)", v)
	}

	// Verify the agent task was rendered with the prior stage output.
	last := mock.LastMsgs[len(mock.LastMsgs)-1].Content
	if !strings.Contains(last, "add OAuth handler") {
		t.Errorf("agent task didn't interpolate prior stage:\n%s", last)
	}
}

// TestStageRunner_RepeatRunsTwice runs an llm stage with repeat=2 and
// asserts the LLM was called twice.
func TestStageRunner_RepeatRunsTwice(t *testing.T) {
	r, mock, _ := newRunner(t, []string{
		`{"ok":true}`,
		`{"ok":true}`,
	})
	cfg := WorkflowConfig{
		Stages: []StageConfig{{
			Name: "verify", Type: StageTypeLLM, Prompt: "verify {{.Key}}",
			JSONMode: true, Repeat: 2,
		}},
	}
	if _, err := r.RunStages(context.Background(), cfg, ticketFixture(), "b", ""); err != nil {
		t.Fatalf("run: %v", err)
	}
	if mock.Calls != 2 {
		t.Errorf("want 2 calls, got %d", mock.Calls)
	}
}

// TestStageRunner_ConditionalSkip checks that a falsy `if` skips the stage.
func TestStageRunner_ConditionalSkip(t *testing.T) {
	// only one reply queued: if `if` works, only the first stage will fire.
	r, mock, _ := newRunner(t, []string{`{"a":1}`})
	cfg := WorkflowConfig{
		Stages: []StageConfig{
			{Name: "a", Type: StageTypeLLM, Prompt: "x", JSONMode: true},
			{Name: "b", Type: StageTypeLLM, Prompt: "y", JSONMode: true, If: "false"},
		},
	}
	if _, err := r.RunStages(context.Background(), cfg, ticketFixture(), "b", ""); err != nil {
		t.Fatalf("run: %v", err)
	}
	if mock.Calls != 1 {
		t.Errorf("want 1 call (skipped second), got %d", mock.Calls)
	}
}

// TestStageRunner_OnErrorContinue ensures an erroring stage with policy
// "continue" doesn't abort the pipeline.
func TestStageRunner_OnErrorContinue(t *testing.T) {
	// Only the second reply will be consumed (the first stage errors at JSON parse).
	r, mock, _ := newRunner(t, []string{
		`not-json-at-all`,
		`{"ok":true}`,
	})
	cfg := WorkflowConfig{
		Stages: []StageConfig{
			{Name: "soft", Type: StageTypeLLM, Prompt: "x", JSONMode: true, OnError: OnErrorContinue},
			{Name: "hard", Type: StageTypeLLM, Prompt: "y", JSONMode: true},
		},
	}
	state, err := r.RunStages(context.Background(), cfg, ticketFixture(), "b", "")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if mock.Calls != 2 {
		t.Errorf("want 2 calls (continue past first), got %d", mock.Calls)
	}
	if state.Stages["soft"] != nil {
		t.Errorf("soft stage should be nil after continue, got %v", state.Stages["soft"])
	}
	if state.Stages["hard"] == nil {
		t.Errorf("hard stage should have output")
	}
}

// TestValidateStages catches schema problems before any LLM call.
func TestValidateStages(t *testing.T) {
	cases := map[string][]StageConfig{
		"missing name":       {{Type: StageTypeLLM, Prompt: "p"}},
		"duplicate name":     {{Name: "x", Type: StageTypeLLM, Prompt: "p"}, {Name: "x", Type: StageTypeAgent, Task: "t"}},
		"unknown type":       {{Name: "x", Type: "wat"}},
		"llm without prompt": {{Name: "x", Type: StageTypeLLM}},
		"agent without task": {{Name: "x", Type: StageTypeAgent}},
	}
	for label, stages := range cases {
		if err := validateStages(stages); err == nil {
			t.Errorf("%s: expected error, got nil", label)
		}
	}
	ok := []StageConfig{
		{Name: "a", Type: StageTypeLLM, Prompt: "p"},
		{Name: "b", Type: StageTypeAgent, Task: "t"},
	}
	if err := validateStages(ok); err != nil {
		t.Errorf("valid stages rejected: %v", err)
	}
}

// TestStageRunner_TemplateError surfaces a bad Go template up to the caller.
// Uses a parse-time error (unbalanced braces) rather than relying on
// execute-time semantics, which vary by template option flags.
func TestStageRunner_TemplateError(t *testing.T) {
	r, _, _ := newRunner(t, nil)
	cfg := WorkflowConfig{
		Stages: []StageConfig{{
			Name: "bad", Type: StageTypeLLM,
			Prompt: "{{ this is not valid template syntax }}",
		}},
	}
	if _, err := r.RunStages(context.Background(), cfg, ticketFixture(), "b", ""); err == nil {
		t.Errorf("expected template parse error, got nil")
	}
}

// TestStageRunner_AgentErrorPropagates verifies the agent stage no longer
// silently swallows agent.Run errors.
func TestStageRunner_AgentErrorPropagates(t *testing.T) {
	// Mock LLM with no replies → agent.Run errors on first call.
	r, _, _ := newRunner(t, nil)
	cfg := WorkflowConfig{
		Stages: []StageConfig{{
			Name: "exec", Type: StageTypeAgent, Task: "do {{.Key}}",
		}},
	}
	_, err := r.RunStages(context.Background(), cfg, ticketFixture(), "b", "")
	if err == nil {
		t.Fatal("expected agent stage error to propagate, got nil")
	}
	if !strings.Contains(err.Error(), "exec") {
		t.Errorf("error should mention stage name, got: %v", err)
	}
}

// TestTemplateFunc_JSON verifies the json template helper round-trips a
// parsed JSON value back into JSON text (used by marketing-brief.json to
// pipe a prior stage's output into the next prompt).
func TestTemplateFunc_JSON(t *testing.T) {
	state := &StageState{
		HookCtx: HookCtx{Key: "X"},
		Stages: map[string]any{
			"brief": map[string]any{"audience": "devs", "channels": []any{"twitter", "blog"}},
		},
	}
	got, err := renderTemplate("t", `BRIEF: {{json .Stages.brief}}`, state)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(got, `"audience":"devs"`) {
		t.Errorf("json helper output missing field: %q", got)
	}
	if !strings.Contains(got, `"channels":["twitter","blog"]`) {
		t.Errorf("json helper output missing array: %q", got)
	}
}

// TestTemplateFunc_GetAndDefault checks the get/default helpers.
func TestTemplateFunc_GetAndDefault(t *testing.T) {
	state := &StageState{
		HookCtx: HookCtx{Key: "X"},
		Stages: map[string]any{
			"q": map[string]any{"tier": "hot"},
		},
	}
	cases := map[string]string{
		`{{get .Stages.q "tier"}}`:                         "hot",
		`{{get .Stages.q "missing"}}`:                      "<no value>",
		`{{default "warm" (get .Stages.q "missing")}}`:     "warm",
		`{{default "warm" (get .Stages.q "tier")}}`:        "hot",
		`{{if ne (get .Stages.q "tier") "cold"}}go{{end}}`: "go",
		`{{if ne (get .Stages.q "tier") "hot"}}go{{end}}`:  "",
	}
	for tpl, want := range cases {
		got, err := renderTemplate("t", tpl, state)
		if err != nil {
			t.Errorf("render %q: %v", tpl, err)
			continue
		}
		if got != want {
			t.Errorf("render %q: got %q want %q", tpl, got, want)
		}
	}
}

// TestStageRunner_HookCtxDescriptionExposed verifies tickets' Description is
// available in templates as {{.Description}} (used by marketing-brief and
// sales-lead presets).
func TestStageRunner_HookCtxDescriptionExposed(t *testing.T) {
	r, mock, _ := newRunner(t, []string{`{"ok":true}`})
	cfg := WorkflowConfig{
		Stages: []StageConfig{{
			Name: "x", Type: StageTypeLLM, JSONMode: true,
			Prompt: "title={{.Title}} desc={{.Description}}",
		}},
	}
	tk := ticketFixture()
	tk.Description = "rich body of the ticket"
	if _, err := r.RunStages(context.Background(), cfg, tk, "b", ""); err != nil {
		t.Fatalf("run: %v", err)
	}
	prompt := mock.LastMsgs[0].Content
	if !strings.Contains(prompt, "desc=rich body of the ticket") {
		t.Errorf("Description not interpolated:\n%s", prompt)
	}
}
