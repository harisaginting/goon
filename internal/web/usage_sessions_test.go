package web

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/harisaginting/goon/internal/memory"
)

// TestFragUsageRenders ensures the token-usage card serves and shows its
// heading (empty-meter path is fine — it renders the "no tokens yet" state).
func TestFragUsageRenders(t *testing.T) {
	s := NewServer(Options{Memory: memory.Disabled()})
	rr := httptest.NewRecorder()
	s.mux().ServeHTTP(rr, httptest.NewRequest("GET", "/fragments/usage", nil))
	if rr.Code != 200 {
		t.Fatalf("code = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Token usage") {
		t.Errorf("usage card missing heading:\n%s", rr.Body.String())
	}
}

// TestFragSessionsRenders ensures the live-sessions card serves and shows its
// heading (no agents running → empty state).
func TestFragSessionsRenders(t *testing.T) {
	s := NewServer(Options{Memory: memory.Disabled()})
	rr := httptest.NewRecorder()
	s.mux().ServeHTTP(rr, httptest.NewRequest("GET", "/fragments/sessions", nil))
	if rr.Code != 200 {
		t.Fatalf("code = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Live agent sessions") {
		t.Errorf("sessions card missing heading:\n%s", rr.Body.String())
	}
}
