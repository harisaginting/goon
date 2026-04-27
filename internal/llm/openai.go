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
)

// OpenAIConfig configures the OpenAI Chat Completions client.
type OpenAIConfig struct {
	APIKey  string
	BaseURL string // e.g. https://api.openai.com/v1
	Model   string // e.g. gpt-4o-mini
	HTTP    *http.Client
}

// OpenAI implements Provider against /v1/chat/completions.
type OpenAI struct {
	cfg  OpenAIConfig
	http *http.Client
}

// NewOpenAI constructs an OpenAI provider.
func NewOpenAI(cfg OpenAIConfig) *OpenAI {
	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &OpenAI{cfg: cfg, http: hc}
}

// Name returns "openai".
func (o *OpenAI) Name() string { return "openai" }

type openAIChatRequest struct {
	Model       string          `json:"model"`
	Messages    []Message       `json:"messages"`
	Temperature float64         `json:"temperature,omitempty"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
	ResponseFmt *map[string]any `json:"response_format,omitempty"`
}

type openAIChatResponse struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

type openAIStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Generate calls /chat/completions with retry and timeout.
func (o *OpenAI) Generate(ctx context.Context, messages []Message, opts Options) (string, error) {
	body := openAIChatRequest{
		Model:       o.cfg.Model,
		Messages:    messages,
		Temperature: opts.Temperature,
		MaxTokens:   opts.MaxTokens,
	}
	if opts.JSONMode {
		jf := map[string]any{"type": "json_object"}
		body.ResponseFmt = &jf
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
			}
		}
		out, err := o.doRequest(ctx, body)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if !isRetryable(err) {
			break
		}
	}
	return "", lastErr
}

func (o *OpenAI) doRequest(ctx context.Context, body openAIChatRequest) (string, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	url := strings.TrimRight(o.cfg.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o.cfg.APIKey)

	resp, err := o.http.Do(req)
	if err != nil {
		return "", &retryableError{err: err}
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode == 429 || resp.StatusCode >= 500 {
		return "", &retryableError{err: fmt.Errorf("openai http %d: %s", resp.StatusCode, string(raw))}
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("openai http %d: %s", resp.StatusCode, string(raw))
	}

	var cr openAIChatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return "", fmt.Errorf("openai decode: %w", err)
	}
	if cr.Error != nil {
		return "", fmt.Errorf("openai error: %s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return "", errors.New("openai: no choices in response")
	}
	return cr.Choices[0].Message.Content, nil
}

// Stream calls /chat/completions with stream=true and writes incremental
// content to onChunk. The full assembled string is also returned.
func (o *OpenAI) Stream(ctx context.Context, messages []Message, opts Options, onChunk func(string)) (string, error) {
	body := openAIChatRequest{
		Model:       o.cfg.Model,
		Messages:    messages,
		Temperature: opts.Temperature,
		MaxTokens:   opts.MaxTokens,
		Stream:      true,
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	url := strings.TrimRight(o.cfg.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o.cfg.APIKey)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := o.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("openai stream http %d: %s", resp.StatusCode, string(raw))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var assembled strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}
		var ch openAIStreamChunk
		if err := json.Unmarshal([]byte(payload), &ch); err != nil {
			continue
		}
		if ch.Error != nil {
			return assembled.String(), fmt.Errorf("openai stream: %s", ch.Error.Message)
		}
		for _, c := range ch.Choices {
			if c.Delta.Content == "" {
				continue
			}
			assembled.WriteString(c.Delta.Content)
			if onChunk != nil {
				onChunk(c.Delta.Content)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return assembled.String(), err
	}
	return assembled.String(), nil
}

type retryableError struct{ err error }

func (e *retryableError) Error() string { return e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

func isRetryable(err error) bool {
	var r *retryableError
	return errors.As(err, &r)
}
