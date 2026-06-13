package llm

import "testing"

func TestParseRoleSpec(t *testing.T) {
	cases := []struct {
		in       string
		provider string
		model    string
	}{
		{"", "", ""},
		{"  ", "", ""},
		{"anthropic:claude-opus-4-1", "anthropic", "claude-opus-4-1"},
		{"ollama:llama3", "ollama", "llama3"},
		{"gemini", "gemini", ""},        // known provider, default model
		{"openai", "openai", ""},        // known provider
		{"gpt-4o", "", "gpt-4o"},        // bare model on default provider
		{"grok-3", "", "grok-3"},        // bare model
		{":gpt-4o-mini", "", "gpt-4o-mini"}, // explicit "default provider, this model"
		{"anthropic:", "anthropic", ""}, // provider, empty model
	}
	for _, c := range cases {
		p, m := parseRoleSpec(c.in)
		if p != c.provider || m != c.model {
			t.Errorf("parseRoleSpec(%q) = (%q,%q), want (%q,%q)", c.in, p, m, c.provider, c.model)
		}
	}
}

func TestRoleEnv(t *testing.T) {
	if got := roleEnv("chat"); got != "GOON_LLM_CHAT" {
		t.Errorf("roleEnv(chat) = %q", got)
	}
	if got := roleEnv("Code"); got != "GOON_LLM_CODE" {
		t.Errorf("roleEnv(Code) = %q", got)
	}
}

func TestNewForRoleOr_FallsBackWhenUnset(t *testing.T) {
	t.Setenv("GOON_LLM_CHAT", "")
	fallback := NewMock(nil)
	if got := NewForRoleOr(RoleChat, fallback); got != fallback {
		t.Error("unset role should return the fallback provider")
	}
}

func TestNewForRoleOr_FallsBackOnBadProvider(t *testing.T) {
	t.Setenv("GOON_LLM_CODE", "nosuchprovider:x")
	fallback := NewMock(nil)
	if got := NewForRoleOr(RoleCode, fallback); got != fallback {
		t.Error("unbuildable role should degrade to the fallback")
	}
}
