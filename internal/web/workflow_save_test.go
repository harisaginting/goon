package web

import (
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/harisaginting/goon/internal/memory"
)

// postSave is a tiny helper that drives /api/workflow/save with a body +
// path and returns the recorder.
func postSave(t *testing.T, s *Server, savePath, body string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{}
	form.Set("path", savePath)
	form.Set("body", body)
	req := httptest.NewRequest("POST", "/api/workflow/save", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	s.mux().ServeHTTP(rr, req)
	return rr
}

// TestWorkflowSave_ValidWritesFile verifies a well-formed config is written
// to the allowlisted path and the success fragment is returned.
func TestWorkflowSave_ValidWritesFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "workflow.json")
	s := NewServer(Options{Memory: memory.Disabled(), WorkflowPath: target})

	body := `{"name":"custom","stages":[{"name":"plan","type":"llm","prompt":"hi"}]}`
	rr := postSave(t, s, target, body)
	if rr.Code != 200 {
		t.Fatalf("code = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "✓") {
		t.Fatalf("expected success fragment, got: %s", rr.Body.String())
	}
	written, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("file not written: %v", err)
	}
	if !strings.Contains(string(written), `"plan"`) {
		t.Errorf("written file missing stage: %s", written)
	}
}

// TestWorkflowSave_RejectsInvalidStages is the core regression: a config that
// parses as JSON but is semantically invalid (duplicate stage names) must be
// rejected by handleWorkflowSave's Validate() call, NOT silently written for
// the daemon to choke on later.
func TestWorkflowSave_RejectsInvalidStages(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "workflow.json")
	s := NewServer(Options{Memory: memory.Disabled(), WorkflowPath: target})

	// Duplicate stage name "dup".
	body := `{"stages":[{"name":"dup","type":"llm","prompt":"a"},{"name":"dup","type":"agent","task":"b"}]}`
	rr := postSave(t, s, target, body)
	if rr.Code != 200 {
		t.Fatalf("code = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "✗") || !strings.Contains(rr.Body.String(), "invalid workflow") {
		t.Fatalf("expected validation error fragment, got: %s", rr.Body.String())
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("invalid config should not have been written; stat err = %v", err)
	}
}

// TestWorkflowSave_RejectsBadJSON keeps the existing JSON-parse guard honest.
func TestWorkflowSave_RejectsBadJSON(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "workflow.json")
	s := NewServer(Options{Memory: memory.Disabled(), WorkflowPath: target})

	rr := postSave(t, s, target, `{not json`)
	if !strings.Contains(rr.Body.String(), "✗") {
		t.Fatalf("expected error fragment, got: %s", rr.Body.String())
	}
}

// TestWorkflowSave_RejectsUnexpectedPath guards the path allowlist so the
// save surface can't be used to write arbitrary files.
func TestWorkflowSave_RejectsUnexpectedPath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "workflow.json")
	s := NewServer(Options{Memory: memory.Disabled(), WorkflowPath: target})

	evil := filepath.Join(dir, "evil.json")
	rr := postSave(t, s, evil, `{"name":"x"}`)
	if !strings.Contains(rr.Body.String(), "✗") {
		t.Fatalf("expected refusal fragment, got: %s", rr.Body.String())
	}
	if _, err := os.Stat(evil); !os.IsNotExist(err) {
		t.Errorf("must not write to non-allowlisted path; stat err = %v", err)
	}
}
