package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
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
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Anthropic{cfg: cfg, http: hc}
}

// Name returns "anthropic".
func (a *Anthropic) Name() string { return "anthropic" }

type anthropicMessageRequest struct {
	Model     string             `json:"model"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	MaxTokens int                `json:"max_tokens"`
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

// Generate calls /messages.
func (a *Anthropic) Generate(ctx context.Context, messages []Message, opts Options) (string, error) {
	system, msgs := splitSystem(messages)
	body := anthropicMessageRequest{
		Model:     a.cfg.Model,
		System:    system,
		Messages:  msgs,
		MaxTokens: opts.MaxTokens,
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
	req.Header.Set("x-api-key", a.cfg.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := a.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
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

// Stream falls back to non-streaming for now (the agent loop handles either).
func (a *Anthropic) Stream(ctx context.Context, messages []Message, opts Options, onChunk func(string)) (string, error) {
	out, err := a.Generate(ctx, messages, opts)
	if err == nil && onChunk != nil {
		onChunk(out)
	}
	return out, err
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
