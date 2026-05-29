package web

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/harisaginting/goon/internal/githost"
	"github.com/harisaginting/goon/internal/llm"
)

// helper to invoke handlePRDraftReview with a minimal Server.
func runDraftReview(t *testing.T, host githost.Host, llmProv llm.Provider, query string) (status int, body string) {
	t.Helper()
	s := &Server{opts: Options{Host: host, LLM: llmProv}}
	req := httptest.NewRequest("GET", "/api/pr/draft-review?"+query, nil)
	w := httptest.NewRecorder()
	s.handlePRDraftReview(w, req)
	return w.Code, w.Body.String()
}

func TestPRDraftReview_NoHost(t *testing.T) {
	_, body := runDraftReview(t, nil, llm.NewMock([]string{"x"}), "repo=o/r&number=1")
	if !strings.Contains(body, "no git host") {
		t.Errorf("expected 'no git host' message, got: %q", body)
	}
}

func TestPRDraftReview_NoLLM(t *testing.T) {
	_, body := runDraftReview(t, githost.NewMock(), nil, "repo=o/r&number=1")
	if !strings.Contains(body, "no LLM") {
		t.Errorf("expected 'no LLM' message, got: %q", body)
	}
}

func TestPRDraftReview_BadParams(t *testing.T) {
	host := githost.NewMock()
	mockLLM := llm.NewMock([]string{"x"})
	// Missing both repo and number.
	if _, body := runDraftReview(t, host, mockLLM, ""); !strings.Contains(body, "repo and number") {
		t.Errorf("missing params: %q", body)
	}
	// Bad number.
	if _, body := runDraftReview(t, host, mockLLM, "repo=o/r&number=abc"); !strings.Contains(body, "invalid PR number") {
		t.Errorf("bad number: %q", body)
	}
}

func TestPRDraftReview_HappyPath(t *testing.T) {
	host := githost.NewMock()
	host.OpenPRs = []githost.PR{{Number: 1269, Repo: "meditap/internal-portal-service", Title: "Add cache"}}
	host.Diffs[1269] = "diff --git a/x b/x\n--- a/x\n+++ b/x\n+new line\n"
	const draft = "SUMMARY — Adds a caching layer.\nRECOMMENDATION: comment"
	mockLLM := llm.NewMock([]string{draft})

	_, body := runDraftReview(t, host, mockLLM,
		"repo=meditap/internal-portal-service&number=1269")
	// The handler html-escapes the draft before writing it; check that
	// the draft body lands in the response and isn't an error.
	if strings.HasPrefix(body, "✗") {
		t.Fatalf("unexpected error response: %q", body)
	}
	for _, want := range []string{"Adds a caching layer", "RECOMMENDATION"} {
		if !strings.Contains(body, want) {
			t.Errorf("draft text missing %q in response:\n%s", want, body)
		}
	}
}

func TestPRDraftReview_EmptyDiff(t *testing.T) {
	host := githost.NewMock()
	host.OpenPRs = []githost.PR{{Number: 7, Repo: "o/r"}}
	// no diff entry → empty
	mockLLM := llm.NewMock([]string{"x"})
	_, body := runDraftReview(t, host, mockLLM, "repo=o/r&number=7")
	if !strings.Contains(body, "empty diff") {
		t.Errorf("expected 'empty diff' message, got: %q", body)
	}
}
