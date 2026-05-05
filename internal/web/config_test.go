package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/harisaginting/goon/internal/memory"
)

// fakeDaemon implements Reconfigurable for tests.
type fakeDaemon struct {
	mu          sync.Mutex
	reconfigure int
	configured  bool
	notes       []string
}

func (f *fakeDaemon) Reconfigure() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reconfigure++
	if f.notes != nil {
		return f.notes
	}
	return []string{"✓ LLM provider: mock", "✓ board: mock"}
}
func (f *fakeDaemon) Configured() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.configured }

func newServer(t *testing.T) (*Server, *fakeDaemon) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	mem := memory.Disabled()
	d := &fakeDaemon{}
	s := NewServer(Options{Memory: mem, Daemon: d})
	return s, d
}

func TestConfigPOST_WritesFileAndReloadsDaemon(t *testing.T) {
	s, d := newServer(t)
	mux := s.mux()

	form := url.Values{
		"GOON_LLM_PROVIDER": {"ollama"},
		"OLLAMA_MODEL":      {"qwen2.5"},
		"OPENAI_API_KEY":    {"sk-secret"},
	}
	req := httptest.NewRequest("POST", "/api/config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("HX-Trigger") != "configChanged" {
		t.Errorf("missing HX-Trigger; headers=%v", rr.Header())
	}
	if d.reconfigure != 1 {
		t.Errorf("Reconfigure called %d times, want 1", d.reconfigure)
	}
	// File should contain all three keys.
	data, err := os.ReadFile(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "goon", ".env"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	body := string(data)
	for _, w := range []string{"GOON_LLM_PROVIDER=ollama", "OLLAMA_MODEL=qwen2.5", "OPENAI_API_KEY=sk-secret"} {
		if !strings.Contains(body, w) {
			t.Errorf("missing %q in config file:\n%s", w, body)
		}
	}
	// os.Setenv should also have been called.
	if os.Getenv("OLLAMA_MODEL") != "qwen2.5" {
		t.Errorf("os env not updated: %q", os.Getenv("OLLAMA_MODEL"))
	}
}

func TestConfigPOST_EmptyFieldUnsets(t *testing.T) {
	s, _ := newServer(t)
	mux := s.mux()
	t.Setenv("OPENAI_API_KEY", "sk-pre-existing")

	// Set then unset.
	post := func(form url.Values) {
		req := httptest.NewRequest("POST", "/api/config", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
	}
	post(url.Values{"OPENAI_API_KEY": {"sk-real"}})
	post(url.Values{"OPENAI_API_KEY": {""}})

	if v := os.Getenv("OPENAI_API_KEY"); v != "" {
		t.Errorf("expected empty after unset, got %q", v)
	}
	data, _ := os.ReadFile(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "goon", ".env"))
	if strings.Contains(string(data), "OPENAI_API_KEY=") {
		t.Errorf("config file still has OPENAI_API_KEY: %s", data)
	}
}

func TestConfigGET_MasksSensitive(t *testing.T) {
	s, _ := newServer(t)
	mux := s.mux()
	t.Setenv("OPENAI_API_KEY", "sk-abcdef1234567890")

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/api/config", nil))
	if !strings.Contains(rr.Body.String(), "OPENAI_API_KEY") {
		t.Fatalf("body missing key: %s", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "sk-abcdef1234567890") {
		t.Fatalf("secret leaked in default GET: %s", rr.Body.String())
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/api/config?reveal=1", nil))
	if !strings.Contains(rr.Body.String(), "sk-abcdef1234567890") {
		t.Fatalf("reveal=1 should print verbatim: %s", rr.Body.String())
	}
}

func TestFragSetup_BannerShowsWhenUnconfigured(t *testing.T) {
	s, _ := newServer(t)
	mux := s.mux()
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/fragments/setup", nil))
	if !strings.Contains(rr.Body.String(), "Welcome to goon") {
		t.Fatalf("expected setup banner: %s", rr.Body.String())
	}
}

func TestFragSetup_HiddenWhenConfigured(t *testing.T) {
	s, d := newServer(t)
	d.configured = true
	mux := s.mux()
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/fragments/setup", nil))
	if strings.Contains(rr.Body.String(), "Welcome to goon") {
		t.Fatalf("expected NO banner when configured: %s", rr.Body.String())
	}
}

func TestFragConfig_RendersForm(t *testing.T) {
	s, _ := newServer(t)
	mux := s.mux()
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/fragments/config", nil))
	body := rr.Body.String()
	for _, w := range []string{
		`hx-post="/api/config"`,
		"GOON_LLM_PROVIDER",
		"GOON_BOARD",
		"OPENAI_API_KEY",
		"BITBUCKET_TOKEN",
		`type="password"`,
	} {
		if !strings.Contains(body, w) {
			t.Errorf("missing %q in config form", w)
		}
	}
}

func TestConfigPOST_BadMethod(t *testing.T) {
	s, _ := newServer(t)
	mux := s.mux()
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("DELETE", "/api/config", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code: %d", rr.Code)
	}
}

func TestSetUnsetConfigKey_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := setConfigKey("KEY1", "value1"); err != nil {
		t.Fatal(err)
	}
	if err := setConfigKey("KEY2", "value2"); err != nil {
		t.Fatal(err)
	}
	if err := setConfigKey("KEY1", "value1-replaced"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "goon", ".env"))
	if !strings.Contains(string(data), "KEY1=value1-replaced") {
		t.Fatalf("replace failed: %s", data)
	}
	if strings.Count(string(data), "KEY1=") != 1 {
		t.Fatalf("expected single KEY1= line: %s", data)
	}
	if err := unsetConfigKey("KEY1"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(dir, "goon", ".env"))
	if strings.Contains(string(data), "KEY1=") {
		t.Fatalf("unset failed: %s", data)
	}
	if !strings.Contains(string(data), "KEY2=value2") {
		t.Fatalf("KEY2 lost: %s", data)
	}
}
