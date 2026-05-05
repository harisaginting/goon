// Package executor binds tool calls to execution policy:
//
//	ModeDryRun   describe what would happen; never run
//	ModeRun      validate, prompt y/n, then run
//	ModeAuto     validate, then run
//	ModeExplain  produce a plan only — agent loop should never reach here
//
// The validator is consulted only for run_command.
package executor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/harisaginting/goon/internal/logx"
	"github.com/harisaginting/goon/internal/safety"
	"github.com/harisaginting/goon/internal/tools"
)

// Mode controls execution behavior.
type Mode int

const (
	ModeDryRun Mode = iota
	ModeRun
	ModeAuto
	ModeExplain
)

func (m Mode) String() string {
	switch m {
	case ModeDryRun:
		return "dry-run"
	case ModeRun:
		return "run"
	case ModeAuto:
		return "auto"
	case ModeExplain:
		return "explain"
	default:
		return "unknown"
	}
}

// Options configures the executor.
type Options struct {
	Mode      Mode
	Validator safety.Validator
	Stdin     io.Reader
	Stdout    io.Writer
	Stderr    io.Writer
}

// Executor coordinates a single tool invocation.
type Executor struct {
	opts Options
}

// New constructs an Executor with sensible defaults.
func New(opts Options) *Executor {
	if opts.Stdin == nil {
		opts.Stdin = os.Stdin
	}
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}
	if opts.Validator == nil {
		opts.Validator = safety.Default()
	}
	return &Executor{opts: opts}
}

// Mode returns the configured mode.
func (e *Executor) Mode() Mode { return e.opts.Mode }

// Execute runs a single tool call subject to the executor mode.
func (e *Executor) Execute(ctx context.Context, t tools.Tool, call tools.ToolCall) (tools.Result, error) {
	if t == nil {
		return tools.Result{}, fmt.Errorf("executor: unknown tool %q", call.Tool)
	}

	// Mode === explain: never run; just narrate.
	if e.opts.Mode == ModeExplain {
		fmt.Fprintf(e.opts.Stdout, "[explain] would call %s args=%v\n", call.Tool, call.Args)
		return tools.Result{ToolName: call.Tool, Stdout: "[explain] not executed"}, nil
	}

	// Validate run_command commands.
	if call.Tool == "run_command" {
		if err := e.opts.Validator.Validate(call.Args["command"]); err != nil {
			return tools.Result{ToolName: call.Tool, Err: err}, err
		}
	}

	// finish has no side-effects; always run regardless of mode.
	if call.Tool == "finish" {
		return t.Run(ctx, call.Args)
	}

	// run_command + read_only tools: dry-run prints intent.
	if e.opts.Mode == ModeDryRun {
		fmt.Fprintf(e.opts.Stdout, "[dry-run] %s args=%v\n", call.Tool, call.Args)
		return tools.Result{ToolName: call.Tool, Stdout: "[dry-run] not executed"}, nil
	}

	// run mode: prompt before mutating tools.
	if e.opts.Mode == ModeRun && isMutating(call.Tool) {
		yes, err := confirm(e.opts.Stdin, e.opts.Stdout, fmt.Sprintf(
			"Execute %s args=%v ? (y/N) ", call.Tool, call.Args))
		if err != nil {
			return tools.Result{ToolName: call.Tool, Err: err}, err
		}
		if !yes {
			return tools.Result{ToolName: call.Tool, Stdout: "[skipped by user]"}, errors.New("user declined")
		}
	}

	start := time.Now()
	res, runErr := t.Run(ctx, call.Args)
	logx.Info("executor.tool",
		"tool", call.Tool,
		"args", call.Args,
		"latency_ms", time.Since(start).Milliseconds(),
		"ok", runErr == nil,
		"stdout_bytes", len(res.Stdout),
		"stderr_bytes", len(res.Stderr),
		"err", errString(runErr),
	)
	logx.Debug("executor.tool_output",
		"tool", call.Tool,
		"stdout", logTruncate(res.Stdout, 4096),
		"stderr", logTruncate(res.Stderr, 4096),
	)
	return res, runErr
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// logTruncate caps a string for log attributes — keeps log lines bounded
// even when a tool spits out a 1MB stdout.
func logTruncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// isMutating returns true for tools the user typically wants to confirm.
func isMutating(name string) bool {
	switch name {
	case "run_command", "telegram":
		return true
	}
	return false
}

func confirm(stdin io.Reader, stdout io.Writer, prompt string) (bool, error) {
	fmt.Fprint(stdout, prompt)
	br := bufio.NewReader(stdin)
	line, err := br.ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes", nil
}
