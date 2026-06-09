package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/harisaginting/goon/internal/logx"
	"github.com/harisaginting/goon/internal/usage"
)

// GeminiConfig configures the Google Gemini provider against the
// `generativelanguage.googleapis.com` v1beta REST surface.
//
// Defaults (when fields are empty / NewFromEnv path):
//   - BaseURL: "https://generativelanguage.googleapis.com/v1beta"
//   - Model:   "gemini-2.5-flash"
//
// Auth is via API key passed as the `key` query parameter (Google's
// public API style). No OAuth — keep it simple and stdlib-only.
type GeminiConfig struct {
	APIKey  string
	BaseURL string
	Model   string
	HTTP    *http.Client
}

// Gemini implements Provider against Google's generativelanguage API.
type Gemini struct {
	cfg  GeminiConfig
	http *http.Client
}

// NewGemini constructs a Gemini provider with sensible HTTP defaults.
// The instrumented client logs every request to ./storage/logs/goon.log
// like every other provider in this package — keep it that way.
func NewGemini(cfg GeminiConfig) *Gemini {
	hc := cfg.HTTP
	if hc == nil {
		hc = logx.InstrumentClient("gemini", &http.Client{Timeout: httpTimeout()})
	}
	return &Gemini{cfg: cfg, http: hc}
}

// Name returns "gemini" so the doctor / status panels can identify it.
func (g *Gemini) Name() string { return "gemini" }

// --- Wire types ----------------------------------------------------------
//
// Gemini's generateContent / streamGenerateContent payload shape:
//
//	{
//	  "system_instruction": { "parts": [{"text": "..."}] },
//	  "contents": [
//	    { "role": "user",  "parts": [{"text": "..."}] },
//	    { "role": "model", "parts": [{"text": "..."}] },
//	    ...
//	  ],
//	  "generationConfig": {
//	    "temperature": 0.4,
//	    "maxOutputTokens": 1024,
//	    "responseMimeType": "application/json"   // for JSON mode
//	  }
//	}
//
// Notes:
//   - role "system" doesn't exist as a turn role; the system prompt
//     goes into `system_instruction`. We collect all system messages
//     into one block, identical to how the Anthropic adapter does it.
//   - Assistant role is `model`, not `assistant`.
//   - Tool/function messages get folded into a user turn — Gemini's
//     function-calling protocol differs from OpenAI's and we don't
//     use it yet (the chat agent's tool protocol is JSON-on-stdout).

// geminiPart represents one chunk of a content block. The Gemini API
// can return text parts AND structured function-call parts in the
// same response, and the model sometimes emits a functionCall part
// even when the request didn't declare any tools (because the system
// prompt mentions tool names like "jira_search"). We decode both
// shapes; geminiCollectText flattens functionCalls into our text-based
// tool-call wire format so the chat agent parser sees them too.
type geminiPart struct {
	Text         string              `json:"text,omitempty"`
	FunctionCall *geminiFunctionCall `json:"functionCall,omitempty"`
}

// geminiFunctionCall mirrors Gemini's native function-call shape.
// We translate it back to our JSON-on-stdout protocol in
// geminiCollectText so the chat agent parser handles it uniformly
// with text-emitted tool calls.
type geminiFunctionCall struct {
	Name string                 `json:"name"`
	Args map[string]interface{} `json:"args"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"` // "user" | "model"
	Parts []geminiPart `json:"parts"`
}

type geminiSystemInstruction struct {
	Parts []geminiPart `json:"parts"`
}

type geminiGenConfig struct {
	Temperature      float64 `json:"temperature,omitempty"`
	MaxOutputTokens  int     `json:"maxOutputTokens,omitempty"`
	ResponseMimeType string  `json:"responseMimeType,omitempty"`
}

type geminiRequest struct {
	SystemInstruction *geminiSystemInstruction `json:"system_instruction,omitempty"`
	Contents          []geminiContent          `json:"contents"`
	GenerationConfig  *geminiGenConfig         `json:"generationConfig,omitempty"`
}

type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []geminiPart `json:"parts"`
		} `json:"content"`
		// FinishReason carries Gemini's reason for stopping. Common
		// values: "STOP" (normal), "MAX_TOKENS" (hit the cap),
		// "SAFETY" (blocked by safety filters), "RECITATION"
		// (blocked due to recitation match), "OTHER" (unknown).
		FinishReason string `json:"finishReason,omitempty"`
	} `json:"candidates"`
	// PromptFeedback surfaces upstream blocks (entire prompt
	// rejected before the model even sees it).
	PromptFeedback *struct {
		BlockReason string `json:"blockReason,omitempty"`
	} `json:"promptFeedback,omitempty"`
	// Error envelope from Google API errors.
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error,omitempty"`
	// UsageMetadata carries token counts for the request + response.
	UsageMetadata *struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		TotalTokenCount      int `json:"totalTokenCount"`
	} `json:"usageMetadata,omitempty"`
}

// Generate sends one non-streaming request to generateContent.
func (g *Gemini) Generate(ctx context.Context, messages []Message, opts Options) (string, error) {
	actID := usage.StartActivity(usage.LabelFrom(ctx), g.cfg.Model)
	defer usage.EndActivity(actID)
	body, err := g.buildRequest(messages, opts)
	if err != nil {
		return "", err
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	u, err := g.endpoint("generateContent", false)
	if err != nil {
		return "", err
	}
	resp, err := doWithRetry(ctx, g.http, func() (*http.Request, error) {
		req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(buf))
		if rerr != nil {
			return nil, rerr
		}
		req.Header.Set("Content-Type", "application/json")
		return req, nil
	}, 3)
	if err != nil {
		return "", fmt.Errorf("gemini: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gemini http %d: %s", resp.StatusCode, string(raw))
	}
	var out geminiResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("gemini decode: %w", err)
	}
	if out.Error != nil {
		return "", fmt.Errorf("gemini error %s: %s", out.Error.Status, out.Error.Message)
	}
	if out.UsageMetadata != nil {
		usage.Global().Record(g.cfg.Model, out.UsageMetadata.PromptTokenCount, out.UsageMetadata.CandidatesTokenCount)
	}
	text := geminiCollectText(out)
	// Surface common "empty response with a reason" failures as
	// errors instead of silently returning "". Without this, callers
	// see "(no response from model)" and have no idea why. Also dump
	// the raw response body to the structured log so we can debug
	// new failure modes when Gemini changes its response shape.
	if text == "" {
		logx.Warn("gemini empty response",
			"model", g.cfg.Model,
			"body", string(raw),
		)
		if out.PromptFeedback != nil && out.PromptFeedback.BlockReason != "" {
			return "", fmt.Errorf("gemini: prompt blocked (%s)", out.PromptFeedback.BlockReason)
		}
		if len(out.Candidates) > 0 {
			fr := out.Candidates[0].FinishReason
			switch fr {
			case "MAX_TOKENS":
				return "", fmt.Errorf("gemini: response truncated (MAX_TOKENS) — try raising the cap; gemini-2.5 spends tokens on internal thinking before answering")
			case "SAFETY":
				return "", fmt.Errorf("gemini: response blocked by safety filter")
			case "RECITATION":
				return "", fmt.Errorf("gemini: response blocked (RECITATION)")
			case "", "STOP":
				// Genuinely empty answer — model finished cleanly
				// with no text. Surface this too so the user sees
				// *something* useful instead of "(no response)".
				return "", fmt.Errorf("gemini: empty response (finishReason=STOP, content had %d candidate(s) with %d part(s)) — check ./storage/logs/goon.log for the raw body",
					len(out.Candidates),
					func() int {
						if len(out.Candidates) == 0 {
							return 0
						}
						return len(out.Candidates[0].Content.Parts)
					}(),
				)
			default:
				return "", fmt.Errorf("gemini: empty response (finishReason=%s)", fr)
			}
		}
		// No candidates at all — surface that too.
		return "", fmt.Errorf("gemini: empty response (no candidates) — check ./storage/logs/goon.log")
	}
	return text, nil
}

// Stream calls streamGenerateContent with ?alt=sse and parses Google's
// SSE-style stream. Each event is a complete geminiResponse fragment;
// we extract text from candidates[].content.parts[] and emit it via
// onChunk. The fully assembled string is also returned for callers
// that ignore onChunk.
//
// Like the Anthropic adapter we do NOT wrap streaming in doWithRetry —
// replaying a partial stream would double-emit chunks.
func (g *Gemini) Stream(ctx context.Context, messages []Message, opts Options, onChunk func(string)) (string, error) {
	actID := usage.StartActivity(usage.LabelFrom(ctx), g.cfg.Model)
	defer usage.EndActivity(actID)
	body, err := g.buildRequest(messages, opts)
	if err != nil {
		return "", err
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	u, err := g.endpoint("streamGenerateContent", true)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := g.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("gemini stream: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("gemini stream http %d: %s", resp.StatusCode, string(raw))
	}
	text, promptTok, compTok, perr := parseGeminiSSE(resp.Body, onChunk)
	if promptTok > 0 || compTok > 0 {
		usage.Global().Record(g.cfg.Model, promptTok, compTok)
	}
	return text, perr
}

// endpoint builds the request URL for the configured model and method.
// Google's URL format is:
//
//	{base}/models/{model}:{method}?key={APIKey}[&alt=sse]
func (g *Gemini) endpoint(method string, stream bool) (string, error) {
	if g.cfg.APIKey == "" {
		return "", errors.New("gemini: API key is empty")
	}
	if g.cfg.Model == "" {
		return "", errors.New("gemini: model is empty")
	}
	base := strings.TrimRight(g.cfg.BaseURL, "/")
	if base == "" {
		base = "https://generativelanguage.googleapis.com/v1beta"
	}
	q := url.Values{}
	q.Set("key", g.cfg.APIKey)
	if stream {
		q.Set("alt", "sse")
	}
	return fmt.Sprintf("%s/models/%s:%s?%s",
		base,
		url.PathEscape(g.cfg.Model),
		method,
		q.Encode(),
	), nil
}

// buildRequest converts goon's Message stream into Gemini's
// system_instruction + contents shape. Mirrors splitSystem() from
// anthropic.go in spirit but for Gemini's role names.
func (g *Gemini) buildRequest(messages []Message, opts Options) (geminiRequest, error) {
	var sys strings.Builder
	contents := make([]geminiContent, 0, len(messages))
	for _, m := range messages {
		switch m.Role {
		case RoleSystem:
			if sys.Len() > 0 {
				sys.WriteString("\n\n")
			}
			sys.WriteString(m.Content)
		case RoleAssistant:
			contents = append(contents, geminiContent{
				Role:  "model",
				Parts: []geminiPart{{Text: m.Content}},
			})
		case RoleTool:
			// Gemini's function-calling API isn't wired here; surface
			// tool results as user turns so the model still sees them.
			contents = append(contents, geminiContent{
				Role:  "user",
				Parts: []geminiPart{{Text: m.Content}},
			})
		default: // RoleUser and anything else
			contents = append(contents, geminiContent{
				Role:  "user",
				Parts: []geminiPart{{Text: m.Content}},
			})
		}
	}
	req := geminiRequest{Contents: contents}
	if sys.Len() > 0 {
		req.SystemInstruction = &geminiSystemInstruction{
			Parts: []geminiPart{{Text: sys.String()}},
		}
	}
	// Default budget: 8192. Gemini 2.5 spends invisible "thinking"
	// tokens before answering, and an 800/4096-token cap regularly
	// produced empty replies. We previously tried thinkingBudget=0
	// but that broke output entirely on some 2.5-Pro variants.
	// Approach now: let the model think and give it plenty of room.
	maxTok := opts.MaxTokens
	if maxTok == 0 {
		maxTok = 8192
	}
	gc := geminiGenConfig{
		Temperature:     opts.Temperature,
		MaxOutputTokens: maxTok,
	}
	if opts.JSONMode {
		gc.ResponseMimeType = "application/json"
	}
	req.GenerationConfig = &gc
	return req, nil
}

// geminiCollectText concatenates every part across all candidates,
// flattening native function-call parts into our text-based tool-call
// wire format. The chat agent's parser sees both forms uniformly.
//
// Why translate functionCall → JSON text? Gemini 2.5 sometimes emits
// a functionCall part for any tool name it sees mentioned in the
// system prompt, even when we didn't declare a `tools` array in the
// request. Without this translation those calls disappear into the
// void and the user sees "(no response)".
func geminiCollectText(out geminiResponse) string {
	var b strings.Builder
	for _, cand := range out.Candidates {
		for _, p := range cand.Content.Parts {
			if p.Text != "" {
				b.WriteString(p.Text)
			}
			if p.FunctionCall != nil {
				// Serialize as the chat agent's tool-call wire format:
				// {"action":"<name>", ...args}
				obj := map[string]interface{}{"action": p.FunctionCall.Name}
				for k, v := range p.FunctionCall.Args {
					obj[k] = v
				}
				if blob, err := json.Marshal(obj); err == nil {
					if b.Len() > 0 {
						b.WriteString("\n")
					}
					b.Write(blob)
				}
			}
		}
	}
	return b.String()
}

// parseGeminiSSE consumes Google's SSE stream and forwards text deltas
// to onChunk. Each `data:` line carries a full geminiResponse-shaped
// JSON object whose candidates[].content.parts[] holds the new text
// for that step. Unlike Anthropic, Gemini doesn't use a separate
// delta type — the parts are the increment directly.
//
// Split out so it's unit-testable against an io.Reader without a live
// HTTP server.
func parseGeminiSSE(r io.Reader, onChunk func(string)) (text string, promptTokens, completionTokens int, err error) {
	br := bufio.NewReader(r)
	var full strings.Builder
	for {
		line, rerr := br.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				continue // event boundary
			}
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(line[len("data:"):])
			if payload == "" || payload == "[DONE]" {
				continue
			}
			var evt geminiResponse
			if jerr := json.Unmarshal([]byte(payload), &evt); jerr != nil {
				continue
			}
			if evt.Error != nil {
				return full.String(), promptTokens, completionTokens, fmt.Errorf("gemini stream error %s: %s", evt.Error.Status, evt.Error.Message)
			}
			// usageMetadata appears on chunks (cumulative); the final chunk
			// carries the totals.
			if evt.UsageMetadata != nil {
				promptTokens = evt.UsageMetadata.PromptTokenCount
				completionTokens = evt.UsageMetadata.CandidatesTokenCount
			}
			chunk := geminiCollectText(evt)
			if chunk != "" {
				full.WriteString(chunk)
				if onChunk != nil {
					onChunk(chunk)
				}
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return full.String(), promptTokens, completionTokens, nil
			}
			return full.String(), promptTokens, completionTokens, rerr
		}
	}
}
