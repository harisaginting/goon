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

	"github.com/harisaginting/goon/internal/checkup"
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
	Stdout io.Writer
	Stderr io.Writer
}

// Server is goon's web frontend.
type Server struct {
	opts Options
	srv  *http.Server
	mu   sync.Mutex
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
	mux.HandleFunc("/api/status", s.handleAPIStatus)
	mux.HandleFunc("/api/tickets", s.handleAPITickets)
	mux.HandleFunc("/api/workflows", s.handleAPIWorkflows)
	mux.HandleFunc("/api/questions", s.handleAPIQuestions)
	mux.HandleFunc("/api/answer", s.handleAnswer)
	mux.HandleFunc("/api/config", s.handleAPIConfig) // GET reads, POST writes
	mux.HandleFunc("/api/config/verify", s.handleConfigVerify)
	mux.HandleFunc("/htmx.min.js", s.handleHTMX)
	mux.HandleFunc("/fragments/status", s.fragStatus)
	mux.HandleFunc("/fragments/tickets", s.fragTickets)
	mux.HandleFunc("/fragments/questions", s.fragQuestions)
	mux.HandleFunc("/fragments/workflows", s.fragWorkflows)
	mux.HandleFunc("/fragments/config", s.fragConfig)
	mux.HandleFunc("/fragments/setup", s.fragSetup)
	return mux
}

// Start begins serving. Blocks until ListenAndServe returns.
func (s *Server) Start() error {
	s.mu.Lock()
	s.srv = &http.Server{
		Addr:              s.opts.Addr,
		Handler:           s.mux(),
		ReadHeaderTimeout: 5 * time.Second,
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

func (s *Server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, indexHTML)
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
	_, _ = io.WriteString(w, "<div class='ok'>recorded ✓</div>")
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
	w.Header().Set("HX-Trigger", "configChanged")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<div class="ok">saved %d field(s) ✓</div>`, len(written))
	if len(notes) > 0 {
		fmt.Fprint(w, `<ul class="notes">`)
		for _, n := range notes {
			cls := "note-ok"
			if strings.HasPrefix(n, "✗") {
				cls = "note-err"
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
	header := `<div class="ok">verify: all checks passed ✓</div>`
	if !allOK {
		header = `<div class="err">verify: failures detected</div>`
	}
	fmt.Fprint(w, header)
	fmt.Fprint(w, `<ul class="notes">`)
	for _, r := range rs {
		cls := "note-ok"
		mark := "✓"
		switch {
		case r.Skipped:
			cls = "note-skip"
			mark = "·"
		case !r.OK:
			cls = "note-err"
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
// without the secret being in HTML.
func (s *Server) fragConfig(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	groups := groupKeys(webConfigKeys)
	order := []string{"agent", "openai", "anthropic", "ollama", "jira", "github", "gitlab", "bitbucket", "telegram"}
	fmt.Fprint(w, `<form class="cfg" hx-post="/api/config" hx-target="#cfg-result" hx-swap="innerHTML">`)
	for _, g := range order {
		ks, ok := groups[g]
		if !ok {
			continue
		}
		fmt.Fprintf(w, `<fieldset><legend>%s</legend>`, html.EscapeString(g))
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
			fmt.Fprintf(w, `<label><span class="lbl">%s</span><input type="%s" name="%s" value="%s" placeholder="%s"></label>`,
				html.EscapeString(k.Name), tp, html.EscapeString(k.Name),
				html.EscapeString(disp), html.EscapeString(placeholder))
			if k.Hint != "" && !k.Sensitive && k.Default == "" {
				fmt.Fprintf(w, `<span class="hint">%s</span>`, html.EscapeString(k.Hint))
			}
		}
		fmt.Fprint(w, `</fieldset>`)
	}
	fmt.Fprint(w, `<div class="cfg-actions">`)
	fmt.Fprint(w, `<button type="button" class="btn-verify"
        hx-post="/api/config/verify"
        hx-include="closest form"
        hx-target="#cfg-result"
        hx-swap="innerHTML"
        hx-indicator="#cfg-spinner">verify connection</button>`)
	fmt.Fprint(w, `<button type="submit" hx-indicator="#cfg-spinner">save &amp; reload daemon</button>`)
	fmt.Fprint(w, `<span id="cfg-spinner" class="htmx-indicator">⏳ probing…</span>`)
	fmt.Fprint(w, `</div>`)
	fmt.Fprint(w, `<div id="cfg-result"></div>`)
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
	_, _ = io.WriteString(w, `<div class="setup-banner">
  <strong>👋 Welcome to goon.</strong>
  Configure your LLM provider and ticket board below to start auto-engineering.
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

func (s *Server) fragStatus(w http.ResponseWriter, _ *http.Request) {
	st := s.opts.Memory.GetStatus()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	cls := "stopped"
	state := "STOPPED"
	if st.Running {
		cls, state = "running", "RUNNING"
	}
	last := "—"
	if !st.LastPoll.IsZero() {
		last = time.Since(st.LastPoll).Round(time.Second).String() + " ago"
	}
	fmt.Fprintf(w, `<div class="kv"><span class="k">status</span><span class="v %s">%s</span></div>`, cls, state)
	fmt.Fprintf(w, `<div class="kv"><span class="k">board</span><span class="v">%s</span></div>`, html.EscapeString(st.BoardName))
	fmt.Fprintf(w, `<div class="kv"><span class="k">git host</span><span class="v">%s</span></div>`, html.EscapeString(st.HostName))
	fmt.Fprintf(w, `<div class="kv"><span class="k">last poll</span><span class="v">%s</span></div>`, last)
	if st.LastTicket != "" {
		fmt.Fprintf(w, `<div class="kv"><span class="k">last ticket</span><span class="v">%s</span></div>`, html.EscapeString(st.LastTicket))
	}
	if st.PID != 0 {
		fmt.Fprintf(w, `<div class="kv"><span class="k">pid</span><span class="v">%d</span></div>`, st.PID)
	}
}

func (s *Server) fragTickets(w http.ResponseWriter, _ *http.Request) {
	tks := s.opts.Memory.ListTickets()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if len(tks) == 0 {
		_, _ = io.WriteString(w, `<div class="empty">No tickets seen yet.</div>`)
		return
	}
	_, _ = io.WriteString(w, `<table class="tickets"><tr><th>key</th><th>title</th><th>status</th><th>updated</th></tr>`)
	for _, t := range tks {
		fmt.Fprintf(w, `<tr><td><a href="%s">%s</a></td><td>%s</td><td>%s</td><td>%s</td></tr>`,
			html.EscapeString(t.URL), html.EscapeString(t.Key),
			html.EscapeString(t.Title), html.EscapeString(t.Status),
			fuzzyTime(t.UpdatedAt))
	}
	_, _ = io.WriteString(w, `</table>`)
}

func (s *Server) fragWorkflows(w http.ResponseWriter, _ *http.Request) {
	wfs := s.opts.Memory.ListWorkflows(20)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if len(wfs) == 0 {
		_, _ = io.WriteString(w, `<div class="empty">No workflows yet.</div>`)
		return
	}
	_, _ = io.WriteString(w, `<table class="workflows"><tr><th>id</th><th>ticket</th><th>state</th><th>plan</th><th>PR</th><th>updated</th></tr>`)
	for _, wf := range wfs {
		done, total := 0, len(wf.Plan)
		for _, s := range wf.Plan {
			if s.Done {
				done++
			}
		}
		pr := "—"
		if wf.PRURL != "" {
			pr = fmt.Sprintf(`<a href="%s">PR</a>`, html.EscapeString(wf.PRURL))
		}
		fmt.Fprintf(w,
			`<tr><td>%s</td><td>%s</td><td><span class="state state-%s">%s</span></td><td>%d/%d</td><td>%s</td><td>%s</td></tr>`,
			html.EscapeString(wf.ID),
			html.EscapeString(wf.TicketKey),
			string(wf.State), string(wf.State),
			done, total,
			pr,
			fuzzyTime(wf.UpdatedAt))
	}
	_, _ = io.WriteString(w, `</table>`)
}

func (s *Server) fragQuestions(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	pending := s.opts.Memory.PendingQuestions()
	if len(pending) == 0 {
		_, _ = io.WriteString(w, `<div class="ok">No pending questions ✓</div>`)
		return
	}
	for _, q := range pending {
		fmt.Fprintf(w, `<form class="qrow" hx-post="/api/answer" hx-target="this" hx-swap="outerHTML">
  <div class="q">[%s] %s</div>
  <div class="ticket">%s</div>
  <input type="hidden" name="id" value="%s">
  <input type="text" name="answer" placeholder="your answer" autofocus>
  <button type="submit">answer</button>
</form>`,
			html.EscapeString(q.ID),
			html.EscapeString(q.Question),
			html.EscapeString(q.TicketID),
			html.EscapeString(q.ID),
		)
	}
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
