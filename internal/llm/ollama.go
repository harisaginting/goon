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

// OllamaConfig configures the Ollama HTTP client.
type OllamaConfig struct {
	BaseURL string // e.g. http://localhost:11434
	Model   string // e.g. llama3, qwen2.5-coder, mistral
	HTTP    *http.Client
}

// Ollama implements Provider against /api/chat on a local Ollama server.
type Ollama struct {
	cfg  OllamaConfig
	http *http.Client
}

// NewOllama constructs an Ollama provider.
func NewOllama(cfg OllamaConfig) *Ollama {
	hc := cfg.HTTP
	if hc == nil {
		// Local models can be slow on first inference — give them headroom.
		hc = &http.Client{Timeout: 120 * time.Second}
	}
	return &Ollama{cfg: cfg, http: hc}
}

// Name returns "ollama".
func (o *Ollama) Name() string { return "ollama" }

type ollamaChatRequest struct {
	Model     string         `json:"model"`
	Messages  []Message      `json:"messages"`
	Stream    bool           `json:"stream"`
	Format    string         `json:"format,omitempty"`
	Options   map[string]any `json:"options,omitempty"`
	KeepAlive string         `json:"keep_alive,omitempty"`
}

type ollamaChatResponse struct {
	Model   string  `json:"model"`
	Message Message `json:"message"`
	Done    bool    `json:"done"`
	Error   string  `json:"error,omitempty"`
}

// Generate calls /api/chat (non-streaming).
func (o *Ollama) Generate(ctx context.Context, messages []Message, opts Options) (string, error) {
	body := ollamaChatRequest{
		Model:    o.cfg.Model,
		Messages: messages,
		Stream:   false,
	}
	if opts.JSONMode {
		body.Format = "json"
	}
	if opts.Temperature > 0 {
		body.Options = map[string]any{"temperature": opts.Temperature}
		if opts.MaxTokens > 0 {
			body.Options["num_predict"] = opts.MaxTokens
		}
	} else if opts.MaxTokens > 0 {
		body.Options = map[string]any{"num_predict": opts.MaxTokens}
	}

	buf, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	url := strings.TrimRight(o.cfg.BaseURL, "/") + "/api/chat"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("ollama: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("ollama http %d: %s", resp.StatusCode, truncate(string(raw), 400))
	}

	var cr ollamaChatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return "", fmt.Errorf("ollama decode: %w", err)
	}
	if cr.Error != "" {
		return "", fmt.Errorf("ollama error: %s", cr.Error)
	}
	if cr.Message.Content == "" {
		return "", errors.New("ollama: empty message content")
	}
	return cr.Message.Content, nil
}

// Stream calls /api/chat with stream=true. Ollama returns NDJSON, one chunk per
// line — we forward each chunk's message.content to onChunk and assemble the
// full response.
func (o *Ollama) Stream(ctx context.Context, messages []Message, opts Options, onChunk func(string)) (string, error) {
	body := ollamaChatRequest{
		Model:    o.cfg.Model,
		Messages: messages,
		Stream:   true,
	}
	if opts.JSONMode {
		body.Format = "json"
	}
	if opts.Temperature > 0 || opts.MaxTokens > 0 {
		body.Options = map[string]any{}
		if opts.Temperature > 0 {
			body.Options["temperature"] = opts.Temperature
		}
		if opts.MaxTokens > 0 {
			body.Options["num_predict"] = opts.MaxTokens
		}
	}

	buf, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	url := strings.TrimRight(o.cfg.BaseURL, "/") + "/api/chat"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("ollama: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("ollama stream http %d: %s", resp.StatusCode, truncate(string(raw), 400))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var assembled strings.Builder
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ch ollamaChatResponse
		if err := json.Unmarshal([]byte(line), &ch); err != nil {
			continue
		}
		if ch.Error != "" {
			return assembled.String(), fmt.Errorf("ollama stream: %s", ch.Error)
		}
		if ch.Message.Content != "" {
			assembled.WriteString(ch.Message.Content)
			if onChunk != nil {
				onChunk(ch.Message.Content)
			}
		}
		if ch.Done {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return assembled.String(), err
	}
	return assembled.String(), nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
