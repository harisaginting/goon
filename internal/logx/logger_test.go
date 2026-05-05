package logx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNew_WritesToFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")
	lg, err := New(Config{Path: path, Level: "debug", AlsoStderr: boolPtr(false)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer lg.Close()

	lg.Info("hello", "k", "v", "n", 42)
	lg.Debug("verbose", "x", 1)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "hello") || !strings.Contains(got, "k=v") {
		t.Errorf("missing info line:\n%s", got)
	}
	if !strings.Contains(got, "verbose") {
		t.Errorf("missing debug line (level=debug should capture it):\n%s", got)
	}
}

func TestNew_LevelFiltersDebug(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")
	lg, _ := New(Config{Path: path, Level: "info", AlsoStderr: boolPtr(false)})
	defer lg.Close()

	lg.Debug("should-not-appear")
	lg.Info("should-appear")

	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "should-not-appear") {
		t.Errorf("debug line leaked at info level")
	}
	if !strings.Contains(string(data), "should-appear") {
		t.Errorf("info line missing")
	}
}

func TestNew_JSONFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")
	lg, _ := New(Config{Path: path, Level: "info", Format: "json", AlsoStderr: boolPtr(false)})
	defer lg.Close()
	lg.Info("event", "k", "v")

	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"msg":"event"`) {
		t.Errorf("json output missing msg field:\n%s", data)
	}
	if !strings.Contains(string(data), `"k":"v"`) {
		t.Errorf("json output missing attribute:\n%s", data)
	}
}

func TestRotation_RotatesAtThreshold(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")
	// Tiny threshold so we can force a rotation deterministically.
	rf, err := newRotatingFile(path, 256, 2)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer rf.Close()

	for i := 0; i < 50; i++ {
		_, _ = rf.Write([]byte("0123456789ABCDEF\n")) // 17 bytes per write
	}

	// Expect at least one rotation: test.log + test.log.1 (and maybe .2)
	for _, name := range []string{"test.log", "test.log.1"} {
		full := filepath.Join(dir, name)
		if _, err := os.Stat(full); err != nil {
			t.Errorf("expected %s to exist after rotation: %v", name, err)
		}
	}
	// Oldest beyond keep should have been dropped.
	full := filepath.Join(dir, "test.log.3")
	if _, err := os.Stat(full); err == nil {
		t.Errorf("test.log.3 should not exist (keep=2)")
	}
}

func TestPackageDefault_LazyInit(t *testing.T) {
	t.Setenv("GOON_LOG_FILE", filepath.Join(t.TempDir(), "lazy.log"))
	t.Setenv("GOON_LOG_LEVEL", "debug")
	SetDefault(nil) // force re-init
	defer SetDefault(nil)

	Info("first", "ok", true)
	Debug("second")

	data, err := os.ReadFile(os.Getenv("GOON_LOG_FILE"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), "first") || !strings.Contains(string(data), "second") {
		t.Errorf("default logger didn't write expected entries:\n%s", data)
	}
}

func TestExpandTilde(t *testing.T) {
	home, _ := os.UserHomeDir()
	if got := expandTilde("~/x"); got != filepath.Join(home, "x") {
		t.Errorf("got %q; want %q", got, filepath.Join(home, "x"))
	}
	if got := expandTilde("/abs/path"); got != "/abs/path" {
		t.Errorf("absolute path should be untouched, got %q", got)
	}
}
