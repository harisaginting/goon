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

// TestAnthropic_Generate_HappyPath verifies headers and the system/user
// message split that the /messages endpoint requires.
func TestAnthropic_Generate_HappyPath(t *testing.T) {
	var got anthropicMessageRequest
	var apiKey, version string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/messages") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		apiKey = r.Header.Get("x-api-key")
		version = r.Header.Get("anthropic-version")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_ = json.NewEncoder(w).Encode(anthropicMessageResponse{
			Content: []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}{{Type: "text", Text: "hello"}},
		})
	}))
	defer ts.Close()

	a := NewAnthropic(AnthropicConfig{APIKey: "ak-test", BaseURL: ts.URL, Model: "claude-test", HTTP: ts.Client()})
	out, err := a.Generate(context.Background(), []Message{
		{Role: RoleSystem, Content: "you are helpful"},
		{Role: RoleUser, Content: "hi"},
	}, Options{Temperature: 0.3, MaxTokens: 256})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if out != "hello" {
		t.Fatalf("content: got %q want hello", out)
	}
	if apiKey != "ak-test" {
		t.Errorf("x-api-key: got %q", apiKey)
	}
	if version != "2023-06-01" {
		t.Errorf("anthropic-version: got %q", version)
	}
	if got.System != "you are helpful" {
		t.Errorf("system: got %q want %q", got.System, "you are helpful")
	}
	if len(got.Messages) != 1 || got.Messages[0].Role != RoleUser || got.Messages[0].Content != "hi" {
		t.Errorf("messages: got %+v", got.Messages)
	}
	if got.MaxTokens != 256 {
		t.Errorf("max_tokens: got %d want 256", got.MaxTokens)
	}
}

// TestAnthropic_DefaultMaxTokens verifies the 1024 fallback when
// opts.MaxTokens is unset (Anthropic requires a non-zero value).
func TestAnthropic_DefaultMaxTokens(t *testing.T) {
	var got anthropicMessageRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_ = json.NewEncoder(w).Encode(anthropicMessageResponse{
			Content: []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}{{Type: "text", Text: "ok"}},
		})
	}))
	defer ts.Close()
	a := NewAnthropic(AnthropicConfig{APIKey: "x", BaseURL: ts.URL, Model: "m", HTTP: ts.Client()})
	if _, err := a.Generate(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, Options{}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got.MaxTokens != 1024 {
		t.Fatalf("default max_tokens: got %d want 1024", got.MaxTokens)
	}
}

// TestAnthropic_ToolRoleRemappedToUser verifies tool-role messages are
// translated to "user" before being sent to /messages.
func TestAnthropic_ToolRoleRemappedToUser(t *testing.T) {
	var got anthropicMessageRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_ = json.NewEncoder(w).Encode(anthropicMessageResponse{
			Content: []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}{{Type: "text", Text: "ok"}},
		})
	}))
	defer ts.Close()
	a := NewAnthropic(AnthropicConfig{APIKey: "x", BaseURL: ts.URL, Model: "m", HTTP: ts.Client()})
	if _, err := a.Generate(context.Background(), []Message{
		{Role: RoleTool, Content: "tool result"},
	}, Options{}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(got.Messages) != 1 || got.Messages[0].Role != "user" {
		t.Fatalf("expected tool role remapped to user, got %+v", got.Messages)
	}
}

// TestAnthropic_RetriesOn429ThenSucceeds verifies a 429 is retried.
func TestAnthropic_RetriesOn429ThenSucceeds(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"slow"}}`)
			return
		}
		_ = json.NewEncoder(w).Encode(anthropicMessageResponse{
			Content: []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}{{Type: "text", Text: "ok"}},
		})
	}))
	defer ts.Close()
	a := NewAnthropic(AnthropicConfig{APIKey: "x", BaseURL: ts.URL, Model: "m", HTTP: ts.Client()})
	out, err := a.Generate(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, Options{})
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

// TestAnthropic_RetryExhausted verifies repeated 503s drain attempts
// and surface an error.
func TestAnthropic_RetryExhausted(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `down`)
	}))
	defer ts.Close()
	a := NewAnthropic(AnthropicConfig{APIKey: "x", BaseURL: ts.URL, Model: "m", HTTP: ts.Client()})
	_, err := a.Generate(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, Options{})
	if err == nil {
		t.Fatal("expected error after retries exhausted")
	}
	if !strings.Contains(err.Error(), "anthropic") {
		t.Errorf("error should mention anthropic: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("expected 3 attempts, got %d", got)
	}
}

// TestAnthropic_CtxCancelAbortsImmediately verifies a pre-cancelled context
// short-circuits before any HTTP call is made.
func TestAnthropic_CtxCancelAbortsImmediately(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a := NewAnthropic(AnthropicConfig{APIKey: "x", BaseURL: ts.URL, Model: "m", HTTP: ts.Client()})
	start := time.Now()
	_, err := a.Generate(ctx, []Message{{Role: RoleUser, Content: "hi"}}, Options{})
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

// TestParseAnthropicSSE drives the SSE parser directly so we don't need a
// live HTTP server. Verifies content_block_delta events accumulate and
// onChunk fires per delta in order; non-text events are ignored.
func TestParseAnthropicSSE(t *testing.T) {
	body := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"x"}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	var chunks []string
	full, err := parseAnthropicSSE(strings.NewReader(body), func(s string) {
		chunks = append(chunks, s)
	})
	if err != nil {
		t.Fatalf("parseAnthropicSSE: %v", err)
	}
	if full != "Hello world" {
		t.Errorf("full = %q, want %q", full, "Hello world")
	}
	if len(chunks) != 2 || chunks[0] != "Hello" || chunks[1] != " world" {
		t.Errorf("chunks = %v, want [\"Hello\", \" world\"]", chunks)
	}
}

// TestParseAnthropicSSE_Error verifies that an explicit error event in
// the stream surfaces as a Go error and we return whatever text was
// already emitted (so the caller has *something* if a partial response
// is useful).
func TestParseAnthropicSSE_Error(t *testing.T) {
	body := strings.Join([]string{
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`,
		``,
		`event: error`,
		`data: {"type":"error","error":{"type":"overloaded_error","message":"capacity"}}`,
		``,
	}, "\n")
	full, err := parseAnthropicSSE(strings.NewReader(body), nil)
	if err == nil {
		t.Fatal("expected error from error event")
	}
	if !strings.Contains(err.Error(), "capacity") {
		t.Errorf("error message: %v", err)
	}
	if full != "partial" {
		t.Errorf("full = %q, want \"partial\"", full)
	}
}
