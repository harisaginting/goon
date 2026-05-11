package daemon

import (
	"bytes"
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/harisaginting/goon/internal/boards"
	"github.com/harisaginting/goon/internal/executor"
	"github.com/harisaginting/goon/internal/githost"
	"github.com/harisaginting/goon/internal/llm"
	"github.com/harisaginting/goon/internal/memory"
	"github.com/harisaginting/goon/internal/safety"
	"github.com/harisaginting/goon/internal/tools"
)

func TestDaemon_PollPicksAndRunsWorkflow(t *testing.T) {
	// Skip the user-approval gates so this end-to-end test runs to completion
	// in a single tick. Production daemons leave gates on by default.
	t.Setenv("GOON_AUTO_APPROVE", "1")
	mem := memory.Disabled()
	board := boards.NewMock([]boards.Ticket{
		{
			ID: "ENG-1", Source: "jira", Key: "ENG-1",
			Title: "Add login", Status: boards.StatusOpen, Project: "ENG",
			UpdatedAt: time.Now(),
		},
	})
	host := githost.NewMock()
	mock := llm.NewMock([]string{
		`{"steps":[{"title":"add login"}]}`,
		`{"tool":"finish","args":{"message":"done"}}`,
		`{"tool":"finish","args":{"message":"verified"}}`,
		`{"tool":"finish","args":{"message":"noted"}}`, // update_memory phase
	})
	var out bytes.Buffer
	exec := executor.New(executor.Options{
		Mode:      executor.ModeAuto,
		Validator: safety.Default(),
		Stdin:     strings.NewReader(""),
		Stdout:    &out, Stderr: &out,
	})
	d := New(Options{
		LLM: mock, Tools: tools.DefaultRegistry(), Executor: exec,
		Memory: mem, Board: board, Host: host,
		Stdout: &out, Stderr: &out,
		PollInterval:       50 * time.Millisecond,
		VerifyRunsOverride: 1,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	_ = d.Run(ctx)

	wfs := mem.ListWorkflows(10)
	if len(wfs) == 0 {
		t.Fatalf("expected at least 1 workflow:\n%s", out.String())
	}
	if wfs[0].State != memory.WFDone {
		t.Fatalf("workflow not done: %v err=%q", wfs[0].State, wfs[0].Error)
	}
	if len(host.Opened) != 1 {
		t.Fatalf("expected 1 PR, got %d", len(host.Opened))
	}
	if !strings.Contains(out.String(), "ENG-1") {
		t.Errorf("output should mention ticket key:\n%s", out.String())
	}
}

func TestDaemon_SkipsBlockedByQuestion(t *testing.T) {
	mem := memory.Disabled()
	mem.AskQuestion(memory.Question{TicketID: "ENG-1", Question: "Which DB?"})
	board := boards.NewMock([]boards.Ticket{
		{ID: "ENG-1", Source: "jira", Key: "ENG-1", Status: boards.StatusOpen, UpdatedAt: time.Now()},
	})
	mock := llm.NewMock(nil)
	var out bytes.Buffer
	d := New(Options{
		LLM: mock, Tools: tools.DefaultRegistry(),
		Executor: executor.New(executor.Options{Mode: executor.ModeAuto, Validator: safety.Default(), Stdout: &out, Stderr: &out, Stdin: strings.NewReader("")}),
		Memory:   mem, Board: board, Host: githost.NewMock(),
		Stdout: &out, Stderr: &out, PollInterval: 50 * time.Millisecond,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = d.Run(ctx)
	if !strings.Contains(out.String(), "blocked on user question") {
		t.Fatalf("expected block message:\n%s", out.String())
	}
	if len(mem.ListWorkflows(10)) != 0 {
		t.Fatal("workflow should not have started while blocked")
	}
}

func TestDaemon_SkipsTicketsWithOpenWorkflow(t *testing.T) {
	mem := memory.Disabled()
	mem.UpsertWorkflow(memory.Workflow{ID: "wf-existing", TicketID: "ENG-1", State: memory.WFExecuting})
	board := boards.NewMock([]boards.Ticket{
		{ID: "ENG-1", Status: boards.StatusOpen, UpdatedAt: time.Now()},
	})
	mock := llm.NewMock(nil)
	var out bytes.Buffer
	d := New(Options{
		LLM: mock, Tools: tools.DefaultRegistry(),
		Executor: executor.New(executor.Options{Mode: executor.ModeAuto, Validator: safety.Default(), Stdout: &out, Stderr: &out, Stdin: strings.NewReader("")}),
		Memory:   mem, Board: board, Host: githost.NewMock(),
		Stdout: &out, Stderr: &out, PollInterval: 50 * time.Millisecond,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = d.Run(ctx)
	if strings.Count(out.String(), "wf-") > 1 {
		t.Errorf("did not expect new workflow started:\n%s", out.String())
	}
}

func TestDaemon_StatusLifecycle(t *testing.T) {
	mem := memory.Disabled()
	board := boards.NewMock(nil)
	mock := llm.NewMock(nil)
	var out bytes.Buffer
	d := New(Options{
		LLM: mock, Tools: tools.DefaultRegistry(),
		Executor: executor.New(executor.Options{Mode: executor.ModeAuto, Validator: safety.Default(), Stdout: &out, Stderr: &out, Stdin: strings.NewReader("")}),
		Memory:   mem, Board: board, Host: githost.NewMock(),
		Stdout: &out, Stderr: &out, PollInterval: 50 * time.Millisecond,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	go func() { _ = d.Run(ctx) }()
	time.Sleep(40 * time.Millisecond)
	if !mem.GetStatus().Running {
		t.Fatal("expected running=true mid-run")
	}
	cancel()
	time.Sleep(50 * time.Millisecond)
	if mem.GetStatus().Running {
		t.Fatal("expected running=false after cancel")
	}
}

func TestDaemon_ResumesPausedWorkflow(t *testing.T) {
	// Pre-seed memory with a paused workflow whose pending question has been
	// answered. The daemon should pick it up via ResumableWorkflow() before
	// looking at fresh tickets, fetch the ticket via board.Get, and call
	// engine.Run to drive it forward. With GOON_AUTO_APPROVE=1 the resumed
	// workflow runs to completion in this single tick.
	t.Setenv("GOON_AUTO_APPROVE", "1")
	mem := memory.Disabled()
	mem.UpsertWorkflow(memory.Workflow{
		ID:                "wf-paused",
		TicketID:          "ENG-1",
		TicketKey:         "ENG-1",
		Title:             "Add login",
		State:             memory.WFAwaitingApproval,
		Stage:             "confirm_repo",
		PendingQuestionID: "q-1",
		Plan:              []memory.PlanStep{{Index: 0, Title: "x"}},
		Repo:              "/tmp/nope",
	})
	mem.AskQuestion(memory.Question{ID: "q-1", TicketID: "ENG-1", Question: "Confirm repo"})
	mem.AnswerQuestion("q-1", "yes")

	board := boards.NewMock([]boards.Ticket{
		{
			ID: "ENG-1", Source: "jira", Key: "ENG-1",
			Title: "Add login", Status: boards.StatusInProgress, Project: "ENG",
			UpdatedAt: time.Now(),
		},
	})
	host := githost.NewMock()
	// With auto-approve, gates pass instantly. Resume runs from confirm_repo,
	// breezes through approve_plan, then needs LLM for execute / verify /
	// update_memory. Triage is skipped because Plan is already populated.
	mock := llm.NewMock([]string{
		`{"tool":"finish","args":{"message":"done"}}`,
		`{"tool":"finish","args":{"message":"verified"}}`,
		`{"tool":"finish","args":{"message":"noted"}}`,
	})
	var out bytes.Buffer
	exec := executor.New(executor.Options{
		Mode: executor.ModeAuto, Validator: safety.Default(),
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &out,
	})
	d := New(Options{
		LLM: mock, Tools: tools.DefaultRegistry(), Executor: exec,
		Memory: mem, Board: board, Host: host,
		Stdout: &out, Stderr: &out,
		PollInterval:       50 * time.Millisecond,
		VerifyRunsOverride: 1,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	_ = d.Run(ctx)

	wf, ok := mem.GetWorkflow("wf-paused")
	if !ok {
		t.Fatalf("workflow disappeared:\n%s", out.String())
	}
	if wf.State != memory.WFDone {
		t.Fatalf("resumed workflow not done: state=%q err=%q\n%s", wf.State, wf.Error, out.String())
	}
	if !strings.Contains(out.String(), "resuming") {
		t.Errorf("expected 'resuming' in output:\n%s", out.String())
	}
	if len(host.Opened) != 1 {
		t.Errorf("expected 1 PR opened, got %d", len(host.Opened))
	}
}

// TestDaemon_PausedSkipsPoll covers the pause/resume semantics. When
// Memory.Status.Paused is set, pollAndRun must short-circuit BEFORE
// touching the board — the test seeds an open ticket, pauses, ticks
// the daemon, and asserts no workflow record was created.
func TestDaemon_PausedSkipsPoll(t *testing.T) {
	mem := memory.Disabled()
	mem.SetPaused(true)
	board := boards.NewMock([]boards.Ticket{
		{ID: "ENG-1", Key: "ENG-1", Title: "x", Status: boards.StatusOpen, UpdatedAt: time.Now()},
	})
	mock := llm.NewMock(nil)
	var out bytes.Buffer
	d := New(Options{
		LLM: mock, Tools: tools.DefaultRegistry(),
		Executor: executor.New(executor.Options{Mode: executor.ModeAuto, Validator: safety.Default(), Stdout: &out, Stderr: &out, Stdin: strings.NewReader("")}),
		Memory:   mem, Board: board, Host: githost.NewMock(),
		Stdout: &out, Stderr: &out, PollInterval: 30 * time.Millisecond,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_ = d.Run(ctx)
	if !strings.Contains(out.String(), "[poll] paused") {
		t.Fatalf("expected paused log line:\n%s", out.String())
	}
	if len(mem.ListWorkflows(10)) != 0 {
		t.Errorf("paused daemon should not have started any workflow")
	}
}

// Confirm the poll loop ticks at least twice with a 50ms interval.
func TestDaemon_PollsRepeatedly(t *testing.T) {
	mem := memory.Disabled()
	var calls int32
	board := &boards.Mock{
		OnList: func() ([]boards.Ticket, error) {
			atomic.AddInt32(&calls, 1)
			return nil, nil
		},
	}
	mock := llm.NewMock(nil)
	var out bytes.Buffer
	d := New(Options{
		LLM: mock, Tools: tools.DefaultRegistry(),
		Executor: executor.New(executor.Options{Mode: executor.ModeAuto, Validator: safety.Default(), Stdout: &out, Stderr: &out, Stdin: strings.NewReader("")}),
		Memory:   mem, Board: board, Host: githost.NewMock(),
		Stdout: &out, Stderr: &out, PollInterval: 30 * time.Millisecond,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = d.Run(ctx)
	if atomic.LoadInt32(&calls) < 2 {
		t.Fatalf("expected ≥2 polls, got %d", calls)
	}
}
