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

// TestResolveCodeWorkdir is the security boundary: only directories in
// the whitelist (workspace root + mapped local checkouts) are accepted;
// everything else — parent dirs, absolute escapes — is refused.
func TestResolveCodeWorkdir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GOON_WORKSPACE_DIR", dir)
	t.Setenv("GOON_STORAGE_DIR", t.TempDir()) // isolate REPOSITORY.md

	abs, _ := filepath.Abs(dir)
	abs = strings.TrimRight(abs, string(os.PathSeparator))

	if got, err := resolveCodeWorkdir(""); err != nil || got != abs {
		t.Fatalf("empty input: got %q err %v, want %q", got, err, abs)
	}
	if got, err := resolveCodeWorkdir(abs); err != nil || got != abs {
		t.Fatalf("exact path: got %q err %v, want %q", got, err, abs)
	}
	if _, err := resolveCodeWorkdir(filepath.Join(dir, "..")); err == nil {
		t.Errorf("expected rejection for parent directory")
	}
	if _, err := resolveCodeWorkdir("/etc"); err == nil {
		t.Errorf("expected rejection for /etc (not in whitelist)")
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
	form := url.Values{"task": {"do a thing"}, "workdir": {dir}}
	req := httptest.NewRequest("POST", "/api/code/run", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.mux().ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("code = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"workdir", "task", "do a thing", "✓ done", "all done"} {
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
