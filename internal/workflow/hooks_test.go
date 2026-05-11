package workflow

import (
	"bytes"
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/harisaginting/goon/internal/boards"
	"github.com/harisaginting/goon/internal/memory"
	"github.com/harisaginting/goon/internal/safety"
)

// hooksRequirePOSIX skips a test on Windows. Hook commands like `false`,
// `touch`, `echo "$VAR" > path` rely on POSIX shell behaviour;
// safety.ShellCommand picks `cmd /C` on Windows where these don't work.
// Cross-platform alternatives exist but rewriting every hook test for
// both shells is more churn than the value — the daemon's hook surface
// is documented as POSIX-only on the user side.
func hooksRequirePOSIX(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("hooks tests use POSIX shell builtins; Windows path is exercised separately")
	}
}

func newHookRunner() (*HookRunner, *bytes.Buffer) {
	var buf bytes.Buffer
	r := &HookRunner{Stdout: &buf, Stderr: &buf, Validator: safety.Default()}
	return r, &buf
}

func sampleCtx() HookCtx {
	return FromTicket(
		boards.Ticket{
			Key: "ENG-42", Title: "Add login",
			URL: "https://x/ENG-42", Source: "jira", Project: "ENG",
		},
		"/tmp/repo", "goon/eng-42",
		[]memory.PlanStep{{Title: "step", Done: true}},
	)
}

func TestHookRunner_RunsCommandsInOrder(t *testing.T) {
	hooksRequirePOSIX(t)
	dir := t.TempDir()
	r, buf := newHookRunner()
	hctx := sampleCtx()
	hctx.Repo = dir // run in tmp
	cmds := []string{
		"echo first > " + filepath.Join(dir, "out.txt"),
		"echo second >> " + filepath.Join(dir, "out.txt"),
	}
	if err := r.Run(context.Background(), "before_pr", cmds, hctx); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	// Output file should contain both lines.
	got, err := readFile(filepath.Join(dir, "out.txt"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(got, "first") || !strings.Contains(got, "second") {
		t.Errorf("output: %q", got)
	}
}

func TestHookRunner_StopsOnFirstError(t *testing.T) {
	hooksRequirePOSIX(t)
	dir := t.TempDir()
	r, _ := newHookRunner()
	hctx := sampleCtx()
	hctx.Repo = dir
	cmds := []string{
		"true",
		"false", // exits non-zero
		"touch " + filepath.Join(dir, "should_not_exist"),
	}
	if err := r.Run(context.Background(), "before_test", cmds, hctx); err == nil {
		t.Fatal("expected error from failing hook")
	}
	if _, err := readFile(filepath.Join(dir, "should_not_exist")); err == nil {
		t.Fatal("subsequent hook ran after failure — should have stopped")
	}
}

func TestHookRunner_BlocksDangerousCommand(t *testing.T) {
	r, _ := newHookRunner()
	hctx := sampleCtx()
	cmds := []string{"rm -rf /"}
	err := r.Run(context.Background(), "before_pr", cmds, hctx)
	if err == nil || !strings.Contains(err.Error(), "blocked by safety") {
		t.Fatalf("expected safety error, got %v", err)
	}
}

func TestHookRunner_ExportsTicketEnv(t *testing.T) {
	hooksRequirePOSIX(t)
	dir := t.TempDir()
	r, buf := newHookRunner()
	hctx := sampleCtx()
	hctx.Repo = dir
	out := filepath.Join(dir, "env.txt")
	cmds := []string{
		`echo "$TICKET_KEY|$TICKET_TITLE|$BRANCH|$REPO" > ` + out,
	}
	if err := r.Run(context.Background(), "before_execute", cmds, hctx); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	got, _ := readFile(out)
	want := "ENG-42|Add login|goon/eng-42|" + dir
	if !strings.Contains(got, want) {
		t.Errorf("env vars: got %q want substring %q", got, want)
	}
}

func TestSubstitute_TemplateValues(t *testing.T) {
	hctx := sampleCtx()
	got, err := substitute(`echo {{.Key}}: {{.Title}} -> {{.Branch}}`, hctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != "echo ENG-42: Add login -> goon/eng-42" {
		t.Errorf("got %q", got)
	}
}

func TestSubstitute_NoBracesIsLiteral(t *testing.T) {
	hctx := sampleCtx()
	got, err := substitute(`echo $(date +%s)`, hctx)
	if err != nil || got != `echo $(date +%s)` {
		t.Errorf("literal got %q err=%v", got, err)
	}
}

func TestRenderTemplate_PRTitleAndBody(t *testing.T) {
	hctx := sampleCtx()
	title, err := RenderTemplate("title", "[{{.Key}}] {{.Title}}", hctx)
	if err != nil {
		t.Fatal(err)
	}
	if title != "[ENG-42] Add login" {
		t.Errorf("title: %q", title)
	}
	body, err := RenderTemplate("body", `Resolves {{.Key}}.

Plan:
{{range .Plan}}- {{.Title}} {{if .Done}}DONE{{end}}
{{end}}`, hctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "Resolves ENG-42") || !strings.Contains(body, "step DONE") {
		t.Fatalf("body: %q", body)
	}
}

func TestRenderTemplate_EmptyReturnsEmpty(t *testing.T) {
	got, err := RenderTemplate("x", "", HookCtx{})
	if err != nil || got != "" {
		t.Errorf("got %q err=%v", got, err)
	}
}

func TestRenderTemplate_BadSyntax(t *testing.T) {
	_, err := RenderTemplate("x", "{{ .Bogus", HookCtx{})
	if err == nil {
		t.Fatal("expected parse error")
	}
}

func TestHookRunner_NoCmds_NoOp(t *testing.T) {
	r, _ := newHookRunner()
	if err := r.Run(context.Background(), "noop", nil, sampleCtx()); err != nil {
		t.Errorf("nil cmds should be no-op: %v", err)
	}
	if err := r.Run(context.Background(), "noop", []string{"", "  "}, sampleCtx()); err != nil {
		t.Errorf("empty strings should be skipped: %v", err)
	}
}

// Small helper so tests don't need to import os everywhere.
func readFile(p string) (string, error) {
	b, err := readBytes(p)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// readBytes is a thin wrapper around os.ReadFile via blank import.
var readBytes = osReadFile
