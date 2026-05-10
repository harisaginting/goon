package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOllama_Generate(t *testing.T) {
	var got ollamaChatRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/api/chat") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_ = json.NewEncoder(w).Encode(ollamaChatResponse{
			Model:   "llama3",
			Message: Message{Role: RoleAssistant, Content: `{"tool":"finish","args":{"message":"ok"}}`},
			Done:    true,
		})
	}))
	defer ts.Close()

	o := NewOllama(OllamaConfig{
		BaseURL: ts.URL,
		Model:   "llama3",
		HTTP:    ts.Client(),
	})

	resp, err := o.Generate(context.Background(),
		[]Message{{Role: RoleUser, Content: "hi"}},
		Options{JSONMode: true, Temperature: 0.1, MaxTokens: 256},
	)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.Contains(resp, "finish") {
		t.Fatalf("unexpected response: %q", resp)
	}
	if got.Model != "llama3" {
		t.Errorf("model: got %q want llama3", got.Model)
	}
	if got.Format != "json" {
		t.Errorf("format: got %q want json", got.Format)
	}
	if got.Stream {
		t.Errorf("stream should be false for Generate")
	}
	if got.Options["temperature"] != 0.1 {
		t.Errorf("temperature: got %v want 0.1", got.Options["temperature"])
	}
}

func TestOllama_Stream(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Stream three NDJSON chunks, last one has done=true.
		chunks := []ollamaChatResponse{
			{Model: "llama3", Message: Message{Role: RoleAssistant, Content: "Hello "}},
			{Model: "llama3", Message: Message{Role: RoleAssistant, Content: "world"}},
			{Model: "llama3", Message: Message{Role: RoleAssistant, Content: "!"}, Done: true},
		}
		for _, ch := range chunks {
			b, _ := json.Marshal(ch)
			_, _ = w.Write(b)
			_, _ = w.Write([]byte("\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer ts.Close()

	o := NewOllama(OllamaConfig{BaseURL: ts.URL, Model: "llama3", HTTP: ts.Client()})

	var seen []string
	full, err := o.Stream(context.Background(),
		[]Message{{Role: RoleUser, Content: "hi"}},
		Options{},
		func(c string) { seen = append(seen, c) },
	)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if full != "Hello world!" {
		t.Fatalf("assembled: got %q want %q", full, "Hello world!")
	}
	if len(seen) != 3 {
		t.Fatalf("expected 3 chunks, got %d: %v", len(seen), seen)
	}
}

func TestOllama_HTTPError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		_, _ = io.WriteString(w, `{"error":"model not found"}`)
	}))
	defer ts.Close()
	o := NewOllama(OllamaConfig{BaseURL: ts.URL, Model: "nonexistent", HTTP: ts.Client()})
	_, err := o.Generate(context.Background(), []Message{{Role: RoleUser, Content: "x"}}, Options{})
	if err == nil || !strings.Contains(err.Error(), "ollama http 500") {
		t.Fatalf("expected http 500 error, got %v", err)
	}
}

// TestOllama_RetriesOn500ThenSucceeds verifies the shared retry helper
// kicks in for transient 5xx responses from the local Ollama server.
func TestOllama_RetriesOn500ThenSucceeds(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `boom`)
			return
		}
		_ = json.NewEncoder(w).Encode(ollamaChatResponse{
			Model:   "llama3",
			Message: Message{Role: RoleAssistant, Content: "ok"},
			Done:    true,
		})
	}))
	defer ts.Close()
	o := NewOllama(OllamaConfig{BaseURL: ts.URL, Model: "llama3", HTTP: ts.Client()})
	out, err := o.Generate(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, Options{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if out != "ok" {
		t.Fatalf("content: got %q want ok", out)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected 2 attempts, got %d", got)
	}
}

// TestOllama_RetryExhausted verifies repeated 502s consume all attempts.
func TestOllama_RetryExhausted(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer ts.Close()
	o := NewOllama(OllamaConfig{BaseURL: ts.URL, Model: "llama3", HTTP: ts.Client()})
	_, err := o.Generate(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, Options{})
	if err == nil {
		t.Fatal("expected error after retries exhausted")
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("expected 3 attempts, got %d", got)
	}
}

// TestOllama_CtxCancelAbortsImmediately verifies a pre-cancelled context
// short-circuits before any HTTP call.
func TestOllama_CtxCancelAbortsImmediately(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	o := NewOllama(OllamaConfig{BaseURL: ts.URL, Model: "llama3", HTTP: ts.Client()})
	start := time.Now()
	_, err := o.Generate(ctx, []Message{{Role: RoleUser, Content: "hi"}}, Options{})
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if time.Since(start) > time.Second {
		t.Errorf("cancellation should be near-instant, took %s", time.Since(start))
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("expected 0 server calls, got %d", got)
	}
}

func TestOllama_FactoryFromEnv(t *testing.T) {
	t.Setenv("GOON_LLM_PROVIDER", "ollama")
	t.Setenv("OLLAMA_BASE_URL", "http://example.test:11434")
	t.Setenv("OLLAMA_MODEL", "qwen2.5-coder")
	prov, err := NewFromEnv()
	if err != nil {
		t.Fatalf("NewFromEnv: %v", err)
	}
	if prov.Name() != "ollama" {
		t.Fatalf("name: got %q want ollama", prov.Name())
	}
}
