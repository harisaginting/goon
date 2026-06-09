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

	"github.com/harisaginting/goon/internal/logx"
	"github.com/harisaginting/goon/internal/usage"
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
		hc = logx.InstrumentClient("openai", &http.Client{Timeout: httpTimeout()})
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
	StreamOpts  *streamOptions  `json:"stream_options,omitempty"`
	ResponseFmt *map[string]any `json:"response_format,omitempty"`
}

// streamOptions asks the API to emit a final usage chunk on streamed
// responses (otherwise streaming returns no token counts).
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openAIChatResponse struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
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
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Generate calls /chat/completions with retry and timeout.
func (o *OpenAI) Generate(ctx context.Context, messages []Message, opts Options) (string, error) {
	actID := usage.StartActivity(usage.LabelFrom(ctx), o.cfg.Model)
	defer usage.EndActivity(actID)
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
	buf, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	url := strings.TrimRight(o.cfg.BaseURL, "/") + "/chat/completions"

	resp, err := doWithRetry(ctx, o.http, func() (*http.Request, error) {
		req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
		if rerr != nil {
			return nil, rerr
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+o.cfg.APIKey)
		return req, nil
	}, 3)
	if err != nil {
		return "", fmt.Errorf("openai: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
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
	usage.Global().Record(o.cfg.Model, cr.Usage.PromptTokens, cr.Usage.CompletionTokens)
	return cr.Choices[0].Message.Content, nil
}

// Stream calls /chat/completions with stream=true and writes incremental
// content to onChunk. The full assembled string is also returned.
func (o *OpenAI) Stream(ctx context.Context, messages []Message, opts Options, onChunk func(string)) (string, error) {
	actID := usage.StartActivity(usage.LabelFrom(ctx), o.cfg.Model)
	defer usage.EndActivity(actID)
	body := openAIChatRequest{
		Model:       o.cfg.Model,
		Messages:    messages,
		Temperature: opts.Temperature,
		MaxTokens:   opts.MaxTokens,
		Stream:      true,
		StreamOpts:  &streamOptions{IncludeUsage: true},
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
		// The final chunk (with stream_options.include_usage) carries token
		// counts and an empty choices array.
		if ch.Usage != nil {
			usage.Global().Record(o.cfg.Model, ch.Usage.PromptTokens, ch.Usage.CompletionTokens)
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
