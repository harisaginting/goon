package daemon

import (
	"bytes"
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"goon/internal/boards"
	"goon/internal/executor"
	"goon/internal/githost"
	"goon/internal/llm"
	"goon/internal/memory"
	"goon/internal/safety"
	"goon/internal/tools"
)

func TestDaemon_PollPicksAndRunsWorkflow(t *testing.T) {
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
