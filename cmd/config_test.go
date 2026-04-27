package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withConfigFile points goon's config file to a temp dir for the duration of
// the test by setting XDG_CONFIG_HOME.
func withConfigFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	return filepath.Join(dir, "goon", ".env")
}

func TestConfig_Path(t *testing.T) {
	want := withConfigFile(t)
	var out bytes.Buffer
	if err := runConfig(context.Background(), []string{"path"}, &out, &out); err != nil {
		t.Fatalf("path: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != want {
		t.Errorf("path: got %q want %q", got, want)
	}
}

func TestConfig_SetGetUnset_ValueForm(t *testing.T) {
	withConfigFile(t)
	var out bytes.Buffer

	if err := runConfig(context.Background(), []string{"set", "OPENAI_MODEL", "gpt-test"}, &out, &out); err != nil {
		t.Fatalf("set: %v", err)
	}
	out.Reset()
	if err := runConfig(context.Background(), []string{"get", "OPENAI_MODEL"}, &out, &out); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "gpt-test" {
		t.Errorf("get: got %q want gpt-test", got)
	}
	out.Reset()
	if err := runConfig(context.Background(), []string{"unset", "OPENAI_MODEL"}, &out, &out); err != nil {
		t.Fatalf("unset: %v", err)
	}
	out.Reset()
	if err := runConfig(context.Background(), []string{"get", "OPENAI_MODEL"}, &out, &out); err != nil {
		t.Fatalf("get after unset: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "" {
		t.Errorf("get after unset: got %q want empty", got)
	}
}

func TestConfig_SetEqualsForm(t *testing.T) {
	withConfigFile(t)
	var out bytes.Buffer
	if err := runConfig(context.Background(), []string{"set", "OLLAMA_MODEL=qwen2.5"}, &out, &out); err != nil {
		t.Fatalf("set =form: %v", err)
	}
	out.Reset()
	if err := runConfig(context.Background(), []string{"get", "OLLAMA_MODEL"}, &out, &out); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "qwen2.5" {
		t.Errorf("get: got %q want qwen2.5", got)
	}
}

func TestConfig_SetReplacesExisting(t *testing.T) {
	withConfigFile(t)
	var out bytes.Buffer
	_ = runConfig(context.Background(), []string{"set", "OPENAI_MODEL", "gpt-1"}, &out, &out)
	out.Reset()
	_ = runConfig(context.Background(), []string{"set", "OPENAI_MODEL", "gpt-2"}, &out, &out)
	out.Reset()
	if err := runConfig(context.Background(), []string{"get", "OPENAI_MODEL"}, &out, &out); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "gpt-2" {
		t.Errorf("expected single replacement to gpt-2, got %q", got)
	}
	// File should contain only one OPENAI_MODEL line.
	data, _ := os.ReadFile(configFilePath())
	if strings.Count(string(data), "OPENAI_MODEL=") != 1 {
		t.Fatalf("expected one OPENAI_MODEL= line, got file:\n%s", data)
	}
}

func TestConfig_ShowMasksSecrets(t *testing.T) {
	withConfigFile(t)
	var out bytes.Buffer
	_ = runConfig(context.Background(), []string{"set", "OPENAI_API_KEY", "sk-abcdef1234567890"}, &out, &out)
	out.Reset()
	if err := runConfig(context.Background(), []string{"show"}, &out, &out); err != nil {
		t.Fatalf("show: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "OPENAI_API_KEY") {
		t.Fatalf("show output missing key:\n%s", got)
	}
	if strings.Contains(got, "sk-abcdef1234567890") {
		t.Fatalf("show leaked secret:\n%s", got)
	}
	if !strings.Contains(got, "sk…890") {
		t.Logf("show masked output:\n%s", got)
		// Mask format is "first2…last3"; allow any contains-…
		if !strings.Contains(got, "…") {
			t.Fatalf("show should mask secrets with …")
		}
	}
}

func TestConfig_ShowReveal(t *testing.T) {
	withConfigFile(t)
	var out bytes.Buffer
	_ = runConfig(context.Background(), []string{"set", "OPENAI_API_KEY", "sk-secret"}, &out, &out)
	out.Reset()
	if err := runConfig(context.Background(), []string{"show", "--reveal"}, &out, &out); err != nil {
		t.Fatalf("show --reveal: %v", err)
	}
	if !strings.Contains(out.String(), "sk-secret") {
		t.Fatalf("--reveal should print secret verbatim:\n%s", out.String())
	}
}

func TestConfig_ShellEnvBeatsConfigFile(t *testing.T) {
	withConfigFile(t)
	var out bytes.Buffer
	_ = runConfig(context.Background(), []string{"set", "OPENAI_MODEL", "gpt-from-file"}, &out, &out)
	t.Setenv("OPENAI_MODEL", "gpt-from-shell")
	out.Reset()
	if err := runConfig(context.Background(), []string{"get", "OPENAI_MODEL"}, &out, &out); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "gpt-from-shell" {
		t.Errorf("expected shell env to win, got %q", got)
	}
}

func TestConfig_UnknownAction(t *testing.T) {
	withConfigFile(t)
	var out bytes.Buffer
	err := runConfig(context.Background(), []string{"melt"}, &out, &out)
	if err == nil || !strings.Contains(err.Error(), "unknown config action") {
		t.Fatalf("expected unknown-action error, got %v", err)
	}
}

func TestConfig_GetRequiresKey(t *testing.T) {
	var out bytes.Buffer
	if err := runConfig(context.Background(), []string{"get"}, &out, &out); err == nil {
		t.Fatal("expected error when no key provided")
	}
}

func TestSortedKnownKeys_Stable(t *testing.T) {
	a := sortedKnownKeys()
	b := sortedKnownKeys()
	if !sliceEq(a, b) {
		t.Fatal("sortedKnownKeys not stable")
	}
}
