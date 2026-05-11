package checkup

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func clearLLMEnv(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("OLLAMA_BASE_URL", "")
	t.Setenv("OLLAMA_MODEL", "")
	t.Setenv("GOON_LLM_PROVIDER", "")
	t.Setenv("GOON_BOARD", "")
	t.Setenv("GOON_GIT_HOST", "")
	t.Setenv("JIRA_BASE_URL", "")
	t.Setenv("JIRA_EMAIL", "")
	t.Setenv("JIRA_API_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GITHUB_REPOS", "")
	t.Setenv("GITHUB_API_URL", "")
	t.Setenv("GITLAB_TOKEN", "")
	t.Setenv("GITLAB_API_URL", "")
	t.Setenv("BITBUCKET_TOKEN", "")
	t.Setenv("BITBUCKET_USERNAME", "")
	t.Setenv("BITBUCKET_APP_PASSWORD", "")
	t.Setenv("BITBUCKET_API_URL", "")
	t.Setenv("TELEGRAM_BOT_TOKEN", "")
	t.Setenv("TELEGRAM_API_BASE_URL", "")
}

func TestProbeOpenAI_OK(t *testing.T) {
	clearLLMEnv(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/models") {
			t.Errorf("path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer KEY" {
			t.Errorf("auth: %s", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer ts.Close()
	t.Setenv("GOON_LLM_PROVIDER", "openai")
	t.Setenv("OPENAI_API_KEY", "KEY")
	t.Setenv("OPENAI_BASE_URL", ts.URL)
	r := probeOpenAI(context.Background(), os.Getenv)
	if !r.OK {
		t.Fatalf("expected OK, got %+v", r)
	}
	if !strings.Contains(r.Detail, "auth OK") {
		t.Errorf("detail: %q", r.Detail)
	}
}

func TestProbeOpenAI_BadAuth(t *testing.T) {
	clearLLMEnv(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer ts.Close()
	t.Setenv("OPENAI_API_KEY", "BAD")
	t.Setenv("OPENAI_BASE_URL", ts.URL)
	r := probeOpenAI(context.Background(), os.Getenv)
	if r.OK {
		t.Fatalf("expected fail, got %+v", r)
	}
	if !strings.Contains(r.Detail, "401") {
		t.Errorf("detail: %q", r.Detail)
	}
}

func TestProbeOpenAI_NoKey(t *testing.T) {
	clearLLMEnv(t)
	r := probeOpenAI(context.Background(), os.Getenv)
	if r.OK {
		t.Fatalf("expected fail when key missing")
	}
	if !strings.Contains(r.Detail, "OPENAI_API_KEY") {
		t.Errorf("detail: %q", r.Detail)
	}
}

func TestProbeAnthropic_OK(t *testing.T) {
	clearLLMEnv(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/messages") {
			t.Errorf("path: %s", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "KEY" {
			t.Errorf("auth: %s", r.Header.Get("x-api-key"))
		}
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"hi"}]}`))
	}))
	defer ts.Close()
	t.Setenv("ANTHROPIC_API_KEY", "KEY")
	t.Setenv("ANTHROPIC_BASE_URL", ts.URL)
	r := probeAnthropic(context.Background(), os.Getenv)
	if !r.OK {
		t.Fatalf("expected OK, got %+v", r)
	}
}

func TestProbeOllama_ServerReachable(t *testing.T) {
	clearLLMEnv(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"models":[{"name":"qwen2.5:7b"},{"name":"llama3"}]}`))
	}))
	defer ts.Close()
	t.Setenv("OLLAMA_BASE_URL", ts.URL)
	t.Setenv("OLLAMA_MODEL", "llama3")
	r := probeOllama(context.Background(), os.Getenv)
	if !r.OK {
		t.Fatalf("expected OK, got %+v", r)
	}
	if !strings.Contains(r.Detail, "2 model(s)") {
		t.Errorf("detail: %q", r.Detail)
	}
}

func TestProbeOllama_ModelNotPulled(t *testing.T) {
	clearLLMEnv(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"models":[{"name":"mistral"}]}`))
	}))
	defer ts.Close()
	t.Setenv("OLLAMA_BASE_URL", ts.URL)
	t.Setenv("OLLAMA_MODEL", "qwen2.5-coder")
	r := probeOllama(context.Background(), os.Getenv)
	// Server reachable but the configured model isn't pulled — the agent
	// loop will fail at first generate(), so doctor flags it red so the
	// user sees the misconfig before launching `goon start`.
	if r.OK {
		t.Fatalf("expected fail when configured model isn't installed: %+v", r)
	}
	if !strings.Contains(r.Detail, "not pulled") {
		t.Errorf("expected 'not pulled' note: %q", r.Detail)
	}
	if !strings.Contains(r.Detail, "ollama pull qwen2.5-coder") {
		t.Errorf("expected actionable hint: %q", r.Detail)
	}
}

func TestProbeOllama_Unreachable(t *testing.T) {
	clearLLMEnv(t)
	t.Setenv("OLLAMA_BASE_URL", "http://127.0.0.1:1") // refused
	r := probeOllama(context.Background(), os.Getenv)
	if r.OK {
		t.Fatalf("expected fail, got %+v", r)
	}
}

func TestProbeJira_OK(t *testing.T) {
	clearLLMEnv(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/rest/api/3/myself") {
			t.Errorf("path: %s", r.URL.Path)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Basic ") {
			t.Errorf("auth: %s", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"displayName":"Harisa","emailAddress":"h@x"}`))
	}))
	defer ts.Close()
	t.Setenv("JIRA_BASE_URL", ts.URL)
	t.Setenv("JIRA_EMAIL", "h@x")
	t.Setenv("JIRA_API_TOKEN", "tok")
	r := probeJira(context.Background(), os.Getenv)
	if !r.OK {
		t.Fatalf("expected OK, got %+v", r)
	}
	if !strings.Contains(r.Detail, "Harisa") {
		t.Errorf("detail: %q", r.Detail)
	}
}

func TestProbeGitHubBoard_NeedsRepos(t *testing.T) {
	clearLLMEnv(t)
	t.Setenv("GITHUB_TOKEN", "x")
	r := probeGitHubBoard(context.Background(), os.Getenv)
	if r.OK {
		t.Fatalf("expected fail without GITHUB_REPOS")
	}
}

func TestProbeGitHubBoard_OK(t *testing.T) {
	clearLLMEnv(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"login":"harisaginting"}`))
	}))
	defer ts.Close()
	t.Setenv("GITHUB_TOKEN", "tok")
	t.Setenv("GITHUB_REPOS", "harisaginting/goon,me/other")
	t.Setenv("GITHUB_API_URL", ts.URL)
	r := probeGitHubBoard(context.Background(), os.Getenv)
	if !r.OK {
		t.Fatalf("expected OK, got %+v", r)
	}
	if !strings.Contains(r.Detail, "@harisaginting") || !strings.Contains(r.Detail, "2 repo") {
		t.Errorf("detail: %q", r.Detail)
	}
}

func TestProbeGitLab_OK(t *testing.T) {
	clearLLMEnv(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") != "T" {
			t.Errorf("auth: %s", r.Header.Get("PRIVATE-TOKEN"))
		}
		_, _ = w.Write([]byte(`{"username":"hari"}`))
	}))
	defer ts.Close()
	t.Setenv("GITLAB_TOKEN", "T")
	t.Setenv("GITLAB_API_URL", ts.URL)
	r := probeGitLab(context.Background(), os.Getenv)
	if !r.OK {
		t.Fatalf("expected OK, got %+v", r)
	}
}

func TestProbeBitbucket_TokenAuth(t *testing.T) {
	clearLLMEnv(t)
	var auth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"username":"hari","display_name":"Hari"}`))
	}))
	defer ts.Close()
	t.Setenv("BITBUCKET_TOKEN", "TOK")
	t.Setenv("BITBUCKET_API_URL", ts.URL)
	r := probeBitbucket(context.Background(), os.Getenv)
	if !r.OK {
		t.Fatalf("expected OK, got %+v", r)
	}
	if auth != "Bearer TOK" {
		t.Errorf("auth: %s", auth)
	}
}

func TestProbeBitbucket_BasicAuth(t *testing.T) {
	clearLLMEnv(t)
	var auth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"username":"hari"}`))
	}))
	defer ts.Close()
	t.Setenv("BITBUCKET_USERNAME", "u@x")
	t.Setenv("BITBUCKET_APP_PASSWORD", "appkey")
	t.Setenv("BITBUCKET_API_URL", ts.URL)
	r := probeBitbucket(context.Background(), os.Getenv)
	if !r.OK {
		t.Fatalf("expected OK, got %+v", r)
	}
	if !strings.HasPrefix(auth, "Basic ") {
		t.Errorf("auth: %s", auth)
	}
}

func TestCheckTelegram_OK(t *testing.T) {
	clearLLMEnv(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":{"username":"goon_bot"}}`))
	}))
	defer ts.Close()
	t.Setenv("TELEGRAM_BOT_TOKEN", "TOK")
	t.Setenv("TELEGRAM_API_BASE_URL", ts.URL)
	t.Setenv("TELEGRAM_CHAT_ID", "12345")
	r := checkTelegram(context.Background(), os.Getenv)
	if !r.OK {
		t.Fatalf("expected OK: %+v", r)
	}
	if !strings.Contains(r.Detail, "@goon_bot") {
		t.Errorf("detail: %q", r.Detail)
	}
}

func TestCheckTelegram_NoToken_Skipped(t *testing.T) {
	clearLLMEnv(t)
	r := checkTelegram(context.Background(), os.Getenv)
	if r.Component != "" {
		t.Fatalf("expected empty result when no token, got %+v", r)
	}
}

func TestRun_AllProbesAggregated(t *testing.T) {
	clearLLMEnv(t)
	t.Setenv("GOON_LLM_PROVIDER", "mock")
	t.Setenv("GOON_BOARD", "mock")
	t.Setenv("GOON_GIT_HOST", "mock")
	rs := Run(context.Background())
	if len(rs) < 4 {
		t.Fatalf("expected ≥4 results, got %d: %+v", len(rs), rs)
	}
	got := map[string]bool{}
	for _, r := range rs {
		got[r.Component] = r.OK
	}
	for _, c := range []string{"memory", "llm", "board", "git_host"} {
		if v, ok := got[c]; !ok || !v {
			t.Errorf("component %s missing or failing in: %+v", c, rs)
		}
	}
}

func TestAllOK_AndFirstFailure(t *testing.T) {
	rs := []Result{
		{Component: "memory", OK: true},
		{Component: "llm", Name: "openai", OK: false, Detail: "401"},
		{Component: "git_host", OK: true, Skipped: true},
	}
	if AllOK(rs) {
		t.Fatal("expected not-AllOK")
	}
	if FirstFailure(rs) != "llm/openai: 401" {
		t.Fatalf("first failure: %q", FirstFailure(rs))
	}
	rs2 := []Result{
		{Component: "memory", OK: true},
		{Component: "llm", Name: "mock", OK: true},
	}
	if !AllOK(rs2) {
		t.Fatal("expected AllOK")
	}
	if FirstFailure(rs2) != "" {
		t.Fatalf("first failure should be empty: %q", FirstFailure(rs2))
	}
}

func TestRunWithEnvOverride_RestoresEnv(t *testing.T) {
	clearLLMEnv(t)
	t.Setenv("GOON_LLM_PROVIDER", "mock")
	t.Setenv("OPENAI_API_KEY", "original")
	override := map[string]string{
		"OPENAI_API_KEY": "temporary",
		"GOON_BOARD":     "mock",
	}
	rs := RunWithEnvOverride(context.Background(), override)
	if len(rs) == 0 {
		t.Fatal("expected results")
	}
	if v := os.Getenv("OPENAI_API_KEY"); v != "original" {
		t.Errorf("env not restored: got %q want original", v)
	}
	if v := os.Getenv("GOON_BOARD"); v != "" {
		t.Errorf("GOON_BOARD should be unset after restore, got %q", v)
	}
}

func TestEmitJSON_ParseableShape(t *testing.T) {
	rs := []Result{{Component: "llm", Name: "openai", OK: true, Detail: "auth OK"}}
	b, _ := json.Marshal(rs)
	if !strings.Contains(string(b), `"component":"llm"`) || !strings.Contains(string(b), `"ok":true`) {
		t.Fatalf("json shape: %s", b)
	}
}
