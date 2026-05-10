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

// TestOpenAI_Generate_HappyPath verifies a single successful round-trip
// returns the assistant's content and emits the expected request shape.
func TestOpenAI_Generate_HappyPath(t *testing.T) {
	var got openAIChatRequest
	var auth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		auth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_ = json.NewEncoder(w).Encode(openAIChatResponse{
			Choices: []struct {
				Message Message `json:"message"`
			}{{Message: Message{Role: RoleAssistant, Content: "hello world"}}},
		})
	}))
	defer ts.Close()

	o := NewOpenAI(OpenAIConfig{APIKey: "sk-test", BaseURL: ts.URL, Model: "gpt-4o-mini", HTTP: ts.Client()})
	out, err := o.Generate(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, Options{Temperature: 0.2, MaxTokens: 16})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if out != "hello world" {
		t.Fatalf("content: got %q want %q", out, "hello world")
	}
	if auth != "Bearer sk-test" {
		t.Errorf("authorization header: got %q", auth)
	}
	if got.Model != "gpt-4o-mini" {
		t.Errorf("model: got %q want gpt-4o-mini", got.Model)
	}
	if got.Temperature != 0.2 {
		t.Errorf("temperature: got %v want 0.2", got.Temperature)
	}
	if got.ResponseFmt != nil {
		t.Errorf("response_format should be nil when JSONMode=false")
	}
}

// TestOpenAI_JSONModeHeader verifies opts.JSONMode produces the
// response_format={"type":"json_object"} hint OpenAI expects.
func TestOpenAI_JSONModeHeader(t *testing.T) {
	var got openAIChatRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_ = json.NewEncoder(w).Encode(openAIChatResponse{
			Choices: []struct {
				Message Message `json:"message"`
			}{{Message: Message{Role: RoleAssistant, Content: "{}"}}},
		})
	}))
	defer ts.Close()
	o := NewOpenAI(OpenAIConfig{APIKey: "x", BaseURL: ts.URL, Model: "gpt-4o-mini", HTTP: ts.Client()})
	if _, err := o.Generate(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, Options{JSONMode: true}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got.ResponseFmt == nil {
		t.Fatalf("ResponseFmt missing")
	}
	if (*got.ResponseFmt)["type"] != "json_object" {
		t.Fatalf("ResponseFmt: got %v want type=json_object", *got.ResponseFmt)
	}
}

// TestOpenAI_RetriesOn429ThenSucceeds verifies a 429 is retried.
func TestOpenAI_RetriesOn429ThenSucceeds(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"slow down"}}`)
			return
		}
		_ = json.NewEncoder(w).Encode(openAIChatResponse{
			Choices: []struct {
				Message Message `json:"message"`
			}{{Message: Message{Role: RoleAssistant, Content: "ok"}}},
		})
	}))
	defer ts.Close()
	o := NewOpenAI(OpenAIConfig{APIKey: "x", BaseURL: ts.URL, Model: "m", HTTP: ts.Client()})
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

// TestOpenAI_RetryExhausted verifies repeated 500s are surfaced as errors
// with all retry attempts consumed.
func TestOpenAI_RetryExhausted(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `boom`)
	}))
	defer ts.Close()
	o := NewOpenAI(OpenAIConfig{APIKey: "x", BaseURL: ts.URL, Model: "m", HTTP: ts.Client()})
	_, err := o.Generate(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, Options{})
	if err == nil {
		t.Fatal("expected error after retries exhausted")
	}
	if !strings.Contains(err.Error(), "openai") {
		t.Errorf("error should mention openai: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("expected 3 attempts, got %d", got)
	}
}

// TestOpenAI_CtxCancelAbortsImmediately verifies a cancelled context returns
// promptly without consuming all retry attempts.
func TestOpenAI_CtxCancelAbortsImmediately(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	o := NewOpenAI(OpenAIConfig{APIKey: "x", BaseURL: ts.URL, Model: "m", HTTP: ts.Client()})
	start := time.Now()
	_, err := o.Generate(ctx, []Message{{Role: RoleUser, Content: "hi"}}, Options{})
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if time.Since(start) > time.Second {
		t.Errorf("cancellation should be near-instant, took %s", time.Since(start))
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("expected 0 server calls after pre-cancelled ctx, got %d", got)
	}
}

// TestOpenAI_NoRetryOn4xx verifies non-retryable status codes (e.g. 401)
// are returned directly without consuming all attempts.
func TestOpenAI_NoRetryOn4xx(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"bad key"}}`)
	}))
	defer ts.Close()
	o := NewOpenAI(OpenAIConfig{APIKey: "x", BaseURL: ts.URL, Model: "m", HTTP: ts.Client()})
	_, err := o.Generate(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, Options{})
	if err == nil {
		t.Fatal("expected error from 401")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected single attempt for 4xx, got %d", got)
	}
}
