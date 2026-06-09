package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"

	"github.com/harisaginting/goon/internal/safety"
)

// RunCommand executes a shell command via the host shell ("sh -c" on POSIX,
// "cmd /C" on Windows). Safety validation is applied by the executor, NOT
// here — this tool is intentionally dumb.
type RunCommand struct{}

func (*RunCommand) Name() string        { return "run_command" }
func (*RunCommand) Description() string {
	return "execute a shell command via the host shell (sh -c on POSIX, cmd /C on Windows)"
}
func (*RunCommand) Schema() map[string]string {
	return map[string]string{"command": "shell command string"}
}

func (*RunCommand) Run(ctx context.Context, args map[string]string) (Result, error) {
	cmd := args["command"]
	if cmd == "" {
		return Result{ToolName: "run_command"}, errors.New(`run_command: "command" is required`)
	}
	c := safety.ShellCommand(ctx, cmd)
	// Run in the selected repo's checkout when the workflow set one, so
	// the agent operates on the RIGHT codebase (not goon's launch dir).
	if d := WorkDirFrom(ctx); d != "" {
		c.Dir = d
	}
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	err := c.Run()
	res := Result{
		ToolName: "run_command",
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: 0,
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
			res.Err = fmt.Errorf("exit %d", ee.ExitCode())
			return res, res.Err
		}
		res.ExitCode = -1
		res.Err = err
		return res, err
	}
	return res, nil
}
