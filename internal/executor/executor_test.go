package executor

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"goon/internal/safety"
	"goon/internal/tools"
)

func TestDryRun_DoesNotExecute(t *testing.T) {
	var out bytes.Buffer
	e := New(Options{Mode: ModeDryRun, Stdout: &out, Stderr: &out, Stdin: strings.NewReader("")})
	res, err := e.Execute(context.Background(), &tools.RunCommand{}, tools.ToolCall{
		Tool: "run_command",
		Args: map[string]string{"command": "echo SHOULD_NOT_RUN"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res.Stdout, "[dry-run]") {
		t.Fatalf("expected dry-run notice, got %q", res.Stdout)
	}
	if strings.Contains(out.String(), "SHOULD_NOT_RUN") && !strings.Contains(out.String(), "[dry-run]") {
		t.Fatalf("dry-run leaked execution: %s", out.String())
	}
}

func TestRun_WithConfirmation(t *testing.T) {
	var out bytes.Buffer
	e := New(Options{
		Mode:      ModeRun,
		Stdout:    &out,
		Stderr:    &out,
		Stdin:     strings.NewReader("y\n"),
		Validator: safety.Default(),
	})
	res, err := e.Execute(context.Background(), &tools.RunCommand{}, tools.ToolCall{
		Tool: "run_command",
		Args: map[string]string{"command": "echo hi"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res.Stdout, "hi") {
		t.Fatalf("expected echo hi, got %q", res.Stdout)
	}
}

func TestRun_DeniedByUser(t *testing.T) {
	var out bytes.Buffer
	e := New(Options{
		Mode:      ModeRun,
		Stdout:    &out,
		Stderr:    &out,
		Stdin:     strings.NewReader("n\n"),
		Validator: safety.Default(),
	})
	_, err := e.Execute(context.Background(), &tools.RunCommand{}, tools.ToolCall{
		Tool: "run_command",
		Args: map[string]string{"command": "echo hi"},
	})
	if err == nil {
		t.Fatal("expected user-decline error")
	}
}

func TestAuto_NoPrompt(t *testing.T) {
	var out bytes.Buffer
	e := New(Options{
		Mode:      ModeAuto,
		Stdout:    &out,
		Stderr:    &out,
		Stdin:     strings.NewReader(""),
		Validator: safety.Default(),
	})
	res, err := e.Execute(context.Background(), &tools.RunCommand{}, tools.ToolCall{
		Tool: "run_command",
		Args: map[string]string{"command": "echo auto"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res.Stdout, "auto") {
		t.Fatalf("expected echo auto, got %q", res.Stdout)
	}
}

func TestSafety_BlocksRMRoot(t *testing.T) {
	e := New(Options{
		Mode:      ModeAuto,
		Stdin:     strings.NewReader(""),
		Validator: safety.Default(),
	})
	_, err := e.Execute(context.Background(), &tools.RunCommand{}, tools.ToolCall{
		Tool: "run_command",
		Args: map[string]string{"command": "rm -rf /"},
	})
	if err == nil {
		t.Fatal("expected safety error")
	}
}

func TestExplain_PlansOnly(t *testing.T) {
	var out bytes.Buffer
	e := New(Options{Mode: ModeExplain, Stdout: &out, Stderr: &out, Stdin: strings.NewReader("")})
	res, err := e.Execute(context.Background(), &tools.RunCommand{}, tools.ToolCall{
		Tool: "run_command",
		Args: map[string]string{"command": "echo nope"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res.Stdout, "[explain]") {
		t.Fatalf("expected explain marker, got %q", res.Stdout)
	}
}
