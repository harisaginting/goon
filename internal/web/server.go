// Package web exposes goon's status, tickets, questions, and config over HTTP
// with a single embedded htmx page. It's designed to be a perfect mirror of
// the CLI: every action available from the command line is also available
// here, backed by the same memory store.
package web

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/harisaginting/goon/internal/boards"
	"github.com/harisaginting/goon/internal/checkup"
	"github.com/harisaginting/goon/internal/llm"
	"github.com/harisaginting/goon/internal/memory"
)

// Reconfigurable is the small slice of *daemon.Daemon the web layer touches.
// Declared as an interface here so the web package doesn't import daemon
// (which would create a cycle: daemon → workflow → … → tools, and we'd lose
// the ability to embed the web package elsewhere).
type Reconfigurable interface {
	Reconfigure() []string
	Configured() bool
}

// Options bundles dependencies for the Server.
type Options struct {
	Addr   string
	Memory *memory.Memory
	Daemon Reconfigurable
	// LLM enables the /api/chat surface — when nil the chat tab
	// renders an "unavailable: configure a provider first" message
	// instead of an input box.
	LLM llm.Provider
	// Board enables /api/refresh and the auto-refresh-on-stale path
	// inside the chat handler. When nil, refresh attempts respond
	// with a friendly "no board configured" message; chat falls back
	// to whatever's already in memory.json.
	Board  boards.Board
	Stdout io.Writer
	Stderr io.Writer
}

// Server is goon's web frontend.
type Server struct {
	opts Options
	srv  *http.Server
	mu   sync.Mutex

	// chatMu serialises access to chatHistory. The web chat is
	// single-session (the dashboard is intended for one operator),
	// so we keep one rolling history per process rather than per
	// browser tab — simpler and matches the user model.
	chatMu      sync.Mutex
	chatHistory []llm.Message
}

// NewServer wires the Server.
func NewServer(opts Options) *Server {
	return &Server{opts: opts}
}

// mux builds the routing table. Split out so tests can use it directly via
// httptest.NewRecorder without binding a real port.
func (s *Server) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/docs", s.handleDocs)
	mux.HandleFunc("/api/status", s.handleAPIStatus)
	mux.HandleFunc("/api/tickets", s.handleAPITickets)
	mux.HandleFunc("/api/workflows", s.handleAPIWorkflows)
	mux.HandleFunc("/api/questions", s.handleAPIQuestions)
	mux.HandleFunc("/api/answer", s.handleAnswer)
	mux.HandleFunc("/api/config", s.handleAPIConfig) // GET reads, POST writes
	mux.HandleFunc("/api/config/verify", s.handleConfigVerify)
	mux.HandleFunc("/api/daemon/pause", s.handleDaemonPause)
	mux.HandleFunc("/api/daemon/resume", s.handleDaemonResume)
	mux.HandleFunc("/api/chat", s.handleChat)
	mux.HandleFunc("/api/chat/reset", s.handleChatReset)
	mux.HandleFunc("/api/knowledge/note", s.handleKnowledgeNote)
	mux.HandleFunc("/api/refresh", s.handleRefresh)
	mux.HandleFunc("/htmx.min.js", s.handleHTMX)
	// Underlying fragments — render the raw component (used by tests
	// and direct htmx polls).
	mux.HandleFunc("/fragments/status", s.fragStatus)
	mux.HandleFunc("/fragments/tickets", s.fragTickets)
	mux.HandleFunc("/fragments/questions", s.fragQuestions)
	mux.HandleFunc("/fragments/workflows", s.fragWorkflows)
	mux.HandleFunc("/fragments/config", s.fragConfig)
	mux.HandleFunc("/fragments/setup", s.fragSetup)
	// Header + chrome fragments served separately so the dashboard
	// can refresh them on different cadences without re-rendering
	// the entire main panel.
	mux.HandleFunc("/fragments/status-pill", s.fragStatusPill)
	mux.HandleFunc("/fragments/questions-banner", s.fragQuestionsBanner)
	// Tab content composers — wrap the underlying fragments with a
	// section title + spacing so each tab feels purpose-built.
	mux.HandleFunc("/fragments/tab-overview", s.fragTabOverview)
	mux.HandleFunc("/fragments/tab-tickets", s.fragTabTickets)
	mux.HandleFunc("/fragments/tab-workflows", s.fragTabWorkflows)
	mux.HandleFunc("/fragments/tab-questions", s.fragTabQuestions)
	mux.HandleFunc("/fragments/tab-config", s.fragTabConfig)
	mux.HandleFunc("/fragments/tab-chat", s.fragTabChat)
	mux.HandleFunc("/fragments/tab-knowledge", s.fragTabKnowledge)
	return mux
}

// Start begins serving. Blocks until ListenAndServe returns.
//
// Timeouts are tuned to defend against slowloris-style hold-opens while
// still permitting the longer-running endpoints (e.g. /api/config/verify
// hits provider HTTPS endpoints). If you need to stream a long response,
// use a different surface — the web UI is short-poll htmx, not SSE.
func (s *Server) Start() error {
	s.mu.Lock()
	s.srv = &http.Server{
		Addr:              s.opts.Addr,
		Handler:           s.mux(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10, // 32 KB — tiny since htmx requests are small.
	}
	srv := s.srv
	s.mu.Unlock()

	st := s.opts.Memory.GetStatus()
	st.WebAddr = s.opts.Addr
	s.opts.Memory.SetStatus(st)

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Stop gracefully shuts down the server.
func (s *Server) Stop() {
	s.mu.Lock()
	srv := s.srv
	s.mu.Unlock()
	if srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

// --- Index page ------------------------------------------------------------

//go:embed static/index.html
var indexHTML string

//go:embed static/htmx.min.js
var htmxJS []byte

//go:embed static/docs.html
var docsHTML string

func (s *Server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, indexHTML)
}

// handleDocs serves the embedded, self-contained documentation page. It's
// the same content as the README but rendered as a styled, sectioned web
// page so a brand-new user can find everything without leaving the UI.
// Keeping it embedded means it ships with every binary — no internet
// required to read the manual.
func (s *Server) handleDocs(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = io.WriteString(w, docsHTML)
}

func (s *Server) handleHTMX(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(htmxJS)
}

// --- JSON API --------------------------------------------------------------

func (s *Server) handleAPIStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.opts.Memory.GetStatus())
}

func (s *Server) handleAPITickets(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.opts.Memory.ListTickets())
}

func (s *Server) handleAPIWorkflows(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.opts.Memory.ListWorkflows(50))
}

func (s *Server) handleAPIQuestions(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("all") == "1" {
		writeJSON(w, s.opts.Memory.AllQuestions())
		return
	}
	writeJSON(w, s.opts.Memory.PendingQuestions())
}

func (s *Server) handleAnswer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id := r.FormValue("id")
	answer := r.FormValue("answer")
	if id == "" || answer == "" {
		http.Error(w, "id and answer required", http.StatusBadRequest)
		return
	}
	if !s.opts.Memory.AnswerQuestion(id, answer) {
		http.Error(w, "question not found or already answered", http.StatusNotFound)
		return
	}
	// htmx reload trigger.
	w.Header().Set("HX-Trigger", "questionsChanged")
	_, _ = io.WriteString(w, `<div class="rounded-md bg-emerald-500/10 border border-emerald-500/30 px-3 py-2 text-sm text-emerald-700 dark:text-emerald-400">recorded ✓ — daemon resumes on next poll</div>`)
}

// handleDaemonPause flips the daemon's Paused flag in shared memory.
// Returns the *alternate* button (resume) so the htmx outerHTML swap
// produces a working control, not a dead-end "paused ✓" text. The
// HX-Trigger fires statusChanged so the header pill + every panel
// listening for it refreshes within the same paint.
func (s *Server) handleDaemonPause(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	s.opts.Memory.SetPaused(true)
	w.Header().Set("HX-Trigger", "statusChanged")
	_, _ = io.WriteString(w, resumeButton())
}

// handleDaemonResume clears the Paused flag.
func (s *Server) handleDaemonResume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	s.opts.Memory.SetPaused(false)
	w.Header().Set("HX-Trigger", "statusChanged")
	_, _ = io.WriteString(w, pauseButton())
}

// pauseButton / resumeButton emit the matching toggle HTML so the
// pause↔resume swap is symmetric and never leaves the user without a
// way to flip it back. Centralised so fragStatus and the API
// handlers render identical markup.
func pauseButton() string {
	return `<button hx-post="/api/daemon/pause" hx-target="this" hx-swap="outerHTML"
		class="inline-flex items-center gap-2 rounded-md bg-amber-500/10 text-amber-700 dark:text-amber-400 border border-amber-500/30 px-3 py-1.5 text-sm font-medium hover:bg-amber-500/20 transition">
		<span>⏸</span><span>pause polling</span>
	</button>`
}
func resumeButton() string {
	return `<button hx-post="/api/daemon/resume" hx-target="this" hx-swap="outerHTML"
		class="inline-flex items-center gap-2 rounded-md bg-emerald-500/10 text-emerald-700 dark:text-emerald-400 border border-emerald-500/30 px-3 py-1.5 text-sm font-medium hover:bg-emerald-500/20 transition">
		<span>▶</span><span>resume polling</span>
	</button>`
}

// configKey describes one settable env var the web UI exposes.
type configKey struct {
	Name      string
	Default   string
	Sensitive bool
	Group     string
	Hint      string
}

// webConfigKeys mirrors cmd/config.go's knownConfigKeys but is local to the
// web package so we don't reach across boundaries. Keep them in sync.
var webConfigKeys = []configKey{
	{Name: "GOON_LLM_PROVIDER", Default: "openai", Group: "agent", Hint: "openai | anthropic | ollama | mock"},
	{Name: "GOON_BOARD", Group: "agent", Hint: "jira | github | mock"},
	{Name: "GOON_GIT_HOST", Group: "agent", Hint: "github | gitlab | bitbucket | mock (optional)"},
	{Name: "GOON_POLL_SECONDS", Default: "300", Group: "agent"},
	{Name: "GOON_VERIFY_RUNS", Default: "3", Group: "agent"},
	{Name: "GOON_REPO_MAP", Group: "agent", Hint: `e.g. ENG=/repos/eng,*=/repos/default`},

	{Name: "OPENAI_API_KEY", Sensitive: true, Group: "openai"},
	{Name: "OPENAI_MODEL", Default: "gpt-4o-mini", Group: "openai"},
	{Name: "OPENAI_BASE_URL", Default: "https://api.openai.com/v1", Group: "openai", Hint: "override for proxy / Azure"},

	{Name: "ANTHROPIC_API_KEY", Sensitive: true, Group: "anthropic"},
	{Name: "ANTHROPIC_MODEL", Default: "claude-sonnet-4-5", Group: "anthropic"},
	{Name: "ANTHROPIC_BASE_URL", Default: "https://api.anthropic.com/v1", Group: "anthropic"},

	{Name: "OLLAMA_BASE_URL", Default: "http://localhost:11434", Group: "ollama"},
	{Name: "OLLAMA_MODEL", Default: "llama3", Group: "ollama"},

	// Shared Atlassian credentials. Both Jira and Confluence fall back to
	// these, so a typical Cloud user only fills these three.
	{Name: "ATLASSIAN_BASE_URL", Group: "atlassian", Hint: "e.g. https://acme.atlassian.net — covers both Jira and Confluence"},
	{Name: "ATLASSIAN_EMAIL", Group: "atlassian"},
	{Name: "ATLASSIAN_API_TOKEN", Sensitive: true, Group: "atlassian", Hint: "from id.atlassian.com/manage-profile/security/api-tokens"},

	{Name: "JIRA_BASE_URL", Group: "jira", Hint: "leave empty to use ATLASSIAN_BASE_URL"},
	{Name: "JIRA_EMAIL", Group: "jira", Hint: "leave empty to use ATLASSIAN_EMAIL"},
	{Name: "JIRA_API_TOKEN", Sensitive: true, Group: "jira", Hint: "leave empty to use ATLASSIAN_API_TOKEN"},
	{Name: "JIRA_JQL", Group: "jira", Hint: "default: assignee=currentUser() AND statusCategory!=Done"},

	{Name: "CONFLUENCE_BASE_URL", Group: "confluence", Hint: "leave empty to use ATLASSIAN_BASE_URL + /wiki"},
	{Name: "CONFLUENCE_EMAIL", Group: "confluence", Hint: "leave empty to use ATLASSIAN_EMAIL"},
	{Name: "CONFLUENCE_API_TOKEN", Sensitive: true, Group: "confluence", Hint: "leave empty to use ATLASSIAN_API_TOKEN"},

	{Name: "GITHUB_TOKEN", Sensitive: true, Group: "github"},
	{Name: "GITHUB_REPOS", Group: "github", Hint: "comma-separated owner/repo,owner/repo"},
	{Name: "GITHUB_LABEL", Group: "github"},
	{Name: "GITHUB_ASSIGNEE", Default: "@me", Group: "github"},
	{Name: "GITHUB_API_URL", Default: "https://api.github.com", Group: "github", Hint: "override for GitHub Enterprise"},

	{Name: "GITLAB_TOKEN", Sensitive: true, Group: "gitlab"},
	{Name: "GITLAB_API_URL", Default: "https://gitlab.com/api/v4", Group: "gitlab"},

	{Name: "BITBUCKET_TOKEN", Sensitive: true, Group: "bitbucket"},
	{Name: "BITBUCKET_USERNAME", Group: "bitbucket"},
	{Name: "BITBUCKET_APP_PASSWORD", Sensitive: true, Group: "bitbucket"},
	{Name: "BITBUCKET_API_URL", Default: "https://api.bitbucket.org/2.0", Group: "bitbucket"},

	{Name: "TELEGRAM_BOT_TOKEN", Sensitive: true, Group: "telegram"},
	{Name: "TELEGRAM_CHAT_ID", Group: "telegram"},
	{Name: "TELEGRAM_API_BASE_URL", Default: "https://api.telegram.org", Group: "telegram"},
}

// handleAPIConfig serves both reads (GET) and writes (POST).
//
// GET  /api/config             → JSON map of all known keys (secrets masked unless ?reveal=1)
// POST /api/config  KEY=VAL ...→ form-encoded; writes to ~/.config/goon/.env, sets os.Setenv,
//
//	triggers daemon Reconfigure, and returns a fragment.
func (s *Server) handleAPIConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.serveConfigJSON(w, r)
	case http.MethodPost:
		s.serveConfigWrite(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) serveConfigJSON(w http.ResponseWriter, r *http.Request) {
	reveal := r.URL.Query().Get("reveal") == "1"
	out := map[string]string{}
	for _, k := range webConfigKeys {
		v := envEcho(k.Name)
		if k.Sensitive && !reveal {
			v = mask(v)
		}
		out[k.Name] = v
	}
	writeJSON(w, out)
}

func (s *Server) serveConfigWrite(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	written := []string{}
	for _, k := range webConfigKeys {
		val, ok := r.Form[k.Name]
		if !ok {
			continue
		}
		v := strings.TrimSpace(val[0])
		// Empty form value = unset.
		if v == "" {
			_ = unsetConfigKey(k.Name)
			_ = os.Unsetenv(k.Name)
			continue
		}
		if err := setConfigKey(k.Name, v); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = os.Setenv(k.Name, v)
		written = append(written, k.Name)
	}
	// Hot-reload providers.
	notes := []string{}
	if s.opts.Daemon != nil {
		notes = s.opts.Daemon.Reconfigure()
	}
	// Saving config calls Reconfigure under the hood, which mutates
	// Memory.Status (BoardName, HostName). Fire BOTH triggers so the
	// header pill (statusChanged) and the form re-listeners
	// (configChanged) all refresh in the same paint. htmx accepts
	// comma-separated triggers.
	w.Header().Set("HX-Trigger", "configChanged, statusChanged")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<div class="rounded-md bg-emerald-500/10 border border-emerald-500/30 px-3 py-2 text-sm text-emerald-700 dark:text-emerald-400">saved %d field(s) ✓</div>`, len(written))
	if len(notes) > 0 {
		fmt.Fprint(w, `<ul class="mt-2 space-y-1 text-xs font-mono">`)
		for _, n := range notes {
			cls := "text-emerald-700 dark:text-emerald-400"
			if strings.HasPrefix(n, "✗") {
				cls = "text-rose-700 dark:text-rose-400"
			}
			fmt.Fprintf(w, `<li class="%s">%s</li>`, cls, html.EscapeString(n))
		}
		fmt.Fprint(w, `</ul>`)
	}
}

// handleConfigVerify accepts the same form payload as POST /api/config but
// does NOT persist anything — it temporarily applies the form values to the
// process env, runs the live probes via internal/checkup, then restores the
// previous env. Returns an HTML fragment with one row per probe.
func (s *Server) handleConfigVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	override := map[string]string{}
	for _, k := range webConfigKeys {
		if vals, ok := r.Form[k.Name]; ok {
			override[k.Name] = strings.TrimSpace(vals[0])
		}
	}
	results := checkup.RunWithEnvOverride(r.Context(), override)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	renderProbeResults(w, results)
}

func renderProbeResults(w io.Writer, rs []checkup.Result) {
	allOK := checkup.AllOK(rs)
	header := `<div class="rounded-md bg-emerald-500/10 border border-emerald-500/30 px-3 py-2 text-sm text-emerald-700 dark:text-emerald-400">verify: all checks passed ✓</div>`
	if !allOK {
		header = `<div class="rounded-md bg-rose-500/10 border border-rose-500/30 px-3 py-2 text-sm text-rose-700 dark:text-rose-400">verify: failures detected</div>`
	}
	fmt.Fprint(w, header)
	fmt.Fprint(w, `<ul class="mt-2 space-y-1 text-xs font-mono">`)
	for _, r := range rs {
		cls := "text-emerald-700 dark:text-emerald-400"
		mark := "✓"
		switch {
		case r.Skipped:
			cls = "text-gray-500"
			mark = "·"
		case !r.OK:
			cls = "text-rose-700 dark:text-rose-400"
			mark = "✗"
		}
		id := r.Component
		if r.Name != "" {
			id += "/" + r.Name
		}
		fmt.Fprintf(w, `<li class="%s">%s <strong>%s</strong> — %s</li>`,
			cls, mark, html.EscapeString(id), html.EscapeString(r.Detail))
	}
	fmt.Fprint(w, `</ul>`)
}

// fragConfig renders the editable config form. Sensitive fields display the
// masked value as the placeholder so the user can see "something is set"
// without the secret being in HTML. All output is Tailwind-classed for
// the redesigned dashboard.
func (s *Server) fragConfig(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	groups := groupKeys(webConfigKeys)
	// Order matters — top-of-form should be the most-likely-to-edit
	// groups (provider + Atlassian shared creds), then per-product
	// overrides, then optional integrations.
	order := []string{"agent", "openai", "anthropic", "ollama", "atlassian", "jira", "confluence", "github", "gitlab", "bitbucket", "telegram"}
	fmt.Fprint(w, `<form hx-post="/api/config" hx-target="#cfg-result" hx-swap="innerHTML" class="space-y-6">`)
	for _, g := range order {
		ks, ok := groups[g]
		if !ok {
			continue
		}
		fmt.Fprintf(w, `<fieldset class="rounded-lg border border-gray-200 dark:border-gray-800 bg-white dark:bg-surface-raised p-4">`)
		fmt.Fprintf(w, `<legend class="px-2 text-xs font-semibold uppercase tracking-wider text-accent">%s</legend>`, html.EscapeString(g))
		fmt.Fprint(w, `<div class="grid grid-cols-1 sm:grid-cols-[220px_1fr] gap-x-4 gap-y-3 mt-2">`)
		for _, k := range ks {
			val := envEcho(k.Name)
			disp := ""
			placeholder := ""
			if k.Sensitive && val != "" {
				placeholder = mask(val)
			} else if k.Default != "" {
				placeholder = k.Default
			} else if k.Hint != "" {
				placeholder = k.Hint
			}
			if !k.Sensitive {
				disp = val
			}
			tp := "text"
			if k.Sensitive {
				tp = "password"
			}
			fmt.Fprintf(w, `<label for="cfg-%s" class="font-mono text-xs text-gray-500 dark:text-gray-400 sm:text-right sm:pt-2">%s</label>`,
				html.EscapeString(k.Name), html.EscapeString(k.Name))
			fmt.Fprintf(w, `<div><input id="cfg-%s" type="%s" name="%s" value="%s" placeholder="%s" autocomplete="off"
                class="w-full font-mono text-sm rounded-md border border-gray-300 dark:border-gray-700 bg-white dark:bg-surface px-3 py-1.5 focus:border-accent focus:ring-1 focus:ring-accent focus:outline-none">`,
				html.EscapeString(k.Name), tp, html.EscapeString(k.Name),
				html.EscapeString(disp), html.EscapeString(placeholder))
			if k.Hint != "" && !k.Sensitive && k.Default == "" {
				fmt.Fprintf(w, `<p class="mt-1 text-xs text-gray-500 dark:text-gray-500">%s</p>`, html.EscapeString(k.Hint))
			}
			fmt.Fprint(w, `</div>`)
		}
		fmt.Fprint(w, `</div></fieldset>`)
	}
	fmt.Fprint(w, `<div class="flex flex-wrap items-center gap-3 pt-2">`)
	fmt.Fprint(w, `<button type="button"
        hx-post="/api/config/verify"
        hx-include="closest form"
        hx-target="#cfg-result"
        hx-swap="innerHTML"
        hx-indicator="#cfg-spinner"
        class="inline-flex items-center gap-2 rounded-md border border-accent text-accent px-4 py-2 text-sm font-medium hover:bg-accent-soft transition-colors">verify connection</button>`)
	fmt.Fprint(w, `<button type="submit" hx-indicator="#cfg-spinner"
        class="inline-flex items-center gap-2 rounded-md bg-accent text-surface px-4 py-2 text-sm font-semibold hover:brightness-110 transition">save &amp; reload daemon</button>`)
	fmt.Fprint(w, `<span id="cfg-spinner" class="htmx-indicator text-xs text-gray-500">⏳ probing…</span>`)
	fmt.Fprint(w, `</div>`)
	fmt.Fprint(w, `<div id="cfg-result" class="mt-2"></div>`)
	fmt.Fprint(w, `</form>`)
}

// fragSetup renders a banner if the daemon isn't fully configured yet.
func (s *Server) fragSetup(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	configured := s.opts.Daemon == nil || s.opts.Daemon.Configured()
	if configured {
		_, _ = io.WriteString(w, ``)
		return
	}
	_, _ = io.WriteString(w, `<div class="mt-4 rounded-lg border border-accent/40 bg-accent-soft p-4 text-sm">
  <strong class="text-accent">👋 Welcome to goon.</strong>
  Configure your LLM provider and ticket board on the
  <button type="button" onclick="document.querySelector('button[data-tab=config]').click()" class="underline hover:text-accent font-medium">Configuration</button>
  tab to start auto-engineering.
</div>`)
}

func groupKeys(keys []configKey) map[string][]configKey {
	out := map[string][]configKey{}
	for _, k := range keys {
		out[k.Group] = append(out[k.Group], k)
	}
	for g := range out {
		sort.Slice(out[g], func(i, j int) bool { return out[g][i].Name < out[g][j].Name })
	}
	return out
}

func mask(v string) string {
	if v == "" {
		return ""
	}
	if len(v) <= 6 {
		return "***"
	}
	return v[:2] + "…" + v[len(v)-3:]
}

// --- HTML fragments (htmx) -------------------------------------------------
//
// Every fragment emits Tailwind-classed markup compatible with the
// dashboard shell at static/index.html. Markup is intentionally
// self-contained — class names live in this Go file rather than a
// separate stylesheet so the binary stays single-file.

// fragStatus renders the full status panel used inside the Overview tab.
// Visual hierarchy: status is the headline, everything else a tidy
// key/value grid. The pause/resume button lives at the bottom.
func (s *Server) fragStatus(w http.ResponseWriter, _ *http.Request) {
	st := s.opts.Memory.GetStatus()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	state, dotClass := statusBadge(st)
	last := "—"
	if !st.LastPoll.IsZero() {
		last = humanizeAgo(time.Since(st.LastPoll))
	}
	// Headline + dot.
	fmt.Fprintf(w, `<div class="flex items-center gap-3 pb-4 mb-4 border-b border-gray-200 dark:border-surface-border">
		<span class="relative flex h-3 w-3">
			<span class="absolute inline-flex h-full w-full rounded-full %s opacity-50 animate-ping"></span>
			<span class="relative inline-flex rounded-full h-3 w-3 %s"></span>
		</span>
		<div class="flex-1">
			<div class="text-xs uppercase tracking-wider text-gray-500">status</div>
			<div class="text-base font-semibold capitalize">%s</div>
		</div>
		<div class="text-right">
			<div class="text-xs text-gray-500">last poll</div>
			<div class="text-sm font-mono">%s</div>
		</div>
	</div>`, dotClass, dotClass, state, html.EscapeString(last))

	// Tidy KV grid for the rest.
	fmt.Fprint(w, `<dl class="space-y-2.5 text-sm">`)
	statusKV(w, "board", st.BoardName)
	statusKV(w, "git host", st.HostName)
	if st.LastTicket != "" {
		statusKV(w, "last ticket", st.LastTicket)
	}
	if st.ActiveWorkflow != "" {
		statusKV(w, "active workflow", st.ActiveWorkflow)
	}
	if st.PID != 0 {
		statusKV(w, "pid", fmt.Sprintf("%d", st.PID))
	}
	fmt.Fprint(w, `</dl>`)

	// Show the pause/resume control whenever there's something to
	// toggle. When daemon is running we offer the obvious toggle.
	// When daemon is STOPPED but Paused=true (user paused before
	// starting, or daemon crashed), still show the resume button
	// with a hint — otherwise the only way to clear the flag is
	// via CLI, which the web user may not realize.
	switch {
	case st.Running:
		fmt.Fprint(w, `<div class="mt-4">`)
		if st.Paused {
			fmt.Fprintln(w, resumeButton())
		} else {
			fmt.Fprintln(w, pauseButton())
		}
		fmt.Fprint(w, `</div>`)
	case st.Paused:
		fmt.Fprint(w, `<div class="mt-4 space-y-2">`)
		fmt.Fprint(w, `<div class="text-xs text-gray-500">Daemon is stopped but the pause flag is set — clear it before next start.</div>`)
		fmt.Fprintln(w, resumeButton())
		fmt.Fprint(w, `</div>`)
	}
}

// statusKV is a one-line rendering helper for the status panel.
func statusKV(w http.ResponseWriter, k, v string) {
	if v == "" {
		v = "—"
	}
	fmt.Fprintf(w, `<div class="flex items-baseline justify-between gap-3">
		<dt class="text-xs uppercase tracking-wider text-gray-500">%s</dt>
		<dd class="font-mono text-sm text-gray-800 dark:text-gray-200 truncate">%s</dd>
	</div>`, html.EscapeString(k), html.EscapeString(v))
}

// statusBadge maps a DaemonStatus to a (label, dot-color-class) pair.
// Stopped is intentionally neutral-grey (not red): it's the cold-start
// zero-state for a fresh process before Reconfigure fires, and a red
// indicator there reads like an error rather than "no work yet."
// Red is reserved for actual failure surfaces.
func statusBadge(st memory.DaemonStatus) (string, string) {
	switch {
	case !st.Running:
		return "stopped", "bg-gray-400 dark:bg-gray-500"
	case st.Paused:
		return "paused", "bg-amber-400"
	default:
		return "running", "bg-emerald-500 pulse-dot"
	}
}

// fragStatusPill is the compact header chip shown next to the brand.
// Pulsing dot when running, neutral when stopped/paused. Hover reveals
// the last-poll time (kept off the default render so the header stays
// quiet on narrow screens).
func (s *Server) fragStatusPill(w http.ResponseWriter, _ *http.Request) {
	st := s.opts.Memory.GetStatus()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	state, dotClass := statusBadge(st)
	last := "never"
	if !st.LastPoll.IsZero() {
		last = humanizeAgo(time.Since(st.LastPoll))
	}
	fmt.Fprintf(w, `<div class="inline-flex items-center gap-2 px-2.5 py-1 rounded-full border border-gray-200 dark:border-surface-border bg-gray-50 dark:bg-surface text-[11px] font-medium text-gray-700 dark:text-gray-300" title="Last poll: %s">
		<span class="relative flex h-2 w-2">
			<span class="absolute inline-flex h-full w-full rounded-full %s opacity-60 animate-ping"></span>
			<span class="relative inline-flex rounded-full h-2 w-2 %s"></span>
		</span>
		<span class="uppercase tracking-wider">%s</span>
		<span class="hidden md:inline text-gray-500">·</span>
		<span class="hidden md:inline text-gray-500 font-mono">%s</span>
	</div>`, html.EscapeString(last), dotClass, dotClass, state, html.EscapeString(last))
}

// fragQuestionsBanner renders a yellow strip above the tabs when there
// are pending approvals — the user's most blocking signal. Empty body
// when nothing is pending so the page doesn't reserve vertical space.
func (s *Server) fragQuestionsBanner(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	pending := s.opts.Memory.PendingQuestions()
	if len(pending) == 0 {
		_, _ = io.WriteString(w, ``)
		return
	}
	plural := "question"
	if len(pending) > 1 {
		plural = "questions"
	}
	fmt.Fprintf(w, `<div class="mt-4 rounded-lg border border-amber-500/40 bg-amber-500/10 px-4 py-3 flex flex-wrap items-center gap-3 text-sm">
		<span class="text-amber-700 dark:text-amber-400 text-lg">⏸</span>
		<div class="flex-1 min-w-0">
			<strong class="text-amber-700 dark:text-amber-300">%d pending %s</strong>
			<span class="text-gray-700 dark:text-gray-400"> — workflows are paused waiting for your approval.</span>
		</div>
		<button type="button" onclick="document.querySelector('button[data-tab=questions]').click()"
			class="rounded-md bg-amber-500 text-gray-900 px-3 py-1 text-xs font-semibold hover:bg-amber-400 transition">
			Review
		</button>
	</div>`, len(pending), plural)
}

// fragTickets renders the ticket table for the Tickets tab. Includes
// assignee/project columns now that those are stored in
// memory.TicketSnapshot. Rows carry data-status and data-ticket-row so
// the tab's client-side filter can target them.
func (s *Server) fragTickets(w http.ResponseWriter, _ *http.Request) {
	tks := s.opts.Memory.ListTickets()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if len(tks) == 0 {
		_, _ = io.WriteString(w, emptyState("No tickets yet",
			"Set GOON_BOARD on the Configuration tab and the daemon will populate this table on its next poll. Or hit the Refresh button to pull now."))
		return
	}
	// Most-recently-updated first.
	sort.Slice(tks, func(i, j int) bool {
		return tks[i].UpdatedAt.After(tks[j].UpdatedAt)
	})
	fmt.Fprint(w, `<div class="overflow-x-auto rounded-lg border border-gray-200 dark:border-surface-border bg-white dark:bg-surface-raised shadow-card">
	<table class="min-w-full text-sm">
		<thead class="border-b border-gray-200 dark:border-surface-border text-[11px] uppercase tracking-wider text-gray-500 bg-gray-50/50 dark:bg-surface-sunken/40">
			<tr>
				<th class="px-4 py-2.5 text-left font-semibold">Key</th>
				<th class="px-4 py-2.5 text-left font-semibold">Title</th>
				<th class="px-4 py-2.5 text-left font-semibold">Status</th>
				<th class="px-4 py-2.5 text-left font-semibold">Assignee</th>
				<th class="px-4 py-2.5 text-left font-semibold">Project</th>
				<th class="px-4 py-2.5 text-left font-semibold">Updated</th>
			</tr>
		</thead>
		<tbody class="divide-y divide-gray-100 dark:divide-surface-border/60">`)
	for _, t := range tks {
		key := html.EscapeString(t.Key)
		if t.URL != "" {
			key = fmt.Sprintf(`<a href="%s" target="_blank" rel="noopener" class="text-accent hover:underline inline-flex items-center gap-1">%s<svg class="h-3 w-3 opacity-50" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M7 17L17 7"/><path d="M7 7h10v10"/></svg></a>`,
				html.EscapeString(t.URL), html.EscapeString(t.Key))
		}
		assignee := html.EscapeString(t.Assignee)
		if assignee == "" {
			assignee = `<span class="text-gray-400">—</span>`
		}
		project := html.EscapeString(t.Project)
		if project == "" {
			project = `<span class="text-gray-400">—</span>`
		}
		fmt.Fprintf(w, `<tr data-ticket-row data-status="%s" class="hover:bg-gray-50 dark:hover:bg-surface-sunken/40 transition-colors">
			<td class="px-4 py-2.5 font-mono whitespace-nowrap">%s</td>
			<td class="px-4 py-2.5 max-w-md truncate">%s</td>
			<td class="px-4 py-2.5 whitespace-nowrap">%s</td>
			<td class="px-4 py-2.5 text-gray-700 dark:text-gray-300 whitespace-nowrap">%s</td>
			<td class="px-4 py-2.5 font-mono text-xs text-gray-600 dark:text-gray-400 whitespace-nowrap">%s</td>
			<td class="px-4 py-2.5 text-gray-500 text-xs whitespace-nowrap">%s</td>
		</tr>`, html.EscapeString(strings.ToLower(t.Status)), key, html.EscapeString(t.Title),
			ticketStatusPill(t.Status), assignee, project,
			html.EscapeString(fuzzyTime(t.UpdatedAt)))
	}
	fmt.Fprint(w, `</tbody></table></div>`)
}

// ticketStatusPill renders a small colored badge for a ticket status.
// Each tone uses paired light/dark text colors so the pill stays
// readable against both surface modes (light = darker text, dark =
// lighter text).
func ticketStatusPill(status string) string {
	low := strings.ToLower(strings.TrimSpace(status))
	cls := "bg-gray-500/15 text-gray-700 dark:text-gray-400 border-gray-500/30"
	switch {
	case low == "" || strings.Contains(low, "open") || strings.Contains(low, "todo") || strings.Contains(low, "ready") || strings.Contains(low, "backlog"):
		cls = "bg-sky-500/15 text-sky-700 dark:text-sky-400 border-sky-500/30"
	case strings.Contains(low, "progress") || strings.Contains(low, "doing"):
		cls = "bg-amber-500/15 text-amber-700 dark:text-amber-400 border-amber-500/30"
	case strings.Contains(low, "review"):
		cls = "bg-violet-500/15 text-violet-700 dark:text-violet-400 border-violet-500/30"
	case strings.Contains(low, "done") || strings.Contains(low, "closed") || strings.Contains(low, "resolved") || strings.Contains(low, "merged"):
		cls = "bg-emerald-500/15 text-emerald-700 dark:text-emerald-400 border-emerald-500/30"
	case strings.Contains(low, "block"):
		cls = "bg-rose-500/15 text-rose-700 dark:text-rose-400 border-rose-500/30"
	}
	label := status
	if label == "" {
		label = "—"
	}
	return fmt.Sprintf(`<span class="inline-flex items-center rounded-full border px-2 py-0.5 text-xs font-medium %s">%s</span>`,
		cls, html.EscapeString(label))
}

// fragWorkflows renders the workflow card list for the Workflows tab.
// Cards beat tables here — each workflow has plan progress that needs
// vertical space, and rows would crowd it.
func (s *Server) fragWorkflows(w http.ResponseWriter, _ *http.Request) {
	wfs := s.opts.Memory.ListWorkflows(50)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if len(wfs) == 0 {
		_, _ = io.WriteString(w, emptyState("No workflows yet.",
			"Workflow runs appear here as soon as a ticket is picked up. Each card shows plan progress, the PR link, and any pending approval."))
		return
	}
	fmt.Fprint(w, `<div class="grid grid-cols-1 lg:grid-cols-2 gap-4">`)
	for _, wf := range wfs {
		done, total := 0, len(wf.Plan)
		for _, ps := range wf.Plan {
			if ps.Done {
				done++
			}
		}
		pct := 0
		if total > 0 {
			pct = (done * 100) / total
		}
		stateChip := workflowStateChip(string(wf.State))
		ticket := html.EscapeString(wf.TicketKey)
		title := html.EscapeString(wf.Title)
		stage := html.EscapeString(wf.Stage)
		if stage == "" {
			stage = "—"
		}

		// Pick a left-edge accent strip that matches the workflow state.
		edgeTone := "bg-gray-300 dark:bg-gray-700"
		switch wf.State {
		case memory.WFDone:
			edgeTone = "bg-emerald-500"
		case memory.WFFailed:
			edgeTone = "bg-rose-500"
		case memory.WFAwaitingApproval:
			edgeTone = "bg-amber-500"
		case memory.WFTriaging:
			edgeTone = "bg-violet-500"
		default:
			if total > 0 && pct < 100 {
				edgeTone = "bg-accent"
			}
		}

		// Pick a progress-bar fill matching tone too.
		barTone := "bg-accent"
		if wf.State == memory.WFDone {
			barTone = "bg-emerald-500"
		} else if wf.State == memory.WFFailed {
			barTone = "bg-rose-500"
		} else if wf.State == memory.WFAwaitingApproval {
			barTone = "bg-amber-500"
		}

		fmt.Fprint(w, `<div class="relative overflow-hidden rounded-xl border border-gray-200 dark:border-surface-border bg-white dark:bg-surface-raised p-5 shadow-card hover:shadow-lift transition-shadow">`)
		fmt.Fprintf(w, `<div class="absolute left-0 top-0 bottom-0 w-1 %s"></div>`, edgeTone)
		fmt.Fprintf(w, `<div class="flex items-start justify-between gap-3 mb-3">
			<div class="min-w-0 flex-1">
				<div class="font-mono text-sm font-semibold text-gray-900 dark:text-gray-100">%s</div>
				<div class="mt-0.5 text-sm text-gray-600 dark:text-gray-400 truncate" title="%s">%s</div>
			</div>
			%s
		</div>`, ticket, title, title, stateChip)

		// Meta row — stage + updated.
		fmt.Fprintf(w, `<div class="flex items-center gap-4 text-[11px] uppercase tracking-wider text-gray-500 mb-4">
			<div class="flex items-center gap-1.5">
				<svg class="h-3 w-3" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="4 17 10 11 4 5"/><line x1="12" y1="19" x2="20" y2="19"/></svg>
				<span>stage</span>
				<span class="font-mono normal-case tracking-normal text-gray-700 dark:text-gray-300">%s</span>
			</div>
			<div class="flex items-center gap-1.5">
				<svg class="h-3 w-3" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><polyline points="12 6 12 12 16 14"/></svg>
				<span class="normal-case tracking-normal text-gray-500">%s</span>
			</div>
		</div>`, stage, html.EscapeString(fuzzyTime(wf.UpdatedAt)))

		// Plan progress bar — only visible when plan exists.
		if total > 0 {
			fmt.Fprintf(w, `<div class="mb-3">
				<div class="flex items-center justify-between text-xs mb-1.5">
					<span class="text-gray-500 dark:text-gray-400">plan progress</span>
					<span class="font-mono text-gray-700 dark:text-gray-300">%d / %d <span class="text-gray-400">· %d%%</span></span>
				</div>
				<div class="h-2 w-full rounded-full bg-gray-100 dark:bg-surface-sunken overflow-hidden">
					<div class="h-full %s transition-all duration-500" style="width: %d%%"></div>
				</div>
			</div>`, done, total, pct, barTone, pct)
		} else {
			fmt.Fprintf(w, `<div class="mb-3 text-xs text-gray-400 italic">no plan yet</div>`)
		}

		// PR link — pill button style.
		if wf.PRURL != "" {
			fmt.Fprintf(w, `<a href="%s" target="_blank" rel="noopener" class="group inline-flex items-center gap-1.5 max-w-full rounded-md border border-accent/30 bg-accent-soft/40 hover:bg-accent-soft hover:border-accent px-2.5 py-1 text-xs font-medium text-accent transition">
				<svg class="h-3.5 w-3.5 shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="18" cy="18" r="3"/><circle cx="6" cy="6" r="3"/><path d="M13 6h3a2 2 0 0 1 2 2v7"/><line x1="6" y1="9" x2="6" y2="21"/></svg>
				<span class="font-mono truncate min-w-0">%s</span>
				<svg class="h-3 w-3 shrink-0 opacity-50 group-hover:opacity-100 transition-opacity" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M7 17L17 7"/><polyline points="7 7 17 7 17 17"/></svg>
			</a>`, html.EscapeString(wf.PRURL), html.EscapeString(wf.PRURL))
		}

		// Pending question hint — amber callout.
		if wf.PendingQuestionID != "" {
			fmt.Fprintf(w, `<button type="button" onclick="document.querySelector('button[data-tab=questions]')?.click()" class="mt-3 flex items-center gap-2 w-full text-left rounded-md border border-amber-500/30 bg-amber-500/5 hover:bg-amber-500/10 px-3 py-2 text-xs text-amber-700 dark:text-amber-400 transition">
				<svg class="h-4 w-4 shrink-0 animate-pulse-dot" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><line x1="10" y1="9" x2="10" y2="15"/><line x1="14" y1="9" x2="14" y2="15"/></svg>
				<span class="flex-1">paused — awaiting <span class="font-mono">%s</span></span>
				<span class="font-medium opacity-80 group-hover:opacity-100">answer →</span>
			</button>`, html.EscapeString(wf.PendingQuestionID))
		}

		// Error — rose callout.
		if wf.Error != "" {
			fmt.Fprintf(w, `<div class="mt-3 flex items-start gap-2 rounded-md border border-rose-500/30 bg-rose-500/5 px-3 py-2 text-xs text-rose-700 dark:text-rose-400">
				<svg class="h-4 w-4 shrink-0 mt-0.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><line x1="12" y1="8" x2="12" y2="12"/><line x1="12" y1="16" x2="12.01" y2="16"/></svg>
				<span class="break-words">%s</span>
			</div>`, html.EscapeString(wf.Error))
		}

		fmt.Fprint(w, `</div>`)
	}
	fmt.Fprint(w, `</div>`)
}

// workflowStateChip renders a small colored badge for a workflow state.
// Paired light/dark text colors keep the chip readable on both modes.
func workflowStateChip(state string) string {
	cls := "bg-gray-500/15 text-gray-700 dark:text-gray-400 border-gray-500/30"
	switch state {
	case "done":
		cls = "bg-emerald-500/15 text-emerald-700 dark:text-emerald-400 border-emerald-500/30"
	case "failed":
		cls = "bg-rose-500/15 text-rose-700 dark:text-rose-400 border-rose-500/30"
	case "awaiting_approval":
		cls = "bg-amber-500/15 text-amber-700 dark:text-amber-400 border-amber-500/30"
	case "executing", "testing", "verifying", "opening_pr", "notifying", "updating_memory":
		cls = "bg-sky-500/15 text-sky-700 dark:text-sky-400 border-sky-500/30"
	case "triaging", "planning":
		cls = "bg-violet-500/15 text-violet-700 dark:text-violet-400 border-violet-500/30"
	}
	return fmt.Sprintf(`<span class="inline-flex shrink-0 items-center rounded-full border px-2 py-0.5 text-xs font-medium %s">%s</span>`,
		cls, html.EscapeString(state))
}

// fragQuestions renders the pending-questions list with answer forms.
func (s *Server) fragQuestions(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	pending := s.opts.Memory.PendingQuestions()
	if len(pending) == 0 {
		_, _ = io.WriteString(w, emptyState("All clear — nothing waiting on you ✓",
			"When a workflow hits an approval gate (confirm_repo / approve_plan) it parks the question here."))
		return
	}
	fmt.Fprint(w, `<div class="space-y-3">`)
	for _, q := range pending {
		// Try to guess at the gate type so we can label appropriately.
		gateLabel := "approval"
		gateTone := "bg-amber-500/15 text-amber-700 dark:text-amber-400 border-amber-500/30"
		ql := strings.ToLower(q.Question)
		switch {
		case strings.Contains(ql, "confirm_repo") || strings.Contains(ql, "repo path") || strings.Contains(ql, "which repo"):
			gateLabel = "confirm repo"
		case strings.Contains(ql, "approve") || strings.Contains(ql, "plan"):
			gateLabel = "approve plan"
			gateTone = "bg-violet-500/15 text-violet-700 dark:text-violet-400 border-violet-500/30"
		}
		ticketLabel := ""
		if q.TicketID != "" {
			ticketLabel = fmt.Sprintf(`<span class="inline-flex items-center gap-1 text-xs font-mono text-gray-500 dark:text-gray-400"><svg class="h-3 w-3" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M20.59 13.41l-7.17 7.17a2 2 0 0 1-2.83 0L2 12V2h10l8.59 8.59a2 2 0 0 1 0 2.82z"/><line x1="7" y1="7" x2="7.01" y2="7"/></svg>%s</span>`, html.EscapeString(q.TicketID))
		}
		fmt.Fprintf(w, `<form hx-post="/api/answer" hx-target="this" hx-swap="outerHTML"
			class="relative overflow-hidden rounded-xl border border-amber-500/40 bg-white dark:bg-surface-raised shadow-card hover:shadow-lift transition-shadow">
			<div class="absolute left-0 top-0 bottom-0 w-1 bg-amber-500"></div>
			<div class="px-5 py-4 space-y-3">
				<div class="flex items-center gap-2 flex-wrap">
					<span class="inline-flex items-center gap-1 rounded-full border px-2 py-0.5 text-[11px] font-semibold uppercase tracking-wider %s">
						<svg class="h-3 w-3 animate-pulse-dot" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><circle cx="12" cy="12" r="4"/></svg>
						%s
					</span>
					%s
					<span class="text-[11px] font-mono text-gray-400 ml-auto">id %s</span>
				</div>
				<div class="text-sm text-gray-800 dark:text-gray-200 whitespace-pre-line leading-relaxed">%s</div>
				<input type="hidden" name="id" value="%s">
				<div class="flex flex-col sm:flex-row gap-2 pt-1">
					<input type="text" name="answer" autocomplete="off" autofocus
						placeholder="yes &nbsp;·&nbsp; no &nbsp;·&nbsp; change=/path/to/repo &nbsp;·&nbsp; free-form feedback"
						class="flex-1 font-mono text-sm rounded-lg border border-gray-300 dark:border-surface-border bg-white dark:bg-surface px-3 py-2 focus:border-accent focus:ring-2 focus:ring-accent/30 focus:outline-none">
					<div class="flex gap-2">
						<button type="submit" name="answer" value="yes" formnovalidate
							class="inline-flex items-center gap-1 rounded-lg bg-emerald-500 text-white px-3 py-2 text-sm font-semibold hover:bg-emerald-600 transition">
							<svg class="h-4 w-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><polyline points="20 6 9 17 4 12"/></svg>
							yes
						</button>
						<button type="submit" name="answer" value="no" formnovalidate
							class="inline-flex items-center gap-1 rounded-lg border border-rose-500/40 bg-rose-500/5 text-rose-700 dark:text-rose-400 px-3 py-2 text-sm font-semibold hover:bg-rose-500/10 transition">
							<svg class="h-4 w-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/></svg>
							no
						</button>
						<button type="submit"
							class="inline-flex items-center gap-1 rounded-lg bg-accent text-surface px-3 py-2 text-sm font-semibold hover:brightness-110 transition">
							send →
						</button>
					</div>
				</div>
			</div>
		</form>`, gateTone, gateLabel, ticketLabel, html.EscapeString(q.ID), html.EscapeString(q.Question), html.EscapeString(q.ID))
	}
	fmt.Fprint(w, `</div>`)
}

// emptyState is the standardized empty-list panel — title + helpful hint.
// Centralized so every empty list looks the same.
func emptyState(title, hint string) string {
	return fmt.Sprintf(`<div class="rounded-lg border border-dashed border-gray-300 dark:border-gray-700 bg-gray-50/50 dark:bg-surface-raised/40 p-8 text-center">
		<div class="text-sm font-medium text-gray-700 dark:text-gray-300">%s</div>
		<div class="mt-1 text-xs text-gray-500 dark:text-gray-500 max-w-md mx-auto">%s</div>
	</div>`, html.EscapeString(title), html.EscapeString(hint))
}

// --- Tab composers ---------------------------------------------------------
//
// Each tab is a small wrapper that sets the section heading + spacing,
// then defers to the underlying fragment for the actual data.

func (s *Server) fragTabOverview(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	st := s.opts.Memory.GetStatus()
	pending := len(s.opts.Memory.PendingQuestions())
	wfs := s.opts.Memory.ListWorkflows(20)
	active, awaiting, done, failed := 0, 0, 0, 0
	for _, w := range wfs {
		switch w.State {
		case memory.WFDone:
			done++
		case memory.WFFailed:
			failed++
		case memory.WFAwaitingApproval:
			awaiting++
		default:
			active++
		}
	}

	// Hero line — "what is goon doing right now?"
	hero := "Idle — no active workflow."
	heroTone := "text-gray-500"
	if !st.Running {
		hero = "Daemon is stopped. Run `goon start` to begin polling."
		heroTone = "text-gray-500"
	} else if st.Paused {
		hero = "Paused — no new tickets are being picked up."
		heroTone = "text-amber-700 dark:text-amber-400"
	} else if active > 0 {
		hero = fmt.Sprintf("Working on %d ticket%s right now.", active, plural(active, ""))
		heroTone = "text-accent"
	} else if awaiting > 0 {
		hero = fmt.Sprintf("Waiting on you — %d workflow%s paused for approval.", awaiting, plural(awaiting, "s"))
		heroTone = "text-amber-700 dark:text-amber-400"
	} else if pending > 0 {
		hero = fmt.Sprintf("%d question%s waiting for your reply.", pending, plural(pending, "s"))
		heroTone = "text-amber-700 dark:text-amber-400"
	}

	// Quick-stat cards across the top.
	fmt.Fprintf(w, `<div class="mb-6">
		<h1 class="text-2xl font-semibold tracking-tight">Overview</h1>
		<p class="mt-1 text-base %s">%s</p>
	</div>

	<div class="grid grid-cols-2 lg:grid-cols-4 gap-3 mb-6">
		%s
		%s
		%s
		%s
	</div>`,
		heroTone, html.EscapeString(hero),
		statCard("Tickets seen", fmt.Sprintf("%d", len(s.opts.Memory.ListTickets())), "indigo"),
		statCard("Active workflows", fmt.Sprintf("%d", active), pickTone(active, "accent")),
		statCard("Pending approval", fmt.Sprintf("%d", awaiting+pending), pickTone(awaiting+pending, "amber")),
		statCard("Completed", fmt.Sprintf("%d", done), "emerald"),
	)

	// Two-column main grid: status (left), questions (right).
	fmt.Fprint(w, `<div class="grid grid-cols-1 lg:grid-cols-3 gap-4">

		<section class="lg:col-span-1 rounded-xl border border-gray-200 dark:border-surface-border bg-white dark:bg-surface-raised p-5 shadow-card">
			<div hx-get="/fragments/status" hx-trigger="load, every 5s, statusChanged from:body" hx-swap="innerHTML">
				<div class="space-y-3"><div class="skel h-4 w-1/3"></div><div class="skel h-3 w-full"></div><div class="skel h-3 w-2/3"></div></div>
			</div>
		</section>

		<section class="lg:col-span-2 rounded-xl border border-gray-200 dark:border-surface-border bg-white dark:bg-surface-raised p-5 shadow-card">
			<h2 class="text-xs font-semibold uppercase tracking-wider text-gray-500 mb-3 flex items-center gap-2">
				<svg class="h-3.5 w-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="10"/><path d="M9.09 9a3 3 0 0 1 5.83 1c0 2-3 3-3 3"/><line x1="12" y1="17" x2="12.01" y2="17"/></svg>
				Pending questions
			</h2>
			<div hx-get="/fragments/questions" hx-trigger="load, every 5s, questionsChanged from:body" hx-swap="innerHTML">
				<div class="space-y-2"><div class="skel h-4 w-3/4"></div><div class="skel h-3 w-1/2"></div></div>
			</div>
		</section>

		<section class="lg:col-span-3 mt-2">
			<div class="flex items-baseline justify-between mb-3">
				<h2 class="text-sm font-semibold text-gray-700 dark:text-gray-300">Recent workflows</h2>
				<button type="button" onclick="document.querySelector('button[data-tab=workflows]').click()" class="text-xs text-gray-500 hover:text-accent transition">view all →</button>
			</div>
			<div hx-get="/fragments/workflows" hx-trigger="load, every 6s" hx-swap="innerHTML">
				<div class="grid grid-cols-1 lg:grid-cols-2 gap-4">
					<div class="rounded-xl border border-gray-200 dark:border-surface-border bg-white dark:bg-surface-raised p-4 space-y-3"><div class="skel h-4 w-1/3"></div><div class="skel h-3 w-full"></div><div class="skel h-2 w-2/3"></div></div>
					<div class="rounded-xl border border-gray-200 dark:border-surface-border bg-white dark:bg-surface-raised p-4 space-y-3"><div class="skel h-4 w-1/3"></div><div class="skel h-3 w-full"></div><div class="skel h-2 w-2/3"></div></div>
				</div>
			</div>
		</section>

	</div>`)
}

// statCard renders a small KPI tile. Tone maps to a Tailwind palette so
// the card visually echoes its meaning (e.g. amber for things needing
// attention, emerald for completed).
func statCard(label, value, tone string) string {
	var ring, accent string
	switch tone {
	case "amber":
		ring = "border-amber-500/30 bg-amber-500/5"
		accent = "text-amber-700 dark:text-amber-400"
	case "rose":
		ring = "border-rose-500/30 bg-rose-500/5"
		accent = "text-rose-700 dark:text-rose-400"
	case "emerald":
		ring = "border-emerald-500/30 bg-emerald-500/5"
		accent = "text-emerald-700 dark:text-emerald-400"
	case "indigo":
		ring = "border-indigo-500/30 bg-indigo-500/5"
		accent = "text-indigo-700 dark:text-indigo-400"
	case "accent":
		ring = "border-accent/30 bg-accent/5"
		accent = "text-accent"
	default:
		ring = "border-gray-200 dark:border-surface-border bg-white dark:bg-surface-raised"
		accent = "text-gray-700 dark:text-gray-300"
	}
	return fmt.Sprintf(`<div class="rounded-xl border %s p-4">
		<div class="text-[11px] uppercase tracking-wider text-gray-500">%s</div>
		<div class="mt-1 text-2xl font-semibold %s">%s</div>
	</div>`, ring, html.EscapeString(label), accent, html.EscapeString(value))
}

func pickTone(n int, tone string) string {
	if n > 0 {
		return tone
	}
	return "neutral"
}

func plural(n int, suffix string) string {
	if n == 1 {
		return ""
	}
	if suffix == "" {
		return "s"
	}
	return suffix
}

func (s *Server) fragTabTickets(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<section>
		<div class="flex items-start justify-between mb-5 gap-4 flex-wrap">
			<div>
				<h2 class="text-xl font-semibold tracking-tight">Tickets</h2>
				<p class="mt-0.5 text-sm text-gray-500">Live mirror of the configured board. Refreshes automatically while polling.</p>
			</div>
			<button type="button" hx-post="/api/refresh" hx-target="#refresh-result" hx-swap="innerHTML"
				class="inline-flex items-center gap-2 rounded-md border border-accent/40 text-accent px-3.5 py-2 text-sm font-medium hover:bg-accent-soft hover:border-accent transition shadow-sm">
				<svg class="h-4 w-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12a9 9 0 1 1-3-6.7L21 8"/><path d="M21 3v5h-5"/></svg>
				<span>Refresh from board</span>
			</button>
		</div>
		<div id="refresh-result" class="mb-4"></div>

		<!-- Filter bar: built-in client-side search across all rendered rows. -->
		<div class="flex flex-wrap items-center gap-3 mb-3 p-3 rounded-lg bg-gray-50 dark:bg-surface-sunken border border-gray-200 dark:border-surface-border">
			<div class="relative flex-1 min-w-[200px]">
				<svg class="absolute left-3 top-1/2 -translate-y-1/2 h-4 w-4 text-gray-400" viewBox="0 0 20 20" fill="currentColor"><path fill-rule="evenodd" d="M9 3.5a5.5 5.5 0 100 11 5.5 5.5 0 000-11zM2 9a7 7 0 1112.452 4.391l3.328 3.329a.75.75 0 11-1.06 1.06l-3.329-3.328A7 7 0 012 9z" clip-rule="evenodd"/></svg>
				<input id="ticket-filter" type="text" placeholder="filter by key, title, assignee, project, label…"
					class="w-full pl-9 pr-3 py-1.5 text-sm rounded-md border border-gray-300 dark:border-surface-border bg-white dark:bg-surface focus:border-accent focus:ring-2 focus:ring-accent/30 focus:outline-none"
					oninput="filterTickets(this.value)">
			</div>
			<div class="flex items-center gap-2 text-xs">
				<span class="text-gray-500">status:</span>
				<select id="ticket-status-filter" onchange="filterTickets(document.getElementById('ticket-filter').value)"
					class="rounded-md border border-gray-300 dark:border-surface-border bg-white dark:bg-surface px-2 py-1 focus:border-accent focus:outline-none">
					<option value="">all</option>
					<option value="open">open</option>
					<option value="in_progress">in progress</option>
					<option value="in_review">in review</option>
					<option value="blocked">blocked</option>
					<option value="done">done</option>
				</select>
			</div>
			<div class="text-xs text-gray-500 ml-auto"><span id="ticket-count">—</span></div>
		</div>

		<div hx-get="/fragments/tickets" hx-trigger="load, every 6s, statusChanged from:body, ticketsChanged from:body" hx-swap="innerHTML">
			<div class="rounded-lg border border-gray-200 dark:border-surface-border bg-white dark:bg-surface-raised p-4 space-y-2">
				<div class="skel h-4 w-1/4"></div>
				<div class="skel h-4 w-full"></div>
				<div class="skel h-4 w-full"></div>
				<div class="skel h-4 w-5/6"></div>
			</div>
		</div>
	</section>

	<script>
	// Client-side filter — searches across every visible row's text content
	// and the data-status attribute. Updates the ticket count summary.
	(function() {
		window.filterTickets = function(q) {
			q = (q || '').trim().toLowerCase();
			const status = document.getElementById('ticket-status-filter')?.value || '';
			const rows = document.querySelectorAll('tr[data-ticket-row]');
			let visible = 0;
			rows.forEach(r => {
				const text = (r.textContent || '').toLowerCase();
				const rowStatus = (r.getAttribute('data-status') || '').toLowerCase();
				const textOK = !q || text.includes(q);
				const statusOK = !status || rowStatus.includes(status);
				const show = textOK && statusOK;
				r.style.display = show ? '' : 'none';
				if (show) visible++;
			});
			const cn = document.getElementById('ticket-count');
			if (cn) cn.textContent = visible + ' of ' + rows.length + ' shown';
		};
		// Re-apply filter after every htmx swap so freshly-loaded rows respect it.
		document.body.addEventListener('htmx:afterSwap', () => {
			const f = document.getElementById('ticket-filter');
			if (f) filterTickets(f.value);
		});
	})();
	</script>`)
}

func (s *Server) fragTabWorkflows(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<section>
		<div class="flex items-start justify-between mb-5 gap-4 flex-wrap">
			<div>
				<h2 class="text-xl font-semibold tracking-tight">Workflow runs</h2>
				<p class="mt-0.5 text-sm text-gray-500">Most recent first. Plan progress, PR link, errors — auto-refreshes every few seconds.</p>
			</div>
		</div>
		<div hx-get="/fragments/workflows" hx-trigger="load, every 4s" hx-swap="innerHTML">
			<div class="grid grid-cols-1 lg:grid-cols-2 gap-4">
				<div class="rounded-xl border border-gray-200 dark:border-surface-border bg-white dark:bg-surface-raised p-5 space-y-3"><div class="skel h-4 w-1/3"></div><div class="skel h-3 w-3/4"></div><div class="skel h-2 w-full"></div><div class="skel h-2 w-1/2"></div></div>
				<div class="rounded-xl border border-gray-200 dark:border-surface-border bg-white dark:bg-surface-raised p-5 space-y-3"><div class="skel h-4 w-1/3"></div><div class="skel h-3 w-3/4"></div><div class="skel h-2 w-full"></div><div class="skel h-2 w-1/2"></div></div>
			</div>
		</div>
	</section>`)
}

func (s *Server) fragTabQuestions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<section>
		<div class="flex items-start justify-between mb-5 gap-4 flex-wrap">
			<div>
				<h2 class="text-xl font-semibold tracking-tight">Pending approvals</h2>
				<p class="mt-0.5 text-sm text-gray-500 max-w-2xl">
					Workflows pause here at the <code class="font-mono text-xs">confirm_repo</code> and
					<code class="font-mono text-xs">approve_plan</code> gates. Answer to unblock — replies
					are routed to the matching workflow on the next daemon tick.
				</p>
			</div>
		</div>
		<div hx-get="/fragments/questions" hx-trigger="load, every 3s, questionsChanged from:body" hx-swap="innerHTML">
			<div class="space-y-3">
				<div class="rounded-xl border border-gray-200 dark:border-surface-border bg-white dark:bg-surface-raised p-5 space-y-3"><div class="skel h-4 w-1/4"></div><div class="skel h-3 w-full"></div><div class="skel h-9 w-full"></div></div>
			</div>
		</div>
	</section>`)
}

func (s *Server) fragTabConfig(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Note: we deliberately do NOT listen to configChanged here.
	// Saving via the form lands a "saved N field(s) ✓" panel in
	// #cfg-result and dispatches configChanged for the toast. If we
	// re-rendered the whole config tab on configChanged, that
	// success panel (and any prior verify-connection output) would
	// be wiped before the user could read it. The form mutates in
	// place; we only refresh on initial load.
	fmt.Fprint(w, `<section>
		<div class="flex items-start justify-between mb-5 gap-4 flex-wrap">
			<div>
				<h2 class="text-xl font-semibold tracking-tight">Configuration</h2>
				<p class="mt-0.5 text-sm text-gray-500 dark:text-gray-400 max-w-2xl">
					All values are persisted to <code class="font-mono text-xs">~/.config/goon/.env</code>.
					Hitting <strong>save</strong> hot-reloads the daemon. Sensitive fields display as masked
					placeholders when set; leave them blank to keep, or type a new value to replace.
				</p>
			</div>
		</div>
		<div hx-get="/fragments/config" hx-trigger="load" hx-swap="innerHTML">
			<div class="space-y-3">
				<div class="rounded-xl border border-gray-200 dark:border-surface-border bg-white dark:bg-surface-raised p-5 space-y-3"><div class="skel h-4 w-1/4"></div><div class="skel h-9 w-full"></div><div class="skel h-9 w-full"></div></div>
				<div class="rounded-xl border border-gray-200 dark:border-surface-border bg-white dark:bg-surface-raised p-5 space-y-3"><div class="skel h-4 w-1/4"></div><div class="skel h-9 w-full"></div></div>
			</div>
		</div>
	</section>`)
}

// humanizeAgo produces a compact relative time string ("5m", "2h", etc.).
// Public-ish helper since the status pill, status panel, and ticket list
// all want the same formatting.
func humanizeAgo(d time.Duration) string {
	if d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
}

// --- helpers ---------------------------------------------------------------

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func envEcho(key string) string {
	if v := strings.TrimSpace(get(key)); v != "" {
		return v
	}
	return ""
}

// get is a small indirection to allow tests to monkey-patch env reads.
var get = func(k string) string { return getenv(k) }

func fuzzyTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return time.Since(t).Round(time.Second).String() + " ago"
}
