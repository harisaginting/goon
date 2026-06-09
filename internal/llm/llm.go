// Package llm provides a provider-agnostic LLM interface.
//
// The agent only depends on the Provider interface — Generate (one-shot) and
// Stream (incremental). New providers (OpenAI, Anthropic, mock) implement the
// interface. The active provider is selected by GOON_LLM_PROVIDER.
package llm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Role constants used in Message.Role.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Message is a single chat turn.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Options tunes a single Generate / Stream call.
type Options struct {
	Temperature float64
	MaxTokens   int
	JSONMode    bool // request strict JSON when supported
	Stream      bool
}

// Provider is the abstract LLM backend.
type Provider interface {
	Name() string
	Generate(ctx context.Context, messages []Message, opts Options) (string, error)
	Stream(ctx context.Context, messages []Message, opts Options, onChunk func(string)) (string, error)
}

// NewFromEnv builds a Provider from environment variables.
func NewFromEnv() (Provider, error) {
	name := strings.ToLower(strings.TrimSpace(os.Getenv("GOON_LLM_PROVIDER")))
	if name == "" {
		name = "openai"
	}
	switch name {
	case "openai":
		key := os.Getenv("OPENAI_API_KEY")
		if key == "" {
			return nil, errors.New("OPENAI_API_KEY is not set")
		}
		base := envOr("OPENAI_BASE_URL", "https://api.openai.com/v1")
		model := envOr("OPENAI_MODEL", "gpt-4o-mini")
		return NewOpenAI(OpenAIConfig{APIKey: key, BaseURL: base, Model: model}), nil
	case "anthropic":
		key := os.Getenv("ANTHROPIC_API_KEY")
		if key == "" {
			return nil, errors.New("ANTHROPIC_API_KEY is not set")
		}
		base := envOr("ANTHROPIC_BASE_URL", "https://api.anthropic.com/v1")
		model := envOr("ANTHROPIC_MODEL", "claude-sonnet-4-5")
		return NewAnthropic(AnthropicConfig{APIKey: key, BaseURL: base, Model: model}), nil
	case "ollama":
		base := envOr("OLLAMA_BASE_URL", "http://localhost:11434")
		model := envOr("OLLAMA_MODEL", "llama3")
		return NewOllama(OllamaConfig{BaseURL: base, Model: model}), nil
	case "gemini", "google":
		// GEMINI_API_KEY is the canonical env name; GOOGLE_API_KEY
		// is the fallback because the same key is used by Google's
		// other AI surfaces and many users export it as that.
		key := envOr("GEMINI_API_KEY", os.Getenv("GOOGLE_API_KEY"))
		if key == "" {
			return nil, errors.New("GEMINI_API_KEY (or GOOGLE_API_KEY) is not set")
		}
		base := envOr("GEMINI_BASE_URL", "https://generativelanguage.googleapis.com/v1beta")
		model := envOr("GEMINI_MODEL", "gemini-2.5-flash")
		return NewGemini(GeminiConfig{APIKey: key, BaseURL: base, Model: model}), nil
	case "mock":
		// Optionally seed replies via GOON_MOCK_REPLIES, separated by "<|>".
		// Each entry should be a complete JSON ToolCall.
		raw := os.Getenv("GOON_MOCK_REPLIES")
		var replies []string
		if raw != "" {
			for _, p := range strings.Split(raw, "<|>") {
				if s := strings.TrimSpace(p); s != "" {
					replies = append(replies, s)
				}
			}
		}
		return NewMock(replies), nil
	default:
		return nil, fmt.Errorf("unknown GOON_LLM_PROVIDER %q (want openai|anthropic|gemini|ollama|mock)", name)
	}
}

// NewWithOverrides builds a Provider where provider and model override the
// corresponding environment variables. Pass empty strings to inherit from env.
// When both are empty this is equivalent to NewFromEnv().
//
// Typical usage in the workflow runner: per-stage provider/model fields.
func NewWithOverrides(provider, model string) (Provider, error) {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)

	name := provider
	if name == "" {
		name = strings.ToLower(strings.TrimSpace(os.Getenv("GOON_LLM_PROVIDER")))
		if name == "" {
			name = "openai"
		}
	}

	switch strings.ToLower(name) {
	case "openai":
		key := os.Getenv("OPENAI_API_KEY")
		if key == "" {
			return nil, errors.New("OPENAI_API_KEY is not set")
		}
		base := envOr("OPENAI_BASE_URL", "https://api.openai.com/v1")
		m := envOr("OPENAI_MODEL", "gpt-4o-mini")
		if model != "" {
			m = model
		}
		return NewOpenAI(OpenAIConfig{APIKey: key, BaseURL: base, Model: m}), nil
	case "anthropic":
		key := os.Getenv("ANTHROPIC_API_KEY")
		if key == "" {
			return nil, errors.New("ANTHROPIC_API_KEY is not set")
		}
		base := envOr("ANTHROPIC_BASE_URL", "https://api.anthropic.com/v1")
		m := envOr("ANTHROPIC_MODEL", "claude-sonnet-4-5")
		if model != "" {
			m = model
		}
		return NewAnthropic(AnthropicConfig{APIKey: key, BaseURL: base, Model: m}), nil
	case "ollama":
		base := envOr("OLLAMA_BASE_URL", "http://localhost:11434")
		m := envOr("OLLAMA_MODEL", "llama3")
		if model != "" {
			m = model
		}
		return NewOllama(OllamaConfig{BaseURL: base, Model: m}), nil
	case "gemini", "google":
		key := envOr("GEMINI_API_KEY", os.Getenv("GOOGLE_API_KEY"))
		if key == "" {
			return nil, errors.New("GEMINI_API_KEY (or GOOGLE_API_KEY) is not set")
		}
		base := envOr("GEMINI_BASE_URL", "https://generativelanguage.googleapis.com/v1beta")
		m := envOr("GEMINI_MODEL", "gemini-2.5-flash")
		if model != "" {
			m = model
		}
		return NewGemini(GeminiConfig{APIKey: key, BaseURL: base, Model: m}), nil
	case "mock":
		return NewMock(nil), nil
	default:
		return nil, fmt.Errorf("unknown provider %q (want openai|anthropic|gemini|ollama|mock)", name)
	}
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
