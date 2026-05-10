package safety

import (
	"context"
	"os/exec"
	"runtime"
)

// ShellCommand returns an *exec.Cmd that runs cmd through the host's default
// shell. On POSIX systems that's "sh -c"; on Windows it's "cmd /C". Callers
// should still validate cmd via a Validator before invoking — this helper
// only handles the cross-platform invocation, NOT safety policy.
func ShellCommand(ctx context.Context, cmd string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.CommandContext(ctx, "cmd", "/C", cmd)
	}
	return exec.CommandContext(ctx, "sh", "-c", cmd)
}
