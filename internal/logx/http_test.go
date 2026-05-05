package logx

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoggingTransport_LogsBasics(t *testing.T) {
	// Capture into a fresh logger writing to a tmp file.
	dir := t.TempDir()
	path := filepath.Join(dir, "http.log")
	lg, _ := New(Config{Path: path, Level: "info", AlsoStderr: boolPtr(false)})
	defer lg.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	tr := &LoggingTransport{Component: "test-svc", Logger: lg}
	client := tr.WrapClient(nil)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api", strings.NewReader(`{"x":1}`))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"ok":true}` {
		t.Errorf("body got mangled by transport: %q", body)
	}

	logged, _ := os.ReadFile(path)
	got := string(logged)
	for _, want := range []string{
		"component=test-svc", "method=POST", "status=200",
		"req_bytes=7", `resp_bytes=11`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log missing %q in:\n%s", want, got)
		}
	}
}

func TestLoggingTransport_DebugCapturesBodies(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "http.log")
	lg, _ := New(Config{Path: path, Level: "debug", AlsoStderr: boolPtr(false)})
	defer lg.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"hello":"world"}`))
	}))
	defer srv.Close()

	tr := &LoggingTransport{Component: "x", Logger: lg}
	client := tr.WrapClient(nil)
	req, _ := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader([]byte(`{"req":1}`)))
	resp, _ := client.Do(req)
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), `world`) {
		t.Errorf("debug log should include resp body:\n%s", got)
	}
	if !strings.Contains(string(got), `req`) {
		t.Errorf("debug log should include req body:\n%s", got)
	}
}

func TestLoggingTransport_RedactsTelegramTokens(t *testing.T) {
	got := redactedURL("https://api.telegram.org/bot12345:SECRET/sendMessage")
	if strings.Contains(got, "SECRET") {
		t.Errorf("token leaked: %s", got)
	}
	if !strings.Contains(got, "/bot***/") {
		t.Errorf("bot path not redacted: %s", got)
	}
}

func TestLoggingTransport_RedactsBasicAuth(t *testing.T) {
	got := redactedURL("https://user:supersecret@api.example.com/v1/things")
	if strings.Contains(got, "supersecret") || strings.Contains(got, "user:") {
		t.Errorf("basic auth leaked: %s", got)
	}
}

func TestLoggingTransport_PreservesExistingTransport(t *testing.T) {
	called := false
	inner := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		called = true
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	c := &http.Client{Transport: inner}
	tr := &LoggingTransport{Component: "x", Logger: silentLogger(t)}
	wrapped := tr.WrapClient(c)
	req, _ := http.NewRequest("GET", "http://x/", nil)
	if _, err := wrapped.Do(req); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Error("inner transport should have been called via wrap")
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func silentLogger(t *testing.T) *Logger {
	t.Helper()
	lg, _ := New(Config{Path: filepath.Join(t.TempDir(), "x.log"), Level: "error", AlsoStderr: boolPtr(false)})
	return lg
}
