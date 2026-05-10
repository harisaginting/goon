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
	"strings"
	"time"

	"github.com/harisaginting/goon/internal/logx"
)

// AnthropicConfig configures the Anthropic Messages API client.
type AnthropicConfig struct {
	APIKey  string
	BaseURL string
	Model   string
	HTTP    *http.Client
}

// Anthropic implements Provider against /v1/messages.
type Anthropic struct {
	cfg  AnthropicConfig
	http *http.Client
}

// NewAnthropic constructs an Anthropic provider.
func NewAnthropic(cfg AnthropicConfig) *Anthropic {
	hc := cfg.HTTP
	if hc == nil {
		hc = logx.InstrumentClient("anthropic", &http.Client{Timeout: 30 * time.Second})
	}
	return &Anthropic{cfg: cfg, http: hc}
}

// Name returns "anthropic".
func (a *Anthropic) Name() string { return "anthropic" }

type anthropicMessageRequest struct {
	Model       string             `json:"model"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	MaxTokens   int                `json:"max_tokens"`
	Temperature float64            `json:"temperature,omitempty"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicMessageResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Generate calls /messages with retry and timeout.
func (a *Anthropic) Generate(ctx context.Context, messages []Message, opts Options) (string, error) {
	system, msgs := splitSystem(messages)
	body := anthropicMessageRequest{
		Model:       a.cfg.Model,
		System:      system,
		Messages:    msgs,
		MaxTokens:   opts.MaxTokens,
		Temperature: opts.Temperature,
	}
	if body.MaxTokens == 0 {
		body.MaxTokens = 1024
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	url := strings.TrimRight(a.cfg.BaseURL, "/") + "/messages"

	resp, err := doWithRetry(ctx, a.http, func() (*http.Request, error) {
		req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
		if rerr != nil {
			return nil, rerr
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", a.cfg.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		return req, nil
	}, 3)
	if err != nil {
		return "", fmt.Errorf("anthropic: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("anthropic http %d: %s", resp.StatusCode, string(raw))
	}
	var out anthropicMessageResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("anthropic decode: %w", err)
	}
	if out.Error != nil {
		return "", fmt.Errorf("anthropic error: %s", out.Error.Message)
	}
	var b strings.Builder
	for _, c := range out.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String(), nil
}

// Stream calls /messages with stream=true and parses Anthropic's
// server-sent-event protocol, calling onChunk for each text delta. The
// final assembled string is also returned so callers that don't care
// about incrementality can ignore onChunk.
//
// Anthropic's SSE wire format (the only event types we consume):
//
//	event: content_block_delta
//	data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}
//
// Other event types (message_start / content_block_start / message_delta /
// message_stop / ping) are intentionally ignored — they don't carry user-
// visible text. Errors during the stream surface either as an HTTP non-200
// before we start parsing, or as an `event: error` block which we return
// as a Go error.
//
// Retry intentionally does NOT wrap streaming requests: replaying a
// partially-emitted stream would double-emit chunks to onChunk. Generate
// has retry; if you need resilience, prefer it.
func (a *Anthropic) Stream(ctx context.Context, messages []Message, opts Options, onChunk func(string)) (string, error) {
	system, msgs := splitSystem(messages)
	body := struct {
		Model       string             `json:"model"`
		System      string             `json:"system,omitempty"`
		Messages    []anthropicMessage `json:"messages"`
		MaxTokens   int                `json:"max_tokens"`
		Temperature float64            `json:"temperature,omitempty"`
		Stream      bool               `json:"stream"`
	}{
		Model: a.cfg.Model, System: system, Messages: msgs,
		MaxTokens: opts.MaxTokens, Temperature: opts.Temperature, Stream: true,
	}
	if body.MaxTokens == 0 {
		body.MaxTokens = 1024
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	url := strings.TrimRight(a.cfg.BaseURL, "/") + "/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("x-api-key", a.cfg.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := a.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("anthropic stream: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("anthropic stream http %d: %s", resp.StatusCode, string(raw))
	}
	return parseAnthropicSSE(resp.Body, onChunk)
}

// parseAnthropicSSE consumes lines from an Anthropic stream body and
// extracts content_block_delta text. Split out so it's unit-testable
// without a live HTTP server (any io.Reader works).
func parseAnthropicSSE(r io.Reader, onChunk func(string)) (string, error) {
	br := bufio.NewReader(r)
	var full strings.Builder
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				continue // event boundary
			}
			if !strings.HasPrefix(line, "data:") {
				continue // skip "event: ..." lines and comments
			}
			payload := strings.TrimSpace(line[len("data:"):])
			if payload == "" || payload == "[DONE]" {
				continue
			}
			var evt struct {
				Type  string `json:"type"`
				Delta struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"delta"`
				Error *struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if jerr := json.Unmarshal([]byte(payload), &evt); jerr != nil {
				continue // tolerate garbage events
			}
			if evt.Error != nil {
				return full.String(), fmt.Errorf("anthropic stream error: %s", evt.Error.Message)
			}
			if evt.Type == "content_block_delta" && evt.Delta.Type == "text_delta" && evt.Delta.Text != "" {
				full.WriteString(evt.Delta.Text)
				if onChunk != nil {
					onChunk(evt.Delta.Text)
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return full.String(), nil
			}
			return full.String(), err
		}
	}
}

func splitSystem(messages []Message) (string, []anthropicMessage) {
	var sys strings.Builder
	out := make([]anthropicMessage, 0, len(messages))
	for _, m := range messages {
		if m.Role == RoleSystem {
			if sys.Len() > 0 {
				sys.WriteString("\n\n")
			}
			sys.WriteString(m.Content)
			continue
		}
		role := m.Role
		if role == RoleTool {
			role = "user"
		}
		out = append(out, anthropicMessage{Role: role, Content: m.Content})
	}
	return sys.String(), out
}
