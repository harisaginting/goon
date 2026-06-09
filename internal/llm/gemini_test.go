package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGemini_BuildRequest verifies that messages are folded into
// Gemini's system_instruction + contents shape correctly: system
// messages concatenate, assistant becomes "model", user passes
// through, generationConfig respects opts.
func TestGemini_BuildRequest(t *testing.T) {
	g := NewGemini(GeminiConfig{APIKey: "k", Model: "gemini-2.5-flash"})
	req, err := g.buildRequest([]Message{
		{Role: RoleSystem, Content: "sys-a"},
		{Role: RoleSystem, Content: "sys-b"},
		{Role: RoleUser, Content: "hello"},
		{Role: RoleAssistant, Content: "hi"},
		{Role: RoleUser, Content: "follow up"},
	}, Options{Temperature: 0.3, MaxTokens: 256, JSONMode: true})
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if req.SystemInstruction == nil {
		t.Fatal("expected SystemInstruction populated")
	}
	got := req.SystemInstruction.Parts[0].Text
	if !strings.Contains(got, "sys-a") || !strings.Contains(got, "sys-b") {
		t.Errorf("system text missing parts: %q", got)
	}
	if len(req.Contents) != 3 {
		t.Fatalf("contents: got %d, want 3", len(req.Contents))
	}
	if req.Contents[0].Role != "user" || req.Contents[1].Role != "model" {
		t.Errorf("role mapping wrong: %+v", req.Contents)
	}
	if req.GenerationConfig == nil ||
		req.GenerationConfig.Temperature != 0.3 ||
		req.GenerationConfig.MaxOutputTokens != 256 ||
		req.GenerationConfig.ResponseMimeType != "application/json" {
		t.Errorf("genConfig: %+v", req.GenerationConfig)
	}
}

// TestGemini_GenerateHappy uses an httptest server to verify the
// happy path end-to-end: URL shape, request body, response decode.
func TestGemini_GenerateHappy(t *testing.T) {
	var gotURL string
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		buf := new(bytes.Buffer)
		_, _ = io.Copy(buf, r.Body)
		gotBody = buf.String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"pong"}]}}]}`))
	}))
	defer srv.Close()

	g := NewGemini(GeminiConfig{
		APIKey: "test-key", BaseURL: srv.URL, Model: "gemini-2.5-flash",
	})
	out, err := g.Generate(context.Background(),
		[]Message{{Role: RoleUser, Content: "ping"}},
		Options{Temperature: 0.2, MaxTokens: 10})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if out != "pong" {
		t.Errorf("output: got %q, want pong", out)
	}
	if !strings.Contains(gotURL, "/models/gemini-2.5-flash:generateContent") {
		t.Errorf("URL shape wrong: %s", gotURL)
	}
	if !strings.Contains(gotURL, "key=test-key") {
		t.Errorf("API key not in query: %s", gotURL)
	}
	// Body should carry the user content and generation config.
	if !strings.Contains(gotBody, `"text":"ping"`) {
		t.Errorf("body missing user content: %s", gotBody)
	}
}

// TestGemini_GenerateAPIError surfaces the structured error envelope
// rather than swallowing it.
func TestGemini_GenerateAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":400,"message":"bad model","status":"INVALID_ARGUMENT"}}`))
	}))
	defer srv.Close()
	g := NewGemini(GeminiConfig{APIKey: "k", BaseURL: srv.URL, Model: "x"})
	_, err := g.Generate(context.Background(),
		[]Message{{Role: RoleUser, Content: "ping"}}, Options{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "bad model") && !strings.Contains(err.Error(), "400") {
		t.Errorf("error didn't surface API detail: %v", err)
	}
}

// TestParseGeminiSSE feeds a synthetic SSE stream through the parser
// and verifies chunk-by-chunk delivery + final assembled string.
func TestParseGeminiSSE(t *testing.T) {
	stream := `data: {"candidates":[{"content":{"parts":[{"text":"hel"}]}}]}

data: {"candidates":[{"content":{"parts":[{"text":"lo"}]}}]}

data: [DONE]
`
	var chunks []string
	full, _, _, err := parseGeminiSSE(strings.NewReader(stream), func(s string) {
		chunks = append(chunks, s)
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if full != "hello" {
		t.Errorf("assembled: got %q, want hello", full)
	}
	if len(chunks) != 2 || chunks[0] != "hel" || chunks[1] != "lo" {
		t.Errorf("chunks: %#v", chunks)
	}
}

// TestParseGeminiSSE_Usage verifies the parser surfaces usageMetadata token
// counts from the final streamed chunk.
func TestParseGeminiSSE_Usage(t *testing.T) {
	stream := `data: {"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}

data: {"candidates":[{"content":{"parts":[{"text":"!"}]}}],"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":5,"totalTokenCount":16}}

data: [DONE]
`
	full, prompt, completion, err := parseGeminiSSE(strings.NewReader(stream), nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if full != "hi!" {
		t.Errorf("text = %q", full)
	}
	if prompt != 11 || completion != 5 {
		t.Errorf("usage = (prompt=%d,completion=%d), want (11,5)", prompt, completion)
	}
}

// TestGemini_Endpoint sanity-checks the URL builder.
func TestGemini_Endpoint(t *testing.T) {
	g := NewGemini(GeminiConfig{
		APIKey: "k", BaseURL: "https://example.com/v1beta", Model: "gemini-2.5-flash",
	})
	u, err := g.endpoint("generateContent", false)
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	if !strings.HasPrefix(u, "https://example.com/v1beta/models/gemini-2.5-flash:generateContent?") {
		t.Errorf("URL shape wrong: %s", u)
	}
	if !strings.Contains(u, "key=k") {
		t.Errorf("missing key: %s", u)
	}
	if strings.Contains(u, "alt=sse") {
		t.Errorf("unexpected alt=sse on non-stream call: %s", u)
	}

	su, err := g.endpoint("streamGenerateContent", true)
	if err != nil {
		t.Fatalf("stream endpoint: %v", err)
	}
	if !strings.Contains(su, "alt=sse") {
		t.Errorf("stream URL missing alt=sse: %s", su)
	}
}

// Sanity: GeminiResponse decodes a typical response.
func TestGeminiResponse_Decode(t *testing.T) {
	in := []byte(`{"candidates":[{"content":{"parts":[{"text":"a"},{"text":"b"}]},"finishReason":"STOP"}]}`)
	var out geminiResponse
	if err := json.Unmarshal(in, &out); err != nil {
		t.Fatal(err)
	}
	if got := geminiCollectText(out); got != "ab" {
		t.Errorf("collect: %q", got)
	}
}

// TestGemini_FunctionCallFlattening covers the regression where
// Gemini emits a native functionCall part instead of text — without
// translation, the chat agent saw an empty response. The text we
// produce must be parseable by agentctx.parseToolCall.
func TestGemini_FunctionCallFlattening(t *testing.T) {
	in := []byte(`{"candidates":[{"content":{"parts":[
		{"functionCall":{"name":"jira_search","args":{"jql":"project = ENG","limit":20}}}
	]},"finishReason":"STOP"}]}`)
	var out geminiResponse
	if err := json.Unmarshal(in, &out); err != nil {
		t.Fatal(err)
	}
	got := geminiCollectText(out)
	// Must serialize back into the action-JSON our chat agent parses.
	if !strings.Contains(got, `"action":"jira_search"`) {
		t.Errorf("missing action: %q", got)
	}
	if !strings.Contains(got, `"jql":"project = ENG"`) {
		t.Errorf("missing jql: %q", got)
	}
}

// TestGemini_EmptyResponseSurfacesError verifies the new "empty
// response with reason" path returns a useful error string rather
// than silently returning "" (which became "(no response from
// model)" in the chat handler).
func TestGemini_EmptyResponseSurfacesError(t *testing.T) {
	cases := []struct {
		name, body, wantSubstr string
	}{
		{
			name:       "max tokens",
			body:       `{"candidates":[{"content":{"parts":[]},"finishReason":"MAX_TOKENS"}]}`,
			wantSubstr: "MAX_TOKENS",
		},
		{
			name:       "safety",
			body:       `{"candidates":[{"content":{"parts":[]},"finishReason":"SAFETY"}]}`,
			wantSubstr: "safety",
		},
		{
			name:       "prompt blocked",
			body:       `{"promptFeedback":{"blockReason":"PROHIBITED_CONTENT"}}`,
			wantSubstr: "PROHIBITED_CONTENT",
		},
		{
			name:       "empty STOP",
			body:       `{"candidates":[{"content":{"parts":[]},"finishReason":"STOP"}]}`,
			wantSubstr: "finishReason=STOP",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			g := NewGemini(GeminiConfig{
				APIKey: "k", BaseURL: srv.URL, Model: "test",
			})
			_, err := g.Generate(context.Background(),
				[]Message{{Role: RoleUser, Content: "x"}}, Options{})
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.wantSubstr)) {
				t.Errorf("error %q missing %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}
