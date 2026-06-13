package llm

import (
	"os"
	"strings"
)

// Per-role model routing.
//
// When several providers are configured, different parts of goon can use
// different models: a strong model to write code, a cheap/fast one to
// chat, a mid one to plan, etc. Each role reads an env var of the form
// GOON_LLM_<ROLE> whose value is a spec:
//
//	provider:model   e.g. "anthropic:claude-opus-4-1"   (provider + model)
//	provider         e.g. "gemini"                      (provider, its default model)
//	model            e.g. "gpt-4o"                       (default provider, this model)
//
// When the role var is unset/empty, the role falls back to the global
// default (GOON_LLM_PROVIDER + that provider's *_MODEL), i.e. NewFromEnv.
const (
	RoleChat   = "chat"   // GOON_LLM_CHAT   — the conversational agent
	RolePlan   = "plan"   // GOON_LLM_PLAN   — ticket triage / planning
	RoleCode   = "code"   // GOON_LLM_CODE   — the execute agent (writes code)
	RoleReview = "review" // GOON_LLM_REVIEW — verify + PR review drafts
)

// Roles is the set of routable roles, for config/docs enumeration.
var Roles = []string{RoleChat, RolePlan, RoleCode, RoleReview}

// roleEnv maps a role to its env var, e.g. "chat" -> "GOON_LLM_CHAT".
func roleEnv(role string) string {
	return "GOON_LLM_" + strings.ToUpper(strings.TrimSpace(role))
}

// knownProviders are the provider names a role spec may name directly.
var knownProviders = map[string]bool{
	"openai": true, "anthropic": true, "gemini": true,
	"google": true, "ollama": true, "mock": true,
}

// parseRoleSpec splits a role spec into (provider, model). Either may be
// empty (meaning "inherit from the global default"). Rules:
//   - "p:m"           -> provider p, model m
//   - "p" (known)     -> provider p, default model
//   - "m" (not known) -> default provider, model m
func parseRoleSpec(spec string) (provider, model string) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return "", ""
	}
	if i := strings.IndexByte(spec, ':'); i >= 0 {
		return strings.TrimSpace(spec[:i]), strings.TrimSpace(spec[i+1:])
	}
	if knownProviders[strings.ToLower(spec)] {
		return spec, ""
	}
	return "", spec
}

// NewForRole builds the Provider for a role, honoring GOON_LLM_<ROLE> and
// falling back to NewFromEnv when the role is not configured.
func NewForRole(role string) (Provider, error) {
	provider, model := parseRoleSpec(os.Getenv(roleEnv(role)))
	if provider == "" && model == "" {
		return NewFromEnv()
	}
	return NewWithOverrides(provider, model)
}

// NewForRoleOr returns the role's Provider, or fallback when the role is
// not configured OR fails to build (so a typo'd role var degrades to the
// already-working default instead of breaking the feature). Pass the
// process's default provider as fallback.
func NewForRoleOr(role string, fallback Provider) Provider {
	spec := strings.TrimSpace(os.Getenv(roleEnv(role)))
	if spec == "" {
		return fallback
	}
	provider, model := parseRoleSpec(spec)
	p, err := NewWithOverrides(provider, model)
	if err != nil || p == nil {
		return fallback
	}
	return p
}
