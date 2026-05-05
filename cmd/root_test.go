package cmd

import (
	"bytes"
	"strings"
	"testing"
)

// TestRun_StripsRedundantGoonPrefix verifies that `go run . goon workflow init`
// (which becomes argv = ["goon", "workflow", "init"]) is recognized as the
// workflow subcommand instead of falling through to the agent loop.
//
// We assert two things: (a) the subcommand actually runs (proven by the fact
// that the workflow `path` subaction prints something to stdout), and (b) the
// hint is printed to stderr so the user learns the right form.
func TestRun_StripsRedundantGoonPrefix(t *testing.T) {
	var stdout, stderr bytes.Buffer
	// Use `workflow path` because it requires no setup, no LLM, no network —
	// it just prints a path and returns. Perfect for testing dispatch.
	err := run([]string{"goon", "workflow", "path"}, &stdout, &stderr, strings.NewReader(""))
	if err != nil {
		t.Fatalf("run: %v\nstderr=%s", err, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "workflow.json") {
		t.Errorf("workflow path didn't run — stdout=%q stderr=%q", out, stderr.String())
	}
	if !strings.Contains(stderr.String(), "hint:") || !strings.Contains(stderr.String(), "leading 'goon'") {
		t.Errorf("missing onboarding hint in stderr: %q", stderr.String())
	}
}

// TestRun_NormalSubcommand_NoHint sanity-checks that the hint does NOT fire
// for the typical case where the user types just the subcommand.
func TestRun_NormalSubcommand_NoHint(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{"workflow", "path"}, &stdout, &stderr, strings.NewReader(""))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(stderr.String(), "hint:") {
		t.Errorf("hint should NOT fire for normal usage; stderr=%q", stderr.String())
	}
}

// TestRun_BareGoonGoesToAgent verifies that `goon` alone (just the program
// name with no other args) is treated as a missing-task error, not as a
// matched subcommand. This is the boundary case for the strip-prefix logic:
// argv = ["goon"] should NOT be stripped (len > 1 guard).
func TestRun_BareGoonNoSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{"goon"}, &stdout, &stderr, strings.NewReader(""))
	if err == nil {
		t.Error("expected error for bare `goon` with no subcommand or task")
	}
	// The hint should NOT fire here — there's no second arg to strip toward.
	if strings.Contains(stderr.String(), "hint:") {
		t.Errorf("hint fired on bare goon; stderr=%q", stderr.String())
	}
}
