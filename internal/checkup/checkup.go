// Package checkup runs a tiny live probe against every configured provider
// and reports back whether goon's setup actually works end-to-end.
//
// The probes are intentionally cheap — each one is a single HTTP call that
// proves auth + network reachability without spending tokens or polluting
// the user's data:
//
//	OpenAI    GET  /models                     (auth check, no tokens)
//	Anthropic POST /messages, max_tokens=1     (smallest possible call)
//	Ollama    GET  /api/tags                   (free, lists local models)
//	Jira      GET  /rest/api/3/myself          (free)
//	GitHub    GET  /user                       (free)
//	GitLab    GET  /user                       (free)
//	Bitbucket GET  /2.0/user                   (free)
//	Telegram  GET  /bot{TOKEN}/getMe           (free)
//	Memory    write+read a tmp value           (filesystem check)
//
// CLI users hit this via `goon doctor`. The web UI calls
// `RunWithEnvOverride` to test pending form values without saving them.
package checkup

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/harisaginting/goon/internal/atlassian"
	"github.com/harisaginting/goon/internal/logx"
	"github.com/harisaginting/goon/internal/memory"
	"github.com/harisaginting/goon/internal/storage"
	"github.com/harisaginting/goon/internal/util"
)

// Result is the outcome of one component's probe.
type Result struct {
	Component string `json:"component"` // memory | llm | board | git_host | telegram
	Name      string `json:"name"`      // openai | jira | github | …
	OK        bool   `json:"ok"`
	Detail    string `json:"detail"`
	Skipped   bool   `json:"skipped,omitempty"`
}

// Pretty returns "✓ name (component): detail" for terminal output.
func (r Result) Pretty() string {
	mark := "✓"
	if !r.OK {
		mark = "✗"
	}
	if r.Skipped {
		mark = "·"
	}
	id := r.Component
	if r.Name != "" {
		id = r.Component + "/" + r.Name
	}
	return fmt.Sprintf("%s %-22s %s", mark, id, r.Detail)
}

// Run probes every component using the current environment.
func Run(ctx context.Context) []Result {
	out := []Result{}
	out = append(out, checkMemory())
	out = append(out, checkLLM(ctx))
	out = append(out, checkBoard(ctx))
	out = append(out, checkGitHost(ctx))
	if t := checkTelegram(ctx); t.Component != "" {
		out = append(out, t)
	}
	return out
}

// envMu serializes RunWithEnvOverride callers so concurrent verify requests
// don't trample each other's temporary env changes.
var envMu sync.Mutex

// RunWithEnvOverride applies the given key/value pairs to os.Environ for the
// duration of the probe, then restores the previous values. Empty values
// unset the key. Callers serialise via envMu so this is safe to call from
// multiple HTTP requests.
func RunWithEnvOverride(ctx context.Context, override map[string]string) []Result {
	envMu.Lock()
	defer envMu.Unlock()

	type prev struct {
		val string
		set bool
	}
	saved := map[string]prev{}
	for k, v := range override {
		old, ok := os.LookupEnv(k)
		saved[k] = prev{old, ok}
		if v == "" {
			_ = os.Unsetenv(k)
		} else {
			_ = os.Setenv(k, v)
		}
	}
	defer func() {
		for k, p := range saved {
			if p.set {
				_ = os.Setenv(k, p.val)
			} else {
				_ = os.Unsetenv(k)
			}
		}
	}()
	return Run(ctx)
}

// --- helpers ---------------------------------------------------------------

func client(timeout time.Duration) *http.Client {
	return logx.InstrumentClient("checkup", &http.Client{Timeout: timeout})
}

// httpClient is overridable in tests so we can target an httptest server.
var httpClient = client(8 * time.Second)

// newReq is a thin wrapper around http.NewRequestWithContext. If
// construction fails (extremely rare — bad method / bad URL), it returns
// a non-nil but unusable Request paired with the error so callers can
// safely check the error before reading req.Header. Probe helpers detect
// the error and report it as "request: ..." in the result detail.
func newReq(ctx context.Context, method, url string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		// Return a placeholder so a caller that forgets the err check
		// still gets a non-nil request and panics later in a clearer
		// place — but the documented contract is "always check err".
		return &http.Request{Header: http.Header{}}, fmt.Errorf("checkup newReq: %w", err)
	}
	return req, nil
}

// --- memory ---------------------------------------------------------------

func checkMemory() Result {
	path := os.Getenv("GOON_MEMORY_PATH")
	if path == "" {
		// Mirror internal/memory.New's default so `goon doctor` reports the
		// same path the agent will actually use.
		path = storage.Path("memory.json")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Result{Component: "memory", OK: false, Detail: "cannot create dir: " + err.Error()}
	}
	m, err := memory.New(path)
	if err != nil {
		return Result{Component: "memory", OK: false, Detail: "open: " + err.Error()}
	}
	// Touch the status to prove writes work.
	st := m.GetStatus()
	m.SetStatus(st)
	return Result{Component: "memory", OK: true, Detail: path}
}

// --- LLM ------------------------------------------------------------------

func checkLLM(ctx context.Context) Result {
	name := strings.ToLower(strings.TrimSpace(os.Getenv("GOON_LLM_PROVIDER")))
	if name == "" {
		name = "openai"
	}
	switch name {
	case "openai":
		return probeOpenAI(ctx)
	case "anthropic":
		return probeAnthropic(ctx)
	case "ollama":
		return probeOllama(ctx)
	case "mock":
		return Result{Component: "llm", Name: "mock", OK: true, Detail: "mock provider — always succeeds"}
	default:
		return Result{Component: "llm", Name: name, OK: false,
			Detail: fmt.Sprintf("unknown GOON_LLM_PROVIDER %q (want openai|anthropic|ollama|mock)", name)}
	}
}

func probeOpenAI(ctx context.Context) Result {
	r := Result{Component: "llm", Name: "openai"}
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		r.Detail = "OPENAI_API_KEY is not set"
		return r
	}
	base := util.EnvOr("OPENAI_BASE_URL", "https://api.openai.com/v1")
	req, err := newReq(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/models", nil)
	if err != nil {
		r.Detail = err.Error()
		return r
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := httpClient.Do(req)
	if err != nil {
		r.Detail = err.Error()
		return r
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		r.Detail = fmt.Sprintf("http %d: %s", resp.StatusCode, util.Truncate(string(body), 120))
		return r
	}
	r.OK = true
	r.Detail = fmt.Sprintf("auth OK · model=%s", util.EnvOr("OPENAI_MODEL", "gpt-4o-mini"))
	return r
}

func probeAnthropic(ctx context.Context) Result {
	r := Result{Component: "llm", Name: "anthropic"}
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		r.Detail = "ANTHROPIC_API_KEY is not set"
		return r
	}
	base := util.EnvOr("ANTHROPIC_BASE_URL", "https://api.anthropic.com/v1")
	body := []byte(`{"model":"` + util.EnvOr("ANTHROPIC_MODEL", "claude-sonnet-4-5") +
		`","max_tokens":1,"messages":[{"role":"user","content":"ping"}]}`)
	req, err := newReq(ctx, http.MethodPost,
		strings.TrimRight(base, "/")+"/messages", strings.NewReader(string(body)))
	if err != nil {
		r.Detail = err.Error()
		return r
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := httpClient.Do(req)
	if err != nil {
		r.Detail = err.Error()
		return r
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		r.Detail = fmt.Sprintf("http %d: %s", resp.StatusCode, util.Truncate(string(raw), 120))
		return r
	}
	r.OK = true
	r.Detail = fmt.Sprintf("auth OK · model=%s", util.EnvOr("ANTHROPIC_MODEL", "claude-sonnet-4-5"))
	return r
}

func probeOllama(ctx context.Context) Result {
	r := Result{Component: "llm", Name: "ollama"}
	base := util.EnvOr("OLLAMA_BASE_URL", "http://localhost:11434")
	model := util.EnvOr("OLLAMA_MODEL", "llama3")
	req, err := newReq(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/api/tags", nil)
	if err != nil {
		r.Detail = err.Error()
		return r
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		r.Detail = "cannot reach " + base + ": " + err.Error()
		return r
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(resp.Body)
		r.Detail = fmt.Sprintf("http %d: %s", resp.StatusCode, util.Truncate(string(raw), 120))
		return r
	}
	var parsed struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &parsed)
	hasModel := false
	for _, m := range parsed.Models {
		if m.Name == model || strings.HasPrefix(m.Name, model+":") {
			hasModel = true
			break
		}
	}
	// Server reachable but the configured model isn't pulled — that's a
	// real failure, not a "skipped". The agent loop will hard-fail at
	// first generate() call, so flag it red here so users see it during
	// `goon doctor`.
	if !hasModel {
		r.OK = false
		r.Detail = fmt.Sprintf("server OK · %d model(s) installed · model %q not pulled — run: ollama pull %s",
			len(parsed.Models), model, model)
		return r
	}
	r.OK = true
	r.Detail = fmt.Sprintf("server OK · %d model(s) installed · target=%s",
		len(parsed.Models), model)
	return r
}

// --- board ----------------------------------------------------------------

func checkBoard(ctx context.Context) Result {
	name := strings.ToLower(strings.TrimSpace(os.Getenv("GOON_BOARD")))
	if name == "" {
		return Result{Component: "board", OK: false, Skipped: true, Detail: "GOON_BOARD is not set"}
	}
	switch name {
	case "jira":
		return probeJira(ctx)
	case "github":
		return probeGitHubBoard(ctx)
	case "mock":
		return Result{Component: "board", Name: "mock", OK: true, Detail: "mock board — always succeeds"}
	default:
		return Result{Component: "board", Name: name, OK: false,
			Detail: fmt.Sprintf("unknown GOON_BOARD %q (want jira|github|mock)", name)}
	}
}

func probeJira(ctx context.Context) Result {
	r := Result{Component: "board", Name: "jira"}
	c := atlassian.Jira()
	if !c.Filled() {
		r.Detail = "set JIRA_BASE_URL/JIRA_EMAIL/JIRA_API_TOKEN (or shared ATLASSIAN_* equivalents)"
		return r
	}
	req, err := newReq(ctx, http.MethodGet, c.BaseURL+"/rest/api/3/myself", nil)
	if err != nil {
		r.Detail = err.Error()
		return r
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Basic "+
		base64.StdEncoding.EncodeToString([]byte(c.Email+":"+c.APIToken)))
	resp, err := httpClient.Do(req)
	if err != nil {
		r.Detail = err.Error()
		return r
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		r.Detail = fmt.Sprintf("http %d: %s", resp.StatusCode, util.Truncate(string(raw), 120))
		return r
	}
	var who struct {
		DisplayName string `json:"displayName"`
		EmailAddr   string `json:"emailAddress"`
	}
	_ = json.Unmarshal(raw, &who)
	r.OK = true
	r.Detail = fmt.Sprintf("auth OK as %s <%s>", who.DisplayName, who.EmailAddr)
	return r
}

func probeGitHubBoard(ctx context.Context) Result {
	r := Result{Component: "board", Name: "github"}
	tok := os.Getenv("GITHUB_TOKEN")
	if tok == "" {
		r.Detail = "GITHUB_TOKEN is not set"
		return r
	}
	repos := strings.TrimSpace(os.Getenv("GITHUB_REPOS"))
	if repos == "" {
		r.Detail = "GITHUB_REPOS is not set (need owner/repo[,...])"
		return r
	}
	api := util.EnvOr("GITHUB_API_URL", "https://api.github.com")
	r2 := pingGitHub(ctx, api, tok)
	if !r2.OK {
		r.Detail = r2.Detail
		return r
	}
	r.OK = true
	r.Detail = fmt.Sprintf("%s · %d repo(s) configured", r2.Detail,
		strings.Count(repos, ",")+1)
	return r
}

// --- git host -------------------------------------------------------------

func checkGitHost(ctx context.Context) Result {
	name := strings.ToLower(strings.TrimSpace(os.Getenv("GOON_GIT_HOST")))
	if name == "" {
		// If a board is configured but no git host is, that's a config gap
		// — the daemon will run the workflow happily but skip the PR step,
		// which usually surprises users. Surface it as a yellow ⚠ skip
		// (OK + Skipped + an actionable Detail) instead of a silent ✓.
		board := strings.ToLower(strings.TrimSpace(os.Getenv("GOON_BOARD")))
		if board != "" && board != "mock" {
			return Result{
				Component: "git_host", OK: true, Skipped: true,
				Detail: "GOON_GIT_HOST is empty — set GOON_GIT_HOST=github|gitlab|bitbucket plus matching auth (e.g. GITHUB_TOKEN) to enable PR creation",
			}
		}
		return Result{Component: "git_host", OK: true, Skipped: true, Detail: "not configured (PR creation will be skipped)"}
	}
	switch name {
	case "github":
		api := util.EnvOr("GITHUB_API_URL", "https://api.github.com")
		tok := os.Getenv("GITHUB_TOKEN")
		if tok == "" {
			return Result{Component: "git_host", Name: "github", Detail: "GITHUB_TOKEN is not set"}
		}
		out := pingGitHub(ctx, api, tok)
		out.Component = "git_host"
		out.Name = "github"
		return out
	case "gitlab":
		return probeGitLab(ctx)
	case "bitbucket":
		return probeBitbucket(ctx)
	case "mock":
		return Result{Component: "git_host", Name: "mock", OK: true, Detail: "mock host — always succeeds"}
	default:
		return Result{Component: "git_host", Name: name, OK: false,
			Detail: fmt.Sprintf("unknown GOON_GIT_HOST %q", name)}
	}
}

func pingGitHub(ctx context.Context, api, tok string) Result {
	r := Result{}
	req, err := newReq(ctx, http.MethodGet, strings.TrimRight(api, "/")+"/user", nil)
	if err != nil {
		r.Detail = err.Error()
		return r
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := httpClient.Do(req)
	if err != nil {
		r.Detail = err.Error()
		return r
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		r.Detail = fmt.Sprintf("http %d: %s", resp.StatusCode, util.Truncate(string(raw), 120))
		return r
	}
	var who struct {
		Login string `json:"login"`
	}
	_ = json.Unmarshal(raw, &who)
	r.OK = true
	r.Detail = fmt.Sprintf("auth OK as @%s", who.Login)
	return r
}

func probeGitLab(ctx context.Context) Result {
	r := Result{Component: "git_host", Name: "gitlab"}
	tok := os.Getenv("GITLAB_TOKEN")
	if tok == "" {
		r.Detail = "GITLAB_TOKEN is not set"
		return r
	}
	api := util.EnvOr("GITLAB_API_URL", "https://gitlab.com/api/v4")
	req, err := newReq(ctx, http.MethodGet, strings.TrimRight(api, "/")+"/user", nil)
	if err != nil {
		r.Detail = err.Error()
		return r
	}
	req.Header.Set("PRIVATE-TOKEN", tok)
	resp, err := httpClient.Do(req)
	if err != nil {
		r.Detail = err.Error()
		return r
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		r.Detail = fmt.Sprintf("http %d: %s", resp.StatusCode, util.Truncate(string(raw), 120))
		return r
	}
	var who struct {
		Username string `json:"username"`
	}
	_ = json.Unmarshal(raw, &who)
	r.OK = true
	r.Detail = fmt.Sprintf("auth OK as @%s", who.Username)
	return r
}

func probeBitbucket(ctx context.Context) Result {
	r := Result{Component: "git_host", Name: "bitbucket"}
	tok := os.Getenv("BITBUCKET_TOKEN")
	user := os.Getenv("BITBUCKET_USERNAME")
	pw := os.Getenv("BITBUCKET_APP_PASSWORD")
	if tok == "" && (user == "" || pw == "") {
		r.Detail = "set BITBUCKET_TOKEN or BITBUCKET_USERNAME + BITBUCKET_APP_PASSWORD"
		return r
	}
	api := util.EnvOr("BITBUCKET_API_URL", "https://api.bitbucket.org/2.0")
	req, err := newReq(ctx, http.MethodGet, strings.TrimRight(api, "/")+"/user", nil)
	if err != nil {
		r.Detail = err.Error()
		return r
	}
	req.Header.Set("Accept", "application/json")
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	} else {
		req.Header.Set("Authorization", "Basic "+
			base64.StdEncoding.EncodeToString([]byte(user+":"+pw)))
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		r.Detail = err.Error()
		return r
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		r.Detail = fmt.Sprintf("http %d: %s", resp.StatusCode, util.Truncate(string(raw), 120))
		return r
	}
	var who struct {
		Username    string `json:"username"`
		DisplayName string `json:"display_name"`
	}
	_ = json.Unmarshal(raw, &who)
	name := who.Username
	if name == "" {
		name = who.DisplayName
	}
	r.OK = true
	r.Detail = fmt.Sprintf("auth OK as %s", name)
	return r
}

// --- telegram (optional) --------------------------------------------------

func checkTelegram(ctx context.Context) Result {
	tok := os.Getenv("TELEGRAM_BOT_TOKEN")
	if tok == "" {
		return Result{} // skip entirely
	}
	r := Result{Component: "telegram", Name: "bot"}
	api := util.EnvOr("TELEGRAM_API_BASE_URL", "https://api.telegram.org")
	req, err := newReq(ctx, http.MethodGet, fmt.Sprintf("%s/bot%s/getMe", api, tok), nil)
	if err != nil {
		r.Detail = err.Error()
		return r
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		r.Detail = err.Error()
		return r
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var parsed struct {
		OK     bool `json:"ok"`
		Result struct {
			Username string `json:"username"`
		} `json:"result"`
		Description string `json:"description"`
	}
	_ = json.Unmarshal(raw, &parsed)
	if !parsed.OK {
		if parsed.Description != "" {
			r.Detail = parsed.Description
		} else {
			r.Detail = fmt.Sprintf("http %d", resp.StatusCode)
		}
		return r
	}
	chat := os.Getenv("TELEGRAM_CHAT_ID")
	if chat == "" {
		r.OK = true
		r.Detail = fmt.Sprintf("auth OK as @%s · NOTE: TELEGRAM_CHAT_ID is empty", parsed.Result.Username)
		return r
	}
	r.OK = true
	r.Detail = fmt.Sprintf("auth OK as @%s · chat=%s", parsed.Result.Username, chat)
	return r
}

// AllOK returns true iff every non-skipped result is OK. Used by `goon doctor`
// to set its exit code.
func AllOK(rs []Result) bool {
	for _, r := range rs {
		if r.Skipped {
			continue
		}
		if !r.OK {
			return false
		}
	}
	return true
}

// First failed reason (for short summary). Returns "" if all OK.
func FirstFailure(rs []Result) string {
	for _, r := range rs {
		if r.Skipped || r.OK {
			continue
		}
		if r.Name != "" {
			return r.Component + "/" + r.Name + ": " + r.Detail
		}
		return r.Component + ": " + r.Detail
	}
	return ""
}

// Sentinel for callers that want to detect "no checks ran" vs "all skipped".
var ErrNothingChecked = errors.New("checkup: nothing to check")
