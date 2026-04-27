package workflow

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"goon/internal/boards"
	"goon/internal/executor"
	"goon/internal/githost"
	"goon/internal/llm"
	"goon/internal/memory"
	"goon/internal/safety"
	"goon/internal/tools"
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
