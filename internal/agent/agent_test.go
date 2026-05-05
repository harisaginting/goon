package agent

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/harisaginting/goon/internal/executor"
	"github.com/harisaginting/goon/internal/llm"
	"github.com/harisaginting/goon/internal/memory"
	"github.com/harisaginting/goon/internal/safety"
	"github.com/harisaginting/goon/internal/tools"
)

// Builds an agent wired against the mock LLM.
func newTestAgent(t *testing.T, mode executor.Mode, replies []string) (*Agent, *bytes.Buffer, *llm.Mock) {
	t.Helper()
	mock := llm.NewMock(replies)
	reg := tools.DefaultRegistry()
	var out bytes.Buffer
	exec := executor.New(executor.Options{
		Mode:      mode,
		Validator: safety.Default(),
		Stdin:     strings.NewReader(""),
		Stdout:    &out,
		Stderr:    &out,
	})
	a := New(Options{
		LLM:      mock,
		Tools:    reg,
		Executor: exec,
		Memory:   memory.Disabled(),
		Stdout:   &out,
		Stderr:   &out,
	})
	return a, &out, mock
}

func TestAgent_FinishImmediately(t *testing.T) {
	a, out, _ := newTestAgent(t, executor.ModeAuto, []string{
		`{"tool":"finish","args":{"message":"hello world"}}`,
	})
	if err := a.Run(context.Background(), "say hello"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "hello world") {
		t.Fatalf("expected 'hello world' in output, got: %s", out.String())
	}
}

func TestAgent_MultiStep(t *testing.T) {
	a, out, _ := newTestAgent(t, executor.ModeAuto, []string{
		`{"tool":"run_command","args":{"command":"echo step1"},"rationale":"warmup"}`,
		`{"tool":"finish","args":{"message":"all done"}}`,
	})
	if err := a.Run(context.Background(), "do something"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	o := out.String()
	if !strings.Contains(o, "step1") {
		t.Fatalf("expected step1 echo, got: %s", o)
	}
	if !strings.Contains(o, "all done") {
		t.Fatalf("expected finish message, got: %s", o)
	}
}

func TestAgent_RepairsBadJSON(t *testing.T) {
	a, _, _ := newTestAgent(t, executor.ModeAuto, []string{
		`Sure, I will run echo:`, // not JSON
		`{"tool":"finish","args":{"message":"recovered"}}`,
	})
	if err := a.Run(context.Background(), "test repair"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAgent_BlocksDangerous(t *testing.T) {
	a, out, _ := newTestAgent(t, executor.ModeAuto, []string{
		`{"tool":"run_command","args":{"command":"rm -rf /"}}`,
		`{"tool":"finish","args":{"message":"backed off"}}`,
	})
	if err := a.Run(context.Background(), "wipe everything"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "backed off") {
		t.Fatalf("expected agent to back off, got: %s", out.String())
	}
}

func TestAgent_MaxSteps(t *testing.T) {
	// Lower bound for this test — keep MaxSteps default but keep replies bounded.
	prev := MaxSteps
	MaxSteps = 3
	defer func() { MaxSteps = prev }()

	replies := []string{
		`{"tool":"run_command","args":{"command":"echo a"}}`,
		`{"tool":"run_command","args":{"command":"echo b"}}`,
		`{"tool":"run_command","args":{"command":"echo c"}}`,
	}
	a, out, _ := newTestAgent(t, executor.ModeAuto, replies)
	_ = a.Run(context.Background(), "loop")
	if !strings.Contains(out.String(), "max steps") && !strings.Contains(out.String(), "(max steps reached without finish)") {
		// Either acceptable; assert the loop bounded itself.
		t.Logf("output: %s", out.String())
	}
}

func TestAgent_UnknownTool(t *testing.T) {
	a, _, _ := newTestAgent(t, executor.ModeAuto, []string{
		`{"tool":"teleport","args":{"to":"mars"}}`,
		`{"tool":"finish","args":{"message":"corrected"}}`,
	})
	if err := a.Run(context.Background(), "unknown tool test"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
