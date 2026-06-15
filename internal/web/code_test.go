package web

import (
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/harisaginting/goon/internal/llm"
	"github.com/harisaginting/goon/internal/memory"
)

// TestResolveCodeWorkdir covers the (intentionally permissive) workdir
// rules: any existing directory is allowed (absolute, or relative to the
// workspace root with no ".." escape); non-existent paths and files are
// refused. The whitelist is only a UI convenience, not the gate.
func TestResolveCodeWorkdir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GOON_WORKSPACE_DIR", dir)
	t.Setenv("GOON_STORAGE_DIR", t.TempDir()) // isolate REPOSITORY.md

	absDir, _ := filepath.Abs(dir)
	absDir = strings.TrimRight(absDir, string(os.PathSeparator))

	// empty → first quick-pick, which must be an existing directory.
	got, err := resolveCodeWorkdir("")
	if err != nil {
		t.Fatalf("empty: unexpected err %v", err)
	}
	if fi, e := os.Stat(got); e != nil || !fi.IsDir() {
		t.Fatalf("empty returned non-dir %q (err %v)", got, e)
	}

	// absolute existing directory → accepted as-is (full freedom, incl.
	// goon's own root and any project on the machine).
	if got, err := resolveCodeWorkdir(absDir); err != nil || got != absDir {
		t.Fatalf("abs dir: got %q err %v, want %q", got, err, absDir)
	}

	// relative path under the workspace root → accepted.
	sub := filepath.Join(dir, "pkg")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	absSub := strings.TrimRight(sub, string(os.PathSeparator))
	if got, err := resolveCodeWorkdir("pkg"); err != nil || got != absSub {
		t.Fatalf("relative subdir: got %q err %v, want %q", got, err, absSub)
	}

	// non-existent → rejected.
	if _, err := resolveCodeWorkdir(filepath.Join(dir, "nope")); err == nil {
		t.Errorf("expected rejection for non-existent directory")
	}
	// relative ".." escape beyond the workspace root → rejected.
	if _, err := resolveCodeWorkdir("../escape"); err == nil {
		t.Errorf("expected rejection for relative .. escape")
	}
	// a file (not a directory) → rejected.
	f := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveCodeWorkdir(f); err == nil {
		t.Errorf("expected rejection for a file path")
	}
}

// TestHandleCodeRun_NoLLM returns 503 when no provider is configured —
// the Code surface needs an LLM to do anything.
func TestHandleCodeRun_NoLLM(t *testing.T) {
	s := NewServer(Options{Memory: memory.Disabled()})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/code/run", strings.NewReader("task=hi"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.mux().ServeHTTP(rr, req)
	if rr.Code != 503 {
		t.Fatalf("code = %d, want 503", rr.Code)
	}
}

// TestHandleCodeRun_EmptyTask returns 400 when the task is blank, even
// with a provider configured.
func TestHandleCodeRun_EmptyTask(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GOON_WORKSPACE_DIR", dir)
	t.Setenv("GOON_STORAGE_DIR", t.TempDir())
	s := NewServer(Options{
		Memory: memory.Disabled(),
		LLM:    llm.NewMock([]string{`{"tool":"finish","args":{"message":"x"}}`}),
	})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/code/run", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.mux().ServeHTTP(rr, req)
	if rr.Code != 400 {
		t.Fatalf("code = %d, want 400", rr.Code)
	}
}

// TestHandleCodeRun_StreamsAndFinishes drives a full run with a mock
// provider that finishes immediately, asserting the transcript header
// and the final result both reach the client.
func TestHandleCodeRun_StreamsAndFinishes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GOON_WORKSPACE_DIR", dir)
	t.Setenv("GOON_STORAGE_DIR", t.TempDir())
	s := NewServer(Options{
		Memory: memory.Disabled(),
		LLM:    llm.NewMock([]string{`{"tool":"finish","args":{"message":"all done"}}`}),
	})
	rr := httptest.NewRecorder()
	form := url.Values{"task": {"do a thing"}, "workdir": {dir}, "max_steps": {"7"}}
	req := httptest.NewRequest("POST", "/api/code/run", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.mux().ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("code = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"workdir", "task", "do a thing", "capped at 7", "✓ done", "all done"} {
		if !strings.Contains(body, want) {
			t.Errorf("transcript missing %q in:\n%s", want, body)
		}
	}
}

// TestFragTabCodeRenders ensures the Code tab serves. With no workdir
// and no LLM it still renders the page header.
func TestFragTabCodeRenders(t *testing.T) {
	t.Setenv("GOON_STORAGE_DIR", t.TempDir())
	s := NewServer(Options{
		Memory: memory.Disabled(),
		LLM:    llm.NewMock([]string{`{"tool":"finish","args":{"message":"x"}}`}),
	})
	rr := httptest.NewRecorder()
	s.mux().ServeHTTP(rr, httptest.NewRequest("GET", "/fragments/tab-code", nil))
	if rr.Code != 200 {
		t.Fatalf("code = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Code") {
		t.Errorf("code tab missing heading:\n%s", rr.Body.String())
	}
}
