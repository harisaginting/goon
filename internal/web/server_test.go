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
	if !strings.Contains(body, "RUNNING") || !strings.Contains(body, "jira") {
		t.Fatalf("body: %s", body)
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
