package workflow

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"text/template"

	"github.com/harisaginting/goon/internal/boards"
	"github.com/harisaginting/goon/internal/memory"
	"github.com/harisaginting/goon/internal/safety"
)

// HookCtx is the data passed to template substitution + exported as env vars
// when running hook commands.
type HookCtx struct {
	Key         string
	Title       string
	Description string
	URL         string
	Source      string
	Project     string
	Branch      string
	Repo        string
	Plan        []memory.PlanStep
}

// FromTicket builds a HookCtx from a Ticket + extra runtime fields.
func FromTicket(t boards.Ticket, repo, branch string, plan []memory.PlanStep) HookCtx {
	return HookCtx{
		Key: t.Key, Title: t.Title, Description: t.Description, URL: t.URL,
		Source: t.Source, Project: t.Project,
		Branch: branch, Repo: repo, Plan: plan,
	}
}

// envSlice converts the HookCtx into KEY=VAL strings prepended to a hook's
// environment, so shell commands can read $TICKET_KEY etc.
func (c HookCtx) envSlice() []string {
	return []string{
		"TICKET_KEY=" + c.Key,
		"TICKET_TITLE=" + c.Title,
		"TICKET_DESCRIPTION=" + c.Description,
		"TICKET_URL=" + c.URL,
		"TICKET_SOURCE=" + c.Source,
		"TICKET_PROJECT=" + c.Project,
		"BRANCH=" + c.Branch,
		"REPO=" + c.Repo,
	}
}

// HookRunner runs the per-phase shell commands. cwd defaults to the repo
// directory (when set) so commands like "go fmt ./..." land in the right
// place. stdout/stderr are forwarded to the engine's writers.
type HookRunner struct {
	Stdout io.Writer
	Stderr io.Writer
	// Validator is the safety check (mirrors run_command). May be nil to skip.
	Validator interface{ Validate(cmd string) error }
}

// Run executes every command in cmds with hctx exported as env. Stops at
// the first error. phase is logged as a tag.
func (r *HookRunner) Run(ctx context.Context, phase string, cmds []string, hctx HookCtx) error {
	if len(cmds) == 0 {
		return nil
	}
	cwd := strings.TrimSpace(hctx.Repo)
	for i, raw := range cmds {
		cmd, err := substitute(raw, hctx)
		if err != nil {
			return fmt.Errorf("hook %s[%d]: bad template: %w", phase, i, err)
		}
		cmd = strings.TrimSpace(cmd)
		if cmd == "" {
			continue
		}
		if r.Validator != nil {
			if err := r.Validator.Validate(cmd); err != nil {
				return fmt.Errorf("hook %s[%d] blocked by safety: %w", phase, i, err)
			}
		}
		fmt.Fprintf(r.Stdout, "→ hook %s[%d/%d] $ %s\n", phase, i+1, len(cmds), cmd)
		if err := r.runOne(ctx, cmd, cwd, hctx); err != nil {
			return fmt.Errorf("hook %s[%d] failed: %w", phase, i, err)
		}
	}
	return nil
}

func (r *HookRunner) runOne(ctx context.Context, cmd, cwd string, hctx HookCtx) error {
	c := safety.ShellCommand(ctx, cmd)
	if cwd != "" {
		if _, err := os.Stat(cwd); err == nil {
			c.Dir = cwd
		}
	}
	c.Env = append(os.Environ(), hctx.envSlice()...)
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	err := c.Run()
	if stdout.Len() > 0 {
		fmt.Fprint(r.Stdout, indent(stdout.String(), "    "))
	}
	if stderr.Len() > 0 {
		fmt.Fprint(r.Stderr, indent(stderr.String(), "    "))
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return fmt.Errorf("exit %d", ee.ExitCode())
		}
		return err
	}
	return nil
}

// substitute renders raw as a Go text/template with hctx as the data.
// Templates that can't compile fall back to a literal — we don't want
// shell strings like "$(date)" failing template parsing to block hooks.
func substitute(raw string, hctx HookCtx) (string, error) {
	if !strings.Contains(raw, "{{") {
		return raw, nil
	}
	tpl, err := template.New("hook").Parse(raw)
	if err != nil {
		return raw, nil // treat as literal
	}
	var b bytes.Buffer
	if err := tpl.Execute(&b, hctx); err != nil {
		return "", err
	}
	return b.String(), nil
}

// RenderTemplate renders an arbitrary template string with hctx (used for
// PR title and body). Empty templates return "".
func RenderTemplate(name, raw string, hctx HookCtx) (string, error) {
	if raw == "" {
		return "", nil
	}
	tpl, err := template.New(name).Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse %s template: %w", name, err)
	}
	var b bytes.Buffer
	if err := tpl.Execute(&b, hctx); err != nil {
		return "", fmt.Errorf("render %s template: %w", name, err)
	}
	return b.String(), nil
}

// indent prefixes every line of s with prefix. Used for clean hook output.
func indent(s, prefix string) string {
	if s == "" {
		return s
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n") + "\n"
}
