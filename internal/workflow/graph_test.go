package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/harisaginting/goon/internal/boards"
	"github.com/harisaginting/goon/internal/llm"
	"github.com/harisaginting/goon/internal/memory"
	"github.com/harisaginting/goon/internal/tools"
)

// ── shared test helpers ─────────────────────────────────────────────────────

func ticketFixture() boards.Ticket {
	return boards.Ticket{
		ID: "o/r#1", Source: "github", Key: "ENG-1",
		Title: "Add login", Description: "OAuth endpoint", Project: "o/r",
	}
}

// fakeTool is a network-free tools.Tool: records its last args and returns a
// canned Result.
type fakeTool struct {
	name     string
	out      string
	err      error
	lastArgs map[string]string
}

func (f *fakeTool) Name() string              { return f.name }
func (f *fakeTool) Description() string       { return "fake " + f.name }
func (f *fakeTool) Schema() map[string]string { return map[string]string{} }
func (f *fakeTool) Run(_ context.Context, args map[string]string) (tools.Result, error) {
	f.lastArgs = args
	if f.err != nil {
		return tools.Result{}, f.err
	}
	return tools.Result{ToolName: f.name, Stdout: f.out}, nil
}

// seqTool is a fake 'telegram' tool that records every message it is asked to
// send, in order — notify nodes named after themselves turn it into an
// execution-order recorder for routing tests.
type seqTool struct{ calls []string }

func (s *seqTool) Name() string              { return "telegram" }
func (s *seqTool) Description() string       { return "fake telegram" }
func (s *seqTool) Schema() map[string]string { return map[string]string{} }
func (s *seqTool) Run(_ context.Context, args map[string]string) (tools.Result, error) {
	s.calls = append(s.calls, args["text"])
	return tools.Result{ToolName: "telegram", Stdout: args["text"]}, nil
}

func notifyStage(name string) StageConfig {
	return StageConfig{Name: name, Type: RoleNotify, Message: name}
}

// graphEngine builds an Engine wired for graph tests: a mock LLM (nil → an
// empty mock), an in-memory store, the given tools, and a temp storage dir so
// learnings/notes writes stay out of the repo. Config{Version:1} keeps
// resolveConfig from touching LoadConfig.
func graphEngine(t *testing.T, mock *llm.Mock, toolset ...tools.Tool) (*Engine, *bytes.Buffer) {
	t.Helper()
	t.Setenv("GOON_STORAGE_DIR", t.TempDir())
	reg := tools.NewRegistry()
	for _, tl := range toolset {
		reg.Register(tl)
	}
	var out bytes.Buffer
	if mock == nil {
		mock = llm.NewMock(nil)
	}
	e := &Engine{
		LLM: mock, Tools: reg, Memory: memory.Disabled(),
		Stdout: &out, Stderr: &out,
		Config: WorkflowConfig{Version: 1},
	}
	return e, &out
}

func freshWF() memory.Workflow {
	tk := ticketFixture()
	return memory.Workflow{
		ID: "wf-test", TicketID: tk.ID, TicketKey: tk.Key,
		State: memory.WFTriaging, Approvals: map[string]string{},
	}
}

func runGraphWF(e *Engine, wf memory.Workflow, cfg WorkflowConfig) (memory.Workflow, error) {
	hr := &HookRunner{Stdout: e.Stdout, Stderr: e.Stderr}
	return e.runGraph(context.Background(), wf, ticketFixture(), cfg, hr, "b")
}

// ── routing ─────────────────────────────────────────────────────────────────

// TestGraph_FanOutOnNext: on_next as an array runs every branch depth-first —
// the first target's chain completes before the second starts.
func TestGraph_FanOutOnNext(t *testing.T) {
	rec := &seqTool{}
	e, _ := graphEngine(t, nil, rec)
	s1 := notifyStage("s1")
	s1.OnNext = StringList{"s3", "s4"}
	s3 := notifyStage("s3")
	s3.OnNext = StringList{"s6"}
	s6 := notifyStage("s6")
	s6.OnNext = StringList{"end"}
	s4 := notifyStage("s4")
	s4.OnNext = StringList{"end"}
	cfg := WorkflowConfig{Stages: []StageConfig{s1, s3, s4, s6}}

	if _, err := runGraphWF(e, freshWF(), cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	want := "s1,s3,s6,s4"
	if got := strings.Join(rec.calls, ","); got != want {
		t.Errorf("execution order = %q, want %q", got, want)
	}
}

// TestGraph_LoopNode: a loop node sends the flow back to on_next until
// max_loops arrivals are spent, then exits via on_done.
func TestGraph_LoopNode(t *testing.T) {
	rec := &seqTool{}
	e, _ := graphEngine(t, nil, rec)
	work := notifyStage("work") // implicit next = loop (array order)
	loop := StageConfig{Name: "loop", Type: RoleLoop, OnNext: StringList{"work"}, MaxLoops: 2, OnDone: "after"}
	after := notifyStage("after")
	after.OnNext = StringList{"end"}
	cfg := WorkflowConfig{Stages: []StageConfig{work, loop, after}}

	if _, err := runGraphWF(e, freshWF(), cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	want := "work,work,work,after"
	if got := strings.Join(rec.calls, ","); got != want {
		t.Errorf("execution order = %q, want %q", got, want)
	}
}

// TestGraph_RejectLoopBounded: reject_if forces a reject; on_reject pointing at
// the node itself is a bounded retry that hard-fails after max_loops.
func TestGraph_RejectLoopBounded(t *testing.T) {
	rec := &seqTool{}
	e, _ := graphEngine(t, nil, rec)
	s := notifyStage("flaky")
	s.RejectIf = "true"
	s.OnReject = "flaky"
	s.MaxLoops = 2
	cfg := WorkflowConfig{Stages: []StageConfig{s}}

	_, err := runGraphWF(e, freshWF(), cfg)
	if err == nil || !strings.Contains(err.Error(), "max reject loops") {
		t.Fatalf("err = %v, want max-reject-loops failure", err)
	}
	if len(rec.calls) != 3 { // initial + 2 retries
		t.Errorf("flaky ran %d times, want 3", len(rec.calls))
	}
}

// ── reviewer ────────────────────────────────────────────────────────────────

// TestGraph_ReviewerLLMApprove: an llm-mode reviewer that approves routes to
// on_approve (fan-out to a notify here).
func TestGraph_ReviewerLLMApprove(t *testing.T) {
	mock := llm.NewMock([]string{`{"decision":"approve","reason":"lgtm"}`})
	rec := &seqTool{}
	e, _ := graphEngine(t, mock, rec)
	rev := StageConfig{Name: "review", Type: RoleReviewer, Mode: ReviewerModeLLM,
		OnApprove: StringList{"done"}, OnReject: "done"}
	done := notifyStage("done")
	done.OnNext = StringList{"end"}
	cfg := WorkflowConfig{Stages: []StageConfig{rev, done}}

	wf, err := runGraphWF(e, freshWF(), cfg)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Join(rec.calls, ",") != "done" {
		t.Errorf("notify calls = %v, want [done]", rec.calls)
	}
	if wf.State != memory.WFDone {
		t.Errorf("state = %q, want done", wf.State)
	}
}

// TestGraph_ReviewerLLMReject: an llm-mode reviewer that rejects routes to
// on_reject.
func TestGraph_ReviewerLLMReject(t *testing.T) {
	mock := llm.NewMock([]string{`{"decision":"reject","reason":"missing tests"}`})
	rec := &seqTool{}
	e, _ := graphEngine(t, mock, rec)
	rev := StageConfig{Name: "review", Type: RoleReviewer, Mode: ReviewerModeLLM,
		OnApprove: StringList{"good"}, OnReject: "bad"}
	good := notifyStage("good")
	good.OnNext = StringList{"end"}
	bad := notifyStage("bad")
	bad.OnNext = StringList{"end"}
	cfg := WorkflowConfig{Stages: []StageConfig{rev, good, bad}}

	if _, err := runGraphWF(e, freshWF(), cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Join(rec.calls, ",") != "bad" {
		t.Errorf("notify calls = %v, want [bad]", rec.calls)
	}
}

// TestGraph_AnalystAsk: an llm reviewer that asks the analyst gets an answer,
// is re-run with it, then approves. Exercises the ask → consult → re-run loop.
func TestGraph_AnalystAsk(t *testing.T) {
	mock := llm.NewMock([]string{
		`{"decision":"ask","question":"does the service use OAuth?"}`,
		`OAuth2 with PKCE.`, // analyst answer
		`{"decision":"approve","reason":"clear now"}`,
	})
	rec := &seqTool{}
	e, _ := graphEngine(t, mock, rec)
	rev := StageConfig{Name: "review", Type: RoleReviewer, Mode: ReviewerModeLLM,
		Ask: "kb", OnApprove: StringList{"done"}, OnReject: "done", MaxLoops: 3}
	kb := StageConfig{Name: "kb", Type: RoleAnalyst, Prompt: "Answer: {{.AskQuestion}}"}
	done := notifyStage("done")
	done.OnNext = StringList{"end"}
	cfg := WorkflowConfig{Stages: []StageConfig{rev, kb, done}}

	if _, err := runGraphWF(e, freshWF(), cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	if mock.Calls != 3 {
		t.Errorf("LLM calls = %d, want 3 (ask, analyst answer, approve)", mock.Calls)
	}
	if strings.Join(rec.calls, ",") != "done" {
		t.Errorf("notify calls = %v, want [done]", rec.calls)
	}
}

// TestGraph_ReviewerHumanGate: a human reviewer pauses (queues a question), and
// resumes on the next runGraph once the answer is on file — the same gate the
// linear pipeline uses, now driving the role-graph.
func TestGraph_ReviewerHumanGate(t *testing.T) {
	rec := &seqTool{}
	e, _ := graphEngine(t, nil, rec)
	rev := StageConfig{Name: "review", Type: RoleReviewer, Mode: ReviewerModeHuman,
		OnApprove: StringList{"done"}, OnReject: "rework"}
	done := notifyStage("done")
	done.OnNext = StringList{"end"}
	cfg := WorkflowConfig{Stages: []StageConfig{rev, done}}

	wf1, err := runGraphWF(e, freshWF(), cfg)
	if err != nil {
		t.Fatalf("run1: %v", err)
	}
	if wf1.State != memory.WFAwaitingApproval {
		t.Fatalf("run1 state = %q, want awaiting_approval", wf1.State)
	}
	if wf1.Stage != "review" {
		t.Fatalf("run1 stage = %q, want review", wf1.Stage)
	}
	if wf1.PendingQuestionID == "" {
		t.Fatal("run1: no pending question queued")
	}
	if len(rec.calls) != 0 {
		t.Fatalf("notify fired before approval: %v", rec.calls)
	}

	// Approve and resume.
	if !e.Memory.AnswerQuestion(wf1.PendingQuestionID, "approve") {
		t.Fatal("AnswerQuestion failed")
	}
	wf2, err := runGraphWF(e, wf1, cfg)
	if err != nil {
		t.Fatalf("run2: %v", err)
	}
	if wf2.State != memory.WFDone {
		t.Errorf("run2 state = %q, want done", wf2.State)
	}
	if strings.Join(rec.calls, ",") != "done" {
		t.Errorf("notify calls = %v, want [done]", rec.calls)
	}
}

// TestGraph_NotifyRendersMessage: a notify node renders its template against
// the ticket and hands the text to the 'telegram' tool.
func TestGraph_NotifyRendersMessage(t *testing.T) {
	rec := &seqTool{}
	e, _ := graphEngine(t, nil, rec)
	cfg := WorkflowConfig{Stages: []StageConfig{
		{Name: "ping", Type: RoleNotify, Message: "Done with {{.Key}}: {{.Title}}"},
	}}
	if _, err := runGraphWF(e, freshWF(), cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	want := "Done with ENG-1: Add login"
	if len(rec.calls) != 1 || rec.calls[0] != want {
		t.Errorf("telegram calls = %v, want [%q]", rec.calls, want)
	}
}

// ── validation ──────────────────────────────────────────────────────────────

func TestValidateStages_Roles(t *testing.T) {
	cases := []struct {
		name    string
		stages  []StageConfig
		wantErr bool
	}{
		{"empty is fine", nil, false},
		{"analyst ok", []StageConfig{{Name: "a", Type: RoleAnalyst}}, false},
		{"executor ok (empty task = default)", []StageConfig{{Name: "x", Type: RoleExecutor}}, false},
		{"executor bad do", []StageConfig{{Name: "x", Type: RoleExecutor, Do: "delete_repo"}}, true},
		{"reviewer ok", []StageConfig{{Name: "r", Type: RoleReviewer, OnApprove: StringList{"end"}}}, false},
		{"reviewer needs routing", []StageConfig{{Name: "r", Type: RoleReviewer}}, true},
		{"reviewer bad mode", []StageConfig{{Name: "r", Type: RoleReviewer, Mode: "robot", OnReject: "r"}}, true},
		{"notify ok", []StageConfig{{Name: "n", Type: RoleNotify, Message: "hi"}}, false},
		{"notify missing message", []StageConfig{{Name: "n", Type: RoleNotify}}, true},
		{"loop ok", []StageConfig{{Name: "l", Type: RoleLoop, OnNext: StringList{"l"}}}, false},
		{"loop without target", []StageConfig{{Name: "l", Type: RoleLoop}}, true},
		{"duplicate names", []StageConfig{{Name: "a", Type: RoleAnalyst}, {Name: "a", Type: RoleNotify, Message: "x"}}, true},
		{"removed llm type", []StageConfig{{Name: "a", Type: "llm", Prompt: "p"}}, true},
		{"removed agent type", []StageConfig{{Name: "a", Type: "agent", Task: "t"}}, true},
		{"unknown type", []StageConfig{{Name: "a", Type: "wat"}}, true},
		{"ask must target analyst", []StageConfig{
			{Name: "r", Type: RoleReviewer, Ask: "n", OnReject: "r"},
			{Name: "n", Type: RoleNotify, Message: "x"},
		}, true},
		{"ask target ok", []StageConfig{
			{Name: "r", Type: RoleReviewer, Ask: "kb", OnReject: "r"},
			{Name: "kb", Type: RoleAnalyst},
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateStages(tc.stages)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateStages err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// TestStringList_JSON: on_next accepts both a bare string and an array, and
// marshals a single element back to a bare string (workflow.json stability).
func TestStringList_JSON(t *testing.T) {
	var s StageConfig
	if err := json.Unmarshal([]byte(`{"name":"a","type":"executor","on_next":"b"}`), &s); err != nil {
		t.Fatalf("string form: %v", err)
	}
	if len(s.OnNext) != 1 || s.OnNext[0] != "b" {
		t.Errorf("string form parsed as %v", s.OnNext)
	}
	if err := json.Unmarshal([]byte(`{"name":"a","type":"reviewer","on_approve":["b","c"]}`), &s); err != nil {
		t.Fatalf("array form: %v", err)
	}
	if len(s.OnApprove) != 2 || s.OnApprove[1] != "c" {
		t.Errorf("array form parsed as %v", s.OnApprove)
	}
	b, err := json.Marshal(StringList{"only"})
	if err != nil || string(b) != `"only"` {
		t.Errorf("single marshals to %s (%v), want \"only\"", b, err)
	}
	b, _ = json.Marshal(StringList{"x", "y"})
	if string(b) != `["x","y"]` {
		t.Errorf("multi marshals to %s, want [\"x\",\"y\"]", b)
	}
}

// ── template helpers ────────────────────────────────────────────────────────

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

func TestTemplateFunc_GetAndDefault(t *testing.T) {
	state := &StageState{
		HookCtx: HookCtx{Key: "X"},
		Stages:  map[string]any{"q": map[string]any{"tier": "hot"}},
	}
	cases := map[string]string{
		`{{get .Stages.q "tier"}}`:                     "hot",
		`{{default "warm" (get .Stages.q "missing")}}`: "warm",
		`{{default "warm" (get .Stages.q "tier")}}`:    "hot",
		`{{if ne (get .Stages.q "tier") "cold"}}go{{end}}`: "go",
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

// TestRenderTemplate_StateFields verifies the durable mirrors (PRURL, Ask,
// AskQuestion) are reachable from node templates.
func TestRenderTemplate_StateFields(t *testing.T) {
	state := &StageState{
		HookCtx:     HookCtx{Key: "ENG-1"},
		Stages:      map[string]any{},
		PRURL:       "https://example.com/pr/1",
		Ask:         "the analyst answer",
		AskQuestion: "the question",
	}
	got, err := renderTemplate("t", `{{.Key}}|{{.PRURL}}|{{.Ask}}|{{.AskQuestion}}`, state)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	want := "ENG-1|https://example.com/pr/1|the analyst answer|the question"
	if got != want {
		t.Errorf("render = %q, want %q", got, want)
	}
}
