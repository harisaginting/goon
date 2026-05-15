package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/harisaginting/goon/internal/memory"
)

func TestIndex_Served(t *testing.T) {
	mem := memory.Disabled()
	s := NewServer(Options{Memory: mem})
	mux := s.mux()
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "goon") {
		t.Fatalf("index: code=%d body=%q", rr.Code, rr.Body.String()[:200])
	}
}

func TestHTMX_Served(t *testing.T) {
	mem := memory.Disabled()
	s := NewServer(Options{Memory: mem})
	mux := s.mux()
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/htmx.min.js", nil))
	if rr.Code != 200 {
		t.Fatalf("htmx code=%d", rr.Code)
	}
	if rr.Body.Len() < 1000 {
		t.Errorf("htmx looks too small: %d bytes", rr.Body.Len())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("content-type: %s", ct)
	}
}

func TestAPI_StatusJSON(t *testing.T) {
	mem := memory.Disabled()
	mem.SetStatus(memory.DaemonStatus{Running: true, BoardName: "jira", PID: 99})
	s := NewServer(Options{Memory: mem})
	mux := s.mux()
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/api/status", nil))
	if rr.Code != 200 {
		t.Fatalf("code: %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"running": true`) || !strings.Contains(rr.Body.String(), `"jira"`) {
		t.Fatalf("body: %s", rr.Body.String())
	}
}

func TestFragment_Status(t *testing.T) {
	mem := memory.Disabled()
	mem.SetStatus(memory.DaemonStatus{Running: true, BoardName: "jira", LastPoll: time.Now()})
	s := NewServer(Options{Memory: mem})
	mux := s.mux()
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/fragments/status", nil))
	if rr.Code != 200 {
		t.Fatalf("code: %d", rr.Code)
	}
	body := rr.Body.String()
	// statusBadge returns the lowercase label "running"; CSS makes it
	// look uppercase visually. Asserting the actual rendered text
	// keeps the test honest about what the server emits.
	if !strings.Contains(body, "running") || !strings.Contains(body, "jira") {
		t.Fatalf("body: %s", body)
	}
	// Pause button must be present when running and not paused —
	// regression-guards the dead-end-button fix from cycle 1.
	if !strings.Contains(body, "/api/daemon/pause") {
		t.Fatalf("expected pause control in status; got:\n%s", body)
	}
}

// TestFragment_StatusPaused covers the paused branch — the daemon is
// running but Paused=true. The status panel must render the resume
// button (not pause), and the badge must say "paused".
func TestFragment_StatusPaused(t *testing.T) {
	mem := memory.Disabled()
	mem.SetStatus(memory.DaemonStatus{Running: true, Paused: true, BoardName: "jira", LastPoll: time.Now()})
	s := NewServer(Options{Memory: mem})
	mux := s.mux()
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/fragments/status", nil))
	body := rr.Body.String()
	if !strings.Contains(body, "paused") {
		t.Fatalf("expected 'paused' label, got:\n%s", body)
	}
	if !strings.Contains(body, "/api/daemon/resume") {
		t.Fatalf("expected resume control on paused daemon, got:\n%s", body)
	}
	if strings.Contains(body, "/api/daemon/pause") {
		t.Fatalf("paused daemon should not show pause button, got:\n%s", body)
	}
}

// TestHandleDaemonPauseReturnsResumeButton covers the round-trip the
// front-end relies on: POST /api/daemon/pause must return the *alternate*
// button (resume) so the htmx outerHTML swap leaves a working control.
func TestHandleDaemonPauseReturnsResumeButton(t *testing.T) {
	mem := memory.Disabled()
	s := NewServer(Options{Memory: mem})
	mux := s.mux()
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("POST", "/api/daemon/pause", nil))
	if rr.Code != 200 {
		t.Fatalf("code: %d", rr.Code)
	}
	if !mem.IsPaused() {
		t.Fatal("paused flag should be set")
	}
	body := rr.Body.String()
	if !strings.Contains(body, "/api/daemon/resume") {
		t.Fatalf("response should be the resume button, got:\n%s", body)
	}
	if rr.Header().Get("HX-Trigger") != "statusChanged" {
		t.Errorf("expected HX-Trigger: statusChanged, got %q", rr.Header().Get("HX-Trigger"))
	}
}

// TestHandleDaemonResumeReturnsPauseButton mirrors the round-trip for
// /api/daemon/resume — must return the pause button so the swap is
// symmetric.
func TestHandleDaemonResumeReturnsPauseButton(t *testing.T) {
	mem := memory.Disabled()
	mem.SetPaused(true)
	s := NewServer(Options{Memory: mem})
	mux := s.mux()
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("POST", "/api/daemon/resume", nil))
	if rr.Code != 200 {
		t.Fatalf("code: %d", rr.Code)
	}
	if mem.IsPaused() {
		t.Fatal("paused flag should be cleared")
	}
	body := rr.Body.String()
	if !strings.Contains(body, "/api/daemon/pause") {
		t.Fatalf("response should be the pause button, got:\n%s", body)
	}
}

// TestNewFragmentRoutesRespond200 smoke-tests every new endpoint added
// in the cycle-1 UI rebuild. Each route must return 200 and a non-empty
// body. Catches mux registration drift and obvious render panics.
func TestNewFragmentRoutesRespond200(t *testing.T) {
	mem := memory.Disabled()
	mem.SetStatus(memory.DaemonStatus{Running: true, BoardName: "jira", LastPoll: time.Now()})
	s := NewServer(Options{Memory: mem})
	mux := s.mux()
	for _, path := range []string{
		"/fragments/status-pill",
		// Legacy + current tab composers — all must still resolve.
		"/fragments/tab-work",
		"/fragments/tab-overview", // legacy alias → work
		"/fragments/tab-tickets",  // legacy alias → work
		"/fragments/tab-workflows", // legacy alias → work
		"/fragments/tab-questions", // legacy alias → work
		"/fragments/tab-memory",
		"/fragments/tab-knowledge", // legacy alias → memory
		"/fragments/tab-skills",    // legacy alias → memory
		"/fragments/tab-config",
		"/fragments/tab-chat",
	} {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code != 200 {
			t.Errorf("%s: code = %d, want 200", path, rr.Code)
			continue
		}
		// /fragments/questions-banner returns empty body when no
		// questions are pending — that's correct, skip the
		// non-empty check for it.
		if path == "/fragments/questions-banner" {
			continue
		}
		if rr.Body.Len() == 0 {
			t.Errorf("%s: empty body", path)
		}
	}
}

// TestQuestionsBanner_RendersOnPending: when at least one question is
// pending, the banner must render with the count + the Review button
// that switches to the Questions tab.
func TestQuestionsBanner_RendersOnPending(t *testing.T) {
	mem := memory.Disabled()
	mem.AskQuestion(memory.Question{TicketID: "ENG-1", Question: "approve plan?"})
	s := NewServer(Options{Memory: mem})
	mux := s.mux()
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/fragments/questions-banner", nil))
	body := rr.Body.String()
	for _, want := range []string{"1 pending", "data-tab=questions", "Review"} {
		if !strings.Contains(body, want) {
			t.Errorf("banner missing %q in:\n%s", want, body)
		}
	}
}

// TestQuestionsBanner_EmptyWhenNoPending: zero pending = empty body so
// the page doesn't reserve vertical space for a banner that has nothing
// to say.
func TestQuestionsBanner_EmptyWhenNoPending(t *testing.T) {
	mem := memory.Disabled()
	s := NewServer(Options{Memory: mem})
	mux := s.mux()
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/fragments/questions-banner", nil))
	if rr.Body.Len() != 0 {
		t.Errorf("banner should be empty body when no pending, got:\n%s", rr.Body.String())
	}
}

// TestStatusPill_LightLabel verifies the header pill shows the correct
// label string when running. Catches drift between the pill renderer
// and statusBadge().
func TestStatusPill_LightLabel(t *testing.T) {
	mem := memory.Disabled()
	mem.SetStatus(memory.DaemonStatus{Running: true, BoardName: "jira", LastPoll: time.Now()})
	s := NewServer(Options{Memory: mem})
	mux := s.mux()
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/fragments/status-pill", nil))
	body := rr.Body.String()
	if !strings.Contains(body, "running") {
		t.Errorf("pill should say 'running', got:\n%s", body)
	}
}

func TestFragment_Questions_Empty(t *testing.T) {
	mem := memory.Disabled()
	s := NewServer(Options{Memory: mem})
	mux := s.mux()
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/fragments/questions", nil))
	if !strings.Contains(rr.Body.String(), "No pending questions") {
		t.Fatalf("body: %s", rr.Body.String())
	}
}

func TestFragment_Questions_WithForm(t *testing.T) {
	mem := memory.Disabled()
	id := mem.AskQuestion(memory.Question{TicketID: "ENG-1", Question: "Which DB?"})
	s := NewServer(Options{Memory: mem})
	mux := s.mux()
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/fragments/questions", nil))
	body := rr.Body.String()
	if !strings.Contains(body, id) || !strings.Contains(body, "Which DB?") {
		t.Fatalf("body: %s", body)
	}
	if !strings.Contains(body, "/api/answer") {
		t.Fatalf("missing form action: %s", body)
	}
}

func TestAnswer_Roundtrip(t *testing.T) {
	mem := memory.Disabled()
	id := mem.AskQuestion(memory.Question{TicketID: "ENG-1", Question: "Which DB?"})
	s := NewServer(Options{Memory: mem})
	mux := s.mux()

	req := httptest.NewRequest("POST", "/api/answer", strings.NewReader("id="+id+"&answer=postgres"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("code: %d body: %s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("HX-Trigger") != "questionsChanged" {
		t.Errorf("missing HX-Trigger: %v", rr.Header())
	}
	if len(mem.PendingQuestions()) != 0 {
		t.Errorf("question not answered")
	}
}

func TestAnswer_BadID(t *testing.T) {
	mem := memory.Disabled()
	s := NewServer(Options{Memory: mem})
	mux := s.mux()
	req := httptest.NewRequest("POST", "/api/answer", strings.NewReader("id=bogus&answer=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("code: %d", rr.Code)
	}
}
