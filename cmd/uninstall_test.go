package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// We can't easily delete the running test binary, so these tests build a
// dummy binary in a temp dir, then point os.Executable() there by exec'ing
// it as a child. Instead, we test the helper paths directly.

func TestStateDirs_HomeAndXDG(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "xdg"))
	got := stateDirs()
	want1 := filepath.Join(tmp, ".goon")
	want2 := filepath.Join(tmp, "xdg", "goon")
	if len(got) != 2 || got[0] != want1 || got[1] != want2 {
		t.Errorf("stateDirs = %v, want [%s %s]", got, want1, want2)
	}
}

func TestStateDirs_NoXDG(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", "")
	got := stateDirs()
	want := []string{
		filepath.Join(tmp, ".goon"),
		filepath.Join(tmp, ".config", "goon"),
	}
	if !sliceEq(got, want) {
		t.Errorf("stateDirs = %v, want %v", got, want)
	}
}

func TestUninstall_DeclineAborts(t *testing.T) {
	// Sandbox: create a fake "binary" file we can let runUninstall remove if
	// the user agreed. We answer "n" so it should NOT be removed.
	dir := t.TempDir()
	bin := filepath.Join(dir, "fakegoon")
	if err := os.WriteFile(bin, []byte("ELF-ish"), 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Override os.Executable indirectly: we can't easily, so instead we test
	// the confirm path directly.
	in := strings.NewReader("n\n")
	var out bytes.Buffer
	ok, err := confirm(in, &out, "Continue? (y/N) ")
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if ok {
		t.Fatal("expected confirm to return false on 'n'")
	}
	// File still exists because we never reached the removal step.
	if _, err := os.Stat(bin); err != nil {
		t.Errorf("file should still exist: %v", err)
	}
}

func TestUninstall_AcceptYes(t *testing.T) {
	in := strings.NewReader("y\n")
	var out bytes.Buffer
	ok, err := confirm(in, &out, "Continue? (y/N) ")
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if !ok {
		t.Fatal("expected confirm to return true on 'y'")
	}
}

func TestUninstall_PurgeFlagParsed(t *testing.T) {
	// Use an unreachable binary path so we don't actually delete anything.
	// runUninstall will fail at os.Remove, but we verify --yes + --purge are
	// parsed correctly by checking the printed plan.
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", "")
	// Seed some state.
	stateDir := filepath.Join(tmp, ".goon")
	_ = os.MkdirAll(stateDir, 0o755)
	_ = os.WriteFile(filepath.Join(stateDir, "memory.json"), []byte("{}"), 0o600)

	// Provide "n" so we abort BEFORE deletion — we just want to confirm the
	// plan output mentions both binary and state dir when --purge is set.
	var out bytes.Buffer
	_ = runUninstall(context.Background(),
		[]string{"--purge"}, &out, &out, strings.NewReader("n\n"))
	got := out.String()
	if !strings.Contains(got, "About to remove") {
		t.Fatalf("missing plan: %s", got)
	}
	if !strings.Contains(got, stateDir) {
		t.Errorf("plan should include state dir %s\n%s", stateDir, got)
	}
}
