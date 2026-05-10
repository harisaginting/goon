package workflow

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/harisaginting/goon/internal/boards"
	"github.com/harisaginting/goon/internal/executor"
	"github.com/harisaginting/goon/internal/githost"
	"github.com/harisaginting/goon/internal/llm"
	"github.com/harisaginting/goon/internal/memory"
	"github.com/harisaginting/goon/internal/safety"
	"github.com/harisaginting/goon/internal/tools"
)

func newEngine(t *testing.T, replies []string) (*Engine, *bytes.Buffer, *githost.Mock, *boards.Mock, *memory.Memory) {
	t.Helper()
	mock := llm.NewMock(replies)
	reg := tools.DefaultRegistry()
	var out bytes.Buffer
	exec := executor.New(executor.Options{
		Mode:      executor.ModeAuto,
		Validator: safety.Default(),
		Stdin:     strings.NewReader(""),
		Stdout:    &out,
		Stderr:    &out,
	})
	mem := memory.Disabled()
	host := githost.NewMock()
	board := boards.NewMock(nil)
	e := &Engine{
		LLM: mock, Tools: reg, Executor: exec, Memory: mem,
		Board: board, Host: host,
		Stdout: &out, Stderr: &out,
		VerifyRunsOverride: 1, // keep tests fast
		// Tests exercise the engine end-to-end without a human at the
		// keyboard; auto-approve skips the new confirm_repo / approve_plan
		// gates. Specific tests that exercise those gates create a separate
		// engine and toggle this off.
		AutoApprove: true,
	}
	return e, &out, host, board, mem
}

func TestEngine_HappyPath(t *testing.T) {
	// Sequence the mock LLM should produce:
	//  1) triage reply (plan with 1 step)
	//  2) execute step #1 → finish
	//  3) verify pass #1 → finish
	replies := []string{
		`{"steps":[{"title":"add login endpoint"}],"repo":"o/r"}`,
		`{"tool":"finish","args":{"message":"step done"}}`,
		`{"tool":"finish","args":{"message":"verified"}}`,
	}
	e, out, host, board, _ := newEngine(t, replies)

	wf, err := e.Run(context.Background(), boards.Ticket{
		ID: "o/r#1", Source: "github", Key: "#1",
		Title: "Add login", Description: "Implement OAuth", Project: "o/r",
	})
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	if wf.State != memory.WFDone {
		t.Fatalf("state: %v err=%q", wf.State, wf.Error)
	}
	if len(host.Opened) != 1 {
		t.Fatalf("expected 1 PR opened, got %d", len(host.Opened))
	}
	if !strings.Contains(host.Opened[0].Title, "Add login") {
		t.Errorf("PR title: %q", host.Opened[0].Title)
	}
	if len(board.Comments) == 0 {
		t.Errorf("expected board comment posted")
	}
}

func TestEngine_TriageBadJSON(t *testing.T) {
	e, _, _, _, _ := newEngine(t, []string{
		`Sure, here's the plan: do everything!`, // not JSON
	})
	wf, err := e.Run(context.Background(), boards.Ticket{ID: "x"})
	if err == nil {
		t.Fatal("expected triage error")
	}
	if wf.State != memory.WFFailed || !strings.Contains(wf.Error, "triage") {
		t.Fatalf("workflow not marked failed: %+v", wf)
	}
}

func TestEngine_NoHostSkipsPR(t *testing.T) {
	replies := []string{
		`{"steps":[{"title":"x"}]}`,
		`{"tool":"finish","args":{"message":"done"}}`,
		`{"tool":"finish","args":{"message":"verified"}}`,
	}
	e, _, host, _, _ := newEngine(t, replies)
	e.Host = nil
	wf, err := e.Run(context.Background(), boards.Ticket{ID: "x", Title: "t", Project: "o/r"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if wf.State != memory.WFDone {
		t.Fatalf("state: %v", wf.State)
	}
	if len(host.Opened) != 0 {
		t.Errorf("PRs opened despite nil host: %v", host.Opened)
	}
}

func TestEngine_VerifyMultiplePasses(t *testing.T) {
	replies := []string{
		`{"steps":[{"title":"x"}]}`,
		`{"tool":"finish","args":{"message":"done"}}`,
		`{"tool":"finish","args":{"message":"v1"}}`,
		`{"tool":"finish","args":{"message":"v2"}}`,
		`{"tool":"finish","args":{"message":"v3"}}`,
	}
	e, _, _, _, _ := newEngine(t, replies)
	e.VerifyRunsOverride = 3

	wf, err := e.Run(context.Background(), boards.Ticket{ID: "x", Title: "t", Project: "o/r"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if wf.VerifyRuns != 3 {
		t.Fatalf("VerifyRuns recorded: %d", wf.VerifyRuns)
	}
	if wf.State != memory.WFDone {
		t.Fatalf("state: %v", wf.State)
	}
}

func TestParseTriage(t *testing.T) {
	cases := []struct {
		in       string
		wantLen  int
		wantRepo string
		wantErr  bool
	}{
		{`{"steps":[{"title":"a"},{"title":"b"}],"repo":"r"}`, 2, "r", false},
		{`prefix {"steps":[{"title":"a"}]} suffix`, 1, "", false},
		{"```json\n{\"steps\":[{\"title\":\"a\"}]}\n```", 1, "", false},
		{`{"steps":[]}`, 0, "", true},
		{`not json`, 0, "", true},
	}
	for _, tc := range cases {
		plan, repo, err := parseTriage(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseTriage(%q) expected error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseTriage(%q): %v", tc.in, err)
			continue
		}
		if len(plan) != tc.wantLen || repo != tc.wantRepo {
			t.Errorf("parseTriage(%q): %d/%q want %d/%q", tc.in, len(plan), repo, tc.wantLen, tc.wantRepo)
		}
	}
}

func TestEngine_RunsHooksAtEachPhase(t *testing.T) {
	dir := t.TempDir()
	// Each hook writes a marker file so we can verify ordering.
	cfg := WorkflowConfig{
		Version:      1,
		BranchPrefix: "feature/",
		Hooks: map[string][]string{
			HookBeforeExecute: {"echo {{.Key}} > " + dir + "/before_execute"},
			HookAfterExecute:  {"echo {{.Key}} > " + dir + "/after_execute"},
			HookBeforeTest:    {"echo {{.Key}} > " + dir + "/before_test"},
			HookAfterTest:     {"echo {{.Key}} > " + dir + "/after_test"},
			HookBeforePR:      {"echo {{.Key}} > " + dir + "/before_pr"},
			HookAfterPR:       {"echo {{.Key}} > " + dir + "/after_pr"},
		},
	}
	replies := []string{
		`{"steps":[{"title":"x"}]}`,
		`{"tool":"finish","args":{"message":"done"}}`,
		`{"tool":"finish","args":{"message":"verify"}}`,
	}
	e, _, host, _, _ := newEngine(t, replies)
	e.VerifyRunsOverride = 1
	e.Config = cfg

	wf, err := e.Run(context.Background(), boards.Ticket{
		ID: "ENG-1", Source: "jira", Key: "ENG-1",
		Title: "Add login", Project: "ENG",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if wf.State != memory.WFDone {
		t.Fatalf("state: %v err=%q", wf.State, wf.Error)
	}
	for _, name := range []string{"before_execute", "after_execute", "before_test", "before_pr", "after_pr"} {
		if _, err := os.Stat(dir + "/" + name); err != nil {
			t.Errorf("hook %s did not run: %v", name, err)
		}
	}
	// Branch should use the custom prefix.
	if len(host.Opened) != 1 || host.Opened[0].Head != "feature/eng-1" {
		t.Errorf("branch prefix: %+v", host.Opened)
	}
}

func TestEngine_HookFailureFailsWorkflow(t *testing.T) {
	cfg := WorkflowConfig{
		Version: 1,
		Hooks:   map[string][]string{HookBeforeExecute: {"false"}},
	}
	e, _, _, _, _ := newEngine(t, []string{
		`{"steps":[{"title":"x"}]}`,
	})
	e.Config = cfg
	wf, err := e.Run(context.Background(), boards.Ticket{ID: "X-1", Title: "t", Project: "X"})
	if err == nil {
		t.Fatal("expected hook failure to fail workflow")
	}
	if wf.State != memory.WFFailed {
		t.Errorf("state: %v", wf.State)
	}
	if !strings.Contains(wf.Error, "before_execute") {
		t.Errorf("error: %q", wf.Error)
	}
}

func TestEngine_OnFailureRunsOnAnyFailure(t *testing.T) {
	dir := t.TempDir()
	marker := dir + "/on_failure"
	cfg := WorkflowConfig{
		Version: 1,
		Hooks: map[string][]string{
			HookBeforeExecute: {"false"},
			HookOnFailure:     {"touch " + marker},
		},
	}
	e, _, _, _, _ := newEngine(t, []string{
		`{"steps":[{"title":"x"}]}`,
	})
	e.Config = cfg
	_, _ = e.Run(context.Background(), boards.Ticket{ID: "X-1", Title: "t", Project: "X"})
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("on_failure hook did not run: %v", err)
	}
}

func TestEngine_TestCommandOverride(t *testing.T) {
	dir := t.TempDir()
	marker := dir + "/test_was_run"
	cfg := WorkflowConfig{
		Version:     1,
		TestCommand: "touch " + marker,
	}
	replies := []string{
		`{"steps":[{"title":"x"}]}`,
		`{"tool":"finish","args":{"message":"done"}}`,
		`{"tool":"finish","args":{"message":"verify"}}`,
	}
	e, _, _, _, _ := newEngine(t, replies)
	e.VerifyRunsOverride = 1
	e.Config = cfg

	// Use the temp dir as the repo so runTests does anything at all.
	t.Setenv("GOON_REPO_MAP", "X="+dir)

	_, err := e.Run(context.Background(), boards.Ticket{ID: "X-1", Title: "t", Project: "X"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("custom test command did not run: %v", err)
	}
}

func TestEngine_PRTitleAndBodyTemplates(t *testing.T) {
	cfg := WorkflowConfig{
		Version:         1,
		PRTitleTemplate: "FIX({{.Key}}): {{.Title}}",
		PRBodyTemplate:  "Branch: {{.Branch}}\nProject: {{.Project}}",
		ExtraLabels:     []string{"customer-x"},
	}
	replies := []string{
		`{"steps":[{"title":"x"}]}`,
		`{"tool":"finish","args":{"message":"done"}}`,
		`{"tool":"finish","args":{"message":"verify"}}`,
	}
	e, _, host, _, _ := newEngine(t, replies)
	e.VerifyRunsOverride = 1
	e.Config = cfg

	_, err := e.Run(context.Background(), boards.Ticket{
		ID: "ENG-1", Source: "jira", Key: "ENG-1",
		Title: "Add login", Project: "ENG",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(host.Opened) != 1 {
		t.Fatalf("expected 1 PR")
	}
	got := host.Opened[0]
	if got.Title != "FIX(ENG-1): Add login" {
		t.Errorf("title: %q", got.Title)
	}
	if !strings.Contains(got.Body, "Branch: goon/eng-1") || !strings.Contains(got.Body, "Project: ENG") {
		t.Errorf("body: %q", got.Body)
	}
	hasLabel := false
	for _, l := range got.Labels {
		if l == "customer-x" {
			hasLabel = true
		}
	}
	if !hasLabel {
		t.Errorf("extra label missing: %v", got.Labels)
	}
}

func TestRepoMap(t *testing.T) {
	t.Setenv("GOON_REPO_MAP", "ENG=/r/eng,WEB=/r/web")
	m := RepoMap()
	if m["ENG"] != "/r/eng" || m["WEB"] != "/r/web" {
		t.Fatalf("RepoMap: %v", m)
	}
}

func TestPickRepo(t *testing.T) {
	t.Setenv("GOON_REPO_MAP", "ENG=/r/eng,*=/r/default")
	if got := pickRepo(boards.Ticket{Project: "ENG"}); got != "/r/eng" {
		t.Errorf("ENG: %q", got)
	}
	if got := pickRepo(boards.Ticket{Project: "OTHER"}); got != "/r/default" {
		t.Errorf("OTHER: %q", got)
	}
}

// gatedEngine is like newEngine but with AutoApprove=false so the gate
// pause/resume behaviour can be exercised end-to-end.
func gatedEngine(t *testing.T, replies []string) (*Engine, *bytes.Buffer, *githost.Mock, *boards.Mock, *memory.Memory) {
	t.Helper()
	mock := llm.NewMock(replies)
	reg := tools.DefaultRegistry()
	var out bytes.Buffer
	exec := executor.New(executor.Options{
		Mode:      executor.ModeAuto,
		Validator: safety.Default(),
		Stdin:     strings.NewReader(""),
		Stdout:    &out,
		Stderr:    &out,
	})
	mem := memory.Disabled()
	host := githost.NewMock()
	board := boards.NewMock(nil)
	e := &Engine{
		LLM: mock, Tools: reg, Executor: exec, Memory: mem,
		Board: board, Host: host,
		Stdout: &out, Stderr: &out,
		VerifyRunsOverride: 1,
		AutoApprove:        false,
	}
	return e, &out, host, board, mem
}

func TestEngine_PausesAtConfirmRepoGate(t *testing.T) {
	replies := []string{
		`{"steps":[{"title":"x"}],"repo":"o/r"}`,
	}
	e, out, host, _, mem := gatedEngine(t, replies)

	wf, err := e.Run(context.Background(), boards.Ticket{
		ID: "ENG-1", Source: "jira", Key: "ENG-1",
		Title: "Add login", Project: "ENG",
	})
	if err != nil {
		t.Fatalf("expected no error on pause, got %v\n%s", err, out.String())
	}
	if wf.State != memory.WFAwaitingApproval {
		t.Fatalf("state = %q, want %q", wf.State, memory.WFAwaitingApproval)
	}
	if wf.Stage != "confirm_repo" {
		t.Fatalf("stage = %q, want confirm_repo", wf.Stage)
	}
	if wf.PendingQuestionID == "" {
		t.Fatal("PendingQuestionID not set")
	}
	if len(host.Opened) != 0 {
		t.Fatalf("PR opened before approval: %v", host.Opened)
	}
	if len(mem.PendingQuestions()) != 1 {
		t.Fatalf("expected 1 pending question, got %d", len(mem.PendingQuestions()))
	}
}

func TestEngine_ResumesAfterApproval(t *testing.T) {
	replies := []string{
		// Run 1: triage
		`{"steps":[{"title":"x"}],"repo":"o/r"}`,
		// Run 3: execute step + verify pass + update_memory agent task
		`{"tool":"finish","args":{"message":"step done"}}`,
		`{"tool":"finish","args":{"message":"verified"}}`,
		`{"tool":"finish","args":{"message":"noted"}}`,
	}
	e, out, host, _, mem := gatedEngine(t, replies)
	ticket := boards.Ticket{
		ID: "ENG-1", Source: "jira", Key: "ENG-1",
		Title: "Add login", Project: "ENG",
	}

	// Run 1: triage runs, pauses at confirm_repo.
	wf, err := e.Run(context.Background(), ticket)
	if err != nil {
		t.Fatalf("run1: %v\n%s", err, out.String())
	}
	if wf.Stage != "confirm_repo" {
		t.Fatalf("run1 stage = %q want confirm_repo (state=%s err=%s)", wf.Stage, wf.State, wf.Error)
	}

	// Answer the confirm_repo question.
	pending := mem.PendingQuestions()
	if len(pending) != 1 {
		t.Fatalf("after run1 pending = %d", len(pending))
	}
	if !mem.AnswerQuestion(pending[0].ID, "yes") {
		t.Fatal("answer 1 failed")
	}

	// Run 2: confirm_repo passes, pauses at approve_plan.
	wf, err = e.Run(context.Background(), ticket)
	if err != nil {
		t.Fatalf("run2: %v\n%s", err, out.String())
	}
	if wf.Stage != "approve_plan" {
		t.Fatalf("run2 stage = %q want approve_plan (state=%s err=%s)", wf.Stage, wf.State, wf.Error)
	}
	if got := wf.Approvals["confirm_repo"]; got != "yes" {
		t.Fatalf("confirm_repo approval = %q want yes", got)
	}

	// Answer the approve_plan question.
	pending = mem.PendingQuestions()
	if len(pending) != 1 {
		t.Fatalf("after run2 pending = %d", len(pending))
	}
	if !mem.AnswerQuestion(pending[0].ID, "yes") {
		t.Fatal("answer 2 failed")
	}

	// Run 3: should run to completion.
	wf, err = e.Run(context.Background(), ticket)
	if err != nil {
		t.Fatalf("run3: %v\n%s", err, out.String())
	}
	if wf.State != memory.WFDone {
		t.Fatalf("run3 state = %q err=%q", wf.State, wf.Error)
	}
	if len(host.Opened) != 1 {
		t.Errorf("expected 1 PR, got %d", len(host.Opened))
	}
	if got := wf.Approvals["approve_plan"]; got != "yes" {
		t.Errorf("approve_plan approval = %q want yes", got)
	}
}

func TestEngine_RejectedPlanFailsWorkflow(t *testing.T) {
	replies := []string{
		`{"steps":[{"title":"x"}],"repo":"o/r"}`,
	}
	e, out, host, _, mem := gatedEngine(t, replies)
	ticket := boards.Ticket{
		ID: "ENG-2", Source: "jira", Key: "ENG-2",
		Title: "Bad idea", Project: "ENG",
	}

	// Run 1: triage runs, pauses at confirm_repo.
	if _, err := e.Run(context.Background(), ticket); err != nil {
		t.Fatalf("run1: %v\n%s", err, out.String())
	}
	pending := mem.PendingQuestions()
	if len(pending) != 1 {
		t.Fatalf("pending after run1: %d", len(pending))
	}
	mem.AnswerQuestion(pending[0].ID, "yes") // confirm repo

	// Run 2: pauses at approve_plan; user rejects.
	if _, err := e.Run(context.Background(), ticket); err != nil {
		t.Fatalf("run2: %v\n%s", err, out.String())
	}
	pending = mem.PendingQuestions()
	if len(pending) != 1 {
		t.Fatalf("pending after run2: %d", len(pending))
	}
	mem.AnswerQuestion(pending[0].ID, "no")

	// Run 3: should fail at approve_plan.
	wf, err := e.Run(context.Background(), ticket)
	if err == nil {
		t.Fatalf("expected rejection error, got none\n%s", out.String())
	}
	if wf.State != memory.WFFailed {
		t.Fatalf("state = %q want failed", wf.State)
	}
	if !strings.Contains(wf.Error, "approve_plan") {
		t.Errorf("error = %q should mention approve_plan", wf.Error)
	}
	if len(host.Opened) != 0 {
		t.Errorf("PR should not be opened on rejection, got %d", len(host.Opened))
	}
}

func TestParseRepoChange(t *testing.T) {
	cases := []struct {
		in       string
		wantPath string
		wantOK   bool
	}{
		{"yes", "", false},
		{"no", "", false},
		{"change=/repo/eng", "/repo/eng", true},
		{"repo=~/code/x", "~/code/x", true},
		{"/abs/path", "/abs/path", true},
		{"./relative", "./relative", true},
		{"some-bare-word", "", false},
		{"path with space", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, ok := parseRepoChange(tc.in)
		if got != tc.wantPath || ok != tc.wantOK {
			t.Errorf("parseRepoChange(%q) = (%q, %v) want (%q, %v)",
				tc.in, got, ok, tc.wantPath, tc.wantOK)
		}
	}
}

func TestIsYes(t *testing.T) {
	yesCases := []string{"yes", "YES", "y", "ok", "Approve", "lgtm", "go", "ship", "auto:approved", "  Yes  "}
	for _, s := range yesCases {
		if !isYes(s) {
			t.Errorf("isYes(%q) = false, want true", s)
		}
	}
	noCases := []string{"no", "n", "reject", "maybe", "later", ""}
	for _, s := range noCases {
		if isYes(s) {
			t.Errorf("isYes(%q) = true, want false", s)
		}
	}
}

func TestIsAutoApprove(t *testing.T) {
	e := &Engine{}
	cfg := WorkflowConfig{}
	t.Setenv("GOON_AUTO_APPROVE", "")
	if e.isAutoApprove(cfg) {
		t.Error("default should be false")
	}
	e.AutoApprove = true
	if !e.isAutoApprove(cfg) {
		t.Error("Engine.AutoApprove ignored")
	}
	e.AutoApprove = false
	cfg.AutoApprove = true
	if !e.isAutoApprove(cfg) {
		t.Error("cfg.AutoApprove ignored")
	}
	cfg.AutoApprove = false
	t.Setenv("GOON_AUTO_APPROVE", "1")
	if !e.isAutoApprove(cfg) {
		t.Error("env var ignored")
	}
	t.Setenv("GOON_AUTO_APPROVE", "no")
	if e.isAutoApprove(cfg) {
		t.Error("env=no should not enable")
	}
}
