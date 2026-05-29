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
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/harisaginting/goon/internal/boards"
	"github.com/harisaginting/goon/internal/checkup"
	"github.com/harisaginting/goon/internal/githost"
	"github.com/harisaginting/goon/internal/llm"
	"github.com/harisaginting/goon/internal/memory"
	"github.com/harisaginting/goon/internal/util"
	"github.com/harisaginting/goon/internal/workflow"
)

// Reconfigurable is the small slice of *daemon.Daemon the web layer touches.
// Declared as an interface here so the web package doesn't import daemon
// (which would create a cycle: daemon → workflow → … → tools, and we'd lose
// the ability to embed the web package elsewhere).
type Reconfigurable interface {
	Reconfigure() []string
	Configured() bool
}

// Waker is an optional companion interface — when implemented, the web
// answer handler calls Wake() so the daemon resumes a paused workflow
// in <1s instead of waiting for the next poll tick (which defaults to
// 5 minutes — long enough for users to assume it's broken).
type Waker interface {
	Wake()
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
	Board boards.Board
	// Host enables direct PR management endpoints in the web UI
	// (list, comment, approve, request-changes) — no LLM needed.
	// When nil, the PR panel renders a "no git host configured"
	// hint instead of a list.
	Host githost.Host
	// Workflow + WorkflowPath surface the active pipeline config so the
	// Workflows tab can show "what pipeline am I running?" (the user's
	// #1 confusion when goon has multiple workflows configured) and the
	// new editor surface (/api/workflow/save) can write changes back to
	// disk. Either may be nil — the UI degrades to a "no workflow.json
	// loaded" hint.
	Workflow     *workflow.WorkflowConfig
	WorkflowPath string
	Stdout       io.Writer
	Stderr       io.Writer
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

	// events is the SSE broker — every mutation handler calls
	// events.Publish("…") so connected browsers refresh in-place
	// instead of polling. nil-safe: handlers degrade to the old
	// "client polls" behaviour if construction is skipped.
	events *eventBus
}

// NewServer wires the Server.
func NewServer(opts Options) *Server {
	return &Server{opts: opts, events: newEventBus()}
}

// mux builds the routing table. Split out so tests can use it directly via
// httptest.NewRecorder without binding a real port.
func (s *Server) mux() *http.ServeMux {
	mux := http.NewServeMux()
	// Root is the marketing landing page; the dashboard moved to /app.
	// /home is an alias of /app for users who type it by habit.
	mux.HandleFunc("/", s.handleLanding)
	mux.HandleFunc("/app", s.handleApp)
	mux.HandleFunc("/home", s.handleApp)
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
	// Persisted chat threads — each turn auto-saves to ./storage/chats/
	// so the user can reopen a past conversation, continue it, delete
	// it, or distill it into a permanent knowledge note that future
	// runs will load via SOUL.md / topic-notes injection.
	mux.HandleFunc("/api/chat/threads", s.handleChatThreads)
	mux.HandleFunc("/api/chat/thread", s.handleChatThread)
	mux.HandleFunc("/api/chat/thread/delete", s.handleChatThreadDelete)
	mux.HandleFunc("/api/chat/save-as-note", s.handleChatSaveAsNote)
	mux.HandleFunc("/api/knowledge/note", s.handleKnowledgeNote)
	mux.HandleFunc("/api/memory/write", s.handleMemoryWrite)
	mux.HandleFunc("/api/memory/delete", s.handleMemoryDelete)
	mux.HandleFunc("/api/skills/note", s.handleSkillNote)
	mux.HandleFunc("/api/skills/write", s.handleSkillWrite)
	mux.HandleFunc("/api/skills/delete", s.handleSkillDelete)
	// /api/personal/save is gone — personal.md was folded into SOUL.md.
	// Edit via /api/memory/write with name=SOUL.md.
	mux.HandleFunc("/fragments/skills-list", s.fragSkillsList)
	mux.HandleFunc("/api/refresh", s.handleRefresh)
	mux.HandleFunc("/api/events", s.handleEvents) // SSE: server → browser change pings

	// Direct Jira/Bitbucket actions — no LLM in the loop.
	mux.HandleFunc("/api/ticket/comment", s.handleTicketComment)
	mux.HandleFunc("/api/ticket/transition", s.handleTicketTransition)
	mux.HandleFunc("/api/ticket/edit", s.handleTicketEdit)
	// Ignore/claim toggles let the user opt a ticket out of the daemon
	// workflow without changing its board status. The daemon's
	// nextTicket() filter respects the ignore set on every poll.
	mux.HandleFunc("/api/ticket/ignore", s.handleTicketIgnore)
	mux.HandleFunc("/api/ticket/unignore", s.handleTicketUnignore)
	mux.HandleFunc("/fragments/prs", s.handlePRList)
	mux.HandleFunc("/api/pr/comment", s.handlePRComment)
	mux.HandleFunc("/api/pr/approve", s.handlePRApprove)
	mux.HandleFunc("/api/pr/request-changes", s.handlePRRequestChanges)
	mux.HandleFunc("/api/pr/draft-review", s.handlePRDraftReview)
	// Repo picker: list all repos visible to the token, save the
	// selected subset into GOON_REVIEW_REPOS without restart.
	mux.HandleFunc("/fragments/repos-picker", s.handleReposPicker)
	// Repositories tab — repo-centric reframing of the old flat-PR view.
	// /fragments/repositories renders the per-repo card list with PR
	// counts + local-mapping status; /fragments/repo?slug=… lazy-loads
	// the per-repo detail panel (map form + clone button + PR list).
	// /api/repo/map writes REPOSITORY.md; /api/repo/clone shells out
	// git clone via the safety validator.
	mux.HandleFunc("/fragments/repositories", s.handleRepositoryList)
	mux.HandleFunc("/fragments/repo", s.handleRepoDetail)
	mux.HandleFunc("/api/repo/map", s.handleRepoMap)
	mux.HandleFunc("/api/repo/clone", s.handleRepoClone)
	mux.HandleFunc("/api/repos/save", s.handleReposSave)
	// Plan editor — replace wf.Plan with user-edited steps and
	// approve the approve_plan gate in one shot.
	mux.HandleFunc("/api/plan/save", s.handlePlanSave)
	// In-browser file tree + editor. Lets the user browse and edit
	// the workspace goon is working on without switching tools. See
	// internal/web/files.go for the safety rules (no "..", no abs
	// paths, 2 MB read cap, refuses binary).
	mux.HandleFunc("/api/files/tree", s.handleFilesTree)
	mux.HandleFunc("/api/files/read", s.handleFilesRead)
	mux.HandleFunc("/api/files/write", s.handleFilesWrite)
	mux.HandleFunc("/htmx.min.js", s.handleHTMX)
	// Brand. Served from a stable URL so favicon, og:image, and external
	// links don't need to be updated when the file moves.
	mux.HandleFunc("/logo.svg", s.handleLogo)
	mux.HandleFunc("/favicon.ico", s.handleLogo) // browsers ask for this automatically; serve the SVG
	// Underlying fragments — render the raw component (used by tests
	// and direct htmx polls).
	mux.HandleFunc("/fragments/status", s.fragStatus)
	mux.HandleFunc("/fragments/tickets", s.fragTickets)
	// Real-Jira-status dropdown source for the Tickets tab. Returns
	// <option> tags inline so the Transition <select> can hx-get
	// straight into innerHTML — no JSON parse needed on the client.
	mux.HandleFunc("/api/ticket/transitions", s.handleTicketTransitions)
	mux.HandleFunc("/fragments/questions", s.fragQuestions)
	mux.HandleFunc("/fragments/workflows", s.fragWorkflows)
	// Per-workflow detail panel — path-parameterized.
	mux.HandleFunc("/fragments/workflow/", s.fragWorkflowDetail)
	// Workflow-config header + editor surface for the Workflows tab.
	// The editor shows workflow.json verbatim in a textarea, validates
	// JSON on save, writes via /api/workflow/save, then fires
	// workflowConfigChanged so the header re-renders with the new name.
	mux.HandleFunc("/fragments/workflow-config", s.fragWorkflowConfig)
	mux.HandleFunc("/api/workflow/save", s.handleWorkflowSave)
	mux.HandleFunc("/fragments/config", s.fragConfig)
	mux.HandleFunc("/fragments/setup", s.fragSetup)
	// Alias so the sidebar "Setup" tab can follow the same
	// /fragments/tab-<name> convention every other tab uses. Some
	// dev-tools fetches and at least one user-extension was hitting
	// the conventional URL and getting a 404; both paths now resolve
	// to the same banner renderer.
	mux.HandleFunc("/fragments/tab-setup", s.fragSetup)
	// Header + chrome fragments served separately so the dashboard
	// can refresh them on different cadences without re-rendering
	// the entire main panel.
	mux.HandleFunc("/fragments/status-pill", s.fragStatusPill)
	mux.HandleFunc("/fragments/questions-banner", s.fragQuestionsBanner)
	// Tab content composers — wrap the underlying fragments with a
	// section title + spacing so each tab feels purpose-built.
	// Tab composers. Four standalone pages now — Questions /
	// Workflows / Tickets / Pull-requests — each one of its own.
	// Legacy aliases (tab-work, tab-overview) route to Questions so
	// old bookmarks still resolve to a sensible landing page.
	// Four standalone primary pages. Each has its own tab composer.
	mux.HandleFunc("/fragments/tab-dashboard", s.fragTabDashboard)
	mux.HandleFunc("/fragments/tab-home", s.fragTabDashboard) // alias
	mux.HandleFunc("/fragments/tab-questions", s.fragTabQuestions)
	mux.HandleFunc("/fragments/tab-workflows", s.fragTabWorkflows)
	mux.HandleFunc("/fragments/tab-tickets", s.fragTabTickets)
	mux.HandleFunc("/fragments/tab-prs", s.fragTabPRs)
	// /fragments/tab-repositories alias so future bookmarks can follow
	// the new tab name — the old tab-prs URL keeps working forever for
	// existing links and in-page navigation (the sidebar still uses
	// data-page="prs" internally).
	mux.HandleFunc("/fragments/tab-repositories", s.fragTabPRs)
	// Legacy compatibility — old bookmarks still resolve.
	mux.HandleFunc("/fragments/tab-work", s.fragTabQuestions)
	mux.HandleFunc("/fragments/tab-overview", s.fragTabQuestions)
	mux.HandleFunc("/fragments/tab-config", s.fragTabConfig)
	mux.HandleFunc("/fragments/tab-chat", s.fragTabChat)
	// Memory tab is a segmented control over Knowledge + Skills.
	// The two stores keep separate endpoints for management; the
	// composer below renders both.
	mux.HandleFunc("/fragments/tab-memory", s.fragTabMemory)
	mux.HandleFunc("/fragments/tab-knowledge", s.fragTabMemory) // legacy → memory
	mux.HandleFunc("/fragments/tab-skills", s.fragTabMemory)    // legacy → memory
	// File browser tab composer (sidebar entry "Files").
	mux.HandleFunc("/fragments/tab-files", s.fragTabFiles)
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

//go:embed static/logo.svg
var logoSVG []byte

//go:embed static/landing.html
var landingHTML string

// handleLanding serves the marketing landing page at `/`. Two CTAs:
// "Go to app" → /app (the dashboard), "Documentation" → /docs. The
// dashboard is a separate URL so deep-linking + back/forward navigation
// behave like a real product, and the landing page is cacheable
// (no per-request state to compute).
func (s *Server) handleLanding(w http.ResponseWriter, r *http.Request) {
	// Only serve the landing on the exact root path — `/foo` would
	// otherwise fall through here from the default ServeMux pattern.
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = io.WriteString(w, landingHTML)
}

// handleApp serves the dashboard SPA shell. Used to live at `/`; now
// at `/app` so the landing page can sit at the root.
func (s *Server) handleApp(w http.ResponseWriter, _ *http.Request) {
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

// handleLogo serves the embedded brand SVG so the favicon, og:image,
// and any external "where's the logo" lookup all resolve from a single
// canonical URL. Cached aggressively — the SVG is immutable per binary.
func (s *Server) handleLogo(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
	_, _ = w.Write(logoSVG)
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
	// Wake the daemon so it resumes the paused workflow immediately
	// instead of waiting up to PollInterval (5 min default). Fall
	// back silently if the daemon doesn't implement Waker — the
	// workflow still resumes on the next scheduled tick.
	if waker, ok := s.opts.Daemon.(Waker); ok {
		waker.Wake()
	}
	// Same triggers via two channels:
	//   (1) HX-Trigger headers — picked up by the originating browser
	//   (2) events bus — broadcast to every connected dashboard via SSE
	// (2) is what makes a second browser tab refresh in step with the
	// one that performed the action.
	w.Header().Set("HX-Trigger", "questionsChanged, workflowsChanged, workflowDetailRefresh")
	s.events.Publish("questionsChanged")
	s.events.Publish("workflowsChanged")
	s.events.Publish("workflowDetailRefresh")
	_, _ = io.WriteString(w, `<div class="rounded-md bg-emerald-500/10 border border-emerald-500/30 px-3 py-2 text-sm text-emerald-700 dark:text-emerald-400">recorded ✓ — daemon resuming now</div>`)
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
	s.events.Publish("statusChanged")
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
	s.events.Publish("statusChanged")
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
	{Name: "GOON_LLM_PROVIDER", Default: "openai", Group: "agent", Hint: "openai | anthropic | gemini | ollama | mock"},
	{Name: "GOON_BOARD", Group: "agent", Hint: "jira | github | mock"},
	{Name: "GOON_GIT_HOST", Group: "agent", Hint: "github | gitlab | bitbucket | mock (optional)"},
	{Name: "GOON_POLL_SECONDS", Default: "300", Group: "agent"},
	{Name: "GOON_VERIFY_RUNS", Default: "3", Group: "agent"},
	{Name: "GOON_REPO_MAP", Group: "agent", Hint: `e.g. ENG=/repos/eng,*=/repos/default`},
	{Name: "GOON_WORKSPACE_DIR", Group: "agent", Hint: `parent directory holding multiple git repos — confirm_repo gate offers them as a numbered menu`},

	{Name: "OPENAI_API_KEY", Sensitive: true, Group: "openai"},
	{Name: "OPENAI_MODEL", Default: "gpt-4o-mini", Group: "openai"},
	{Name: "OPENAI_BASE_URL", Default: "https://api.openai.com/v1", Group: "openai", Hint: "override for proxy / Azure"},

	{Name: "ANTHROPIC_API_KEY", Sensitive: true, Group: "anthropic"},
	{Name: "ANTHROPIC_MODEL", Default: "claude-sonnet-4-5", Group: "anthropic"},
	{Name: "ANTHROPIC_BASE_URL", Default: "https://api.anthropic.com/v1", Group: "anthropic"},

	{Name: "OLLAMA_BASE_URL", Default: "http://localhost:11434", Group: "ollama"},
	{Name: "OLLAMA_MODEL", Default: "llama3", Group: "ollama"},

	{Name: "GEMINI_API_KEY", Sensitive: true, Group: "gemini", Hint: "from aistudio.google.com/apikey — falls back to GOOGLE_API_KEY"},
	{Name: "GEMINI_MODEL", Default: "gemini-2.5-flash", Group: "gemini"},
	{Name: "GEMINI_BASE_URL", Default: "https://generativelanguage.googleapis.com/v1beta", Group: "gemini"},

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
	s.events.Publish("configChanged")
	s.events.Publish("statusChanged")
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

// fragWorkflowConfig renders the "what pipeline am I running?" header
// for the Workflows tab. Shows the active workflow's name + description
// + source path, plus a collapsible JSON editor that POSTs to
// /api/workflow/save. Without this band the user had no way to know
// which workflow.json was loaded (or that they could edit it from the
// web — most users only knew the CLI subcommand).
func (s *Server) fragWorkflowConfig(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	cfg := s.opts.Workflow
	path := s.opts.WorkflowPath
	// Compose the header. When no workflow.json is loaded (daemon
	// started before any was found, or workflow.LoadConfig errored)
	// we still show the band — but with a "create one" CTA instead
	// of the read-current-config form.
	name := "default"
	desc := "Built-in pipeline (no workflow.json found)."
	stageCount := 0
	autoApprove := false
	branchPrefix := ""
	if cfg != nil {
		if cfg.Name != "" {
			name = cfg.Name
		}
		if cfg.Description != "" {
			desc = cfg.Description
		}
		stageCount = len(cfg.Stages)
		autoApprove = cfg.AutoApprove
		branchPrefix = cfg.BranchPrefix
	}
	srcLabel := path
	if srcLabel == "" {
		srcLabel = "(no file — using built-in default)"
	}
	approveBadge := `<span class="inline-flex items-center gap-1 rounded-full bg-amber-500/15 text-amber-700 dark:text-amber-400 border border-amber-500/40 px-2 py-0.5 text-[11px] font-medium" title="goon will pause at confirm_repo + approve_plan gates and wait for your answer">⏸ gated · asks before run</span>`
	if autoApprove {
		approveBadge = `<span class="inline-flex items-center gap-1 rounded-full bg-emerald-500/15 text-emerald-700 dark:text-emerald-400 border border-emerald-500/40 px-2 py-0.5 text-[11px] font-medium" title="auto_approve=true — gates are skipped; goon runs end-to-end without asking">⚡ auto-approve · runs unattended</span>`
	}
	stageLabel := fmt.Sprintf("%d stage%s", stageCount, pluralS(stageCount))
	if stageCount == 0 {
		stageLabel = "built-in stages"
	}
	branchLabel := ""
	if branchPrefix != "" {
		branchLabel = fmt.Sprintf(`<span class="text-[11px] font-mono text-muted">branch: <span class="text-accent">%s</span></span>`, html.EscapeString(branchPrefix))
	}

	fmt.Fprintf(w, `<div class="rounded-xl border border-accent/30 bg-gradient-to-br from-accent-soft to-transparent shadow-card">
	<div class="px-5 py-4">
		<div class="flex items-start gap-3 flex-wrap">
			<div class="min-w-0 flex-1">
				<div class="flex items-center gap-2 mb-1 flex-wrap">
					<span class="text-[11px] font-semibold uppercase tracking-wider text-accent">workflow</span>
					<span class="text-base font-semibold text-white">%s</span>
					<span class="text-[11px] text-muted">·</span>
					<span class="text-[11px] text-muted">%s</span>
				</div>
				<div class="text-xs text-muted leading-relaxed max-w-2xl">%s</div>
				<div class="mt-2 flex items-center gap-3 flex-wrap text-[11px]">
					<span class="font-mono text-muted/80">source: <span class="text-accent">%s</span></span>
					%s
				</div>
			</div>
			<div class="flex items-center gap-2 flex-shrink-0">
				%s
				<button type="button" onclick="goonWorkflowEditorToggle()"
					class="inline-flex items-center gap-1.5 rounded-md border border-accent/40 text-accent px-3 py-1.5 text-xs font-medium hover:bg-accent/10 transition">
					<svg class="h-3.5 w-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M11 4H4a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7"/><path d="M18.5 2.5a2.121 2.121 0 0 1 3 3L12 15l-4 1 1-4 9.5-9.5z"/></svg>
					<span id="wf-editor-toggle-label">view / edit</span>
				</button>
			</div>
		</div>
	</div>`,
		html.EscapeString(name), html.EscapeString(stageLabel),
		html.EscapeString(desc),
		html.EscapeString(srcLabel),
		branchLabel,
		approveBadge,
	)

	// Editor — hidden by default. Loads workflow.json source so the
	// user can edit + save. We render it inside the band so a Save
	// posts back without losing the header.
	rawJSON := ""
	if cfg != nil {
		// Marshal the in-memory cfg to JSON so what the user edits
		// matches what goon is actually running, not whatever stale
		// disk content might exist. Pretty-printed for readability.
		if b, err := json.MarshalIndent(cfg, "", "  "); err == nil {
			rawJSON = string(b)
		}
	}
	// Default save target: the path goon loaded from, or the standard
	// repo-root ./workflow.json if no file was found yet.
	saveTarget := path
	if saveTarget == "" {
		saveTarget = workflow.DefaultConfigFilePath()
	}
	fmt.Fprintf(w, `<div id="wf-editor" class="hidden border-t border-surface-border">
		<form hx-post="/api/workflow/save" hx-target="#wf-editor-result" hx-swap="innerHTML"
			class="px-5 py-4 space-y-2">
			<div class="flex items-center justify-between gap-2">
				<label class="text-[11px] font-semibold uppercase tracking-wider text-muted">workflow.json</label>
				<span class="text-[11px] text-muted">writes to <span class="font-mono text-accent">%s</span></span>
			</div>
			<textarea name="body" rows="18" spellcheck="false"
				class="w-full font-mono text-xs leading-relaxed rounded-md border border-surface-border bg-surface text-white px-3 py-2 focus:border-accent focus:ring-1 focus:ring-accent/30 focus:outline-none">%s</textarea>
			<input type="hidden" name="path" value="%s">
			<div class="flex items-center gap-2">
				<button type="submit" class="inline-flex items-center gap-1 rounded-md bg-accent text-surface px-3 py-1.5 text-sm font-semibold hover:brightness-110 transition">save workflow</button>
				<button type="button" onclick="goonWorkflowEditorToggle()" class="text-xs text-muted hover:text-white transition">cancel</button>
				<span class="text-[11px] text-muted ml-auto">JSON only — invalid syntax is rejected. Daemon picks up the new config on its next poll.</span>
			</div>
			<div id="wf-editor-result" class="text-xs"></div>
		</form>
	</div>
</div>`,
		html.EscapeString(saveTarget),
		html.EscapeString(rawJSON),
		html.EscapeString(saveTarget),
	)
}

// handleWorkflowSave writes the posted body to workflow.json after
// validating that it parses as a workflow.WorkflowConfig. Returns a small
// HTML success/error fragment for the editor's result slot and fires
// workflowConfigChanged so the header re-renders with the new name.
//
// Path safety: the target path must be either the currently-loaded
// WorkflowPath or workflow.DefaultConfigFilePath(). We don't let the
// caller write to arbitrary paths via this surface — that would be
// an obvious file-system-write vector.
func (s *Server) handleWorkflowSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		fragErr(w, "invalid form: "+err.Error())
		return
	}
	body := r.FormValue("body")
	if strings.TrimSpace(body) == "" {
		fragErr(w, "empty body — paste JSON or click cancel")
		return
	}
	target := strings.TrimSpace(r.FormValue("path"))
	if target == "" {
		target = workflow.DefaultConfigFilePath()
	}
	// Allowlist the destination — only the loaded path or the default.
	allowed := target == s.opts.WorkflowPath || target == workflow.DefaultConfigFilePath()
	if !allowed {
		fragErr(w, "refusing to write to unexpected path: "+target)
		return
	}
	// Validate as a workflow.WorkflowConfig — same code path LoadConfig uses,
	// so any error here is the same error the daemon would hit on next
	// poll. Better to surface it now in the editor than let the daemon
	// silently skip the bad config.
	var probe workflow.WorkflowConfig
	if err := json.Unmarshal([]byte(body), &probe); err != nil {
		fragErr(w, "JSON parse error: "+err.Error())
		return
	}
	// Write atomically — tmp file + rename — so the daemon never sees
	// a half-written workflow.json mid-save.
	dir := filepath.Dir(target)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fragErr(w, "mkdir failed: "+err.Error())
			return
		}
	}
	tmp, err := os.CreateTemp(dir, ".workflow.*.tmp")
	if err != nil {
		fragErr(w, "create tmp: "+err.Error())
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(body); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		fragErr(w, "write tmp: "+err.Error())
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		fragErr(w, "close tmp: "+err.Error())
		return
	}
	if err := os.Rename(tmpName, target); err != nil {
		_ = os.Remove(tmpName)
		fragErr(w, "rename: "+err.Error())
		return
	}
	// Patch the in-memory copy so the header reflects the new state
	// immediately (without waiting for a daemon restart).
	s.opts.Workflow = &probe
	s.opts.WorkflowPath = target
	// Fire both triggers so the header band re-renders AND any external
	// listener (e.g. the Home tab) refreshes too.
	w.Header().Set("HX-Trigger", "workflowConfigChanged, workflowsChanged")
	s.events.Publish("workflowConfigChanged")
	fragOK(w, fmt.Sprintf("saved to %s — daemon picks it up on next poll", target))
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
// statusBadge maps daemon state to (label, css-class-for-dot). The dot
// colours match the new palette's semantic roles:
//   - stopped → cool grey (muted), no pulse
//   - paused  → vibrant amber (highlight), no pulse — paused is a
//               deliberate state, not an error, but should stand out
//   - running → neon purple (accent) with the pulse-dot animation, so
//               the brand-purple matches the header logo glow when live
func statusBadge(st memory.DaemonStatus) (string, string) {
	switch {
	case !st.Running:
		return "stopped", "bg-muted"
	case st.Paused:
		return "paused", "bg-highlight"
	default:
		return "running", "bg-accent pulse-dot"
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
	pausedFlag := "0"
	if st.Paused {
		pausedFlag = "1"
	}
	runningFlag := "0"
	if st.Running {
		runningFlag = "1"
	}
	// data-paused + data-running let the sidebar pause/resume button
	// and the brand-mark live-glow JS query the current state without
	// a separate API call. The label colour tracks the dot — purple
	// when running, amber when paused, muted when stopped — so the
	// pill reads at a glance.
	labelColor := "text-muted"
	if st.Running && !st.Paused {
		labelColor = "text-accent"
	} else if st.Paused {
		labelColor = "text-highlight"
	}
	fmt.Fprintf(w, `<div class="flex items-center gap-3 text-[11px] font-medium text-white" data-paused="%s" data-running="%s" title="Last poll: %s">
		<span class="relative flex h-2.5 w-2.5 shrink-0">
			<span class="absolute inline-flex h-full w-full rounded-full %s opacity-60 animate-ping"></span>
			<span class="relative inline-flex rounded-full h-2.5 w-2.5 %s"></span>
		</span>
		<div class="min-w-0 flex-1">
			<div class="uppercase tracking-wider text-[10px] text-muted">Daemon</div>
			<div class="text-sm font-semibold %s">%s</div>
			<div class="text-[11px] text-muted font-mono mt-0.5">last poll %s</div>
		</div>
	</div>`, pausedFlag, runningFlag, html.EscapeString(last), dotClass, dotClass, labelColor, state, html.EscapeString(last))
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
	// Banner is the highest-attention surface on the page when goon is
	// waiting on the user. Amber tint with a glowing CTA button so it's
	// impossible to miss without being shouty — the surface is already
	// obsidian so amber really pops. The .cta-glow class on the button
	// adds the gentle outward pulse defined in the global stylesheet.
	fmt.Fprintf(w, `<div class="mt-4 rounded-xl border border-highlight/50 bg-highlight/10 px-4 py-3 flex flex-wrap items-center gap-3 text-sm shadow-glow-amber">
		<span class="text-highlight text-lg leading-none">⏸</span>
		<div class="flex-1 min-w-0">
			<strong class="text-highlight">%d pending %s</strong>
			<span class="text-muted"> — workflows are paused waiting for your approval.</span>
		</div>
		<button type="button" onclick="if (typeof showPage==='function') showPage('questions'); else document.querySelector('button[data-page-target=questions]')?.click()"
			class="cta-glow rounded-md bg-highlight text-surface px-3.5 py-1.5 text-xs font-bold tracking-wide hover:brightness-110 transition">
			Review now →
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
				<th class="px-4 py-2.5 text-right font-semibold">Actions</th>
			</tr>
		</thead>
		<tbody class="divide-y divide-gray-100 dark:divide-surface-border/60">`)
	ignored := s.opts.Memory.IgnoredTickets()
	for _, t := range tks {
		isIgnored := false
		if ignored != nil {
			_, isIgnored = ignored[t.Key]
			if !isIgnored {
				_, isIgnored = ignored[t.ID]
			}
		}
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
		// Visual treatment when ignored: opacity-50 + a small badge
		// next to the title. Row is still fully interactive — the
		// user can claim it back from the same drawer.
		rowOpacity := ""
		titleBadge := ""
		if isIgnored {
			rowOpacity = " opacity-50"
			titleBadge = `<span class="ml-2 inline-flex items-center gap-1 rounded-full bg-amber-500/15 text-amber-700 dark:text-amber-400 border border-amber-500/40 px-1.5 py-0 text-[10px] font-medium align-middle" title="daemon is skipping this ticket">🚫 ignored</span>`
		}
		// Actions popover — toggles a hidden row revealing the action
		// drawer. The drawer is laid out as a single flex row of three
		// focused sub-sections (status / comment / edit + the ignore
		// toggle), each with its own primary button — replaces the
		// previous cramped 3-column grid that read as duplicate forms.
		safeID := strings.ReplaceAll(html.EscapeString(t.Key), "/", "-")
		actionsRowID := "ta-" + safeID
		actions := fmt.Sprintf(`<button type="button" onclick="document.getElementById('%s').classList.toggle('hidden')" class="text-xs text-accent hover:underline">⋯ actions</button>`, actionsRowID)

		// Build the ignore/claim toggle form per row. Two distinct
		// endpoints so the button posts a single semantic action;
		// the row re-renders via the ticketsChanged SSE.
		var ignoreToggle string
		if isIgnored {
			ignoreToggle = fmt.Sprintf(`<form hx-post="/api/ticket/unignore" hx-target="#%s-r" hx-swap="innerHTML" class="inline-flex">
				<input type="hidden" name="key" value="%s">
				<button type="submit" class="inline-flex items-center gap-1 rounded-md bg-emerald-500/15 border border-emerald-500/40 text-emerald-700 dark:text-emerald-400 px-3 py-1.5 text-xs font-semibold hover:bg-emerald-500/25 transition" title="let the daemon consider this ticket again">↺ claim back into workflow</button>
			</form>`, actionsRowID, html.EscapeString(t.Key))
		} else {
			ignoreToggle = fmt.Sprintf(`<form hx-post="/api/ticket/ignore" hx-target="#%s-r" hx-swap="innerHTML" class="inline-flex">
				<input type="hidden" name="key" value="%s">
				<button type="submit" class="inline-flex items-center gap-1 rounded-md border border-amber-500/40 text-amber-700 dark:text-amber-400 px-3 py-1.5 text-xs font-medium hover:bg-amber-500/10 transition" title="daemon will skip this ticket on the next poll">🚫 ignore from workflow</button>
			</form>`, actionsRowID, html.EscapeString(t.Key))
		}

		fmt.Fprintf(w, `<tr data-ticket-row data-status="%s" class="hover:bg-gray-50 dark:hover:bg-surface-sunken/40 transition-colors%s">
			<td class="px-4 py-2.5 font-mono whitespace-nowrap align-top">%s</td>
			<td class="px-4 py-2.5 align-top">
				<div class="text-sm font-medium text-gray-900 dark:text-white max-w-xl truncate">%s%s</div>
			</td>
			<td class="px-4 py-2.5 whitespace-nowrap align-top">%s</td>
			<td class="px-4 py-2.5 text-gray-700 dark:text-gray-300 whitespace-nowrap align-top text-xs">%s</td>
			<td class="px-4 py-2.5 font-mono text-xs text-gray-600 dark:text-gray-400 whitespace-nowrap align-top">%s</td>
			<td class="px-4 py-2.5 text-gray-500 text-xs whitespace-nowrap align-top">%s</td>
			<td class="px-4 py-2.5 text-right whitespace-nowrap align-top">%s</td>
		</tr>
		<tr id="%s" data-action-row class="hidden bg-gray-50/40 dark:bg-surface-sunken/40">
			<td colspan="7" class="px-4 py-4">
				<!-- Three sub-sections side-by-side (each one focused on
				     a single action), then a footer with the workflow
				     ignore/claim toggle. Cleaner than the old 3-column
				     grid which made all three forms look like the same
				     submit button. -->
				<div class="grid grid-cols-1 lg:grid-cols-3 gap-4 text-xs">
					<div class="rounded-lg border border-surface-border bg-surface p-3 space-y-2">
						<div class="text-[10px] uppercase tracking-wider text-muted font-semibold flex items-center gap-1">
							<svg class="h-3 w-3" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><path d="M21 11.5a8.38 8.38 0 0 1-.9 3.8 8.5 8.5 0 0 1-7.6 4.7 8.38 8.38 0 0 1-3.8-.9L3 21l1.9-5.7a8.38 8.38 0 0 1-.9-3.8 8.5 8.5 0 0 1 4.7-7.6 8.38 8.38 0 0 1 3.8-.9h.5a8.48 8.48 0 0 1 8 8v.5z"/></svg>
							Add comment
						</div>
						<form hx-post="/api/ticket/comment" hx-target="#%s-r" hx-swap="innerHTML" hx-on::after-request="if(event.detail.successful) this.reset()" class="space-y-2">
							<input type="hidden" name="key" value="%s">
							<textarea name="body" rows="2" required placeholder="comment text…"
								class="w-full font-mono text-xs rounded-md border border-surface-border bg-surface text-white px-2 py-1.5 focus:border-accent focus:ring-1 focus:ring-accent/30 focus:outline-none"></textarea>
							<button type="submit" class="w-full rounded-md bg-accent text-surface px-3 py-1.5 font-semibold hover:brightness-110 transition">send →</button>
						</form>
					</div>
					<div class="rounded-lg border border-surface-border bg-surface p-3 space-y-2">
						<div class="text-[10px] uppercase tracking-wider text-muted font-semibold flex items-center gap-1">
							<svg class="h-3 w-3" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><polyline points="9 18 15 12 9 6"/></svg>
							Move to status
						</div>
						<form hx-post="/api/ticket/transition" hx-target="#%s-r" hx-swap="innerHTML" class="space-y-2">
							<input type="hidden" name="key" value="%s">
							<select name="status"
								hx-get="/api/ticket/transitions?key=%s"
								hx-trigger="toggle from:closest details once, load delay:200ms"
								hx-swap="innerHTML"
								class="w-full rounded-md border border-surface-border bg-surface text-white px-2 py-1.5 focus:border-accent focus:outline-none">
								<option value="" disabled selected>loading statuses…</option>
							</select>
							<button type="submit" class="w-full rounded-md border border-accent/40 text-accent px-3 py-1.5 font-medium hover:bg-accent/10 transition">move →</button>
						</form>
					</div>
					<div class="rounded-lg border border-surface-border bg-surface p-3 space-y-2">
						<div class="text-[10px] uppercase tracking-wider text-muted font-semibold flex items-center gap-1">
							<svg class="h-3 w-3" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><path d="M11 4H4a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7"/><path d="M18.5 2.5a2.121 2.121 0 0 1 3 3L12 15l-4 1 1-4 9.5-9.5z"/></svg>
							Edit field
						</div>
						<form hx-post="/api/ticket/edit" hx-target="#%s-r" hx-swap="innerHTML" hx-on::after-request="if(event.detail.successful) this.reset()" class="space-y-2">
							<input type="hidden" name="key" value="%s">
							<select name="field" class="w-full rounded-md border border-surface-border bg-surface text-white px-2 py-1.5 focus:border-accent focus:outline-none">
								<option value="title">title</option>
								<option value="desc">description</option>
								<option value="labels">labels (a,b,c)</option>
							</select>
							<input type="text" name="value" required placeholder="new value…"
								class="w-full font-mono text-xs rounded-md border border-surface-border bg-surface text-white px-2 py-1.5 focus:border-accent focus:ring-1 focus:ring-accent/30 focus:outline-none">
							<button type="submit" class="w-full rounded-md border border-accent/40 text-accent px-3 py-1.5 font-medium hover:bg-accent/10 transition">apply →</button>
						</form>
					</div>
				</div>
				<!-- Workflow opt-out lives in its own bar at the bottom
				     — it's a meta-action (does NOT touch the board),
				     so putting it next to the board-action sections
				     would muddle the mental model. -->
				<div class="flex items-center justify-between gap-3 mt-3 pt-3 border-t border-surface-border">
					<div class="text-[11px] text-muted leading-snug max-w-md">
						Ignoring a ticket tells goon's daemon to skip it on every poll. It does NOT change the ticket's status on the board.
					</div>
					%s
				</div>
				<div id="%s-r" class="mt-2 text-xs"></div>
			</td>
		</tr>`,
			html.EscapeString(strings.ToLower(t.Status)), rowOpacity, key,
			html.EscapeString(t.Title), titleBadge,
			ticketStatusPill(t.Status), assignee, project,
			html.EscapeString(fuzzyTime(t.UpdatedAt)), actions,
			actionsRowID,
			actionsRowID, html.EscapeString(t.Key),
			actionsRowID, html.EscapeString(t.Key), html.EscapeString(t.Key),
			actionsRowID, html.EscapeString(t.Key),
			ignoreToggle,
			actionsRowID,
		)
	}
	fmt.Fprint(w, `</tbody></table></div>`)
}

// ticketStatusPill renders a small colored badge for a ticket status.
// Each tone uses paired light/dark text colors so the pill stays
// readable against both surface modes (light = darker text, dark =
// lighter text).
// ticketStatusPill maps a ticket's status string to a colored badge.
// Tones in the new palette:
//   - open/todo/backlog → muted grey (the resting state)
//   - in-progress       → amber highlight (work is happening)
//   - in-review         → neon purple accent (waiting on a human)
//   - done/merged       → emerald (universally "shipped")
//   - blocked           → rose (universally "stop")
// Keeping emerald + rose for the universal traffic-light semantics; the
// rest tie back to the brand.
func ticketStatusPill(status string) string {
	low := strings.ToLower(strings.TrimSpace(status))
	cls := "bg-surface-raised text-muted border-surface-border"
	switch {
	case low == "" || strings.Contains(low, "open") || strings.Contains(low, "todo") || strings.Contains(low, "ready") || strings.Contains(low, "backlog"):
		cls = "bg-surface-raised text-muted border-surface-border"
	case strings.Contains(low, "progress") || strings.Contains(low, "doing"):
		cls = "bg-highlight/15 text-highlight border-highlight/40"
	case strings.Contains(low, "review"):
		cls = "bg-accent/15 text-accent border-accent/40"
	case strings.Contains(low, "done") || strings.Contains(low, "closed") || strings.Contains(low, "resolved") || strings.Contains(low, "merged"):
		cls = "bg-emerald-500/15 text-emerald-400 border-emerald-500/40"
	case strings.Contains(low, "block"):
		cls = "bg-rose-500/15 text-rose-400 border-rose-500/40"
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
//
// De-dupe by ticket: we show only the most-recent workflow per
// TicketID. Older attempts (failed triage, replans, re-runs of the
// same ticket) live inside the detail view's history list. Without
// this the list got cluttered when one ticket failed multiple times
// in a row (e.g. before we raised the MaxTokens cap).
func (s *Server) fragWorkflows(w http.ResponseWriter, _ *http.Request) {
	all := s.opts.Memory.ListWorkflows(0) // 0 = unbounded; we cap after dedupe
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// Keep one workflow per ticket — the newest (ListWorkflows already
	// returns newest first). Workflows with no TicketID (synthetic)
	// pass through with a unique key so they don't collide.
	seen := map[string]int{}
	wfs := make([]memory.Workflow, 0, len(all))
	for _, wf := range all {
		key := wf.TicketID
		if key == "" {
			key = "_" + wf.ID
		}
		if n, ok := seen[key]; ok {
			seen[key] = n + 1
			continue
		}
		seen[key] = 1
		wfs = append(wfs, wf)
	}
	if len(wfs) > 50 {
		wfs = wfs[:50]
	}

	if len(wfs) == 0 {
		_, _ = io.WriteString(w, emptyState("No workflows yet.",
			"Workflow runs appear here as soon as a ticket is picked up. Click a card to see plan progress, approvals, errors, and answer any pending question."))
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
		// Edge colour mirrors workflowStateChip — emerald=done,
		// rose=failed, amber-highlight=awaiting-approval, soft purple
		// for triaging (planning hasn't earned the full brand colour
		// yet), full purple-accent for active mid-flight work.
		edgeTone := "bg-surface-border"
		switch wf.State {
		case memory.WFDone:
			edgeTone = "bg-emerald-500"
		case memory.WFFailed:
			edgeTone = "bg-rose-500"
		case memory.WFAwaitingApproval:
			edgeTone = "bg-highlight"
		case memory.WFTriaging:
			edgeTone = "bg-accent/60"
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

		// History badge: when this ticket has prior attempts in
		// memory.json, surface a tiny "Nx" pill so the user knows
		// the detail view will show more than one entry.
		historyBadge := ""
		if wf.TicketID != "" {
			if n, ok := seen[wf.TicketID]; ok && n > 1 {
				historyBadge = fmt.Sprintf(`<span class="ml-1 inline-flex items-center rounded-full bg-gray-100 dark:bg-surface-sunken text-gray-600 dark:text-gray-300 px-1.5 py-0.5 text-[10px] font-mono" title="%d total attempts for this ticket">%dx</span>`, n, n)
			}
		}

		// Flat card. No shadow at rest — borders carry the visual weight.
		// Hover gets the subtle lift so interactivity is still hinted.
		// The 2px left-edge color is the only chrome that depends on
		// state; everything else is plain typography.
		fmt.Fprintf(w, `<div class="group relative rounded-lg border border-gray-200 dark:border-surface-border bg-white dark:bg-surface-raised hover:border-gray-300 dark:hover:border-gray-700 transition-colors">
			<div class="absolute left-0 top-0 bottom-0 w-0.5 %s rounded-l-lg"></div>
			<details>
				<summary class="cursor-pointer list-none px-4 py-3 select-none">`, edgeTone)
		fmt.Fprintf(w, `<div class="flex items-center justify-between gap-3">
			<div class="min-w-0 flex-1">
				<div class="flex items-center gap-2 text-sm font-semibold text-gray-900 dark:text-gray-100">
					<span class="font-mono">%s</span>%s
				</div>
				<div class="mt-0.5 text-sm text-gray-600 dark:text-gray-400 truncate" title="%s">%s</div>
			</div>
			%s
		</div>`, ticket, historyBadge, title, title, stateChip)

		// Compact meta + progress on a single row. No SVG icons, no
		// uppercase tracking labels — just text. Quieter.
		fmt.Fprintf(w, `<div class="mt-2.5 flex items-center justify-between gap-4 text-[11px] text-gray-500">
			<div class="flex items-center gap-3">
				<span><span class="text-gray-400">stage</span> <span class="font-mono text-gray-700 dark:text-gray-300">%s</span></span>
				<span class="text-gray-300 dark:text-gray-600">·</span>
				<span class="font-mono">%s</span>
			</div>`, stage, html.EscapeString(fuzzyTime(wf.UpdatedAt)))

		if total > 0 {
			fmt.Fprintf(w, `<span class="font-mono">%d/%d</span>`, done, total)
		}
		fmt.Fprint(w, `</div>`)

		if total > 0 {
			fmt.Fprintf(w, `<div class="mt-2 h-1 w-full rounded-full bg-gray-100 dark:bg-surface-sunken overflow-hidden">
				<div class="h-full %s transition-all duration-500" style="width: %d%%"></div>
			</div>`, barTone, pct)
		}

		fmt.Fprintf(w, `</summary>
				<div class="border-t border-gray-100 dark:border-surface-border/60 px-4 py-3"
					hx-get="/fragments/workflow/%s" hx-trigger="toggle from:closest details once, workflowDetailRefresh from:body" hx-swap="innerHTML">
					<div class="text-xs text-gray-500">Loading detail…</div>
				</div>
			</details>`, html.EscapeString(wf.ID))

		// Action strip — flatter than before. No animated dot, no SVG
		// noise. Each row a plain link/button with one accent color.
		hasAction := wf.PRURL != "" || wf.PendingQuestionID != "" || wf.Error != ""
		if hasAction {
			fmt.Fprint(w, `<div class="px-4 pb-3 pt-0 space-y-1.5">`)
			if wf.PRURL != "" {
				fmt.Fprintf(w, `<a href="%s" target="_blank" rel="noopener" class="inline-flex items-center gap-1.5 max-w-full text-xs text-accent hover:underline">
					<span class="font-mono truncate min-w-0">↗ %s</span>
				</a>`, html.EscapeString(wf.PRURL), html.EscapeString(wf.PRURL))
			}
			if wf.PendingQuestionID != "" {
				qid := html.EscapeString(wf.PendingQuestionID)
				// Two affordances side-by-side: (a) expand the in-card
				// detail to see plan + history without leaving the tab,
				// (b) jump to the Questions tab and scroll the matching
				// q-N card into view. Most users pick (b) — the picker
				// only lives on Questions — but (a) is still useful for
				// glancing at the plan first.
				fmt.Fprintf(w, `<div class="flex gap-1.5 items-stretch">
					<button type="button"
						onclick="this.closest('div.group').querySelector('details').open = true"
						class="flex-1 flex items-center gap-2 text-left rounded-md bg-amber-500/10 hover:bg-amber-500/15 px-2.5 py-1.5 text-xs text-amber-700 dark:text-amber-400 transition">
						<span class="flex-1">⏸ paused — answer needed (<span class="font-mono">%s</span>)</span>
						<span class="font-medium">open</span>
					</button>
					<button type="button"
						onclick="goonJumpToQuestion('%s')"
						title="open the Questions tab and scroll to this question"
						class="inline-flex items-center gap-1 rounded-md bg-accent/15 hover:bg-accent/25 px-2.5 py-1.5 text-xs text-accent font-medium transition">
						→ answer
					</button>
				</div>`, qid, qid)
			}
			if wf.Error != "" {
				fmt.Fprintf(w, `<div class="rounded-md bg-rose-500/10 px-2.5 py-1.5 text-xs text-rose-700 dark:text-rose-400 break-words">
					✗ %s
				</div>`, html.EscapeString(wf.Error))
			}
			fmt.Fprint(w, `</div>`)
		}

		fmt.Fprint(w, `</div>`) // close card frame
	}
	fmt.Fprint(w, `</div>`)
}

// fragWorkflowDetail renders the in-card detail panel for one workflow:
// plan steps with done state, approvals/feedback, branch+repo info,
// the pending-question form (when paused), and a history block listing
// prior attempts for the same ticket. URL shape:
//
//	/fragments/workflow/{id}
//
// We embed an answer form here too so users don't have to flip to the
// Questions tab. handleAnswer routes the POST back through the same
// memory.AnswerQuestion + daemon.Wake path.
func (s *Server) fragWorkflowDetail(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/fragments/workflow/")
	id = strings.TrimSpace(id)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	wf, ok := s.opts.Memory.GetWorkflow(id)
	if !ok {
		fmt.Fprintf(w, `<div class="text-xs text-rose-500">workflow %s not found</div>`, html.EscapeString(id))
		return
	}

	// Top: ticket meta strip.
	fmt.Fprint(w, `<div class="space-y-4">`)
	fmt.Fprintf(w, `<div class="flex flex-wrap items-center gap-x-4 gap-y-2 text-xs text-gray-500 dark:text-gray-400">
		<span><span class="uppercase tracking-wider">started</span> <span class="font-mono text-gray-700 dark:text-gray-300">%s</span></span>
		<span><span class="uppercase tracking-wider">updated</span> <span class="font-mono text-gray-700 dark:text-gray-300">%s</span></span>
		<span><span class="uppercase tracking-wider">id</span> <span class="font-mono text-gray-700 dark:text-gray-300">%s</span></span>
	</div>`,
		html.EscapeString(fuzzyTime(wf.StartedAt)),
		html.EscapeString(fuzzyTime(wf.UpdatedAt)),
		html.EscapeString(wf.ID),
	)
	if wf.Repo != "" || wf.Branch != "" || len(wf.Repos) > 0 {
		fmt.Fprint(w, `<div class="flex flex-wrap items-center gap-x-4 gap-y-2 text-xs">`)
		if wf.Repo != "" {
			fmt.Fprintf(w, `<span class="text-gray-500">primary repo:</span> <span class="font-mono text-gray-700 dark:text-gray-300">%s</span>`, html.EscapeString(wf.Repo))
		}
		if len(wf.Repos) > 1 {
			extras := wf.Repos[1:]
			parts := make([]string, len(extras))
			for i, r := range extras {
				parts[i] = html.EscapeString(r)
			}
			fmt.Fprintf(w, `<span class="text-gray-500">+ %d other%s:</span> <span class="font-mono text-gray-700 dark:text-gray-300">%s</span>`,
				len(extras), plural(len(extras), "s"), strings.Join(parts, ", "))
		}
		if wf.Branch != "" {
			fmt.Fprintf(w, `<span class="text-gray-500">branch:</span> <span class="font-mono text-gray-700 dark:text-gray-300">%s</span>`, html.EscapeString(wf.Branch))
		}
		fmt.Fprint(w, `</div>`)
	}

	// Pending question — inline answer form. For the approve_plan
	// gate specifically, also surface an editable plan so the user
	// can tweak steps directly (delete, edit, reorder via drag) and
	// approve the modified version in one shot.
	if wf.PendingQuestionID != "" {
		if q, ok := s.opts.Memory.GetQuestion(wf.PendingQuestionID); ok && q.Pending() {
			isApprovePlan := wf.Stage == "approve_plan" && len(wf.Plan) > 0
			pickButtons := renderRepoPickButtons(q.Question)
			qBody := q.Question
			if pickButtons != "" {
				qBody = stripRepoMenu(q.Question)
			}

			fmt.Fprint(w, `<div class="rounded-xl border border-amber-500/40 bg-amber-500/5 p-4 space-y-3">
				<div class="flex items-center gap-2 text-[11px] uppercase tracking-wider text-amber-700 dark:text-amber-400 font-semibold">
					<span class="inline-block h-1.5 w-1.5 rounded-full bg-amber-500"></span>
					paused — awaiting your answer
					<span class="ml-auto font-mono normal-case tracking-normal text-gray-400">id `+html.EscapeString(q.ID)+`</span>
				</div>
				<div class="text-sm text-gray-800 dark:text-gray-200 whitespace-pre-line leading-relaxed">`+html.EscapeString(qBody)+`</div>`)
			fmt.Fprint(w, pickButtons)

			// Plan editor for approve_plan only. Each step is an
			// editable text input; an "+ add step" button appends a
			// blank input; the ✕ on each row removes it. Submit
			// posts step[] in DOM order to /api/plan/save which
			// replaces wf.Plan and approves the gate.
			if isApprovePlan {
				fmt.Fprintf(w, `<form hx-post="/api/plan/save" hx-target="#plan-save-result-%s" hx-swap="innerHTML"
					class="rounded-lg border border-gray-200 dark:border-surface-border bg-white dark:bg-surface-raised p-3 space-y-2">
					<div class="flex items-baseline justify-between gap-2">
						<h4 class="text-[11px] font-semibold uppercase tracking-wider text-gray-500">Edit plan</h4>
						<span class="text-[11px] text-gray-400">drag titles or rewrite — empty rows are dropped on save</span>
					</div>
					<input type="hidden" name="wf_id" value="%s">
					<input type="hidden" name="q_id" value="%s">
					<ol id="plan-editor-%s" class="space-y-1.5">`,
					html.EscapeString(wf.ID),
					html.EscapeString(wf.ID),
					html.EscapeString(q.ID),
					html.EscapeString(wf.ID),
				)
				for i, ps := range wf.Plan {
					fmt.Fprintf(w, `<li class="flex items-center gap-2">
						<span class="font-mono text-xs text-gray-400 w-6 text-right shrink-0">%d.</span>
						<input type="text" name="step" value="%s"
							class="flex-1 font-mono text-sm rounded-md border border-gray-200 dark:border-surface-border bg-white dark:bg-surface px-2 py-1 focus:border-accent focus:ring-1 focus:ring-accent/30 focus:outline-none">
						<button type="button" onclick="this.closest('li').remove()" title="remove step"
							class="text-xs text-gray-400 hover:text-rose-500 transition px-2 py-1">✕</button>
					</li>`, i+1, html.EscapeString(ps.Title))
				}
				fmt.Fprintf(w, `</ol>
					<div class="flex items-center justify-between gap-2 pt-1">
						<button type="button"
							onclick="(function(ol){var li=document.createElement('li');li.className='flex items-center gap-2';li.innerHTML='<span class=\'font-mono text-xs text-gray-400 w-6 text-right shrink-0\'>+</span><input type=\'text\' name=\'step\' placeholder=\'new step…\' class=\'flex-1 font-mono text-sm rounded-md border border-gray-200 dark:border-surface-border bg-white dark:bg-surface px-2 py-1 focus:border-accent focus:ring-1 focus:ring-accent/30 focus:outline-none\'><button type=\'button\' onclick=\'this.closest(&quot;li&quot;).remove()\' class=\'text-xs text-gray-400 hover:text-rose-500 transition px-2 py-1\'>✕</button>';ol.appendChild(li);li.querySelector('input').focus();})(document.getElementById('plan-editor-%s'))"
							class="text-xs rounded-md border border-gray-300 dark:border-surface-border px-2 py-1 hover:border-accent hover:text-accent transition">+ add step</button>
						<div class="flex items-center gap-2">
							<span id="plan-save-result-%s" class="text-xs"></span>
							<button type="submit"
								class="inline-flex items-center gap-1 rounded-lg bg-accent text-surface px-3 py-1.5 text-sm font-semibold hover:brightness-110 transition">save plan &amp; approve</button>
						</div>
					</div>
				</form>`,
					html.EscapeString(wf.ID),
					html.EscapeString(wf.ID),
				)
			}

			// Always-on yes / no / free-form path. For approve_plan
			// this is the "accept as-is" / "reject" / "rephrase &
			// replan" alternative to editing.
			fmt.Fprintf(w, `<form hx-post="/api/answer" hx-target="this" hx-swap="outerHTML"
				class="space-y-2">
				<input type="hidden" name="id" value="%s">
				<div class="flex flex-col sm:flex-row gap-2">
					<input type="text" name="answer" autocomplete="off"
						placeholder="yes &nbsp;·&nbsp; no &nbsp;·&nbsp; change=/path/to/repo &nbsp;·&nbsp; free-form feedback"
						class="flex-1 font-mono text-sm rounded-lg border border-gray-300 dark:border-surface-border bg-white dark:bg-surface px-3 py-2 focus:border-accent focus:ring-2 focus:ring-accent/30 focus:outline-none">
					<div class="flex gap-2">
						<button type="submit" name="answer" value="yes" formnovalidate class="inline-flex items-center gap-1 rounded-lg bg-emerald-500 text-white px-3 py-2 text-sm font-semibold hover:bg-emerald-600 transition">yes</button>
						<button type="submit" name="answer" value="no" formnovalidate class="inline-flex items-center gap-1 rounded-lg border border-rose-500/40 bg-rose-500/5 text-rose-700 dark:text-rose-400 px-3 py-2 text-sm font-semibold hover:bg-rose-500/10 transition">no</button>
						<button type="submit" class="inline-flex items-center gap-1 rounded-lg bg-accent text-surface px-3 py-2 text-sm font-semibold hover:brightness-110 transition">send →</button>
					</div>
				</div>
			</form>`,
				html.EscapeString(q.ID),
			)
			fmt.Fprint(w, `</div>`)
		}
	}

	// Plan steps — checklist.
	if len(wf.Plan) > 0 {
		fmt.Fprintf(w, `<div>
			<h4 class="text-[11px] font-semibold uppercase tracking-wider text-gray-500 mb-2">Plan (%d steps)</h4>
			<ol class="space-y-1.5">`, len(wf.Plan))
		for i, ps := range wf.Plan {
			mark := `<span class="inline-flex h-4 w-4 items-center justify-center rounded-full border border-gray-300 dark:border-gray-700"></span>`
			cls := "text-gray-700 dark:text-gray-300"
			if ps.Done {
				mark = `<span class="inline-flex h-4 w-4 items-center justify-center rounded-full bg-emerald-500 text-white"><svg class="h-3 w-3" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="3" stroke-linecap="round" stroke-linejoin="round"><polyline points="20 6 9 17 4 12"/></svg></span>`
				cls = "text-gray-500 dark:text-gray-500 line-through"
			}
			fmt.Fprintf(w, `<li class="flex items-start gap-2 text-sm %s">
				%s
				<div class="flex-1 min-w-0">
					<span class="font-mono text-xs text-gray-400 mr-1">%d.</span>
					<span>%s</span>
				</div>
			</li>`, cls, mark, i+1, html.EscapeString(ps.Title))
		}
		fmt.Fprint(w, `</ol></div>`)
	}

	// Approvals dict — chronological.
	if len(wf.Approvals) > 0 {
		// Sort keys for stable output (otherwise map iteration order
		// makes the panel jiggle on every refresh).
		keys := make([]string, 0, len(wf.Approvals))
		for k := range wf.Approvals {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprint(w, `<div>
			<h4 class="text-[11px] font-semibold uppercase tracking-wider text-gray-500 mb-2">Approvals &amp; gates</h4>
			<dl class="space-y-1 text-sm">`)
		for _, k := range keys {
			v := wf.Approvals[k]
			fmt.Fprintf(w, `<div class="flex items-baseline gap-2">
				<dt class="font-mono text-xs text-gray-500 min-w-[140px]">%s</dt>
				<dd class="font-mono text-xs text-gray-700 dark:text-gray-300 break-words">%s</dd>
			</div>`, html.EscapeString(k), html.EscapeString(v))
		}
		fmt.Fprint(w, `</dl></div>`)
	}

	// Note (occasional engine annotation).
	if wf.Note != "" {
		fmt.Fprintf(w, `<div class="rounded-md border border-gray-200 dark:border-surface-border bg-gray-50 dark:bg-surface-sunken px-3 py-2 text-xs text-gray-700 dark:text-gray-300"><span class="text-gray-500 uppercase tracking-wider mr-2">note</span>%s</div>`,
			html.EscapeString(wf.Note))
	}

	// Verify runs (only meaningful when set).
	if wf.VerifyRuns > 0 {
		fmt.Fprintf(w, `<div class="text-xs text-gray-500">verify runs: <span class="font-mono text-gray-700 dark:text-gray-300">%d</span></div>`, wf.VerifyRuns)
	}

	// History: prior workflow attempts for this ticket.
	if wf.TicketID != "" {
		history := s.opts.Memory.HistoryWorkflowsFor(wf.TicketID)
		// Filter out the current workflow.
		filtered := make([]memory.Workflow, 0, len(history))
		for _, h := range history {
			if h.ID != wf.ID {
				filtered = append(filtered, h)
			}
		}
		if len(filtered) > 0 {
			fmt.Fprintf(w, `<div>
				<h4 class="text-[11px] font-semibold uppercase tracking-wider text-gray-500 mb-2">History (%d earlier attempt%s)</h4>
				<ul class="space-y-1 text-xs">`, len(filtered), plural(len(filtered), "s"))
			for _, h := range filtered {
				note := h.Stage
				if h.Error != "" {
					note = "✗ " + h.Error
				}
				fmt.Fprintf(w, `<li class="flex items-baseline gap-2">
					<span class="font-mono text-gray-400 min-w-[88px]">%s</span>
					%s
					<span class="text-gray-700 dark:text-gray-300 truncate">%s</span>
				</li>`,
					html.EscapeString(fuzzyTime(h.UpdatedAt)),
					workflowStateChip(string(h.State)),
					html.EscapeString(util.Truncate(note, 200)),
				)
			}
			fmt.Fprint(w, `</ul></div>`)
		}
	}

	fmt.Fprint(w, `</div>`)
}

// workflowStateChip renders a small workflow-state badge. Flat —
// no border, low-opacity background, paired text colors. Less
// visual weight than the bordered-and-shadowed previous version.
// workflowStateChip maps a workflow lifecycle state to a colored badge.
// Palette tones:
//   - done        → emerald (universally "shipped")
//   - failed      → rose (universally "stop / broken")
//   - awaiting_approval → amber highlight (the user must act)
//   - active phases (executing/testing/verifying/etc) → purple accent
//                   (live brand state — matches the daemon dot)
//   - planning phases (triaging/planning) → soft purple wash
func workflowStateChip(state string) string {
	cls := "bg-surface-raised text-muted border border-surface-border"
	switch state {
	case "done":
		cls = "bg-emerald-500/15 text-emerald-400 border border-emerald-500/40"
	case "failed":
		cls = "bg-rose-500/15 text-rose-400 border border-rose-500/40"
	case "awaiting_approval":
		cls = "bg-highlight/15 text-highlight border border-highlight/40"
	case "executing", "testing", "verifying", "opening_pr", "notifying", "updating_memory":
		cls = "bg-accent/15 text-accent border border-accent/40"
	case "triaging", "planning":
		cls = "bg-accent-soft text-accent border border-accent/25"
	}
	return fmt.Sprintf(`<span class="inline-flex shrink-0 items-center rounded-full px-2 py-0.5 text-[11px] font-medium %s">%s</span>`,
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
	fmt.Fprint(w, `<div class="space-y-4">`)
	for i, q := range pending {
		// Try to guess at the gate type so we can label appropriately.
		// Both gate kinds use the same amber rail so the user's eye
		// learns "amber stripe = goon needs me". Tone variation is
		// reserved for the small pill, which whispers the gate kind.
		gateLabel := "approval"
		gateTone := "bg-highlight/15 text-highlight border-highlight/40"
		ql := strings.ToLower(q.Question)
		switch {
		case strings.Contains(ql, "confirm_repo") || strings.Contains(ql, "repo path") || strings.Contains(ql, "which repo"):
			gateLabel = "confirm repo"
		case strings.Contains(ql, "approve") || strings.Contains(ql, "plan"):
			gateLabel = "approve plan"
			gateTone = "bg-accent/15 text-accent border-accent/40"
		}
		ticketLabel := ""
		if q.TicketID != "" {
			ticketLabel = fmt.Sprintf(`<span class="inline-flex items-center gap-1 text-xs font-mono text-muted"><svg class="h-3 w-3" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M20.59 13.41l-7.17 7.17a2 2 0 0 1-2.83 0L2 12V2h10l8.59 8.59a2 2 0 0 1 0 2.82z"/><line x1="7" y1="7" x2="7.01" y2="7"/></svg>%s</span>`, html.EscapeString(q.TicketID))
		}
		// Parse the question body for a numbered repo menu (lines like
		// " * 1. eng-app"). When present, render each as a clickable
		// button that submits the corresponding number — no typing
		// required.
		pickButtons := renderRepoPickButtons(q.Question)
		qBody := q.Question
		if pickButtons != "" {
			qBody = stripRepoMenu(q.Question)
		}
		// Only the FIRST card gets the cta-glow animation. If we glow
		// every card the loudness loses meaning; the user's eye should
		// land on the top of the stack and work down.
		cardGlow := ""
		if i == 0 {
			cardGlow = " shadow-glow-amber"
		}
		// data-question-id is the scroll-into-view target for
		// goonJumpToQuestion (called from a workflow card's
		// "→ answer" button).
		qIDEsc := html.EscapeString(q.ID)
		fmt.Fprintf(w, `<form hx-post="/api/answer" hx-target="this" hx-swap="outerHTML"
			data-question-id="%s"
			class="relative overflow-hidden rounded-xl border border-highlight/40 bg-surface-raised shadow-card hover:shadow-lift transition-shadow%s">
			<div class="absolute left-0 top-0 bottom-0 w-1 bg-highlight"></div>
			<div class="px-5 py-4 space-y-3">
				<div class="flex items-center gap-2 flex-wrap">
					<span class="inline-flex items-center gap-1 rounded-full border px-2 py-0.5 text-[11px] font-semibold uppercase tracking-wider %s">
						<svg class="h-3 w-3 animate-pulse-dot" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><circle cx="12" cy="12" r="4"/></svg>
						%s
					</span>
					%s
					<span class="text-[11px] font-mono text-muted/70 ml-auto">id %s</span>
				</div>
				<div class="text-sm text-white whitespace-pre-line leading-relaxed">%s</div>
				%s
				<input type="hidden" name="id" value="%s">
				<div class="flex flex-col sm:flex-row gap-2 pt-1">
					<input type="text" name="answer" autocomplete="off" autofocus
						placeholder="yes &nbsp;·&nbsp; no &nbsp;·&nbsp; change=/path/to/repo &nbsp;·&nbsp; free-form feedback"
						class="flex-1 font-mono text-sm rounded-lg border border-surface-border bg-surface text-white placeholder:text-muted/60 px-3 py-2 focus:border-accent focus:ring-2 focus:ring-accent/30 focus:outline-none">
					<div class="flex gap-2">
						<button type="submit" name="answer" value="yes" formnovalidate
							class="inline-flex items-center gap-1 rounded-lg bg-emerald-500 text-surface px-3.5 py-2 text-sm font-bold hover:bg-emerald-400 transition shadow-card">
							<svg class="h-4 w-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><polyline points="20 6 9 17 4 12"/></svg>
							yes
						</button>
						<button type="submit" name="answer" value="no" formnovalidate
							class="inline-flex items-center gap-1 rounded-lg border border-rose-500/40 bg-rose-500/5 text-rose-300 px-3.5 py-2 text-sm font-semibold hover:bg-rose-500/15 transition">
							<svg class="h-4 w-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/></svg>
							no
						</button>
						<button type="submit"
							class="inline-flex items-center gap-1 rounded-lg bg-accent text-white px-3.5 py-2 text-sm font-bold hover:brightness-110 hover:shadow-neon transition">
							send →
						</button>
					</div>
				</div>
			</div>
		</form>`, qIDEsc, cardGlow, gateTone, gateLabel, ticketLabel, html.EscapeString(q.ID), html.EscapeString(qBody), pickButtons, html.EscapeString(q.ID))
	}
	fmt.Fprint(w, `</div>`)
}

// renderRepoPickButtons scans the question body for the numbered menu
// format that buildRepoGateQuestion in internal/workflow emits:
//
//	   1. repo-a
//	 * 2. repo-b              (the "*" marks the suggested one)
//	   3. repo-c
//	   4. owner/svc (remote)  (remote-tagged entries)
//
// and returns a multi-select panel: a row of checkboxes (so the user
// can pick more than one), a typeahead filter (essential when an org
// has 100+ repos), an overflow expander, plus a submit button that
// posts a comma-separated answer. Returns "" if no menu is detected.
//
// Ordering: suggested options first (preserving their original number
// badges), then alphabetical. Anything past initialVisibleOthers in
// the non-suggested set is hidden under a "show all" expander until
// the user clicks it or types into the filter.
func renderRepoPickButtons(question string) string {
	type opt struct {
		num      int
		name     string
		isSug    bool
		isRemote bool
	}
	var opts []opt
	for _, raw := range strings.Split(question, "\n") {
		line := strings.TrimRight(raw, " \t")
		trimmed := strings.TrimLeft(line, " \t")
		isSug := false
		if strings.HasPrefix(trimmed, "*") {
			isSug = true
			trimmed = strings.TrimSpace(trimmed[1:])
		}
		dot := strings.IndexByte(trimmed, '.')
		if dot <= 0 {
			continue
		}
		// Bumped from 99 → 999: orgs routinely have 100+ repos, and
		// the old cap silently dropped items 100+ from the UI (the
		// user could see them in the prompt text but not click them).
		n, err := strconv.Atoi(strings.TrimSpace(trimmed[:dot]))
		if err != nil || n < 1 || n > 999 {
			continue
		}
		rest := strings.TrimSpace(trimmed[dot+1:])
		if rest == "" {
			continue
		}
		isRemote := false
		if strings.HasSuffix(rest, "(remote)") {
			isRemote = true
			rest = strings.TrimSpace(strings.TrimSuffix(rest, "(remote)"))
		}
		opts = append(opts, opt{num: n, name: rest, isSug: isSug, isRemote: isRemote})
	}
	if len(opts) < 2 {
		return ""
	}

	// Suggested first, then alphabetical — the original numeric ordering
	// is preserved by the per-pill `num` badge, but on long lists the
	// alphabetical sort makes a name easy to find with the eye.
	sort.SliceStable(opts, func(i, j int) bool {
		if opts[i].isSug != opts[j].isSug {
			return opts[i].isSug
		}
		return strings.ToLower(opts[i].name) < strings.ToLower(opts[j].name)
	})

	// Visibility budget: every suggested option always shows; among the
	// non-suggested set, only the first initialVisibleOthers render by
	// default. The rest are tagged data-overflow="1" and hidden until
	// the user expands or types into the filter.
	// Cut from 8 → 5 after a 100-repo org filled the screen with
	// alphabetical noise even when triage had marked 1-2 picks.
	const initialVisibleOthers = 5
	overflowNums := map[int]bool{}
	others := 0
	for _, o := range opts {
		if o.isSug {
			continue
		}
		if others < initialVisibleOthers {
			others++
		} else {
			overflowNums[o.num] = true
		}
	}
	overflowCount := len(overflowNums)
	showFilter := len(opts) > initialVisibleOthers+1

	// Unique form id so multiple pending questions on one page don't
	// collide on their JS hooks.
	mid := fmt.Sprintf("ms%d", time.Now().UnixNano())

	var sb strings.Builder
	sb.WriteString(`<div class="space-y-2 pt-1">`)
	fmt.Fprintf(&sb, `<div class="text-[11px] uppercase tracking-wider text-muted">Pick one or more — first selected becomes the primary repo (%d options):</div>`, len(opts))
	if showFilter {
		fmt.Fprintf(&sb, `<input type="text" data-pick-filter="%s" placeholder="filter repos by name…" autocomplete="off"
			class="w-full rounded-lg border border-surface-border bg-surface px-3 py-1.5 text-sm focus:border-accent focus:outline-none"
			oninput="goonPickRefresh('%s')">`, mid, mid)
	}
	fmt.Fprintf(&sb, `<div class="flex flex-wrap gap-2" data-pick-group="%s">`, mid)
	for _, o := range opts {
		cls := "border-surface-border bg-surface text-muted hover:border-accent hover:text-white"
		// Leading marker per pill. A ★ for suggested replaces the old
		// "suggested" word badge — eye-trackable in a long list, no
		// horizontal whitespace cost.
		marker := `<span class="text-xs opacity-30" aria-hidden="true">·</span>`
		if o.isSug {
			cls = "border-accent/50 bg-accent-soft text-accent hover:bg-accent hover:text-white"
			marker = `<span class="text-amber-400" aria-label="suggested by triage" title="suggested by triage">★</span>`
		}
		remoteBadge := ""
		if o.isRemote {
			remoteBadge = `<span class="text-[10px] uppercase tracking-wider text-highlight">remote</span>`
		}
		overflowAttr := ""
		hiddenStyle := ""
		if overflowNums[o.num] {
			overflowAttr = ` data-overflow="1"`
			hiddenStyle = ` style="display:none"`
		}
		// Pill markup. data-pick still carries the menu number (the
		// answer parser is index-driven, so this can't go away), but
		// it is NOT shown to the user — the leading integer was just
		// noise (random map-iteration order) that read like a rank.
		fmt.Fprintf(&sb, `<label data-pick-pill="%s" data-pick-name="%s"%s%s class="inline-flex items-center gap-2 rounded-lg border px-3 py-1.5 text-sm font-medium transition cursor-pointer %s">
			<input type="checkbox" data-pick="%d" data-group="%s" class="h-3.5 w-3.5 accent-accent" onchange="goonPickToggle('%s')">
			%s
			<span>%s</span>
			%s
		</label>`, mid, html.EscapeString(strings.ToLower(o.name)), overflowAttr, hiddenStyle, cls, o.num, mid, mid, marker, html.EscapeString(o.name), remoteBadge)
	}
	sb.WriteString(`</div>`)
	if overflowCount > 0 {
		fmt.Fprintf(&sb, `<button type="button" data-pick-expand="%s" onclick="goonPickExpand('%s')"
			class="text-[11px] text-muted underline-offset-2 hover:text-accent hover:underline transition">
			show all %d (%d more) →
		</button>`, mid, mid, len(opts), overflowCount)
	}
	fmt.Fprintf(&sb, `<div class="flex items-center gap-2 pt-1">
		<button type="submit" name="answer" data-pick-submit="%s" formnovalidate disabled
			class="inline-flex items-center gap-1.5 rounded-lg bg-accent text-white px-3 py-1.5 text-sm font-bold opacity-40 cursor-not-allowed transition">
			<span data-pick-label="%s">pick a repo</span>
		</button>
		<span class="text-[11px] text-muted" data-pick-summary="%s"></span>
	</div>`, mid, mid, mid)
	sb.WriteString(`</div>`)
	// goonPickToggle: existing behavior + auto-graduate a checked
	// overflow pill out of the hidden set, so the user never loses
	// sight of what they've picked when the filter clears.
	// goonPickRefresh: filter + overflow visibility recomputation.
	// goonPickExpand: reveal the long tail.
	sb.WriteString(`<script>
		window.goonPickToggle = window.goonPickToggle || function(mid) {
			var boxes = document.querySelectorAll('input[data-group="' + mid + '"]:checked');
			boxes.forEach(function(b) {
				var pill = b.closest('[data-pick-pill="' + mid + '"]');
				if (pill && pill.getAttribute('data-overflow') === '1') {
					pill.removeAttribute('data-overflow');
				}
			});
			var btn = document.querySelector('button[data-pick-submit="' + mid + '"]');
			var lbl = document.querySelector('[data-pick-label="' + mid + '"]');
			var sum = document.querySelector('[data-pick-summary="' + mid + '"]');
			var nums = Array.from(boxes).map(function(b){ return b.getAttribute('data-pick'); });
			// Look up the human-readable repo name for each selected pill,
			// so the summary reads "primary: meditap/api · others: …"
			// instead of "#71 · #52" (the leading integer was removed
			// from the pill, so a numeric summary is now meaningless).
			var names = Array.from(boxes).map(function(b){
				var pill = b.closest('[data-pick-pill="' + mid + '"]');
				var name = pill && pill.querySelector('span:not([class*="text-amber"]):not([class*="opacity-30"]):not([class*="text-highlight"])');
				return name ? name.textContent.trim() : '#' + b.getAttribute('data-pick');
			});
			if (nums.length === 0) {
				btn.disabled = true;
				btn.classList.add('opacity-40','cursor-not-allowed');
				btn.value = '';
				lbl.textContent = 'pick a repo';
				if (sum) sum.textContent = '';
			} else {
				btn.disabled = false;
				btn.classList.remove('opacity-40','cursor-not-allowed');
				btn.value = nums.join(',');
				lbl.textContent = (nums.length === 1 ? 'use pick' : 'use ' + nums.length + ' picks') + ' →';
				if (sum) sum.textContent = 'primary: ' + names[0] + (names.length > 1 ? ' · others: ' + names.slice(1).join(', ') : '');
			}
		};
		window.goonPickRefresh = window.goonPickRefresh || function(mid) {
			var input = document.querySelector('input[data-pick-filter="' + mid + '"]');
			var q = ((input && input.value) || '').toLowerCase().trim();
			document.querySelectorAll('[data-pick-pill="' + mid + '"]').forEach(function(p) {
				var name = (p.getAttribute('data-pick-name') || '').toLowerCase();
				var matchFilter = !q || name.indexOf(q) >= 0;
				var inOverflow = p.getAttribute('data-overflow') === '1' && !q;
				p.style.display = (matchFilter && !inOverflow) ? '' : 'none';
			});
			var btn = document.querySelector('[data-pick-expand="' + mid + '"]');
			if (btn) {
				var hasOverflow = document.querySelectorAll('[data-pick-pill="' + mid + '"][data-overflow="1"]').length > 0;
				btn.style.display = (hasOverflow && !q) ? '' : 'none';
			}
		};
		window.goonPickExpand = window.goonPickExpand || function(mid) {
			document.querySelectorAll('[data-pick-pill="' + mid + '"][data-overflow="1"]').forEach(function(p) {
				p.removeAttribute('data-overflow');
			});
			window.goonPickRefresh(mid);
		};
	</script>`)
	return sb.String()
}

// stripRepoMenu removes the numbered "Available repos:" block from a
// confirm_repo question body, leaving just the preamble. Called when
// the picker is being rendered right below — the prose enumeration of
// 100+ repos would otherwise duplicate every checkbox and bury the
// actual ticket context. The numbered references are still preserved
// for answer parsing (the picker submits the same numbers).
func stripRepoMenu(body string) string {
	lines := strings.Split(body, "\n")
	out := make([]string, 0, len(lines))
	repoCount := 0
	headerStripped := false
	hintStripped := false
	for _, raw := range lines {
		line := strings.TrimRight(raw, " \t")
		if strings.EqualFold(strings.TrimSpace(line), "Available repos:") {
			headerStripped = true
			continue
		}
		// Drop the CLI/Telegram "Reply with ..." hint line(s) from the
		// web rendering — web users have buttons for every option, so
		// the prose is pure noise. Covers the short new form ("Reply
		// with a number, `yes`, or `no`.") and the old verbose form
		// ("Reply: <n> or <n>,<n>,<n> ... change=<path> ... no").
		trimLower := strings.ToLower(strings.TrimSpace(line))
		if strings.HasPrefix(trimLower, "reply:") || strings.HasPrefix(trimLower, "reply with") {
			hintStripped = true
			continue
		}
		// Also drop a "Triage suggests N repos ..." multi-pick preamble
		// — the picker shows the same picks as ★ badges below.
		if strings.HasPrefix(trimLower, "triage suggests ") {
			hintStripped = true
			continue
		}
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, "*") || strings.HasPrefix(trimmed, "→") {
			trimmed = strings.TrimSpace(trimmed[1:])
		}
		if dot := strings.IndexByte(trimmed, '.'); dot > 0 {
			if n, err := strconv.Atoi(strings.TrimSpace(trimmed[:dot])); err == nil && n >= 1 && n <= 999 {
				repoCount++
				continue
			}
		}
		out = append(out, raw)
	}
	if !headerStripped && repoCount == 0 && !hintStripped {
		return body
	}
	// Collapse runs of blank lines the strip leaves behind.
	var sb strings.Builder
	prevBlank := false
	for _, line := range out {
		blank := strings.TrimSpace(line) == ""
		if blank && prevBlank {
			continue
		}
		sb.WriteString(line)
		sb.WriteByte('\n')
		prevBlank = blank
	}
	cleaned := strings.TrimRight(sb.String(), "\n")
	if repoCount > 0 {
		cleaned += fmt.Sprintf("\n\n(picker below — %d repo option%s)", repoCount, pluralS(repoCount))
	}
	return cleaned
}

// emptyState is the standardized empty-list panel — title + helpful hint.
// A dashed accent-purple border + a soft surface-raised wash signals
// "this slot is ready for content" without screaming.
func emptyState(title, hint string) string {
	return fmt.Sprintf(`<div class="rounded-xl border border-dashed border-accent/25 bg-surface-raised/40 p-8 text-center">
		<div class="text-sm font-semibold text-white">%s</div>
		<div class="mt-1 text-xs text-muted max-w-md mx-auto">%s</div>
	</div>`, html.EscapeString(title), html.EscapeString(hint))
}

// --- Tab composers ---------------------------------------------------------
//
// Each tab is a small wrapper that sets the section heading + spacing,
// then defers to the underlying fragment for the actual data.

// pageHeader renders the common title + optional description + action
// strip used by every standalone tab composer. Keeps tabs visually
// consistent.
func pageHeader(title, blurb, action string) string {
	desc := ""
	if blurb != "" {
		desc = fmt.Sprintf(`<p class="mt-1 text-sm text-muted max-w-2xl">%s</p>`, blurb)
	}
	act := ""
	if action != "" {
		act = action
	}
	return fmt.Sprintf(`<div class="mb-5 flex items-start justify-between gap-4 flex-wrap">
		<div>
			<h2 class="text-2xl font-semibold tracking-tight text-white">%s</h2>
			%s
		</div>
		%s
	</div>`, html.EscapeString(title), desc, act)
}

// refreshButton is the small "↻ refresh from board" button reused on
// Questions/Workflows/Tickets/PRs page headers.
func refreshButton() string {
	return `<button type="button" hx-post="/api/refresh" hx-target="#page-refresh-result" hx-swap="innerHTML"
		class="inline-flex items-center gap-1.5 rounded-md border border-surface-border px-2.5 py-1.5 text-xs text-muted hover:border-accent hover:text-accent hover:bg-accent-soft transition">
		<svg class="h-3 w-3" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12a9 9 0 1 1-3-6.7L21 8"/><path d="M21 3v5h-5"/></svg>
		refresh
	</button>
	<span id="page-refresh-result"></span>`
}

// fragTabQuestions — landing page. Pending approvals, blocking work.
// Inline answer forms (yes / no / repo-pick / free text) so users
// unblock workflows in one place without flipping tabs.
func (s *Server) fragTabQuestions(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, pageHeader("Questions",
		"Workflows pause here at <code class=\"font-mono text-xs\">confirm_repo</code> and <code class=\"font-mono text-xs\">approve_plan</code> gates. Answer to unblock — replies route to the matching workflow within a second.",
		""))
	fmt.Fprint(w, `<div hx-get="/fragments/questions" hx-trigger="load, questionsChanged from:body" hx-swap="morph">
		<div class="text-sm text-gray-500">Loading approvals…</div>
	</div>`)
}

// fragTabWorkflows — workflow runs, deduped per ticket, expandable
// cards. Auto-refreshes via the workflowsChanged SSE event.
func (s *Server) fragTabWorkflows(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, pageHeader("Workflows",
		"What goon is doing right now. Each card shows plan progress, the open PR, errors, and a clickable history of earlier attempts.",
		""))
	// Workflow-config header band — shows the user "which pipeline am
	// I running?" (the #1 source of confusion when goon has multiple
	// workflow.json files) plus a one-tap "view / edit config" expander.
	// Lazy-loads via /fragments/workflow-config so the user can edit
	// the workflow.json from this tab without dropping to a terminal.
	fmt.Fprint(w, `<div class="mb-4" hx-get="/fragments/workflow-config" hx-trigger="load, workflowConfigChanged from:body" hx-swap="innerHTML">
		<div class="rounded-xl border border-dashed border-surface-border bg-surface-raised/40 px-4 py-3 text-sm text-muted">Loading workflow config…</div>
	</div>`)
	fmt.Fprint(w, `<div hx-get="/fragments/workflows" hx-trigger="load, workflowsChanged from:body" hx-swap="morph">
		<div class="text-sm text-gray-500">Loading workflows…</div>
	</div>`)
}

// fragTabTickets — full ticket table + client-side filter + refresh
// button. The board mirror, unfiltered by default.
func (s *Server) fragTabTickets(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, pageHeader("Tickets",
		"Live mirror of the configured board. Click <code class=\"font-mono text-xs\">⋯ actions</code> on any row to comment, transition, or edit the ticket directly.",
		refreshButton()))
	// Filter bar.
	fmt.Fprint(w, `<div class="flex flex-wrap items-center gap-3 mb-3 p-3 rounded-lg bg-gray-50 dark:bg-surface-sunken border border-gray-200 dark:border-surface-border">
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

	<div hx-get="/fragments/tickets" hx-trigger="load, ticketsChanged from:body" hx-swap="morph">
		<div class="text-sm text-gray-500">Loading tickets…</div>
	</div>

	<script>
	(function() {
		if (window.filterTickets) return; // defined on first reveal, reuse after.
		window.filterTickets = function(q) {
			q = (q || '').trim().toLowerCase();
			const status = document.getElementById('ticket-status-filter')?.value || '';
			const rows = document.querySelectorAll('tr[data-ticket-row]');
			let visible = 0;
			rows.forEach(function(r) {
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
		document.body.addEventListener('htmx:afterSwap', function() {
			const f = document.getElementById('ticket-filter');
			if (f) filterTickets(f.value);
		});
	})();
	</script>`)
}

// fragTabPRs — Repositories page. Reframed from a flat PR firehose
// to a repo-centric list (REPOSITORY.md ∪ repos that returned PRs),
// each row expandable to per-repo detail with map / clone / PR
// actions. Internal route name stays "tab-prs" + data-page="prs"
// so existing bookmarks and the sidebar's showPage() logic don't
// need to change; the user-visible label is "Repositories".
func (s *Server) fragTabPRs(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, pageHeader("Repositories",
		"Every repo goon works on, with its open PRs underneath. Click a row to expand — map a local path, clone if it isn't on disk yet, or act on a PR (comment / approve / block).",
		""))
	fmt.Fprint(w, `<div hx-get="/fragments/repositories" hx-trigger="load, prsChanged from:body, repositoriesChanged from:body" hx-swap="innerHTML">
		<div class="text-sm text-gray-500">Loading repositories…</div>
	</div>`)
}

// fragTabDashboard — the Home page. Snapshot-of-everything view:
// stats strip, the live workflow goon is chewing on, blocking
// questions, recent tickets. Designed to be the first thing a user
// sees on every visit so they don't have to click around to know
// "what's happening right now."
//
// Every section listens to its own SSE event so the page is
// incremental — no full reload, just morphs in place when state
// changes.
func (s *Server) fragTabDashboard(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	mem := s.opts.Memory
	st := mem.GetStatus()
	pending := mem.PendingQuestions()
	wfs := mem.ListWorkflows(50)
	tix := mem.ListTickets()

	// --- stats strip --------------------------------------------------
	// Active workflow count = anything not in a terminal state. Cheap
	// linear scan — workflow list capped at 50 by ListWorkflows.
	active := 0
	var liveWF *memory.Workflow
	for i := range wfs {
		w := wfs[i]
		switch w.State {
		case memory.WFDone, memory.WFFailed:
			// terminal
		default:
			active++
			if liveWF == nil || w.UpdatedAt.After(liveWF.UpdatedAt) {
				cp := w
				liveWF = &cp
			}
		}
	}
	lastPoll := "—"
	if !st.LastPoll.IsZero() {
		lastPoll = humanizeSince(st.LastPoll)
	}
	pendingTone := "neutral"
	if len(pending) > 0 {
		pendingTone = "amber"
	}
	activeTone := "neutral"
	if active > 0 {
		activeTone = "accent"
	}
	daemonState := "stopped"
	daemonTone := "rose"
	if st.Running {
		daemonState = "running"
		daemonTone = "emerald"
		if st.Paused {
			daemonState = "paused"
			daemonTone = "amber"
		}
	}

	fmt.Fprint(w, pageHeader("Home",
		"At-a-glance view of what goon is doing — blocking questions, the live workflow, the daemon's pulse. Everything below auto-refreshes via SSE.",
		refreshButton()))

	fmt.Fprint(w, `<div class="grid grid-cols-2 lg:grid-cols-4 gap-3 mb-6">`)
	fmt.Fprint(w, statCard("pending questions", fmt.Sprintf("%d", len(pending)), pendingTone))
	fmt.Fprint(w, statCard("active workflows", fmt.Sprintf("%d", active), activeTone))
	fmt.Fprint(w, statCard("tickets seen", fmt.Sprintf("%d", len(tix)), "neutral"))
	fmt.Fprint(w, statCard("daemon · "+lastPoll, daemonState, daemonTone))
	fmt.Fprint(w, `</div>`)

	// --- two-column body: live workflow + sidebar list ----------------
	fmt.Fprint(w, `<div class="grid grid-cols-1 lg:grid-cols-3 gap-6">`)

	// Left column (2/3): live workflow + recent tickets.
	fmt.Fprint(w, `<div class="lg:col-span-2 space-y-6">`)
	fmt.Fprint(w, `<section>
		<div class="flex items-center justify-between mb-2">
			<h3 class="text-sm font-semibold text-gray-700 dark:text-gray-300">Live workflow</h3>
			<a href="#" onclick="showPage('workflows');return false;" class="text-xs text-gray-500 hover:text-accent transition">see all →</a>
		</div>`)
	if liveWF == nil {
		fmt.Fprint(w, `<div class="rounded-lg border border-dashed border-gray-300 dark:border-surface-border bg-gray-50/60 dark:bg-surface-sunken/40 p-6 text-center text-sm text-gray-500">
			Nothing running right now. goon polls the board every <code class="font-mono">PollInterval</code> — once a ticket matches, it'll appear here.
		</div>`)
	} else {
		stage := liveWF.Stage
		if stage == "" {
			stage = string(liveWF.State)
		}
		title := liveWF.Title
		if title == "" {
			title = liveWF.TicketKey
		}
		fmt.Fprintf(w, `<div class="rounded-lg border border-accent/30 bg-accent/5 p-4">
			<div class="flex items-start justify-between gap-3">
				<div class="min-w-0">
					<div class="text-xs font-mono text-accent">%s · %s</div>
					<div class="mt-1 text-sm font-semibold truncate">%s</div>
					<div class="mt-2 text-xs text-gray-500">stage: <code class="font-mono">%s</code> · updated %s</div>
				</div>
				<a href="#" onclick="showPage('workflows');return false;" class="text-xs rounded-md border border-accent/40 text-accent px-2.5 py-1 hover:bg-accent/10 transition">open</a>
			</div>
		</div>`,
			html.EscapeString(liveWF.TicketKey),
			html.EscapeString(string(liveWF.State)),
			html.EscapeString(title),
			html.EscapeString(stage),
			html.EscapeString(humanizeSince(liveWF.UpdatedAt)),
		)
	}
	fmt.Fprint(w, `</section>`)

	// Recent tickets (top 5).
	fmt.Fprint(w, `<section>
		<div class="flex items-center justify-between mb-2">
			<h3 class="text-sm font-semibold text-gray-700 dark:text-gray-300">Recent tickets</h3>
			<a href="#" onclick="showPage('tickets');return false;" class="text-xs text-gray-500 hover:text-accent transition">see all →</a>
		</div>`)
	if len(tix) == 0 {
		fmt.Fprint(w, `<div class="rounded-lg border border-dashed border-gray-300 dark:border-surface-border bg-gray-50/60 dark:bg-surface-sunken/40 p-6 text-center text-sm text-gray-500">
			No tickets yet. Configure a board under <a href="#" onclick="showPage('setup');return false;" class="text-accent hover:underline">Setup</a> and hit refresh.
		</div>`)
	} else {
		// Sort by UpdatedAt desc, take top 5.
		recent := append([]memory.TicketSnapshot(nil), tix...)
		sort.SliceStable(recent, func(i, j int) bool {
			return recent[i].UpdatedAt.After(recent[j].UpdatedAt)
		})
		if len(recent) > 5 {
			recent = recent[:5]
		}
		fmt.Fprint(w, `<ul class="rounded-lg border border-gray-200 dark:border-surface-border bg-white dark:bg-surface-raised divide-y divide-gray-100 dark:divide-surface-border/60">`)
		for _, t := range recent {
			key := t.Key
			if key == "" {
				key = t.ID
			}
			title := t.Title
			if title == "" {
				title = "(no title)"
			}
			status := t.Status
			if status == "" {
				status = "—"
			}
			fmt.Fprintf(w, `<li class="px-3 py-2.5 flex items-center gap-3 text-sm">
				<span class="font-mono text-xs text-gray-500 shrink-0">%s</span>
				<span class="flex-1 min-w-0 truncate">%s</span>
				<span class="text-[11px] font-mono text-gray-500 shrink-0">%s</span>
				<span class="text-[11px] text-gray-400 shrink-0">%s</span>
			</li>`,
				html.EscapeString(key),
				html.EscapeString(title),
				html.EscapeString(status),
				html.EscapeString(humanizeSince(t.UpdatedAt)),
			)
		}
		fmt.Fprint(w, `</ul>`)
	}
	fmt.Fprint(w, `</section>`)
	fmt.Fprint(w, `</div>`) // end left column

	// Right column (1/3): pending questions + quick actions.
	fmt.Fprint(w, `<div class="space-y-6">`)
	fmt.Fprint(w, `<section>
		<div class="flex items-center justify-between mb-2">
			<h3 class="text-sm font-semibold text-gray-700 dark:text-gray-300">Blocking questions</h3>
			<a href="#" onclick="showPage('questions');return false;" class="text-xs text-gray-500 hover:text-accent transition">answer →</a>
		</div>`)
	if len(pending) == 0 {
		fmt.Fprint(w, `<div class="rounded-lg border border-emerald-500/30 bg-emerald-500/5 p-4 text-sm text-emerald-700 dark:text-emerald-400">
			✓ no pending approvals. Nothing is waiting on you.
		</div>`)
	} else {
		preview := pending
		if len(preview) > 3 {
			preview = preview[:3]
		}
		fmt.Fprint(w, `<ul class="space-y-2">`)
		for _, q := range preview {
			text := q.Question
			if len(text) > 140 {
				text = text[:137] + "…"
			}
			fmt.Fprintf(w, `<li class="rounded-lg border border-amber-500/30 bg-amber-500/5 p-3 text-sm">
				<div class="text-[11px] font-mono text-amber-700 dark:text-amber-400">%s · %s</div>
				<div class="mt-1 text-gray-700 dark:text-gray-300">%s</div>
			</li>`,
				html.EscapeString(q.TicketID),
				html.EscapeString(humanizeSince(q.When)),
				html.EscapeString(text),
			)
		}
		fmt.Fprint(w, `</ul>`)
		if len(pending) > 3 {
			fmt.Fprintf(w, `<div class="mt-2 text-xs text-gray-500">+%d more — <a href="#" onclick="showPage('questions');return false;" class="text-accent hover:underline">go answer them</a></div>`,
				len(pending)-3)
		}
	}
	fmt.Fprint(w, `</section>`)

	// Quick actions strip.
	fmt.Fprint(w, `<section>
		<h3 class="text-sm font-semibold text-gray-700 dark:text-gray-300 mb-2">Quick actions</h3>
		<div class="rounded-lg border border-gray-200 dark:border-surface-border bg-white dark:bg-surface-raised divide-y divide-gray-100 dark:divide-surface-border/60 text-sm">
			<button type="button" hx-post="/api/refresh" hx-target="#dash-action-result" hx-swap="innerHTML"
				class="w-full text-left flex items-center gap-3 px-3 py-2.5 hover:bg-gray-50 dark:hover:bg-surface-sunken/40 transition">
				<svg class="h-4 w-4 text-gray-500" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12a9 9 0 1 1-3-6.7L21 8"/><path d="M21 3v5h-5"/></svg>
				<span>Refresh from board</span>
			</button>
			<button type="button" onclick="goonDaemonToggle()"
				class="w-full text-left flex items-center gap-3 px-3 py-2.5 hover:bg-gray-50 dark:hover:bg-surface-sunken/40 transition">
				<svg class="h-4 w-4 text-gray-500" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="6" y="4" width="4" height="16"/><rect x="14" y="4" width="4" height="16"/></svg>
				<span>Pause / resume daemon</span>
			</button>
			<a href="#" onclick="showPage('chat');return false;"
				class="block flex items-center gap-3 px-3 py-2.5 hover:bg-gray-50 dark:hover:bg-surface-sunken/40 transition">
				<svg class="h-4 w-4 text-gray-500" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/></svg>
				<span>Open chat with goon</span>
			</a>
			<a href="#" onclick="showPage('files');return false;"
				class="block flex items-center gap-3 px-3 py-2.5 hover:bg-gray-50 dark:hover:bg-surface-sunken/40 transition">
				<svg class="h-4 w-4 text-gray-500" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z"/></svg>
				<span>Browse the workspace</span>
			</a>
			<a href="/docs" target="_blank" rel="noopener"
				class="block flex items-center gap-3 px-3 py-2.5 hover:bg-gray-50 dark:hover:bg-surface-sunken/40 transition">
				<svg class="h-4 w-4 text-gray-500" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><path d="M14 2v6h6"/></svg>
				<span>Open documentation</span>
			</a>
		</div>
		<div id="dash-action-result" class="mt-2 text-xs text-gray-500"></div>
	</section>`)

	fmt.Fprint(w, `</div>`) // end right column
	fmt.Fprint(w, `</div>`) // end grid
}

// humanizeSince formats a "X ago" string for any time. Empty for zero
// values so callers can decide what to show.
func humanizeSince(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("2006-01-02")
	}
}

// statCard / pickTone were used by the old Overview tab's KPI grid.
// The consolidated Work tab dropped that stat strip in favour of a
// single hero sentence + actionable sections, but the helpers stay
// here so future tabs can reuse the look without redefining.
//
//nolint:unused // retained as a reusable UI primitive.
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
		// "indigo" was the pre-redesign neutral-but-cool accent. With
		// the new palette the equivalent slot is the brand accent
		// itself, so collapse the case onto the accent variant.
		ring = "border-accent/30 bg-accent/5"
		accent = "text-accent"
	case "accent":
		ring = "border-accent/30 bg-accent/5"
		accent = "text-accent"
	default:
		ring = "border-surface-border bg-surface-raised"
		accent = "text-muted"
	}
	return fmt.Sprintf(`<div class="rounded-xl border %s p-4">
		<div class="text-[11px] uppercase tracking-wider text-gray-500">%s</div>
		<div class="mt-1 text-2xl font-semibold %s">%s</div>
	</div>`, ring, html.EscapeString(label), accent, html.EscapeString(value))
}

// pickTone is the neutral-vs-active selector that complements statCard.
//
//nolint:unused
func pickTone(n int, tone string) string {
	if n > 0 {
		return tone
	}
	return "neutral"
}

// Keep package-level references so the unused-helpers above survive
// dead-code elimination warnings on stricter tooling. The blank var
// is the conventional Go idiom.
var _ = statCard
var _ = pickTone

func plural(n int, suffix string) string {
	if n == 1 {
		return ""
	}
	if suffix == "" {
		return "s"
	}
	return suffix
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
