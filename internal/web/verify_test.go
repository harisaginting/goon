package web

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestConfigVerify_HittingMockProvidersReturnsAllOK(t *testing.T) {
	s, _ := newServer(t)
	mux := s.mux()

	form := url.Values{
		"GOON_LLM_PROVIDER": {"mock"},
		"GOON_BOARD":        {"mock"},
		"GOON_GIT_HOST":     {"mock"},
	}
	req := httptest.NewRequest("POST", "/api/config/verify", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("code: %d body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "all checks passed") {
		t.Fatalf("expected pass header: %s", body)
	}
	if !strings.Contains(body, "<strong>llm/mock</strong>") {
		t.Fatalf("missing llm row: %s", body)
	}
	if !strings.Contains(body, "<strong>board/mock</strong>") {
		t.Fatalf("missing board row: %s", body)
	}
}

func TestConfigVerify_FailureSurfacedInFragment(t *testing.T) {
	s, _ := newServer(t)
	mux := s.mux()
	// openai with no key → expect ✗ row + "failures detected"
	form := url.Values{
		"GOON_LLM_PROVIDER": {"openai"},
		"OPENAI_API_KEY":    {""},
		"GOON_BOARD":        {""},
	}
	req := httptest.NewRequest("POST", "/api/config/verify", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("code: %d body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "failures detected") {
		t.Fatalf("expected failure header: %s", body)
	}
	if !strings.Contains(body, "OPENAI_API_KEY") {
		t.Fatalf("expected detail mentioning the missing key: %s", body)
	}
}

func TestConfigVerify_BadMethod(t *testing.T) {
	s, _ := newServer(t)
	mux := s.mux()
	req := httptest.NewRequest("GET", "/api/config/verify", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != 405 {
		t.Fatalf("code: %d", rr.Code)
	}
}

func TestConfigVerify_DoesNotPersist(t *testing.T) {
	s, _ := newServer(t)
	mux := s.mux()

	form := url.Values{
		"GOON_LLM_PROVIDER": {"mock"},
		"OPENAI_API_KEY":    {"sk-temporary-test-value"},
	}
	req := httptest.NewRequest("POST", "/api/config/verify", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("code: %d", rr.Code)
	}
	// Read the config back via /api/config — OPENAI_API_KEY must NOT be set
	// to "sk-temporary-test-value" (it should be unset since we never wrote
	// to env outside of the override window).
	rr2 := httptest.NewRecorder()
	mux.ServeHTTP(rr2, httptest.NewRequest("GET", "/api/config?reveal=1", nil))
	if strings.Contains(rr2.Body.String(), "sk-temporary-test-value") {
		t.Fatalf("verify should not persist values; body=%s", rr2.Body.String())
	}
}

func TestFragConfig_HasVerifyButton(t *testing.T) {
	s, _ := newServer(t)
	mux := s.mux()
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/fragments/config", nil))
	body := rr.Body.String()
	for _, w := range []string{
		`hx-post="/api/config/verify"`,
		`verify connection`,
		`save &amp; reload daemon`,
	} {
		if !strings.Contains(body, w) {
			t.Errorf("missing %q in form: %s", w, body[:200])
		}
	}
}
