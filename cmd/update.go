package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Upstream defaults — overridable via env so forks can self-update.
const (
	defaultUpstreamRepo = "https://github.com/harisaginting/goon"
	defaultUpstreamRef  = "master"
	binaryName          = "goon"
)

// runUpdate fetches the requested ref from upstream, builds a fresh binary,
// and atomically replaces the currently-running goon binary in place.
//
// args[0] is an optional ref:
//
//   - empty               → master branch
//   - 7..40 hex characters → commit hash (will fetch full history)
//   - any other string    → branch / tag (passed through to git checkout)
//
// Requires `git` and `go` on PATH.
func runUpdate(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	repo := envOr("GOON_UPSTREAM", defaultUpstreamRepo)
	ref := defaultUpstreamRef
	if len(args) > 0 && strings.TrimSpace(args[0]) != "" {
		ref = strings.TrimSpace(args[0])
	}

	// Locate dependencies up front so we fail fast with a clear message.
	git, err := exec.LookPath("git")
	if err != nil {
		return errors.New("update: git is required but not on PATH (install git, then retry)")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		return errors.New("update: go is required but not on PATH (install Go, then retry)")
	}

	// Locate the running binary so we can replace it.
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("update: cannot find current binary: %w", err)
	}
	if resolved, lerr := filepath.EvalSymlinks(self); lerr == nil {
		self = resolved
	}

	work, err := os.MkdirTemp("", "goon-update-")
	if err != nil {
		return fmt.Errorf("update: tmpdir: %w", err)
	}
	defer os.RemoveAll(work)
	repoDir := filepath.Join(work, "repo")

	fmt.Fprintf(stdout, "→ updating goon from %s @ %s\n", repo, ref)

	// Shallow clone is fast; we'll deepen if the ref turns out to be a hash.
	if err := runCmd(ctx, stderr, "", git, "clone", "--depth=20", repo, repoDir); err != nil {
		return fmt.Errorf("git clone: %w", err)
	}

	if isCommitHash(ref) {
		// Ensure the requested commit exists locally — best-effort unshallow.
		_ = runCmd(ctx, stderr, repoDir, git, "fetch", "--unshallow")
	}
	if err := runCmd(ctx, stderr, repoDir, git, "checkout", ref); err != nil {
		return fmt.Errorf("git checkout %s: %w", ref, err)
	}

	// Resolve the actual commit we landed on, for nicer reporting.
	short := resolveShortSHA(ctx, git, repoDir)

	fmt.Fprintf(stdout, "→ building (commit %s)\n", short)
	out := filepath.Join(work, binaryName)
	if err := runCmd(ctx, stderr, repoDir, goBin, "build",
		"-trimpath", "-ldflags=-s -w", "-o", out, "."); err != nil {
		return fmt.Errorf("go build: %w", err)
	}

	fmt.Fprintf(stdout, "→ replacing %s\n", self)
	if err := atomicReplace(out, self); err != nil {
		return fmt.Errorf("replace binary: %w", err)
	}

	// Show the new version.
	ver := exec.CommandContext(ctx, self, "--version")
	ver.Stdout = stdout
	ver.Stderr = stderr
	_ = ver.Run()

	fmt.Fprintf(stdout, "✓ updated to %s @ %s\n", ref, short)
	return nil
}

func runCmd(ctx context.Context, stderr io.Writer, dir, name string, args ...string) error {
	c := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		c.Dir = dir
	}
	c.Stdout = stderr // git/go are noisy on stdout; keep our own stdout clean
	c.Stderr = stderr
	return c.Run()
}

func resolveShortSHA(ctx context.Context, git, dir string) string {
	c := exec.CommandContext(ctx, git, "-C", dir, "rev-parse", "--short=10", "HEAD")
	out, err := c.Output()
	if err != nil {
		return "?"
	}
	return strings.TrimSpace(string(out))
}

// isCommitHash returns true iff s is 7–40 hex characters.
func isCommitHash(s string) bool {
	if len(s) < 7 || len(s) > 40 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// atomicReplace moves src to dst, falling back to copy when src and dst are on
// different filesystems (which is common: src in /tmp, dst in ~/.local/bin).
//
// On Linux/macOS replacing a running binary is allowed because the OS holds an
// open file descriptor to the original inode while the new file takes its
// path. The next invocation picks up the new binary.
func atomicReplace(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	tmp := dst + ".new"
	if err := copyFile(src, tmp); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// envOr is a tiny local helper duplicated here so cmd has zero internal deps
// outside of the agent runtime imports.
func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
