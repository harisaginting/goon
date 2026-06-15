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

	"github.com/harisaginting/goon/internal/agentpool"
	"github.com/harisaginting/goon/internal/boards"
	"github.com/harisaginting/goon/internal/checkup"
	"github.com/harisaginting/goon/internal/githost"
	"github.com/harisaginting/goon/internal/google"
	"github.com/harisaginting/goon/internal/llm"
	"github.com/harisaginting/goon/internal/memory"
	"github.com/harisaginting/goon/internal/repository"
	"github.com/harisaginting/goon/internal/usage"
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

// AutomationRunner is implemented by the daemon — the web "run now" button on a
// scheduled automation calls RunAutomationNow(name) to fire it immediately,
// ignoring its schedule. Optional: when the daemon doesn't implement it (or
// isn't wired), the run-now button reports that the daemon isn't running.
type AutomationRunner interface {
	RunAutomationNow(name string)
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
	mux.HandleFunc("/app/", s.handleApp) // catch /app/setup, /app/tickets, etc. on reload
	mux.HandleFunc("/home", s.handleApp)
	mux.HandleFunc("/docs", s.handleDocs)
	mux.HandleFunc("/api/status", s.handleAPIStatus)
	mux.HandleFunc("/api/tickets", s.handleAPITickets)
	mux.HandleFunc("/api/workflows", s.handleAPIWorkflows)
	mux.HandleFunc("/api/questions", s.handleAPIQuestions)
	mux.HandleFunc("/api/answer", s.handleAnswer)
	mux.HandleFunc("/api/answer-all", s.handleAnswerAll)
	mux.HandleFunc("/api/workflow/reset", s.handleWorkflowReset)
	mux.HandleFunc("/api/workflow/reset-stuck", s.handleWorkflowResetStuck)
	mux.HandleFunc("/fragments/workflow-diff", s.handleWorkflowDiff)
	mux.HandleFunc("/api/repo/forget-rule", s.handleRepoForgetRule)
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
	mux.HandleFunc("/api/tickets/clear", s.handleTicketsClear)
	mux.HandleFunc("/api/ticket/pick", s.handleTicketPick)
	mux.HandleFunc("/api/ticket/ignore", s.handleTicketIgnore)
	mux.HandleFunc("/api/ticket/unignore", s.handleTicketUnignore)
	// Local (goon-native) tickets — no external board required.
	mux.HandleFunc("/api/ticket/create", s.handleTicketCreate)
	mux.HandleFunc("/api/ticket/local-delete", s.handleTicketLocalDelete)
	mux.HandleFunc("/api/ticket/local-status", s.handleTicketLocalStatus)
	mux.HandleFunc("/fragments/local-tickets", s.fragLocalTickets)
	// Jira filter panel (replaces the JIRA_JQL config field).
	mux.HandleFunc("/api/ticket/jira-filter", s.handleTicketJiraFilter)
	mux.HandleFunc("/fragments/jira-filter", s.fragJiraFilter)
	mux.HandleFunc("/fragments/prs", s.handlePRList)
	mux.HandleFunc("/api/pr/comment", s.handlePRComment)
	mux.HandleFunc("/api/pr/approve", s.handlePRApprove)
	mux.HandleFunc("/api/pr/request-changes", s.handlePRRequestChanges)
	mux.HandleFunc("/api/pr/draft-review", s.handlePRDraftReview)
	// Repo picker: list all repos visible to the token, save the
	// selected subset into GOON_REVIEW_REPOS without restart.
	mux.HandleFunc("/fragments/repos-picker", s.handleReposPicker)
	mux.HandleFunc("/fragments/repo-add-list", s.handleRepoAddList)
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
	// In-browser agentic coding ("Code" tab). Pick a workdir, run the
	// agent loop against a task, stream the transcript live. Executes
	// commands + edits files — see internal/web/code.go for the safety
	// guards (workdir whitelist, safety validator, step cap, timeout).
	mux.HandleFunc("/api/code/run", s.handleCodeRun)
	mux.HandleFunc("/htmx.min.js", s.handleHTMX)
	// Brand. Served from a stable URL so favicon, og:image, and external
	// links don't need to be updated when the file moves.
	mux.HandleFunc("/logo.svg", s.handleLogo)
	mux.HandleFunc("/favicon.png", s.handleFaviconPNG)
	mux.HandleFunc("/favicon.ico", s.handleFaviconPNG) // serve PNG — wider browser support than SVG for .ico
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
	mux.HandleFunc("/fragments/automations", s.fragAutomations)
	mux.HandleFunc("/api/automation/create", s.handleAutomationCreate)
	mux.HandleFunc("/api/automation/toggle", s.handleAutomationToggle)
	mux.HandleFunc("/api/automation/run", s.handleAutomationRun)
	mux.HandleFunc("/api/automation/delete", s.handleAutomationDelete)
	mux.HandleFunc("/fragments/config", s.fragConfig)
	mux.HandleFunc("/fragments/setup", s.fragSetup)
	mux.HandleFunc("/oauth/google/start", s.handleGoogleOAuthStart)
	mux.HandleFunc("/oauth/google/callback", s.handleGoogleOAuthCallback)
	mux.HandleFunc("/api/google/disconnect", s.handleGoogleDisconnect)
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
	mux.HandleFunc("/fragments/questions-history", s.fragQuestionsHistory)
	// Dashboard cards: token usage (per model) + live child-agent sessions.
	mux.HandleFunc("/fragments/usage", s.fragUsage)
	mux.HandleFunc("/fragments/sessions", s.fragSessions)
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
	// Obsidian vault — sync button in the Knowledge tab.
	mux.HandleFunc("/api/obsidian/sync", s.handleObsidianSync)
	mux.HandleFunc("/api/obsidian/read", s.handleObsidianRead)
	mux.HandleFunc("/api/obsidian/write", s.handleObsidianWrite)
	mux.HandleFunc("/api/obsidian/push", s.handleObsidianPush)
	mux.HandleFunc("/fragments/obsidian-notes", s.fragObsidianNotes)
	// The two stores keep separate endpoints for management; the
	// composer below renders both.
	mux.HandleFunc("/fragments/tab-memory", s.fragTabMemory)
	mux.HandleFunc("/fragments/tab-knowledge", s.fragTabMemory) // legacy → memory
	mux.HandleFunc("/fragments/tab-skills", s.fragTabMemory)    // legacy → memory
	// File browser tab composer (sidebar entry "Files").
	mux.HandleFunc("/fragments/tab-files", s.fragTabFiles)
	// Code tab composer (sidebar entry "Code") — agentic coding session.
	mux.HandleFunc("/fragments/tab-code", s.fragTabCode)
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

//go:embed static/favicon.png
var faviconPNG []byte

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

// handleLogo serves the embedded brand SVG (for og:image and direct /logo.svg use).
func (s *Server) handleLogo(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
	_, _ = w.Write(logoSVG)
}

// handleFaviconPNG serves the 64×64 PNG favicon. PNG has universally better
// browser support than SVG for favicons and .ico requests.
func (s *Server) handleFaviconPNG(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=3600") // shorter cache so updates propagate faster
	_, _ = w.Write(faviconPNG)
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

// handleAnswerAll bulk-approves every pending gate question whose
// workflow is parked at a given stage (default "confirm_repo") by
// answering "yes" — i.e. accept goon's own suggestion. This is the
// relief valve for the common case where dozens of tickets all stack
// up at the repo-confirm gate; clicking each one is approval fatigue.
//
// "yes" is the exact value the single-question form's green button
// posts, so bulk approval is behaviourally identical to clicking yes
// on each card — just N at once. We deliberately scope to one stage so
// "approve all repo confirmations" never silently accepts a plan.
func (s *Server) handleAnswerAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	_ = r.ParseForm()
	stage := strings.TrimSpace(r.FormValue("stage"))
	if stage == "" {
		stage = "confirm_repo"
	}
	n, skipped := 0, 0
	for _, q := range s.opts.Memory.PendingGateQuestions() {
		// Match the question to its workflow's current stage. Skip any
		// question we can't resolve to a workflow at the target stage so
		// we never answer something unintended.
		if q.WorkflowID == "" {
			continue
		}
		wf, ok := s.opts.Memory.GetWorkflow(q.WorkflowID)
		if !ok || wf.Stage != stage {
			continue
		}
		// For repo confirmations, only "yes" workflows that have a REAL
		// suggested repo. Bulk-accepting a workflow with no suggestion
		// (or just the project key) would commit garbage and fail the
		// run — those need a manual pick.
		if stage == "confirm_repo" && s.repoSuggestionFor(wf) == "" {
			skipped++
			continue
		}
		if s.opts.Memory.AnswerQuestion(q.ID, "yes") {
			n++
		}
	}
	if waker, ok := s.opts.Daemon.(Waker); ok {
		waker.Wake()
	}
	w.Header().Set("HX-Trigger", "questionsChanged, workflowsChanged, workflowDetailRefresh")
	s.events.Publish("questionsChanged")
	s.events.Publish("workflowsChanged")
	s.events.Publish("workflowDetailRefresh")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	msg := fmt.Sprintf("accepted %d suggestion%s ✓ — daemon resuming", n, pluralS(n))
	if skipped > 0 {
		msg += fmt.Sprintf(" · %d skipped (no suggestion — pick a repo)", skipped)
	}
	fmt.Fprintf(w, `<div class="rounded-md bg-emerald-500/10 border border-emerald-500/30 px-3 py-2 text-sm text-emerald-700 dark:text-emerald-400">%s</div>`, html.EscapeString(msg))
}

// handleWorkflowReset rewinds a workflow to the very start (triage),
// clearing the plan, repo selection, approvals, pending question, error,
// and PR link. The daemon then re-runs the ticket from scratch on its
// next poll. This is the escape hatch for a workflow that went down the
// wrong path (wrong repo, stale plan, a failure you've since fixed).
func (s *Server) handleWorkflowReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	_ = r.ParseForm()
	id := strings.TrimSpace(r.FormValue("wf_id"))
	if id == "" {
		fragErr(w, "wf_id required")
		return
	}
	wf, ok := s.opts.Memory.GetWorkflow(id)
	if !ok {
		fragErr(w, "workflow not found")
		return
	}
	// Cancel any pending gate question so it no longer blocks the ticket.
	if wf.PendingQuestionID != "" {
		s.opts.Memory.AnswerQuestion(wf.PendingQuestionID, "(reset)")
	}
	wf.Stage = "triage"
	wf.State = memory.WFTriaging
	wf.Plan = nil
	wf.Repo = ""
	wf.Repos = nil
	wf.NeedsRepo = nil
	wf.PendingQuestionID = ""
	wf.Error = ""
	wf.PRURL = ""
	wf.Note = ""
	wf.VerifyRuns = 0
	wf.Approvals = map[string]string{}
	s.opts.Memory.UpsertWorkflow(wf)
	if waker, ok := s.opts.Daemon.(Waker); ok {
		waker.Wake()
	}
	w.Header().Set("HX-Trigger", "questionsChanged, workflowsChanged, workflowDetailRefresh")
	s.events.Publish("questionsChanged")
	s.events.Publish("workflowsChanged")
	s.events.Publish("workflowDetailRefresh")
	fragOK(w, "workflow reset — goon will re-run this ticket from triage on the next poll")
}

// handleRepoForgetRule deletes a remembered project→repo rule so future
// tickets in that project go back to asking. The safety valve for the
// confirm_repo "remember for this project" auto-confirm.
func (s *Server) handleRepoForgetRule(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	_ = r.ParseForm()
	project := strings.TrimSpace(r.FormValue("project"))
	if project == "" {
		fragErr(w, "project required")
		return
	}
	s.opts.Memory.ForgetRepoChoice(project)
	w.Header().Set("HX-Trigger", "repositoriesChanged")
	s.events.Publish("repositoriesChanged")
	fragOK(w, "forgot rule for "+project+" — goon will ask again next time")
}

// handleWorkflowResetStuck bulk-resets every workflow that is wedged in
// WFAwaitingApproval but whose pending question is gone (answered or
// pruned) — a dead-end that can't be answered from the UI. Each is reset
// to triage so the daemon re-runs it cleanly.
func (s *Server) handleWorkflowResetStuck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	n := 0
	for _, wf := range s.opts.Memory.ListWorkflows(0) {
		if wf.State != memory.WFAwaitingApproval {
			continue
		}
		// Stuck = has a pending-question pointer that no longer resolves
		// to an actually-pending question.
		if wf.PendingQuestionID != "" {
			if q, ok := s.opts.Memory.GetQuestion(wf.PendingQuestionID); ok && q.Pending() {
				continue // legitimately awaiting — leave it
			}
		}
		wf.Stage = "triage"
		wf.State = memory.WFTriaging
		wf.Plan = nil
		wf.Repo = ""
		wf.Repos = nil
		wf.NeedsRepo = nil
		wf.PendingQuestionID = ""
		wf.Error = ""
		wf.PRURL = ""
		wf.Note = ""
		wf.VerifyRuns = 0
		wf.Approvals = map[string]string{}
		s.opts.Memory.UpsertWorkflow(wf)
		n++
	}
	if waker, ok := s.opts.Daemon.(Waker); ok {
		waker.Wake()
	}
	w.Header().Set("HX-Trigger", "workflowsChanged, workflowDetailRefresh")
	s.events.Publish("workflowsChanged")
	s.events.Publish("workflowDetailRefresh")
	fragOK(w, fmt.Sprintf("reset %d stuck workflow%s — goon will re-run them from triage", n, pluralS(n)))
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
	// Nudge the daemon to poll immediately rather than waiting up to a
	// full PollInterval — important for recovering from an auto-pause,
	// where the user just fixed the provider and wants an instant retry.
	// SetPaused(false) already cleared the circuit breaker.
	if waker, ok := s.opts.Daemon.(Waker); ok {
		waker.Wake()
	}
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
	{Name: "GOON_LLM_CHAT", Group: "agent", Hint: `per-role model for chat — "provider:model" | "provider" | "model" (empty = use default provider)`},
	{Name: "GOON_LLM_PLAN", Group: "agent", Hint: `per-role model for triage/planning — e.g. "gemini:gemini-2.5-flash"`},
	{Name: "GOON_LLM_CODE", Group: "agent", Hint: `per-role model for the execute agent (writes code) — e.g. "anthropic:claude-sonnet-4-5"`},
	{Name: "GOON_LLM_REVIEW", Group: "agent", Hint: `per-role model for verify + PR-review drafts`},
	{Name: "GOON_BOARD", Group: "agent", Hint: "jira | github | mock"},
	{Name: "GOON_GIT_HOST", Group: "agent", Hint: "github | gitlab | bitbucket | mock (optional)"},
	{Name: "GOON_POLL_SECONDS", Default: "300", Group: "agent"},
	{Name: "GOON_VERIFY_RUNS", Default: "3", Group: "agent"},
	{Name: "GOON_TICKET_STATUSES", Default: "open,in_progress", Group: "agent", Hint: "comma-separated: open | in_progress | in_review | blocked | done"},
	{Name: "GOON_DAEMON_AUTO_START", Default: "true", Group: "agent", Hint: "set to false to start daemon paused — useful when you want to review config before polling begins"},
	{Name: "GOON_AUTO_LEARN", Default: "true", Group: "agent", Hint: "self-learning (post-run notes + daily standby reflection). off/false/0 to disable"},
	{Name: "GOON_LEARN_INTERVAL_HOURS", Default: "24", Group: "agent", Hint: "how often standby self-learning runs while idle (hours)"},
	{Name: "GOON_AUTO_CONFIRM_REPO", Group: "agent", Hint: "auto-skip the confirm_repo gate when triage picks a single repo already in REPOSITORY.md. 1/true to enable"},
	{Name: "GOON_AUTO_APPROVE_PLAN", Group: "agent", Hint: "auto-accept the plan (keeps the repo gate) so your only actions are set-repo + approve-PR. 1/true to enable"},
	{Name: "GOON_LLM_HTTP_TIMEOUT_SEC", Default: "120", Group: "agent", Hint: "per-request LLM timeout (seconds). Raise if your provider/proxy is slow and you see 'context deadline exceeded'."},
	{Name: "GOOGLE_OAUTH_CLIENT_ID", Group: "agent", Hint: "Google OAuth client id (Web application type). Paste here then click Connect Google in Setup."},
	{Name: "GOOGLE_OAUTH_CLIENT_SECRET", Sensitive: true, Group: "agent", Hint: "Google OAuth client secret (from Google Cloud Console)."},
	{Name: "GOOGLE_OAUTH_REFRESH_TOKEN", Sensitive: true, Group: "agent", Hint: "set automatically by the Connect Google button in Setup — do not edit manually."},
	{Name: "GOOGLE_CLOUD_PROJECT", Group: "agent", Hint: "GCP project id for Cloud Logging (log search) queries."},
	{Name: "GOON_WORKSPACE_DIR", Group: "agent", Hint: `parent directory holding multiple git repos — confirm_repo gate offers them as a numbered menu`},
	{Name: "GOON_MAX_STEPS", Default: "5", Group: "agent", Hint: "max tool-call steps the one-shot agent takes per task"},
	{Name: "GOON_AUTO_APPROVE", Group: "agent", Hint: "skip ALL gates (repo + plan) for fully unattended runs. 1/true to enable — use with care"},
	{Name: "GOON_LOG_LEVEL", Default: "info", Group: "agent", Hint: "debug | info | warn | error"},

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

	{Name: "CONFLUENCE_BASE_URL", Group: "confluence", Hint: "leave empty to use ATLASSIAN_BASE_URL + /wiki"},
	{Name: "CONFLUENCE_EMAIL", Group: "confluence", Hint: "leave empty to use ATLASSIAN_EMAIL"},
	{Name: "CONFLUENCE_API_TOKEN", Sensitive: true, Group: "confluence", Hint: "leave empty to use ATLASSIAN_API_TOKEN"},

	{Name: "GITHUB_TOKEN", Sensitive: true, Group: "github"},
	{Name: "GITHUB_REPOS", Group: "github", Hint: "comma-separated owner/repo,owner/repo"},
	{Name: "GITHUB_LABEL", Group: "github"},
	{Name: "GITHUB_ASSIGNEE", Default: "@me", Group: "github"},
	{Name: "GITHUB_STATE", Default: "open", Group: "github", Hint: "open | closed | all"},
	{Name: "GITHUB_API_URL", Default: "https://api.github.com", Group: "github", Hint: "override for GitHub Enterprise"},

	{Name: "GITLAB_TOKEN", Sensitive: true, Group: "gitlab"},
	{Name: "GITLAB_API_URL", Default: "https://gitlab.com/api/v4", Group: "gitlab"},

	{Name: "BITBUCKET_TOKEN", Sensitive: true, Group: "bitbucket"},
	{Name: "BITBUCKET_USERNAME", Group: "bitbucket"},
	{Name: "BITBUCKET_APP_PASSWORD", Sensitive: true, Group: "bitbucket"},
	{Name: "BITBUCKET_API_URL", Default: "https://api.bitbucket.org/2.0", Group: "bitbucket"},

	{Name: "TELEGRAM_BOT_TOKEN", Sensitive: true, Group: "telegram"},
	{Name: "GOON_TELEGRAM_SECRET", Sensitive: true, Group: "telegram", Hint: "passphrase a chat sends via /auth to authorize itself"},
	{Name: "TELEGRAM_CHAT_ID", Group: "telegram"},
	{Name: "TELEGRAM_API_BASE_URL", Default: "https://api.telegram.org", Group: "telegram"},
	{Name: "GOON_AUTO_REVIEW", Group: "telegram", Hint: "auto-draft PR reviews for PRs awaiting you (Telegram). 1/true"},
	{Name: "GOON_AUTO_NOTIFY", Group: "telegram", Hint: "forward new review-request / mention notifications. 1/true"},
	{Name: "GOON_AUTO_INTERVAL", Group: "telegram", Hint: "how often the auto review/notify loop runs (e.g. 10m)"},
	{Name: "GOON_REVIEW_REPOS", Group: "telegram", Hint: "comma-separated repos to scope proactive PR review to"},

	{Name: "JIRA_JQL", Group: "jira", Hint: `JQL filter, e.g. project="EB" AND assignee=currentUser() — empty uses a sensible default`},

	{Name: "GOON_OBSIDIAN_VAULT", Group: "obsidian", Hint: "absolute path to your Obsidian vault (enables obsidian_* chat tools)"},
	{Name: "GOON_OBSIDIAN_REPO", Group: "obsidian", Hint: "optional git repo to sync the vault"},

	// System paths — most users never change these; goon defaults to ./storage.
	{Name: "GOON_STORAGE_DIR", Default: "./storage", Group: "system", Hint: "root for all goon state (logs, memory, pid)"},
	{Name: "GOON_MEMORY_PATH", Group: "system", Hint: "override path for memory.json"},
	{Name: "GOON_MEMORY_DIR", Group: "system", Hint: "override path for the memory/ notes folder"},
	{Name: "GOON_LOG_FILE", Group: "system", Hint: "override path for goon.log"},
	{Name: "GOON_PID_FILE", Group: "system", Hint: "override path for goon.pid"},
	{Name: "GOON_WORKFLOW_FILE", Group: "system", Hint: "override path for workflow.json"},
	{Name: "GOON_UPSTREAM", Group: "system", Hint: "git remote used by `goon update`"},
}

// handleAPIConfig serves both reads (GET) and writes (POST).
//
// GET  /api/config             → JSON map of all known keys (secrets masked unless ?reveal=1)
// POST /api/config  KEY=VAL ...→ form-encoded; writes to ~/.config/goon/.env, sets os.Setenv,
//
//	triggers daemon Reconfigure, and returns a fragment.
// ── Google OAuth handlers ──────────────────────────────────────────────────

// handleGoogleOAuthStart redirects the browser to Google's OAuth consent
// screen. CLIENT_ID + CLIENT_SECRET must already be set in config.
func (s *Server) handleGoogleOAuthStart(w http.ResponseWriter, r *http.Request) {
	cfg := google.ConfigFromEnv()
	if !cfg.HasClient() {
		http.Error(w, "Set GOOGLE_OAUTH_CLIENT_ID and GOOGLE_OAUTH_CLIENT_SECRET in Setup first", http.StatusBadRequest)
		return
	}
	redirectURI := "http://" + r.Host + "/oauth/google/callback"
	authURL := google.AuthCodeURL(cfg.ClientID, redirectURI, google.PersonalScopes)
	http.Redirect(w, r, authURL, http.StatusFound)
}

// handleGoogleOAuthCallback receives the authorization code from Google,
// exchanges it for a refresh token, and saves it to config.json.
func (s *Server) handleGoogleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if errParam := r.URL.Query().Get("error"); errParam != "" {
		// User denied access or something went wrong on Google's side.
		http.Error(w, "Google OAuth error: "+errParam+"\n\nGo back to Setup and try again.", http.StatusBadRequest)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "Missing authorization code — please try again from Setup.", http.StatusBadRequest)
		return
	}
	cfg := google.ConfigFromEnv()
	if !cfg.HasClient() {
		http.Error(w, "OAuth credentials missing — save CLIENT_ID and CLIENT_SECRET in Setup first.", http.StatusBadRequest)
		return
	}
	redirectURI := "http://" + r.Host + "/oauth/google/callback"
	hc := &http.Client{Timeout: 15 * time.Second}
	refresh, _, err := google.ExchangeCode(r.Context(), hc, cfg, code, redirectURI)
	if err != nil {
		http.Error(w, "Token exchange failed: "+err.Error()+"\n\nCheck that the redirect URI is registered in Google Cloud Console.", http.StatusBadGateway)
		return
	}
	if err := setConfigKey("GOOGLE_OAUTH_REFRESH_TOKEN", refresh); err != nil {
		http.Error(w, "Failed to save token: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Apply immediately so the running process sees it without restart.
	_ = os.Setenv("GOOGLE_OAUTH_REFRESH_TOKEN", refresh)
	// Redirect back to the main app — Setup will now show "● connected".
	http.Redirect(w, r, "/", http.StatusFound)
}

// handleGoogleDisconnect clears the stored refresh token.
func (s *Server) handleGoogleDisconnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	_ = unsetConfigKey("GOOGLE_OAUTH_REFRESH_TOKEN")
	_ = os.Unsetenv("GOOGLE_OAUTH_REFRESH_TOKEN")
	http.Redirect(w, r, "/", http.StatusFound)
}

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
			v := strings.TrimSpace(vals[0])
			// Sensitive fields render as password inputs with an empty value
			// (the mask is in the placeholder). Skip empty submissions so the
			// probe falls back to os.Getenv and reads the real stored secret.
			if v != "" {
				override[k.Name] = v
			}
		}
	}
	all := checkup.RunWithEnvOverride(r.Context(), override)

	// ?only=<component> filters to just that probe (llm, board, git_host, etc.)
	// so per-section "Test" buttons don't show unrelated results.
	results := all
	if only := r.URL.Query().Get("only"); only != "" {
		results = nil
		for _, res := range all {
			if res.Component == only {
				results = append(results, res)
			}
		}
	}

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

// configHumanLabel returns a human-readable label for a known env var name.
// Falls back to the raw env var name when no mapping is defined.
func configHumanLabel(name string) string {
	labels := map[string]string{
		"GOON_LLM_PROVIDER":    "LLM Provider",
		"GOON_LLM_MODEL":       "LLM Model",
		"OPENAI_API_KEY":       "OpenAI API Key",
		"ANTHROPIC_API_KEY":    "Anthropic API Key",
		"GEMINI_API_KEY":       "Gemini API Key",
		"GOON_GIT_HOST":        "Git Host",
		"GOON_TELEGRAM_SECRET": "Telegram Secret",
		"GOON_WORKSPACE_DIR":   "Workspace Directory",
		"GOON_TICKET_STATUSES": "Ticket Statuses to Pull",
		"GOON_BOARD":           "Board Type",
		"GOON_POLL_SECONDS":    "Poll Interval (seconds)",
		"GITHUB_STATE":         "GitHub Issue State",
		"GITHUB_REPOS":         "GitHub Repos",
		"GITHUB_LABEL":         "GitHub Label Filter",
		"GITHUB_ASSIGNEE":      "GitHub Assignee Filter",
		"GOON_LLM_CHAT":        "Chat model",
		"GOON_LLM_PLAN":        "Plan / triage model",
		"GOON_LLM_CODE":        "Code (execute) model",
		"GOON_LLM_REVIEW":      "Review model",
		"ATLASSIAN_BASE_URL":   "Atlassian URL",
		"ATLASSIAN_EMAIL":      "Atlassian Email",
		"ATLASSIAN_API_TOKEN":  "Atlassian API Token",
	}
	if l, ok := labels[name]; ok {
		return l
	}
	return name
}

// fragConfig renders the editable config form. Sensitive fields display the
// masked value as the placeholder so the user can see "something is set"
// without the secret being in HTML. All output is Tailwind-classed for
// the redesigned dashboard.
func (s *Server) fragConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// Current values for pre-selecting pickers.
	llmProvider := envEcho("GOON_LLM_PROVIDER")
	if llmProvider == "" {
		llmProvider = "openai"
	}
	board := envEcho("GOON_BOARD")
	gitHost := envEcho("GOON_GIT_HOST")

	// rendered records every key surfaced by a curated section so the
	// "advanced" block can show ALL remaining keys — guaranteeing every
	// config value is editable in the UI and new keys never go missing.
	rendered := map[string]bool{}

	// Helper: render a labelled input row.
	field := func(k configKey, rowClass string) string {
		rendered[k.Name] = true
		val := envEcho(k.Name)
		disp, placeholder := "", ""
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
		hl := configHumanLabel(k.Name)
		if hl == k.Name {
			hl = ""
		}
		labelHTML := ""
		if hl != "" {
			labelHTML = fmt.Sprintf(`<label for="cfg-%s" class="block text-xs font-medium text-gray-700 dark:text-gray-300 mb-1">%s<span class="font-mono text-[10px] text-muted ml-1">%s</span></label>`,
				html.EscapeString(k.Name), html.EscapeString(hl), html.EscapeString(k.Name))
		} else {
			labelHTML = fmt.Sprintf(`<label for="cfg-%s" class="block font-mono text-xs text-muted mb-1">%s</label>`,
				html.EscapeString(k.Name), html.EscapeString(k.Name))
		}
		hintHTML := ""
		if k.Hint != "" && !k.Sensitive && k.Default == "" {
			hintHTML = fmt.Sprintf(`<p class="mt-1 text-[11px] text-muted">%s</p>`, html.EscapeString(k.Hint))
		}
		cls := "w-full font-mono text-sm rounded-md border border-surface-border bg-surface px-3 py-1.5 focus:border-accent focus:ring-1 focus:ring-accent/30 focus:outline-none"
		inputHTML := fmt.Sprintf(`<input id="cfg-%s" type="%s" name="%s" value="%s" placeholder="%s" autocomplete="off" class="%s">`,
			html.EscapeString(k.Name), tp, html.EscapeString(k.Name),
			html.EscapeString(disp), html.EscapeString(placeholder), cls)
		rc := rowClass
		if rc == "" {
			rc = "space-y-1"
		}
		return fmt.Sprintf(`<div class="%s">%s%s%s</div>`, rc, labelHTML, inputHTML, hintHTML)
	}

	// Helper: lookup a key by name.
	keyByName := func(name string) configKey {
		for _, k := range webConfigKeys {
			if k.Name == name {
				return k
			}
		}
		return configKey{Name: name}
	}

	// ── JS for show/hide provider / board / git-host panels ──────────────────
	fmt.Fprint(w, `<script>
function cfgPickLLM(v){
  ['openai','anthropic','gemini','ollama'].forEach(function(p){
    var el=document.getElementById('llm-panel-'+p);
    if(el) el.style.display=(p===v?'':'none');
  });
  document.getElementById('cfg-GOON_LLM_PROVIDER').value=v;
  document.querySelectorAll('[data-llm-pick]').forEach(function(b){
    b.classList.toggle('ring-2',b.dataset.llmPick===v);
    b.classList.toggle('ring-accent',b.dataset.llmPick===v);
    b.classList.toggle('bg-accent/10',b.dataset.llmPick===v);
  });
}
function cfgPickBoard(v){
  ['jira','github'].forEach(function(b){
    var el=document.getElementById('board-panel-'+b);
    if(el) el.style.display=(b===v?'':'none');
  });
  document.getElementById('cfg-GOON_BOARD').value=v;
  document.querySelectorAll('[data-board-pick]').forEach(function(b){
    b.classList.toggle('ring-2',b.dataset.boardPick===v);
    b.classList.toggle('ring-accent',b.dataset.boardPick===v);
    b.classList.toggle('bg-accent/10',b.dataset.boardPick===v);
  });
}
function cfgPickHost(v){
  ['github','gitlab','bitbucket'].forEach(function(h){
    var el=document.getElementById('host-panel-'+h);
    if(el) el.style.display=(h===v?'':'none');
  });
  document.getElementById('cfg-GOON_GIT_HOST').value=v;
  document.querySelectorAll('[data-host-pick]').forEach(function(b){
    b.classList.toggle('ring-2',b.dataset.hostPick===v);
    b.classList.toggle('ring-accent',b.dataset.hostPick===v);
    b.classList.toggle('bg-accent/10',b.dataset.hostPick===v);
  });
}
function cfgToggleAdv(){
  var el=document.getElementById('cfg-advanced');
  var btn=document.getElementById('cfg-adv-btn');
  if(!el) return;
  var open=el.style.display!=='none';
  el.style.display=open?'none':'';
  btn.textContent=open?'show advanced ▾':'hide advanced ▴';
}
document.addEventListener('DOMContentLoaded',function(){
  cfgPickLLM('`+llmProvider+`');
  cfgPickBoard('`+board+`');
  cfgPickHost('`+gitHost+`');
});
</script>`)

	// Helper: inline "Test" button + result area for a section.
	testStrip := func(component, id string) string {
		return fmt.Sprintf(
			`<div class="flex items-center gap-2 mt-3 pt-3 border-t border-surface-border/60">`+
				`<button type="button" `+
				`hx-post="/api/config/verify?only=%s" `+
				`hx-include="closest form" `+
				`hx-target="#test-result-%s" `+
				`hx-swap="innerHTML" `+
				`hx-indicator="#test-spin-%s" `+
				`class="text-xs rounded-md border border-surface-border px-3 py-1.5 hover:border-accent hover:text-accent transition font-medium">`+
				`Test connection</button>`+
				`<span id="test-spin-%s" class="htmx-indicator text-[11px] text-muted">testing…</span>`+
				`</div>`+
				`<div id="test-result-%s" class="mt-2 text-xs"></div>`,
			component, id, id, id, id)
	}

	// ── Provider picker chips ────────────────────────────────────────────────
	pickBtn := func(label, subtitle, dataAttr, val, onclick, active string) string {
		ring := ""
		if val == active {
			ring = " ring-2 ring-accent bg-accent/10"
		}
		return fmt.Sprintf(
			`<button type="button" %s="%s" onclick="%s('%s')" `+
				`class="flex-1 min-w-[90px] rounded-lg border border-surface-border px-3 py-2 text-center text-xs font-medium hover:bg-accent/5 hover:border-accent/50 transition%s">%s`+
				`<div class="text-[10px] text-muted mt-0.5">%s</div></button>`,
			dataAttr, val, onclick, val, ring, label, subtitle)
	}

	sectionHead := func(step, title, desc string) string {
		return fmt.Sprintf(`<div class="flex items-start gap-3 mb-3">
			<span class="shrink-0 w-6 h-6 rounded-full bg-accent/15 text-accent text-[11px] font-bold flex items-center justify-center">%s</span>
			<div><p class="text-sm font-semibold text-ink">%s</p><p class="text-xs text-muted">%s</p></div></div>`, step, title, desc)
	}

	card := func(inner string) string {
		return `<div class="rounded-xl border border-surface-border bg-surface-raised p-4 space-y-3">` + inner + `</div>`
	}

	fmt.Fprint(w, `<form hx-post="/api/config" hx-target="#cfg-result" hx-swap="innerHTML" class="space-y-4">`)

	// Hidden inputs for picker values (actual form fields).
	fmt.Fprintf(w, `<input type="hidden" id="cfg-GOON_LLM_PROVIDER" name="GOON_LLM_PROVIDER" value="%s">`, html.EscapeString(llmProvider))
	fmt.Fprintf(w, `<input type="hidden" id="cfg-GOON_BOARD"        name="GOON_BOARD"        value="%s">`, html.EscapeString(board))
	fmt.Fprintf(w, `<input type="hidden" id="cfg-GOON_GIT_HOST"     name="GOON_GIT_HOST"     value="%s">`, html.EscapeString(gitHost))

	// ── Step 1: LLM ──────────────────────────────────────────────────────────
	step1 := sectionHead("1", "LLM Provider", "goon uses this to read tickets, write code and open PRs. Required.")
	step1 += `<div class="flex flex-wrap gap-2">`
	step1 += pickBtn("OpenAI", "GPT-4o", "data-llm-pick", "openai", "cfgPickLLM", llmProvider)
	step1 += pickBtn("Anthropic", "Claude", "data-llm-pick", "anthropic", "cfgPickLLM", llmProvider)
	step1 += pickBtn("Gemini", "Google AI", "data-llm-pick", "gemini", "cfgPickLLM", llmProvider)
	step1 += pickBtn("Ollama", "local model", "data-llm-pick", "ollama", "cfgPickLLM", llmProvider)
	step1 += `</div>`

	// Provider-specific fields.
	panelOpenAI := `<div id="llm-panel-openai">` +
		`<div class="grid grid-cols-1 sm:grid-cols-2 gap-3">` +
		field(keyByName("OPENAI_API_KEY"), "") +
		field(keyByName("OPENAI_MODEL"), "") +
		`</div>` +
		`<p class="text-[11px] text-muted mt-1">Get a key at <a href="https://platform.openai.com/api-keys" target="_blank" class="text-accent hover:underline">platform.openai.com/api-keys</a></p>` +
		`</div>`

	panelAnthropic := `<div id="llm-panel-anthropic" style="display:none">` +
		`<div class="grid grid-cols-1 sm:grid-cols-2 gap-3">` +
		field(keyByName("ANTHROPIC_API_KEY"), "") +
		field(keyByName("ANTHROPIC_MODEL"), "") +
		`</div>` +
		`<p class="text-[11px] text-muted mt-1">Get a key at <a href="https://console.anthropic.com/settings/keys" target="_blank" class="text-accent hover:underline">console.anthropic.com</a></p>` +
		`</div>`

	panelGemini := `<div id="llm-panel-gemini" style="display:none">` +
		`<div class="grid grid-cols-1 sm:grid-cols-2 gap-3">` +
		field(keyByName("GEMINI_API_KEY"), "") +
		field(keyByName("GEMINI_MODEL"), "") +
		`</div>` +
		`<p class="text-[11px] text-muted mt-1">Get a key at <a href="https://aistudio.google.com/apikey" target="_blank" class="text-accent hover:underline">aistudio.google.com/apikey</a></p>` +
		`</div>`

	panelOllama := `<div id="llm-panel-ollama" style="display:none">` +
		`<div class="grid grid-cols-1 sm:grid-cols-2 gap-3">` +
		field(keyByName("OLLAMA_BASE_URL"), "") +
		field(keyByName("OLLAMA_MODEL"), "") +
		`</div>` +
		`<p class="text-[11px] text-muted mt-1">Install from <a href="https://ollama.com" target="_blank" class="text-accent hover:underline">ollama.com</a>, run <code class="font-mono">ollama serve</code></p>` +
		`</div>`

	fmt.Fprint(w, card(step1+panelOpenAI+panelAnthropic+panelGemini+panelOllama+testStrip("llm", "llm")))

	// ── Step 2: Board ─────────────────────────────────────────────────────────
	step2 := sectionHead("2", "Ticket board", "Where should goon pick up tasks? Skip if you just want the one-shot agent.")
	step2 += `<div class="flex flex-wrap gap-2">`
	step2 += pickBtn("Jira", "Atlassian Cloud", "data-board-pick", "jira", "cfgPickBoard", board)
	step2 += pickBtn("GitHub Issues", "github.com", "data-board-pick", "github", "cfgPickBoard", board)
	step2 += `</div>`

	panelJira := `<div id="board-panel-jira"` + func() string {
		if board != "jira" {
			return ` style="display:none"`
		}
		return ""
	}() + `>` +
		`<p class="text-[11px] text-muted mb-2">Fill <strong>Atlassian (shared)</strong> creds below — Jira and Confluence both use them.</p>` +
		`<div class="grid grid-cols-1 sm:grid-cols-2 gap-3">` +
		field(keyByName("ATLASSIAN_BASE_URL"), "") +
		field(keyByName("ATLASSIAN_EMAIL"), "") +
		field(keyByName("ATLASSIAN_API_TOKEN"), "") +
		field(keyByName("JIRA_JQL"), "") +
		`</div></div>`

	panelGitHubBoard := `<div id="board-panel-github"` + func() string {
		if board != "github" {
			return ` style="display:none"`
		}
		return ""
	}() + `>` +
		`<div class="grid grid-cols-1 sm:grid-cols-2 gap-3">` +
		field(keyByName("GITHUB_TOKEN"), "") +
		field(keyByName("GITHUB_REPOS"), "") +
		field(keyByName("GITHUB_ASSIGNEE"), "") +
		field(keyByName("GITHUB_LABEL"), "") +
		`</div></div>`

	// Ticket statuses — applies to all board types, shown below the per-board creds.
	ticketStatusField := `<div class="pt-3 mt-1 border-t border-surface-border/60">` +
		`<p class="text-[11px] text-muted mb-2">Which ticket statuses should goon pick up?</p>` +
		`<div class="grid grid-cols-1 sm:grid-cols-2 gap-3">` +
		field(keyByName("GOON_TICKET_STATUSES"), "") +
		field(keyByName("GOON_POLL_SECONDS"), "") +
		`</div></div>`

	fmt.Fprint(w, card(step2+panelJira+panelGitHubBoard+ticketStatusField+testStrip("board", "board")))

	// ── Step 3: Git host ──────────────────────────────────────────────────────
	step3 := sectionHead("3", "Git host", "Where should goon open pull requests?")
	step3 += `<div class="flex flex-wrap gap-2">`
	step3 += pickBtn("GitHub", "github.com", "data-host-pick", "github", "cfgPickHost", gitHost)
	step3 += pickBtn("GitLab", "gitlab.com", "data-host-pick", "gitlab", "cfgPickHost", gitHost)
	step3 += pickBtn("Bitbucket", "bitbucket.org", "data-host-pick", "bitbucket", "cfgPickHost", gitHost)
	step3 += `</div>`

	panelGitHubHost := `<div id="host-panel-github"` + func() string {
		if gitHost != "github" {
			return ` style="display:none"`
		}
		return ""
	}() + `>` +
		`<p class="text-[11px] text-muted mb-2">Same token as board — already filled if you set it above.</p>` +
		`<div class="grid grid-cols-1 sm:grid-cols-2 gap-3">` +
		field(keyByName("GITHUB_TOKEN"), "") +
		field(keyByName("GITHUB_API_URL"), "") +
		`</div></div>`

	panelGitLab := `<div id="host-panel-gitlab"` + func() string {
		if gitHost != "gitlab" {
			return ` style="display:none"`
		}
		return ""
	}() + `>` +
		`<div class="grid grid-cols-1 sm:grid-cols-2 gap-3">` +
		field(keyByName("GITLAB_TOKEN"), "") +
		field(keyByName("GITLAB_API_URL"), "") +
		`</div></div>`

	panelBitbucket := `<div id="host-panel-bitbucket"` + func() string {
		if gitHost != "bitbucket" {
			return ` style="display:none"`
		}
		return ""
	}() + `>` +
		`<div class="grid grid-cols-1 sm:grid-cols-2 gap-3">` +
		field(keyByName("BITBUCKET_TOKEN"), "") +
		field(keyByName("BITBUCKET_USERNAME"), "") +
		field(keyByName("BITBUCKET_APP_PASSWORD"), "") +
		`</div></div>`

	fmt.Fprint(w, card(step3+panelGitHubHost+panelGitLab+panelBitbucket+testStrip("git_host", "host")))

	// ── Step 4: Learning ──────────────────────────────────────────────────────
	step4 := sectionHead("4", "Self-learning", "goon learns from every task and reflects while idle, writing findings to LEARNED.md and asking you questions it can't answer.")
	step4 += `<div class="grid grid-cols-1 sm:grid-cols-2 gap-3">` +
		field(keyByName("GOON_AUTO_LEARN"), "") +
		field(keyByName("GOON_LEARN_INTERVAL_HOURS"), "") +
		`</div>` +
		`<p class="text-[11px] text-muted mt-2">` +
		`<strong>When it runs:</strong> (1) after every one-shot task — appends to HISTORY.md and distils findings; ` +
		`(2) while the daemon is idle — reviews recent changes and writes to LEARNED.md. ` +
		`Answer pending questions in the <strong>Questions</strong> tab.` +
		`</p>`
	fmt.Fprint(w, card(step4))

	// ── Step 5: Google (optional) ─────────────────────────────────────────────
	gConnected := envEcho("GOOGLE_OAUTH_REFRESH_TOKEN") != ""
	gHasClient := envEcho("GOOGLE_OAUTH_CLIENT_ID") != "" && envEcho("GOOGLE_OAUTH_CLIENT_SECRET") != ""
	var gPill string
	switch {
	case gConnected:
		gPill = `<span class="inline-flex items-center gap-1 rounded-full bg-emerald-500/15 text-emerald-700 dark:text-emerald-400 px-2.5 py-0.5 text-[11px] font-semibold">● connected</span>`
	case gHasClient:
		gPill = `<span class="inline-flex items-center gap-1 rounded-full bg-amber-500/15 text-amber-700 dark:text-amber-400 px-2.5 py-0.5 text-[11px] font-semibold">○ credentials saved — click Connect below</span>`
	default:
		gPill = `<span class="inline-flex items-center gap-1 rounded-full bg-surface-sunken text-muted px-2.5 py-0.5 text-[11px] font-semibold">○ not connected</span>`
	}
	// Derive the OAuth redirect URI from the current request host so we can
	// show it in the instructions — users must register this exact URI in Google Cloud.
	gRedirectURI := "http://" + r.Host + "/oauth/google/callback"
	step5 := `<div class="flex items-start gap-3 mb-3">
		<span class="shrink-0 w-6 h-6 rounded-full bg-accent/15 text-accent text-[11px] font-bold flex items-center justify-center">5</span>
		<div class="flex-1"><div class="flex items-center gap-2 flex-wrap"><p class="text-sm font-semibold text-ink">Google (Gmail + Calendar)</p>` + gPill + `</div>
		<p class="text-xs text-muted">Read-only access to your personal Gmail and Google Calendar. Optional — connect once, use forever.</p></div></div>`
	// Credential fields (CLIENT_ID + SECRET).
	step5 += `<div class="grid grid-cols-1 sm:grid-cols-2 gap-3">` +
		field(keyByName("GOOGLE_OAUTH_CLIENT_ID"), "") +
		field(keyByName("GOOGLE_OAUTH_CLIENT_SECRET"), "") +
		`</div>`
	// Connect / Disconnect button.
	if gConnected {
		step5 += `<div class="flex items-center gap-3 pt-1">` +
			`<span class="text-[11px] text-emerald-700 dark:text-emerald-400 font-semibold">✓ Google account connected</span>` +
			`<form method="post" action="/api/google/disconnect" class="inline">` +
			`<button type="submit" class="text-[11px] text-rose-600 dark:text-rose-400 underline underline-offset-2 bg-transparent border-0 cursor-pointer p-0">disconnect</button>` +
			`</form></div>`
	} else if gHasClient {
		step5 += `<div class="pt-1">` +
			`<a href="/oauth/google/start" class="inline-flex items-center gap-2 rounded-lg bg-accent px-4 py-2 text-[13px] font-semibold text-white shadow hover:bg-accent-strong transition-colors">` +
			`<svg class="w-4 h-4" viewBox="0 0 24 24" fill="currentColor"><path d="M22.56 12.25c0-.78-.07-1.53-.2-2.25H12v4.26h5.92c-.26 1.37-1.04 2.53-2.21 3.31v2.77h3.57c2.08-1.92 3.28-4.74 3.28-8.09z"/><path d="M12 23c2.97 0 5.46-.98 7.28-2.66l-3.57-2.77c-.98.66-2.23 1.06-3.71 1.06-2.86 0-5.29-1.93-6.16-4.53H2.18v2.84C3.99 20.53 7.7 23 12 23z"/><path d="M5.84 14.09c-.22-.66-.35-1.36-.35-2.09s.13-1.43.35-2.09V7.07H2.18C1.43 8.55 1 10.22 1 12s.43 3.45 1.18 4.93l3.66-2.84z"/><path d="M12 5.38c1.62 0 3.06.56 4.21 1.64l3.15-3.15C17.45 2.09 14.97 1 12 1 7.7 1 3.99 3.47 2.18 7.07l3.66 2.84c.87-2.6 3.3-4.53 6.16-4.53z"/></svg>` +
			`Connect Google Account</a></div>`
	} else {
		step5 += `<p class="text-[11px] text-muted pt-1">Save your Client ID + Secret above first, then the Connect button will appear.</p>`
	}
	// Setup instructions.
	step5 += `<details class="mt-3"><summary class="text-[11px] text-muted cursor-pointer select-none hover:text-ink">How to get credentials (one-time, free)</summary>` +
		`<div class="rounded-lg border border-surface-border bg-surface-sunken/50 p-3 text-[11px] text-muted space-y-1.5 mt-2">` +
		`<p class="font-semibold text-ink">Google Cloud setup (5 min, free)</p>` +
		`<p>1. Go to <strong>console.cloud.google.com</strong> → create a project.</p>` +
		`<p>2. APIs &amp; Services → <strong>Enable</strong> Gmail API + Google Calendar API.</p>` +
		`<p>3. OAuth consent screen → External → Testing → add your Gmail as test user.</p>` +
		`<p>4. Credentials → <strong>Create OAuth 2.0 Client ID</strong> → type: <strong>Web application</strong>.</p>` +
		`<p>5. Add this Authorized redirect URI: <code class="font-mono text-ink bg-surface px-1 py-0.5 rounded select-all">` + html.EscapeString(gRedirectURI) + `</code></p>` +
		`<p>6. Copy Client ID + Secret here → Save &amp; Apply → click <strong>Connect Google Account</strong>.</p>` +
		`<p class="text-[10px] opacity-70">Uses your personal @gmail.com account. Free forever in Testing mode. Read-only: Gmail + Calendar.</p>` +
		`</div></details>`
	gExamples := []string{
		"what meetings do I have today?",
		"check my email from finance last week",
		"any unread emails from my boss?",
	}
	step5 += `<div class="flex flex-wrap gap-1.5 pt-2">`
	for _, ex := range gExamples {
		step5 += `<span class="rounded-full border border-surface-border bg-surface px-2.5 py-1 text-[11px] text-muted">&ldquo;` + html.EscapeString(ex) + `&rdquo;</span>`
	}
	step5 += `</div>`
	fmt.Fprint(w, card(step5))

	// ── Step 6: Models per role (optional) ────────────────────────────────────

	// Compute the effective default model string from GOON_LLM_PROVIDER + its
	// model key, so the UI can show "currently using: openai:gpt-4o-mini".
	providerDefaultModel := func(p string) string {
		switch p {
		case "openai":
			m := envEcho("OPENAI_MODEL")
			if m == "" {
				m = "gpt-4o-mini"
			}
			return "openai:" + m
		case "anthropic":
			m := envEcho("ANTHROPIC_MODEL")
			if m == "" {
				m = "claude-sonnet-4-5"
			}
			return "anthropic:" + m
		case "gemini":
			m := envEcho("GEMINI_MODEL")
			if m == "" {
				m = "gemini-2.5-flash"
			}
			return "gemini:" + m
		case "ollama":
			m := envEcho("OLLAMA_MODEL")
			if m == "" {
				m = "llama3"
			}
			return "ollama:" + m
		}
		if p != "" {
			return p
		}
		return "openai:gpt-4o-mini"
	}
	defaultModel := providerDefaultModel(llmProvider)

	// Build datalist suggestions from whichever providers have keys configured.
	type modelOpt struct{ val, label string }
	var modelOpts []modelOpt
	seen6 := map[string]bool{}
	addOpt := func(val, label string) {
		if !seen6[val] {
			seen6[val] = true
			modelOpts = append(modelOpts, modelOpt{val, label})
		}
	}
	if envEcho("OPENAI_API_KEY") != "" || llmProvider == "openai" {
		om := envEcho("OPENAI_MODEL")
		if om == "" {
			om = "gpt-4o-mini"
		}
		addOpt("openai:"+om, "OpenAI — "+om+" (your model)")
		addOpt("openai:gpt-4o-mini", "OpenAI — gpt-4o-mini")
		addOpt("openai:gpt-4o", "OpenAI — gpt-4o")
		addOpt("openai:o3-mini", "OpenAI — o3-mini")
	}
	if envEcho("ANTHROPIC_API_KEY") != "" || llmProvider == "anthropic" {
		am := envEcho("ANTHROPIC_MODEL")
		if am == "" {
			am = "claude-sonnet-4-5"
		}
		addOpt("anthropic:"+am, "Anthropic — "+am+" (your model)")
		addOpt("anthropic:claude-sonnet-4-5", "Anthropic — claude-sonnet-4-5")
		addOpt("anthropic:claude-haiku-4-5-20251001", "Anthropic — claude-haiku (fast/cheap)")
	}
	if envEcho("GEMINI_API_KEY") != "" || envEcho("GOOGLE_API_KEY") != "" || llmProvider == "gemini" {
		gm := envEcho("GEMINI_MODEL")
		if gm == "" {
			gm = "gemini-2.5-flash"
		}
		addOpt("gemini:"+gm, "Gemini — "+gm+" (your model)")
		addOpt("gemini:gemini-2.5-flash", "Gemini — gemini-2.5-flash")
		addOpt("gemini:gemini-2.5-pro", "Gemini — gemini-2.5-pro")
	}
	if llmProvider == "ollama" || envEcho("OLLAMA_BASE_URL") != "" {
		olm := envEcho("OLLAMA_MODEL")
		if olm == "" {
			olm = "llama3"
		}
		addOpt("ollama:"+olm, "Ollama — "+olm+" (your model)")
	}
	dlID6 := "cfg-model-datalist"
	dlHTML6 := `<datalist id="` + dlID6 + `">`
	for _, o := range modelOpts {
		dlHTML6 += fmt.Sprintf(`<option value="%s">%s</option>`, html.EscapeString(o.val), html.EscapeString(o.label))
	}
	dlHTML6 += `</datalist>`

	// roleField renders a role input with a live "currently using" badge and
	// the datalist for dropdown suggestions.
	roleField := func(roleKey, roleLabel, roleDesc string) string {
		rendered[roleKey] = true
		val := envEcho(roleKey)
		eff := defaultModel
		if val != "" {
			eff = val
		}
		badgeText := "default: " + eff
		badgeCls := "text-[10px] text-muted"
		if val != "" {
			badgeCls = "text-[10px] text-accent font-semibold"
			badgeText = eff
		}
		badge := fmt.Sprintf(`<span class="%s font-mono">%s</span>`, badgeCls, html.EscapeString(badgeText))
		inputCls := "w-full font-mono text-sm rounded-md border border-surface-border bg-surface px-3 py-1.5 focus:border-accent focus:ring-1 focus:ring-accent/30 focus:outline-none text-ink"
		return fmt.Sprintf(
			`<div class="space-y-1">`+
				`<div class="flex items-baseline justify-between gap-2">`+
				`<label for="cfg-%s" class="text-xs font-medium text-ink shrink-0">%s`+
				`<span class="font-normal text-muted ml-1">%s</span></label>`+
				badge+
				`</div>`+
				`<input id="cfg-%s" type="text" name="%s" value="%s"`+
				` placeholder="empty = %s" autocomplete="off" list="%s" class="%s">`+
				`</div>`,
			html.EscapeString(roleKey),
			html.EscapeString(roleLabel),
			html.EscapeString(roleDesc),
			html.EscapeString(roleKey),
			html.EscapeString(roleKey),
			html.EscapeString(val),
			html.EscapeString(defaultModel),
			dlID6,
			inputCls)
	}

	step6 := sectionHead("6", "Models per role",
		"Route each job to a different model. Leave empty to use the provider from Step 1.")
	step6 += dlHTML6
	step6 += `<div class="grid grid-cols-1 sm:grid-cols-2 gap-3">` +
		roleField("GOON_LLM_CODE", "Code", "execute agent — writes &amp; edits files") +
		roleField("GOON_LLM_CHAT", "Chat", "web &amp; Telegram chat") +
		roleField("GOON_LLM_PLAN", "Plan", "ticket triage &amp; planning") +
		roleField("GOON_LLM_REVIEW", "Review", "verify + PR-review drafts") +
		`</div>` +
		`<p class="text-[11px] text-muted mt-1">Format: <span class="font-mono text-ink">provider:model</span> · <span class="font-mono text-ink">provider</span> · or bare <span class="font-mono text-ink">model</span>. Each provider uses its own API key from Step 1.</p>`
	fmt.Fprint(w, card(step6))

	// The three picker values are saved via hidden inputs, not field(), so
	// mark them rendered too — otherwise the advanced block would duplicate
	// them as raw text boxes.
	rendered["GOON_LLM_PROVIDER"] = true
	rendered["GOON_BOARD"] = true
	rendered["GOON_GIT_HOST"] = true

	// ── Advanced (collapsed) — EVERY config key not already shown above ───────
	// Built from webConfigKeys minus what the curated sections rendered, so
	// no config value is ever un-editable in the UI (and a newly-added key
	// shows up here automatically). Grouped by area, with friendly headers.
	groupTitles := map[string]string{
		"agent": "Daemon & agent", "openai": "OpenAI", "anthropic": "Anthropic",
		"gemini": "Gemini", "ollama": "Ollama", "atlassian": "Atlassian (shared)",
		"jira": "Jira", "confluence": "Confluence", "github": "GitHub",
		"gitlab": "GitLab", "bitbucket": "Bitbucket", "telegram": "Telegram",
		"obsidian": "Obsidian", "system": "System paths",
	}
	groupOrder := []string{"agent", "openai", "anthropic", "gemini", "ollama",
		"atlassian", "jira", "confluence", "github", "gitlab", "bitbucket",
		"telegram", "obsidian", "system"}
	byGroup := map[string][]configKey{}
	for _, k := range webConfigKeys {
		if rendered[k.Name] {
			continue
		}
		byGroup[k.Group] = append(byGroup[k.Group], k)
	}
	advHTML := `<div id="cfg-advanced" style="display:none" class="space-y-4 pt-3 border-t border-surface-border">`
	seen := map[string]bool{}
	emit := func(g string) {
		keys := byGroup[g]
		if len(keys) == 0 || seen[g] {
			return
		}
		seen[g] = true
		title := groupTitles[g]
		if title == "" {
			title = g
		}
		advHTML += `<div><p class="text-[11px] font-semibold uppercase tracking-wider text-muted mb-2">` + html.EscapeString(title) + `</p>` +
			`<div class="grid grid-cols-1 sm:grid-cols-2 gap-3">`
		for _, k := range keys {
			advHTML += field(k, "")
		}
		advHTML += `</div></div>`
	}
	for _, g := range groupOrder {
		emit(g)
	}
	// Any group not in the known order (future-proofing) renders last.
	for g := range byGroup {
		emit(g)
	}
	advHTML += `</div>`

	advSection := `<div class="rounded-xl border border-surface-border bg-surface-raised p-4">` +
		`<button type="button" id="cfg-adv-btn" onclick="cfgToggleAdv()" ` +
		`class="text-xs text-muted hover:text-ink transition font-medium">show all settings ▾</button>` +
		advHTML + `</div>`
	fmt.Fprint(w, advSection)

	// ── Actions ───────────────────────────────────────────────────────────────
	fmt.Fprint(w, `<div class="flex flex-wrap items-center gap-3 pt-1">`)
	fmt.Fprint(w, `<button type="submit" hx-indicator="#cfg-spinner" `+
		`class="rounded-md bg-accent text-surface px-5 py-2 text-sm font-semibold hover:brightness-110 transition">save &amp; apply</button>`)
	fmt.Fprint(w, `<button type="button" `+
		`hx-post="/api/config/verify" hx-include="closest form" `+
		`hx-target="#cfg-result" hx-swap="innerHTML" hx-indicator="#cfg-spinner" `+
		`class="rounded-md border border-surface-border text-sm px-4 py-2 hover:border-accent hover:text-accent transition">verify connection</button>`)
	fmt.Fprint(w, `<span id="cfg-spinner" class="htmx-indicator text-xs text-muted">checking…</span>`)
	fmt.Fprint(w, `</div>`)
	fmt.Fprint(w, `<div id="cfg-result" class="mt-1"></div>`)
	fmt.Fprint(w, `</form>`)
}

// fragWorkflowConfig renders the pipeline header band + a read-only stage
// flowchart, plus a full-screen guided STEP-LIST editor opened by the
// "edit pipeline" button.
//
// The editor was rebuilt from a 2D node-graph canvas into a vertical,
// numbered step list (think GitHub Actions / Zapier): each step is a card you
// can expand to edit, reorder with a drag handle or up/down arrows, duplicate,
// or delete. Order is simply the list order — there are no canvas coordinates,
// pan/zoom, or position-based ordering, which eliminated an entire class of
// drag/select bugs. Branching/routing lives under an "Advanced" section per
// step so beginners never see it. The backend save path, data model, starter
// templates, and validation are unchanged.
func (s *Server) fragWorkflowConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	cfg := s.opts.Workflow
	path := s.opts.WorkflowPath

	// ?file=<slug> opens a scheduled automation in the same island editor.
	// The slug is sanitised + confined to the automations dir (no traversal),
	// and the editor saves back to that file (not the active pipeline).
	autoMode := false
	if slug := strings.TrimSpace(r.URL.Query().Get("file")); slug != "" {
		ap := filepath.Join(workflow.AutomationsDir(), workflow.AutomationSlug(slug)+".json")
		if data, err := os.ReadFile(ap); err == nil {
			var ac workflow.WorkflowConfig
			if json.Unmarshal(data, &ac) == nil {
				cfg = &ac
				path = ap
				autoMode = true
			}
		}
		if !autoMode {
			fmt.Fprint(w, `<div class="rounded-xl border border-rose-500/40 bg-rose-500/10 px-4 py-3 text-sm text-rose-700 dark:text-rose-300">Automation not found. <button type="button" class="underline" hx-get="/fragments/tab-workflows" hx-target="closest [data-page]" hx-swap="innerHTML">← back to Workflows</button></div>`)
			return
		}
	}

	name := "default"
	desc := "Built-in pipeline (no workflow.json found)."
	autoApprove := false
	branchPrefix := ""
	if cfg != nil {
		if cfg.Name != "" {
			name = cfg.Name
		}
		if cfg.Description != "" {
			desc = cfg.Description
		}
		autoApprove = cfg.AutoApprove
		branchPrefix = cfg.BranchPrefix
	}
	srcLabel := path
	if srcLabel == "" {
		srcLabel = "(no file — using built-in default)"
	}
	approveBadge := `<span class="inline-flex items-center gap-1 rounded-full bg-amber-500/15 text-amber-700 dark:text-amber-400 border border-amber-500/40 px-2 py-0.5 text-[11px] font-medium">⏸ gated</span>`
	if autoApprove {
		approveBadge = `<span class="inline-flex items-center gap-1 rounded-full bg-emerald-500/15 text-emerald-700 dark:text-emerald-400 border border-emerald-500/40 px-2 py-0.5 text-[11px] font-medium">⚡ auto-approve</span>`
	}
	branchLabel := ""
	if branchPrefix != "" {
		branchLabel = fmt.Sprintf(`<span class="text-[11px] font-mono text-muted/80">%s</span>`, html.EscapeString(branchPrefix))
	}

	// When editing an automation, a back bar returns to the Workflows tab and
	// names what's being edited + its schedule.
	if autoMode {
		fmt.Fprintf(w, `<div class="mb-2 flex items-center gap-2 text-xs">
			<button type="button" class="inline-flex items-center gap-1 rounded-md border border-surface-border text-muted px-2 py-1 hover:border-accent hover:text-accent transition" hx-get="/fragments/tab-workflows" hx-target="closest [data-page]" hx-swap="innerHTML">
				<svg class="h-3.5 w-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M19 12H5M12 19l-7-7 7-7"/></svg>back to Workflows</button>
			<span class="text-muted">editing automation <b class="text-ink">%s</b> · <span class="font-mono">%s</span></span>
		</div>`, html.EscapeString(cfg.Name), html.EscapeString(cfg.Trigger.ScheduleHint()))
	}

	// ── Header band ──────────────────────────────────────────────────────
	fmt.Fprintf(w, `<div class="rounded-xl border border-accent/30 bg-surface-raised shadow-card">
	<div class="px-5 py-3.5 flex items-center gap-3 flex-wrap">
		<svg class="h-4 w-4 text-accent shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="3"/><path d="M19.07 4.93a10 10 0 0 1 0 14.14M4.93 4.93a10 10 0 0 0 0 14.14"/></svg>
		<div class="flex-1 min-w-0">
			<div class="flex items-center gap-2 flex-wrap">
				<span class="text-sm font-semibold text-ink">%s</span>
				<span class="text-muted text-xs">·</span>
				<span class="text-xs text-muted font-mono">%s</span>
				%s
				%s
			</div>
			<div class="text-xs text-muted mt-0.5 truncate">%s</div>
		</div>
		<button type="button" id="wf-editor-toggle" onclick="goonWfEditorToggle()"
			class="inline-flex items-center gap-1.5 rounded-md border border-surface-border text-muted px-3 py-1.5 text-xs font-medium hover:border-accent hover:text-accent transition">
			<svg class="h-3.5 w-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M11 4H4a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7"/><path d="M18.5 2.5a2.121 2.121 0 0 1 3 3L12 15l-4 1 1-4 9.5-9.5z"/></svg>
			<span id="wf-editor-toggle-label">edit pipeline</span>
		</button>
	</div>`,
		html.EscapeString(name),
		html.EscapeString(srcLabel),
		branchLabel,
		approveBadge,
		html.EscapeString(desc),
	)

	fmt.Fprint(w, `</div>`) // close outer card

	// ── Prepare JS data ───────────────────────────────────────────────────
	escape := func(s string) string {
		return strings.ReplaceAll(strings.ReplaceAll(s, "</", "<\\/"), "<!--", "<\\!--")
	}
	marshal := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			return "null"
		}
		return escape(string(b))
	}

	cfgJSON := "{}"
	if cfg != nil {
		cfgJSON = marshal(cfg)
	}
	defaultsJSON := marshal(workflow.DefaultConfig())
	seedJSON := marshal(workflow.BuiltinStageSeed())
	startersJSON := "[]"
	if ts, err := workflow.StarterTemplates(); err == nil {
		startersJSON = marshal(ts)
	}
	saveTarget := path
	if saveTarget == "" {
		saveTarget = workflow.DefaultConfigFilePath()
	}

	// ── Full-screen step-list editor overlay ───────────────────────────────
	fmt.Fprint(w, `
<!-- Workflow step-list editor overlay — fixed, full-screen, hidden until "edit pipeline" -->
<div id="wf-graph-overlay" class="fixed inset-0 z-[100] flex flex-col hidden bg-surface text-ink">

	<style>
	/* Graph-canvas editor styles. Pure CSS so re-renders are free. */
	#wg-canvas { position: absolute; inset: 0; overflow: hidden; touch-action: none; cursor: grab;
		background-color: #2E8FC9;
		background-image: linear-gradient(#5FB8E6 0%, #2E8FC9 52%, #1F6FA9 100%);
		background-size: 100% 100%; background-repeat: no-repeat; }
	/* flat-cel wave bands anchored to the bottom of the sea (decorative backdrop) */
	.wg-wave { position: absolute; left: -2%; width: 104%; height: 74px; pointer-events: none; }
	#wg-canvas.wg-panning { cursor: grabbing; }
	#wg-world { position: absolute; left: 0; top: 0; transform-origin: 0 0; will-change: transform; }
	#wg-world .wg-life { transition: box-shadow 150ms, border-color 150ms; box-shadow: 0 8px 16px -10px rgb(0 0 0 / 0.5); }
	/* Island nodes: the .wg-step box is transparent — the SVG ellipse + name-plate ARE the art. */
	#wg-world .wg-step { background: transparent; }
	#wg-world .wg-step .wg-isle { transition: stroke 120ms; }
	#wg-world .wg-step .wg-plate { transition: transform 120ms, border-color 120ms, box-shadow 120ms; }
	#wg-world .wg-step:hover .wg-plate { transform: translateY(-2px); }
	#wg-world .wg-step.wg-sel .wg-isle { stroke: #6366F1; stroke-width: 5; }
	#wg-world .wg-step.wg-sel .wg-plate { border-color: #6366F1; box-shadow: 0 0 0 2px #6366F1, 0 3px 0 -1px rgba(20,70,110,0.5); }
	#wg-world .wg-dragging { z-index: 30; cursor: grabbing;
		box-shadow: 0 18px 40px -12px rgb(0 0 0 / 0.5); }
	#wg-edges { position: absolute; left: 0; top: 0; width: 8px; height: 8px; overflow: visible; pointer-events: none; }
	#wg-edges path { pointer-events: stroke; cursor: pointer; }
	#wg-edges path#wg-ghost { pointer-events: none; }
	.wg-edge { fill: none; stroke-linecap: round; stroke-dasharray: 2 8; animation: wgDash 1.4s linear infinite; }
	.wg-edge-case { fill: none; stroke: rgb(13 27 42 / 0.32); pointer-events: none; }
	.wg-edge-sel { filter: drop-shadow(0 0 4px rgb(var(--c-accent))); }
	/* fat invisible hit area so thin wires are easy to click + select */
	.wg-hit { fill: none; stroke: transparent; stroke-width: 18; cursor: pointer; }
	/* delete badge shown on the selected wire */
	#wg-edges .wg-del { pointer-events: auto; cursor: pointer; }
	#wg-edges .wg-del circle:hover { fill: rgb(244 63 94 / 0.18); }
	@keyframes wgDash { to { stroke-dashoffset: -10; } }
	.wg-port { position: absolute; width: 12px; height: 12px; border-radius: 9999px; z-index: 12;
		border: 2.5px solid #1E2433; background: #FBF2DF;
		cursor: crosshair; transition: transform 120ms; }
	.wg-port:hover { transform: scale(1.4); }
	.wg-pal-item { cursor: grab; transition: transform 120ms, border-color 120ms; }
	.wg-pal-item:hover { transform: translateY(-1px); }
	#wg-pal-ghost { position: fixed; z-index: 200; pointer-events: none; opacity: 0.9; }
	.wg-fade { animation: wgFade 180ms ease-out both; }
	@keyframes wgFade { from { opacity: 0; transform: translateY(-4px); } to { opacity: 1; transform: translateY(0); } }
	@media (prefers-reduced-motion: reduce) {
		.wg-edge { animation: none; }
		.wg-port, .wg-pal-item, .wg-pal-item:hover { transition: none; transform: none; }
		.wg-fade { animation: none; }
	}
	</style>

	<!-- Top bar -->
	<div class="flex items-center gap-3 px-4 py-2.5 border-b border-surface-border bg-surface-raised shrink-0">
		<div class="flex items-center gap-2 min-w-0">
			<svg class="h-4 w-4 text-accent shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="3"/><path d="M19.07 4.93a10 10 0 0 1 0 14.14M4.93 4.93a10 10 0 0 0 0 14.14"/></svg>
			<span class="text-sm font-semibold text-ink">Pipeline Editor</span>
		</div>
		<div class="ml-auto flex items-center gap-2">
			<span id="wg-save-msg" class="text-xs"></span>
			<button onclick="wgRevertBuiltin()" id="wg-revert-btn"
				class="hidden text-xs text-amber-700 dark:text-amber-400 hover:text-amber-300 px-3 py-1.5 rounded-md border border-amber-500/40 hover:bg-amber-500/10 transition">revert to built-in</button>
			<button onclick="wgUndo()" id="wg-undo-btn" disabled title="Undo (Ctrl+Z)"
				class="inline-flex items-center gap-1 text-xs text-muted hover:text-ink px-2.5 py-1.5 rounded-md hover:bg-surface-sunken transition disabled:opacity-30 disabled:cursor-not-allowed">
				<svg class="h-3.5 w-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="1 4 1 10 7 10"/><path d="M3.51 15a9 9 0 1 0 .49-3.5"/></svg>
				undo
			</button>
			<button onclick="wgClose()" class="text-xs text-muted hover:text-ink px-3 py-1.5 rounded-md hover:bg-surface-sunken transition">cancel</button>
			<button onclick="wgSave()" id="wg-save-btn" title="Save (Ctrl+S)" class="relative inline-flex items-center gap-1.5 rounded-md bg-accent text-black px-4 py-1.5 text-xs font-bold hover:brightness-110 transition">
				<svg class="h-3.5 w-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><path d="M19 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h11l5 5v11a2 2 0 0 1-2 2z"/><polyline points="17 21 17 13 7 13 7 21"/><polyline points="7 3 7 8 15 8"/></svg>
				Save Workflow
				<span id="wg-dirty-dot" class="hidden absolute -top-1 -right-1 h-2.5 w-2.5 rounded-full bg-amber-400 ring-2 ring-surface" title="Unsaved changes"></span>
			</button>
		</div>
	</div>

	<!-- Tabs -->
	<div class="flex items-center gap-1 px-4 pt-2 border-b border-surface-border bg-surface-raised shrink-0">
		<button id="wg-tab-steps" onclick="wgTab('steps')" class="px-4 py-2 text-[12px] font-semibold rounded-t-md border-b-2 transition">Steps</button>
		<button id="wg-tab-settings" onclick="wgTab('settings')" class="px-4 py-2 text-[12px] font-semibold rounded-t-md border-b-2 transition">Settings</button>
		<div class="ml-auto pb-1">
			<select id="wg-template-select" onchange="wgLoadTemplate(this.value); this.value='';"
				class="text-[11px] rounded-md border border-surface-border bg-surface-sunken px-2 py-1 text-ink focus:border-accent focus:outline-none">
				<option value="">start from template…</option>
			</select>
		</div>
	</div>

	<!-- Built-in conversion notice -->
	<div id="wg-builtin-banner" class="hidden shrink-0 px-4 py-2 bg-sky-500/10 border-b border-sky-500/30 text-[11px] text-sky-700 dark:text-sky-300">
		You're editing the <strong>built-in pipeline</strong>. Saving turns it into a custom step pipeline — the two approval gates become automatic (toggle <em>auto-approve</em> in Settings); PR-opening and notify still run for you. Hit <strong>revert to built-in</strong> any time to restore the original.
	</div>

	<!-- Body: node palette (left) + graph canvas (center) + step editor panel
	     (right; stacks below on small screens). The panel edits whichever node
	     is selected on the canvas — selection never rebuilds canvas DOM. -->
	<div class="flex-1 min-h-0 flex flex-col lg:flex-row">
		<div id="wg-palette" class="shrink-0 flex flex-row lg:flex-col gap-2 lg:gap-0 items-center lg:items-stretch overflow-x-auto lg:overflow-x-visible lg:w-56 border-b lg:border-b-0 lg:border-r border-surface-border bg-surface-raised/40 px-3 py-2 lg:py-3">
			<input id="wg-pal-search" type="search" placeholder="Search nodes…" oninput="wgPalFilter(this.value)"
				class="hidden lg:block w-full mb-3 text-xs rounded-md border border-surface-border bg-surface-sunken px-2 py-1.5 text-ink focus:border-accent focus:outline-none">
			<div class="hidden lg:block text-[9px] uppercase tracking-widest text-muted font-semibold mb-2">Steps</div>
			<div id="wg-pal-items" class="flex flex-row lg:flex-col gap-2 min-w-max lg:min-w-0"></div>
			<div class="hidden lg:block mt-3 text-[10px] text-muted leading-relaxed">Drag onto the canvas to add a node, or click to append.<br><br>Drag from a port to wire a branch · click a wire and press Delete to remove it.</div>
		</div>
		<div class="flex-1 min-h-0 relative" id="wg-body"></div>
		<div id="wg-panel" class="hidden shrink-0 lg:w-[400px] max-h-[45vh] lg:max-h-none min-h-0 overflow-y-auto border-t lg:border-t-0 lg:border-l border-surface-border bg-surface-raised/40"></div>
	</div>
</div>`)

	// ── JavaScript ────────────────────────────────────────────────────────
	fmt.Fprintf(w, `<script>
(function(){
'use strict';
var _cfg      = %s;
var _defaults = %s;
var _seed     = %s;
var _starters = %s;
var _savePath = %q;

// ── State ────────────────────────────────────────────────────────────
var CFG    = {};       // working copy of workflow config (settings + hooks)
var STEPS  = [];       // [{id, type, name, props:{}}] — order IS the pipeline order
var selId  = null;     // expanded step id (accordion)
var tab    = 'steps';
var seededFromBuiltin = false;
var _seq   = 0;
function newId(){ _seq++; return 'n'+_seq; }

var HOOK_PHASES = ['before_triage','before_execute','after_execute','before_test','after_test','before_verify','after_verify','before_pr','after_pr','on_failure'];

// ── Stage type metadata (single source of truth) ─────────────────────
var TYPES = {
	analyst: { label:'Analyst', letter:'A', color:'#f59e0b', short:'Answers questions (ask target)',
		blurb:'A knowledge node other nodes ASK. It answers using the repo context and any URLs you give it; the asking node then re-runs with the answer. It never routes forward on its own.',
		when:'Use for: a Product Owner / domain consultant the implementers and reviewers can consult.',
		example:'Answer the team question about scope and acceptance criteria.',
		ports:[] },
	executor: { label:'Executor', letter:'E', color:'#6366F1', short:'Does the work (implementer)',
		blurb:'Runs goon\'s agent loop inside the repo: reads/edits files, runs commands, opens the PR. Emits NEXT when done, or ASK to consult the analyst.',
		when:'Use for: the implementers — architect, backend engineer — and the open-PR step (set do = open_pr).',
		example:'Implement the change for {{.Key}} and run the tests.',
		ports:[{port:'ask',label:'ASK',color:'#F59E0B'},{port:'next',label:'NEXT',color:'#6366F1'}] },
	reviewer: { label:'Reviewer', letter:'R', color:'#8B5CF6', short:'Reviews + gates (approve/reject)',
		blurb:'Judges the executor\'s work. mode=human shows a person a change summary to APPROVE or REJECT; mode=llm decides automatically. APPROVE advances (fan-out); REJECT routes back, usually to a loop.',
		when:'Use for: code review, security review, QA sign-off.',
		example:'Review {{.Key}} for correctness and regressions.',
		ports:[{port:'ask',label:'ASK',color:'#F59E0B'},{port:'approve',label:'APPROVE',color:'#10B981'},{port:'reject',label:'REJECT',color:'#F43F5E'}] },
	loop: { label:'Loop', letter:'↻', color:'#F43F5E', short:'Bounded rework loop',
		blurb:'Routing node, no model call. Each arrival jumps back to its LOOP target until max loops is reached, then it exits via DONE.',
		when:'Use for: rework cycles — wire a reviewer REJECT here and the LOOP port back to the implementer, capped at N rounds.',
		example:'max loops: 3 → body runs at most 3 times',
		ports:[{port:'next',label:'LOOP',color:'#F43F5E'},{port:'done',label:'DONE',color:'#10B981'}] },
	notify: { label:'Notify', letter:'N', color:'#10B981', short:'Send a message',
		blurb:'Sends a message to your configured Telegram channel. Stored output = the message text.',
		when:'Use for: announcing a shipped PR or pinging mid-pipeline.',
		example:'{{.Key}} shipped — {{.Title}}',
		ports:[{port:'next',label:'NEXT',color:'#6366F1'}] }
};
function typeMeta(t){ return TYPES[t] || {label:(t||'?').toUpperCase(), letter:'?', color:'#6366F1', blurb:'', when:'', example:'', ports:[{port:'ask',label:'ASK',color:'#F59E0B'},{port:'next',label:'NEXT',color:'#6366F1'}]}; }
// on_approve may be a string or an array (fan-out) — normalize, like on_next.
function approveList(p){
	if(!p||p.on_approve==null||p.on_approve==='') return [];
	if(Object.prototype.toString.call(p.on_approve)==='[object Array]') return p.on_approve.slice();
	return [String(p.on_approve)];
}
function setApproveList(st,arr){
	st.props=st.props||{};
	if(!arr.length){ delete st.props.on_approve; }
	else if(arr.length===1){ st.props.on_approve=arr[0]; }
	else { st.props.on_approve=arr; }
}

// ── Helpers ──────────────────────────────────────────────────────────
function clone(o){ return o==null?o:JSON.parse(JSON.stringify(o)); }
function stepById(id){ for(var i=0;i<STEPS.length;i++) if(STEPS[i].id===id) return STEPS[i]; return null; }
function stepIndex(id){ for(var i=0;i<STEPS.length;i++) if(STEPS[i].id===id) return i; return -1; }
function hasKeys(o){ return o && typeof o==='object' && Object.keys(o).length>0; }
function escX(s){ return String(s==null?'':s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;').replace(/'/g,'&#39;'); }
function escH(s){ return String(s==null?'':s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;'); }
function fieldLabel(t){ return '<div class="text-[9px] uppercase tracking-widest text-muted font-semibold mb-1">'+t+'</div>'; }
function inputCls(){ return 'w-full text-xs rounded-md border border-surface-border bg-surface-sunken px-2 py-1.5 text-ink focus:border-accent focus:outline-none'; }
function fieldHint(t){ return '<div class="text-[10px] text-muted mt-1 leading-snug">'+t+'</div>'; }
function varHint(){ return fieldHint('Variables: <span class="font-mono text-muted">{{.Key}} {{.Title}} {{.Project}}</span> · earlier output: <span class="font-mono text-muted">{{.Stages.NAME.field}}</span>'); }
function typeHelpBlock(t){
	var m=typeMeta(t);
	if(!m.blurb) return '';
	var h='<div class="rounded-md border-l-2 px-2.5 py-2 bg-surface-sunken/40 space-y-1" style="border-color:'+m.color+'">';
	h+='<div class="text-[11px] text-ink leading-snug">'+escH(m.blurb)+'</div>';
	if(m.when) h+='<div class="text-[10px] text-muted leading-snug">'+escH(m.when)+'</div>';
	if(m.example) h+='<div class="text-[10px] text-muted">Example: <span class="font-mono text-muted">'+escH(m.example)+'</span></div>';
	return h+'</div>';
}
function stepPreview(st){
	var p=st.props||{};
	if(st.type==='loop'){ return 'max '+(p.max_loops||3)+' loops'+(p.on_done?(' · done → '+p.on_done):''); }
	if(st.type==='executor'&&p.do){ return 'built-in: '+p.do; }
	var v = (st.type==='executor'||st.type==='reviewer')?p.task : st.type==='notify'?p.message : p.prompt;
	v=(v||'').replace(/\s+/g,' ').trim();
	if(v.length>72) v=v.slice(0,71)+'…';
	return v;
}
// on_next may be a string (legacy) or an array (fan-out) — normalize.
function nextList(p){
	if(!p||p.on_next==null||p.on_next==='') return [];
	if(Object.prototype.toString.call(p.on_next)==='[object Array]') return p.on_next.slice();
	return [String(p.on_next)];
}
function setNextList(st,arr){
	st.props=st.props||{};
	if(!arr.length){ delete st.props.on_next; }
	else if(arr.length===1){ st.props.on_next=arr[0]; }
	else { st.props.on_next=arr; }
}

// ── Inline SVG icons (consistent outline family — no emoji) ──────────
var ICONS={
	grip:'<svg class="h-3.5 w-3.5" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><circle cx="9" cy="6" r="1.6"/><circle cx="15" cy="6" r="1.6"/><circle cx="9" cy="12" r="1.6"/><circle cx="15" cy="12" r="1.6"/><circle cx="9" cy="18" r="1.6"/><circle cx="15" cy="18" r="1.6"/></svg>',
	up:'<svg class="h-3 w-3" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M18 15l-6-6-6 6"/></svg>',
	down:'<svg class="h-3 w-3" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M6 9l6 6 6-6"/></svg>',
	copy:'<svg class="h-3.5 w-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg>',
	trash:'<svg class="h-3.5 w-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><polyline points="3 6 5 6 21 6"/><path d="M19 6l-1 14a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2L5 6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/></svg>',
	chevR:'<svg class="h-3.5 w-3.5 transition-transform duration-150" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M9 18l6-6-6-6"/></svg>',
	chevD:'<svg class="h-3.5 w-3.5 transition-transform duration-150" style="transform:rotate(90deg)" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M9 18l6-6-6-6"/></svg>',
	ship:'<svg class="h-4 w-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="11" width="18" height="10" rx="2"/><path d="M7 11V7a5 5 0 0110 0v4"/></svg>'
};

// ── Dirty state — amber dot on Save, guard on close ──────────────────
var _dirty=false;
function setDirty(v){
	_dirty=v;
	var d=document.getElementById('wg-dirty-dot');
	if(d) d.classList.toggle('hidden', !v);
}

// ── Undo ─────────────────────────────────────────────────────────────
var _history=[], _MAX_HISTORY=60;
function pushHistory(){ setDirty(true); _history.push({STEPS:clone(STEPS), selId:selId, CFG:clone(CFG)}); if(_history.length>_MAX_HISTORY) _history.shift(); updUndoBtn(); }
function updUndoBtn(){ var b=document.getElementById('wg-undo-btn'); if(b) b.disabled=(_history.length===0); }
window.wgUndo=function(){ if(!_history.length) return; var s=_history.pop(); STEPS=s.STEPS; selId=s.selId; CFG=s.CFG; renderAll(); updUndoBtn(); };

// ── Tabs + top-level render ──────────────────────────────────────────
window.wgTab=function(which){ tab=which; renderAll(); };
window.wgTogglePalette=function(){ window._palHidden=!window._palHidden; var p=document.getElementById('wg-palette'); if(p) p.style.display=(tab==='steps'&&!window._palHidden)?'':'none'; };
window.wgTogglePanel=function(){ window._panelHidden=!window._panelHidden; renderPanel(); };
function setTabUI(){
	var on='text-ink border-accent bg-surface-sunken/40';
	var off='text-muted border-transparent hover:text-ink';
	var ts=document.getElementById('wg-tab-steps'), tw=document.getElementById('wg-tab-settings');
	if(ts) ts.className='px-4 py-2 text-[12px] font-semibold rounded-t-md border-b-2 transition '+(tab==='steps'?on:off);
	if(tw) tw.className='px-4 py-2 text-[12px] font-semibold rounded-t-md border-b-2 transition '+(tab==='settings'?on:off);
}
function renderAll(){
	setTabUI();
	var pal=document.getElementById('wg-palette');
	if(pal) pal.style.display=(tab==='steps'&&!window._palHidden)?'':'none';
	if(tab==='settings'){
		var body=document.getElementById('wg-body');
		if(body){
			body.innerHTML='<div class="absolute inset-0 overflow-y-auto"><div id="wg-settings" class="px-6 py-6"></div></div>';
			renderSettings(document.getElementById('wg-settings'));
		}
		renderPanel();
	} else {
		renderSteps();
	}
}

// ── Graph canvas — free 2D node editor ───────────────────────────────
// Nexus-style canvas: pan/zoom world, nodes at arbitrary positions with
// typed ports (ask / next / reject), bezier wires you create by dragging
// from a port onto another node, Start/Finish lifecycle nodes, a minimap,
// and a palette you drag node types from. STEPS array order stays the
// engine's spine (implicit "next" wires); explicit wires write the
// on_next / on_reject / ask_stage props. Layout persists per-pipeline in
// localStorage. CRITICAL (history): selection must NEVER rebuild node
// DOM mid-pointer interaction — wgSelectStep only toggles classes.
var NODE_W=150, NODE_H=126, LIFE_W=120, LIFE_H=46;
var WORLD={x:60,y:40,k:1};
var NODEPOS={};   // node id (or '__start'/'__finish') -> {x,y} world coords
var selEdge=null; // selected explicit wire key, deletable via Delete key
var MODE=null;    // active pointer interaction: pan | node | port | pal

function posStoreKey(){ return 'goon-wg-pos:'+_savePath; }
function loadSavedPos(){ try{ return JSON.parse(localStorage.getItem(posStoreKey())||'{}'); }catch(err){ return {}; } }
function persistPos(){
	try{
		var o={};
		STEPS.forEach(function(s){ var p=NODEPOS[s.id]; if(p&&s.name) o[s.name]={x:Math.round(p.x),y:Math.round(p.y)}; });
		['__start','__finish'].forEach(function(k){ if(NODEPOS[k]) o[k]={x:Math.round(NODEPOS[k].x),y:Math.round(NODEPOS[k].y)}; });
		localStorage.setItem(posStoreKey(), JSON.stringify(o));
	}catch(err){}
}
function spinePos(i){ return {x:40+LIFE_W+60+i*(NODE_W+118), y:250}; }
// autoLayout: forward spine in a row, analysts floated ABOVE it, loops BELOW
// it, Start at the left and Finish at the right — readable, room for wires.
function autoLayout(){
	var COLW=NODE_W+118, ROWY=330, baseX=210;
	var byName={}; STEPS.forEach(function(s,j){ byName[s.name]=j; });
	var order=[], seen={}, cur=STEPS[0], guard=0;
	while(cur && guard<STEPS.length+2){ guard++; if(seen[cur.id]) break; seen[cur.id]=true; order.push(cur); var pr=cur.props||{}; var nx=nextList(pr)[0]; if(!nx) nx=approveList(pr)[0]; if(!nx||nx==='end'||byName[nx]==null) break; cur=STEPS[byName[nx]]; }
	var spine=[];
	order.forEach(function(s){ if(s.type!=='analyst'&&s.type!=='loop') spine.push(s); });
	STEPS.forEach(function(s){ if(s.type!=='analyst'&&s.type!=='loop'&&spine.indexOf(s)<0) spine.push(s); });
	var pos={};
	spine.forEach(function(s,i){ pos[s.id]={x:baseX+i*COLW, y:ROWY}; });
	var midX=baseX+(Math.max(0,spine.length-1)/2)*COLW;
	var an=STEPS.filter(function(s){return s.type==='analyst';});
	var lp=STEPS.filter(function(s){return s.type==='loop';});
	an.forEach(function(s,i){ pos[s.id]={x:Math.round(midX+(i-(an.length-1)/2)*COLW), y:ROWY-255}; });
	lp.forEach(function(s,i){ pos[s.id]={x:Math.round(midX+(i-(lp.length-1)/2)*COLW), y:ROWY+260}; });
	pos.__start={x:baseX-LIFE_W-60, y:ROWY+(NODE_H-LIFE_H)/2};
	pos.__finish={x:baseX+spine.length*COLW, y:ROWY+(NODE_H-LIFE_H)/2};
	return pos;
}
function ensureLayout(){
	var saved=loadSavedPos(), auto=autoLayout();
	var shared=(CFG&&CFG.layout)||{}; // positions shared inside workflow.json win
	if(!NODEPOS.__start) NODEPOS.__start=shared.__start||saved.__start||auto.__start||{x:30,y:289};
	STEPS.forEach(function(s){ if(!NODEPOS[s.id]) NODEPOS[s.id]=(s.name&&shared[s.name])||(s.name&&saved[s.name])||auto[s.id]||{x:210,y:320}; });
	if(!NODEPOS.__finish) NODEPOS.__finish=shared.__finish||saved.__finish||auto.__finish||{x:700,y:289};
}

// waveBand / waveDeco draw the landing-page cel sea: flat blue wave bands with
// a bold ink crest, anchored to the bottom of the canvas as a fixed backdrop.
function waveBand(fill,bottom){ return '<svg class="wg-wave" style="bottom:'+bottom+'px" viewBox="0 0 1200 90" preserveAspectRatio="none" aria-hidden="true"><path d="M0 50 Q75 18 150 50 T300 50 T450 50 T600 50 T750 50 T900 50 T1050 50 T1200 50 V90 H0 Z" fill="'+fill+'" stroke="#1E2433" stroke-width="4"></path></svg>'; }
function waveDeco(){ return '<div style="position:absolute;left:0;right:0;bottom:0;height:200px;pointer-events:none;overflow:hidden">'+waveBand('#2E8FC9',120)+waveBand('#1F6FA9',55)+waveBand('#175A8C',-8)+'</div>'; }
function renderSteps(){
	var body=document.getElementById('wg-body');
	if(!body) return;
	renderPalette();
	ensureLayout();
	var h='<div id="wg-canvas" class="select-none">';
	h+=waveDeco();
	h+='<div id="wg-world">';
	h+='<svg id="wg-edges" aria-hidden="true"></svg>';
	h+=lifeNode('__start');
	STEPS.forEach(function(st,i){ h+=graphNode(st,i); });
	h+=lifeNode('__finish');
	h+='</div>';
	// floating toolbar
	var tb='inline-flex items-center justify-center h-7 min-w-7 px-1.5 rounded text-xs text-muted hover:text-ink hover:bg-surface-sunken transition';
	h+='<div class="absolute left-3 top-3 z-20 flex items-center gap-0.5 rounded-lg border border-surface-border bg-surface-raised/95 px-1 py-0.5 shadow-card">';
	h+='<button type="button" onclick="wgZoomBtn(1)" title="Zoom in" aria-label="Zoom in" class="'+tb+'">+</button>';
	h+='<button type="button" onclick="wgZoomBtn(-1)" title="Zoom out" aria-label="Zoom out" class="'+tb+'">&minus;</button>';
	h+='<button type="button" onclick="wgFit()" title="Fit view" class="'+tb+'">fit</button>';
	h+='<button type="button" onclick="wgTidy()" title="Auto-organize the islands into a clean map" class="'+tb+'">organize</button>';
	h+='<span class="w-px h-4 mx-0.5" style="background:rgb(var(--c-border))"></span>';
	h+='<button type="button" onclick="wgTogglePalette()" title="Hide / show steps panel (left)" aria-label="Toggle left panel" class="'+tb+'">◧</button>';
	h+='<button type="button" onclick="wgTogglePanel()" title="Hide / show editor panel (right)" aria-label="Toggle right panel" class="'+tb+'">◨</button>';
	h+='</div>';
	if(STEPS.length===0){
		h+='<div class="absolute left-1/2 top-1/2 -translate-x-1/2 -translate-y-1/2 z-10 rounded-xl border border-dashed border-surface-border bg-surface-raised/80 px-5 py-4 text-center pointer-events-none">'
		  +'<svg class="h-7 w-7 text-muted mx-auto mb-2" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="3" width="7" height="7" rx="1"/><rect x="14" y="3" width="7" height="7" rx="1"/><rect x="3" y="14" width="7" height="7" rx="1"/><rect x="14" y="14" width="7" height="7" rx="1"/></svg>'
		  +'<div class="text-xs text-muted">Drag a step from the palette onto the canvas.</div></div>';
	}
	h+='<div id="wg-mini" class="absolute bottom-3 right-3 z-20 w-44 h-28 rounded-lg border border-surface-border bg-surface-raised/95 shadow-card overflow-hidden"></div>';
	h+='</div>';
	body.innerHTML=h;
	bindCanvas();
	applyWorld();
	wgDrawEdges();
	renderMini();
	renderPanel();
}

// roleGlyph returns the island's icon SVG for a node role (the open_pr
// executor becomes a treasure chest — the shipped PR). currentColor is the
// glyph colour; the caller sets it via the badge's style.
var GLYPH_MAG='<svg class="h-5 w-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="10.5" cy="10.5" r="6"/><line x1="15" y1="15" x2="20" y2="20"/></svg>';
var GLYPH_LOOP='<svg class="h-5 w-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M20 11a8 8 0 1 0-2.3 6"/><path d="M20 4v6h-6"/></svg>';
var GLYPH_SHIP='<svg class="h-5 w-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="11" width="18" height="10" rx="2"/><path d="M7 11V7a5 5 0 0110 0v4"/><circle cx="12" cy="16" r="1.5" fill="currentColor"/></svg>';
var GLYPH_USER='<svg class="h-5 w-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="8" r="4"/><path d="M4 20c0-4 3.6-7 8-7s8 3 8 7"/></svg>';
var GLYPH_GEAR='<svg class="h-5 w-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="3"/><path d="M12 1v4M12 19v4M4.9 4.9l2.8 2.8M16.3 16.3l2.8 2.8M1 12h4M19 12h4M4.9 19.1l2.8-2.8M16.3 7.7l2.8-2.8"/></svg>';
var GLYPH_BELL='<svg class="h-5 w-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M18 8A6 6 0 006 8c0 7-3 9-3 9h18s-3-2-3-9"/><path d="M13.73 21a2 2 0 01-3.46 0"/></svg>';
function roleGlyph(t,ship){
	if(ship) return GLYPH_SHIP;
	if(t==='analyst') return GLYPH_USER;
	if(t==='executor') return GLYPH_GEAR;
	if(t==='notify') return GLYPH_BELL;
	if(t==='reviewer') return GLYPH_MAG;
	if(t==='loop') return GLYPH_LOOP;
	return '<svg class="h-5 w-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="3" width="18" height="18" rx="3"/><path d="M3 9h18M9 21V9"/></svg>';
}
// islandObject draws the role's landmark (drawn relative to the island's centre,
// rising above the sandy ellipse) — matching the landing-page voyage art:
// analyst=palm+note, executor=hut, reviewer=document+check, loop=mountain,
// notify=flag, open_pr ship=treasure chest.
function islandObject(t,ship){
	if(ship) return '<rect x="-26" y="-14" width="52" height="28" rx="5" fill="#A9713F" stroke="#1E2433" stroke-width="4"></rect><path d="M-26 -14 q0 -18 26 -18 q26 0 26 18 Z" fill="#8A5A33" stroke="#1E2433" stroke-width="4"></path><rect x="-5" y="-4" width="10" height="12" rx="2" fill="#FFD66B" stroke="#1E2433" stroke-width="3"></rect>';
	if(t==='analyst') return '<path d="M-12 10 L-12 -24 q-24 -12 -38 2 q18 -2 24 8 M-12 -24 q22 -14 36 0 q-17 -3 -24 9" fill="none" stroke="#2C6E47" stroke-width="6" stroke-linecap="round"></path><rect x="6" y="-12" width="26" height="22" rx="3" fill="#FBF2DF" stroke="#1E2433" stroke-width="3.5"></rect><line x1="11" y1="-4" x2="27" y2="-4" stroke="#1E2433" stroke-width="2"></line><line x1="11" y1="2" x2="23" y2="2" stroke="#1E2433" stroke-width="2"></line>';
	if(t==='executor') return '<path d="M-22 14 L-22 -18 L18 -18 L18 14" fill="#A9713F" stroke="#1E2433" stroke-width="4" stroke-linejoin="round"></path><path d="M-30 -18 L26 -18 L20 -32 L-24 -32 Z" fill="#8A5A33" stroke="#1E2433" stroke-width="4" stroke-linejoin="round"></path><rect x="-10" y="-6" width="18" height="20" rx="2" fill="#FBF2DF" stroke="#1E2433" stroke-width="3"></rect>';
	if(t==='reviewer') return '<rect x="-28" y="-24" width="44" height="34" rx="4" fill="#FBF2DF" stroke="#1E2433" stroke-width="4"></rect><line x1="-20" y1="-12" x2="8" y2="-12" stroke="#1E2433" stroke-width="2.5"></line><line x1="-20" y1="-4" x2="4" y2="-4" stroke="#1E2433" stroke-width="2.5"></line><line x1="-20" y1="4" x2="8" y2="4" stroke="#1E2433" stroke-width="2.5"></line><path d="M18 2 l7 7 l14 -16" fill="none" stroke="#1E8E5A" stroke-width="5" stroke-linecap="round" stroke-linejoin="round"></path>';
	if(t==='loop') return '<path d="M-26 12 L-8 -30 L4 -8 L16 -34 L34 12 Z" fill="#7BA05B" stroke="#1E2433" stroke-width="4" stroke-linejoin="round"></path><path d="M-14 -14 l8 -8 M22 -20 l-6 -8" stroke="#1E2433" stroke-width="3" stroke-linecap="round"></path>';
	if(t==='notify') return '<line x1="0" y1="14" x2="0" y2="-34" stroke="#1E2433" stroke-width="4" stroke-linecap="round"></line><path d="M0 -34 L26 -27 L0 -19 Z" fill="#E8833A" stroke="#1E2433" stroke-width="3.5" stroke-linejoin="round"></path>';
	return '<rect x="-20" y="-20" width="40" height="40" rx="6" fill="#FBF2DF" stroke="#1E2433" stroke-width="4"></rect>';
}
// SHIP_SVG is goon's voyage ship (from the landing page), scaled + centred on
// its origin so animateMotion can sail it along the spine path.
var SHIP_SVG='<g transform="translate(-15.6,-13) scale(0.13)">'
	+'<line x1="120" y1="22" x2="120" y2="128" stroke="#6B4426" stroke-width="9" stroke-linecap="round"/>'
	+'<path d="M120 30 L120 96 L66 90 Q88 62 66 36 Z" fill="#FBF2DF" stroke="#1E2433" stroke-width="7" stroke-linejoin="round"/>'
	+'<path d="M120 22 L162 32 L120 44 Z" fill="#E24B4A" stroke="#1E2433" stroke-width="6" stroke-linejoin="round"/>'
	+'<path d="M34 128 L206 128 L182 168 Q120 180 58 168 Z" fill="#8A5A33" stroke="#1E2433" stroke-width="7" stroke-linejoin="round"/>'
	+'</g>';
function graphNode(st,i){
	var m=typeMeta(st.type), on=(st.id===selId);
	var p=NODEPOS[st.id]||spinePos(i);
	// Each node is a voyage-map ISLAND: a sandy ellipse landmass carrying the
	// role's landmark (palm+note / hut / document+check / mountain / flag /
	// treasure chest) with a cream name-plate label below it. The .wg-step box
	// is transparent — the art is the SVG; ports sit on the box edges.
	var isShip=!!(st.type==='executor' && st.props && st.props.do==='open_pr');
	var cx=NODE_W/2;
	var h='<div class="wg-step absolute cursor-grab '+(on?'wg-sel':'')+'"'
	  +' data-id="'+st.id+'" tabindex="0" role="button"'
	  +' aria-label="Step '+(i+1)+': '+escX(st.name||'unnamed')+' ('+escX(m.label)+')"'
	  +' onkeydown="if(event.key===\'Enter\'||event.key===\' \'){event.preventDefault();wgSelectStep(\''+st.id+'\');}"'
	  +' style="left:'+p.x+'px;top:'+p.y+'px;width:'+NODE_W+'px;height:'+NODE_H+'px">';
	// island illustration (sandy ellipse + role landmark)
	h+='<svg class="absolute left-0 top-0 pointer-events-none" width="'+NODE_W+'" height="96" viewBox="0 0 '+NODE_W+' 96" style="overflow:visible">';
	h+='<ellipse class="wg-isle" cx="'+cx+'" cy="72" rx="60" ry="18" fill="#EFD9A0" stroke="#1E2433" stroke-width="4"></ellipse>';
	h+='<g transform="translate('+cx+',56)">'+islandObject(st.type,isShip)+'</g>';
	h+='</svg>';
	// cream name-plate label beneath the island
	h+='<div class="absolute" style="left:0;right:0;bottom:3px;display:flex;justify-content:center">';
	h+='<div class="wg-plate flex items-center gap-1.5" style="max-width:'+(NODE_W-4)+'px;background:#FBF2DF;border:2.5px solid #1E2433;border-radius:10px;padding:2px 9px;box-shadow:0 3px 0 -1px rgba(20,70,110,0.5)">';
	h+='<span class="shrink-0" style="width:9px;height:9px;border-radius:99px;background:'+m.color+';border:1.5px solid #1E2433"></span>';
	h+='<span class="truncate" title="'+escX(st.name||'')+'" style="font-size:12.5px;font-weight:800;color:#1E2433;line-height:1.35" data-stepname="'+st.id+'">'+escX(st.name||'(unnamed)')+'</span>';
	h+='</div></div>';
	h+='<div class="absolute pointer-events-none" style="left:2px;top:0;font-size:9px;font-family:monospace;color:#1E2433;opacity:.45">'+(i+1)+'</div>';
	// ports: input (left) + typed outputs (right). Normal steps get
	// ask/next/reject; loop nodes get LOOP (the body, repeats) + DONE
	// (the exit once max loops is spent). NEXT supports several wires —
	// drop on more nodes to fan out.
	var inLoop=(st.type==='loop');
	h+='<span class="wg-port" data-node="'+st.id+'" data-port="in" style="'+(inLoop?'right:-7px;top:40px':'left:-7px;top:64px')+'" title="arrival"></span>';
	// ports are data-driven per role (TYPES[type].ports): executor ask+next,
	// reviewer ask+approve+reject, loop loop+done, notify next, analyst none.
	var PORTY={ask:40, next:64, approve:64, reject:86, done:86};
	(m.ports||[]).forEach(function(pc){
		var active=false;
		if(pc.port==='next') active=nextList(st.props).length>0;
		else if(pc.port==='approve') active=approveList(st.props).length>0;
		else if(pc.port==='ask') active=!!(st.props&&st.props.ask);
		else if(pc.port==='reject') active=!!(st.props&&st.props.on_reject);
		else if(pc.port==='done') active=!!(st.props&&st.props.on_done);
		var pTop=PORTY[pc.port], pSide='r';
		if(st.type==='loop'){ if(pc.port==='next'){ pSide='l'; pTop=64; } else if(pc.port==='done'){ pTop=86; } }
		h+=outPort(st.id, pc.port, pTop, pc.label, pc.color, active, pSide);
	});
	h+='</div>';
	return h;
}
function outPort(id,port,top,label,col,active,side){
	var L=(side==='l'), dotPos=L?'left:-7px':'right:-7px', lblPos=L?'left:9px':'right:9px';
	return '<span class="wg-port" data-node="'+id+'" data-port="'+port+'" title="Drag to wire: '+label.toLowerCase()
	  +'" style="'+dotPos+';top:'+top+'px;border-color:'+col+(active?';background:'+col:'')+'"></span>'
	  +'<span class="absolute pointer-events-none font-bold" style="'+lblPos+';top:'+(top-1)+'px;font-size:7px;color:'+col+'">'+label+'</span>';
}
function lifeNode(id){
	var start=(id==='__start');
	var p=NODEPOS[id]||{x:0,y:0};
	var col=start?'#6366F1':'#10B981';
	var icon=start
		?'<svg class="h-3.5 w-3.5" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M8 5v14l11-7z"/></svg>'
		:'<svg class="h-4 w-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><polyline points="20 6 9 17 4 12"/></svg>';
	var h='<div class="wg-life absolute rounded-lg border-2 border-surface-border bg-surface-raised cursor-grab" data-life="'+id+'"'
	  +' style="left:'+p.x+'px;top:'+p.y+'px;width:'+LIFE_W+'px;height:'+LIFE_H+'px">';
	h+='<div class="flex items-center gap-2 px-2.5 h-full pointer-events-none">';
	h+='<span class="w-6 h-6 rounded-md flex items-center justify-center shrink-0" style="background:'+col+'22;color:'+col+'">'+icon+'</span>';
	h+='<div class="min-w-0"><div class="text-[11px] font-semibold text-ink">'+(start?'Start':'Finish')+'</div><div class="text-[8px] uppercase tracking-wider text-muted">lifecycle</div></div>';
	h+='</div>';
	if(start){ h+='<span class="wg-port" data-node="__start" data-port="start" title="Drag to choose the first step" style="right:-7px;top:'+(LIFE_H/2-6)+'px;border-color:#6366F1"></span>'; }
	else { h+='<span class="wg-port" data-node="__finish" data-port="in" style="left:-7px;top:'+(LIFE_H/2-6)+'px;border-color:#10B981" title="end of pipeline"></span>'; }
	h+='</div>';
	return h;
}

// Selection — class toggles only, never a DOM rebuild (see note above).
window.wgSelectStep=function(id){
	if(selEdge){ selEdge=null; wgDrawEdges(); }
	if(selId!==id){
		var prev=document.querySelector('.wg-step[data-id="'+selId+'"]');
		if(prev){ prev.classList.remove('wg-sel'); }
		selId=id;
		var cur=document.querySelector('.wg-step[data-id="'+id+'"]');
		if(cur){ cur.classList.add('wg-sel'); }
	}
	renderPanel();
};
window.wgDeselect=function(){ selId=null; renderSteps(); };

// ── World transform helpers ──────────────────────────────────────────
function applyWorld(){
	var w=document.getElementById('wg-world');
	var cv=document.getElementById('wg-canvas');
	if(w) w.style.transform='translate('+WORLD.x+'px,'+WORLD.y+'px) scale('+WORLD.k+')';
	if(cv){
		var g=22*WORLD.k;
		cv.style.backgroundSize=g+'px '+g+'px';
		cv.style.backgroundPosition=WORLD.x+'px '+WORLD.y+'px';
	}
}
function worldFromClient(cx,cy){
	var cv=document.getElementById('wg-canvas');
	var r=cv.getBoundingClientRect();
	return { x:(cx-r.left-WORLD.x)/WORLD.k, y:(cy-r.top-WORLD.y)/WORLD.k };
}
window.wgZoomBtn=function(dir){
	var cv=document.getElementById('wg-canvas'); if(!cv) return;
	zoomAt(cv.clientWidth/2, cv.clientHeight/2, dir>0?1.2:1/1.2);
};
function zoomAt(px,py,factor){
	var k2=Math.min(2.5, Math.max(0.3, WORLD.k*factor));
	WORLD.x=px-(px-WORLD.x)*(k2/WORLD.k);
	WORLD.y=py-(py-WORLD.y)*(k2/WORLD.k);
	WORLD.k=k2;
	applyWorld(); renderMini();
}
function graphBBox(){
	var xs=[],ys=[],xe=[],ye=[];
	Object.keys(NODEPOS).forEach(function(id){
		var p=NODEPOS[id]; if(!p) return;
		var w=(id==='__start'||id==='__finish')?LIFE_W:NODE_W;
		var hh=(id==='__start'||id==='__finish')?LIFE_H:NODE_H;
		xs.push(p.x); ys.push(p.y); xe.push(p.x+w); ye.push(p.y+hh);
	});
	if(!xs.length) return {x:0,y:0,w:600,h:400};
	var x0=Math.min.apply(null,xs), y0=Math.min.apply(null,ys);
	return {x:x0, y:y0, w:Math.max.apply(null,xe)-x0, h:Math.max.apply(null,ye)-y0};
}
window.wgHome=function(){
	var cv=document.getElementById('wg-canvas'); if(!cv) return;
	var b=graphBBox();
	// Open at a READABLE zoom: fit to width but never below 0.72, anchored near
	// the start so the first islands are legible (pan / minimap for the rest).
	var k=Math.min((cv.clientWidth-90)/Math.max(b.w,1), 1.0);
	WORLD.k=Math.max(0.72, k);
	WORLD.x=90 - b.x*WORLD.k;
	WORLD.y=(cv.clientHeight-b.h*WORLD.k)/2 - b.y*WORLD.k;
	applyWorld(); renderMini();
};
window.wgFit=function(){
	var cv=document.getElementById('wg-canvas'); if(!cv) return;
	var b=graphBBox();
	var k=Math.min((cv.clientWidth-100)/Math.max(b.w,1), (cv.clientHeight-100)/Math.max(b.h,1), 1.15);
	WORLD.k=Math.max(0.5, k);
	WORLD.x=(cv.clientWidth-b.w*WORLD.k)/2 - b.x*WORLD.k;
	WORLD.y=(cv.clientHeight-b.h*WORLD.k)/2 - b.y*WORLD.k;
	applyWorld(); renderMini();
};
window.wgTidy=function(){
	try{ localStorage.removeItem(posStoreKey()); }catch(err){}
	NODEPOS={};
	renderSteps();
	wgFit();
};
function wgCenterOn(id){
	var cv=document.getElementById('wg-canvas'); var p=NODEPOS[id];
	if(!cv||!p) return;
	WORLD.x=cv.clientWidth/2-(p.x+NODE_W/2)*WORLD.k;
	WORLD.y=cv.clientHeight/2-(p.y+NODE_H/2)*WORLD.k;
	applyWorld(); renderMini();
}

// ── Wires ────────────────────────────────────────────────────────────
function edgeDefs(){
	function mk(id,col){ return '<marker id="wg-arr-'+id+'" viewBox="0 0 10 10" refX="8" refY="5" markerWidth="6.5" markerHeight="6.5" orient="auto-start-reverse"><path d="M 0 1 L 8 5 L 0 9 z" style="fill:'+col+'"></path></marker>'; }
	return '<defs>'+mk('seq','rgb(var(--c-muted) / 0.8)')+mk('next','rgb(var(--c-accent))')+mk('reject','rgb(244 63 94 / 0.9)')+mk('ask','rgb(245 158 11 / 0.9)')+mk('approve','rgb(34 197 94 / 0.9)')+mk('loop','rgb(244 63 94 / 0.9)')+mk('done','rgb(34 197 94 / 0.9)')+'</defs>';
}
function edgeColor(kind,sel){
	if(kind==='reject'||kind==='loop') return sel?'rgb(244 63 94)':'rgb(244 63 94 / 0.9)';
	if(kind==='ask')    return sel?'rgb(245 158 11)':'rgb(245 158 11 / 0.85)';
	if(kind==='approve'||kind==='done') return sel?'rgb(34 197 94)':'rgb(34 197 94 / 0.9)';
	if(kind==='next')   return sel?'rgb(var(--c-accent))':'rgb(var(--c-accent) / 0.9)';
	return 'rgb(var(--c-muted) / 0.55)';
}
function edgeKey(e){ return e.src+'|'+(e.prop||'seq')+'|'+e.dst; }
function edgeList(){
	var L=[]; var byName={}; STEPS.forEach(function(s,j){ byName[s.name]=j; });
	if(STEPS.length){ L.push({src:'__start',port:'start',dst:STEPS[0].id,kind:'seq'}); }
	else { L.push({src:'__start',port:'start',dst:'__finish',kind:'seq'}); }
	STEPS.forEach(function(st,i){
		var p=st.props||{};
		var mp=typeMeta(st.type);
		function hasPort(pn){ return (mp.ports||[]).some(function(x){ return x.port===pn; }); }
		var isLoop=(st.type==='loop');
		// next / loop-body — only nodes with a NEXT/LOOP port (executor, notify, loop)
		if(hasPort('next')){
			var nxts=nextList(p);
			if(nxts.length){
				nxts.forEach(function(nm){
					var kind=isLoop?'loop':'next';
					if(nm==='end'){ L.push({src:st.id,port:'next',dst:'__finish',kind:kind,prop:'on_next'}); }
					else if(byName[nm]!=null){ L.push({src:st.id,port:'next',dst:STEPS[byName[nm]].id,kind:kind,prop:'on_next'}); }
				});
			}
			else if(!isLoop){
				// Implicit fall-through skips analyst sidecars (ask-only nodes),
				// mirroring the engine's nextTargets — so a trailing Product Owner
				// never gets a phantom wire from a notify/executor with no on_next.
				var fx=null;
				for(var k=i+1;k<STEPS.length;k++){ if(STEPS[k].type==='analyst') continue; fx=STEPS[k]; break; }
				if(fx){ L.push({src:st.id,port:'next',dst:fx.id,kind:'seq'}); }
				else { L.push({src:st.id,port:'next',dst:'__finish',kind:'seq'}); }
			}
		}
		// approve — reviewer fan-out (green)
		if(hasPort('approve')){
			approveList(p).forEach(function(nm){
				if(nm==='end'){ L.push({src:st.id,port:'approve',dst:'__finish',kind:'approve',prop:'on_approve'}); }
				else if(byName[nm]!=null){ L.push({src:st.id,port:'approve',dst:STEPS[byName[nm]].id,kind:'approve',prop:'on_approve'}); }
			});
		}
		// reject — reviewer
		if(hasPort('reject')){
			if(p.on_reject==='end'){ L.push({src:st.id,port:'reject',dst:'__finish',kind:'reject',prop:'on_reject'}); }
			else if(p.on_reject&&byName[p.on_reject]!=null){ L.push({src:st.id,port:'reject',dst:STEPS[byName[p.on_reject]].id,kind:'reject',prop:'on_reject'}); }
		}
		// done — loop exit
		if(hasPort('done')){
			if(p.on_done==='end'){ L.push({src:st.id,port:'done',dst:'__finish',kind:'done',prop:'on_done'}); }
			else if(p.on_done&&byName[p.on_done]!=null){ L.push({src:st.id,port:'done',dst:STEPS[byName[p.on_done]].id,kind:'done',prop:'on_done'}); }
		}
		// ask → analyst (any node may wire one)
		if(p.ask&&byName[p.ask]!=null){ L.push({src:st.id,port:'ask',dst:STEPS[byName[p.ask]].id,kind:'ask',prop:'ask'}); }
	});
	return L;
}
function isLoopNode(id){ for(var i=0;i<STEPS.length;i++){ if(STEPS[i].id===id) return STEPS[i].type==='loop'; } return false; }
function outAnchor(id,port){
	var p=NODEPOS[id]; if(!p) return {x:0,y:0};
	if(id==='__start') return {x:p.x+LIFE_W, y:p.y+LIFE_H/2};
	if(isLoopNode(id)){
		if(port==='next') return {x:p.x, y:p.y+69};        // LOOP exits LEFT (island mid)
		return {x:p.x+NODE_W, y:p.y+91};                   // DONE exits RIGHT (lower)
	}
	var t = port==='ask'?45 : (port==='reject'||port==='done')?91 : 69;
	return {x:p.x+NODE_W, y:p.y+t};
}
function inAnchor(id){
	var p=NODEPOS[id]; if(!p) return {x:0,y:0};
	if(id==='__finish') return {x:p.x, y:p.y+LIFE_H/2};
	if(isLoopNode(id)) return {x:p.x+NODE_W, y:p.y+45};          // loop ARRIVAL on the RIGHT (upper)
	return {x:p.x, y:p.y+69};
}
// ── Wave wires ───────────────────────────────────────────────────────
// Each connection is drawn as a gentle sine ripple riding the routing
// bezier — water flowing between islands. The ripple tapers to 0 at both
// ports so it meets the island cleanly; the flowing dash animation makes
// the wave travel.
function cubicAt(p0,p1,p2,p3,t){
	var m=1-t;
	return { x:m*m*m*p0.x+3*m*m*t*p1.x+3*m*t*t*p2.x+t*t*t*p3.x,
		y:m*m*m*p0.y+3*m*m*t*p1.y+3*m*t*t*p2.y+t*t*t*p3.y };
}
function cubicTan(p0,p1,p2,p3,t){
	var m=1-t;
	return { x:3*m*m*(p1.x-p0.x)+6*m*t*(p2.x-p1.x)+3*t*t*(p3.x-p2.x),
		y:3*m*m*(p1.y-p0.y)+6*m*t*(p2.y-p1.y)+3*t*t*(p3.y-p2.y) };
}
function wavePath(a,b,dx){
	var p0=a, p1={x:a.x+dx,y:a.y}, p2={x:b.x-dx,y:b.y}, p3=b;
	var len=Math.hypot(b.x-a.x,b.y-a.y);
	var amp=Math.max(3,Math.min(6.5,len/26));
	var n=30, pts=[];
	for(var i=0;i<=n;i++){
		var t=i/n;
		var pt=cubicAt(p0,p1,p2,p3,t), tn=cubicTan(p0,p1,p2,p3,t);
		var tl=Math.hypot(tn.x,tn.y)||1;
		var w=Math.sin(t*Math.PI*3.2)*amp*Math.sin(t*Math.PI);
		pts.push((pt.x+(-tn.y/tl)*w).toFixed(1)+' '+(pt.y+(tn.x/tl)*w).toFixed(1));
	}
	return 'M '+pts.join(' L ');
}
// Focus mode: hovering an island highlights only its wires and dims the rest,
// so a complex route map stays readable.
function wgFocus(id){
	var eds=document.querySelectorAll('#wg-edges .wg-wire');
	for(var i=0;i<eds.length;i++){ var p=eds[i]; var sc=p.getAttribute('data-esrc'), dc=p.getAttribute('data-edst'); p.style.opacity=(sc===id||dc===id)?'1':'0.08'; }
}
function wgClearFocus(){ var eds=document.querySelectorAll('#wg-edges .wg-wire'); for(var i=0;i<eds.length;i++){ eds[i].style.opacity=''; } }
function wgDrawEdges(){
	var svg=document.getElementById('wg-edges');
	if(!svg) return;
	var h=edgeDefs();
	var ships=[];   // flow lanes that each get a sailing ship (one per branch)
	edgeList().forEach(function(e){
		var a=outAnchor(e.src,e.port), b=inAnchor(e.dst);
		var dx=Math.max(46, Math.abs(b.x-a.x)/2);
		var d='M '+a.x+' '+a.y+' C '+(a.x+dx)+' '+a.y+', '+(b.x-dx)+' '+b.y+', '+b.x+' '+b.y;
		var sel=(selEdge===edgeKey(e));
		var ek=edgeKey(e);
		// Each work-flow edge (entry/next/approve/done/loop) is a sea lane with
		// its own ship — a node that fans out launches a ship per branch. The
		// ask (consult) and reject (bounce) wires stay ship-free.
		if(e.kind==='seq'||e.kind==='next'||e.kind==='approve'||e.kind==='done'||e.kind==='loop'){
			ships.push({d:d, len:Math.hypot(b.x-a.x,b.y-a.y)});
		}
		// Fat transparent hit-path under the visible wire so it's easy to click
		// (only explicit, deletable wires get one — implicit "seq" links don't).
		if(e.prop){
			h+='<path class="wg-hit" d="'+d+'" data-ek="'+ek+'" data-eprop="'+e.prop+'" data-esrc="'+e.src+'"></path>';
		}
		h+='<g class="wg-wire" data-esrc="'+e.src+'" data-edst="'+e.dst+'">';
		h+='<path class="wg-edge-case" d="'+d+'" style="pointer-events:none" stroke-width="'+(sel?8:6)+'"></path>';
		h+='<path class="wg-edge'+(sel?' wg-edge-sel':'')+'" d="'+d+'"'
		  +' style="stroke:'+edgeColor(e.kind,sel)+';pointer-events:none" stroke-width="'+(sel?5.5:4.5)+'" marker-end="url(#wg-arr-'+e.kind+')"></path>';
		var mid=cubicAt({x:a.x,y:a.y},{x:a.x+dx,y:a.y},{x:b.x-dx,y:b.y},{x:b.x,y:b.y},0.5);
		if(e.kind==='reject'||e.kind==='ask'||e.kind==='loop'||e.kind==='done'||e.kind==='approve'){
			var mx=mid.x, my=mid.y, lw=e.kind.length*6+16;
			// The label rides the curve and is itself a click target — clicking it
			// selects the wire (people aim for the label, not the thin line).
			h+='<rect class="wg-lbl" data-ek="'+ek+'" data-eprop="'+e.prop+'" data-esrc="'+e.src+'" x="'+(mx-lw/2)+'" y="'+(my-18)+'" width="'+lw+'" height="17" rx="8.5" fill="#FBF2DF" stroke="'+edgeColor(e.kind,sel)+'" stroke-width="'+(sel?2.5:1.5)+'" style="cursor:pointer"></rect>';
			h+='<text x="'+mx+'" y="'+(my-6)+'" text-anchor="middle" style="fill:#1E2433;font-size:9.5px;font-weight:700;pointer-events:none">'+e.kind+'</text>';
		}
		// Visible delete badge on the selected wire (click the × to remove it).
		if(sel&&e.prop){
			var cx=mid.x, cy=mid.y+18;
			h+='<g class="wg-del" data-del="'+ek+'"><circle cx="'+cx+'" cy="'+cy+'" r="9" fill="#FBF2DF" stroke="rgb(244 63 94)" stroke-width="1.5"></circle>'
			  +'<text x="'+cx+'" y="'+(cy+3.6)+'" text-anchor="middle" style="fill:rgb(244 63 94);font-size:12px;font-weight:800;pointer-events:none">×</text></g>';
		}
		h+='</g>';
	});
	// One ship per flow lane — goon's fleet "working around the sea". A node
	// with several outgoing branches launches several ships. Length-based
	// duration keeps speed roughly constant; a staggered negative begin desyncs
	// them (SMIL wraps the phase, so the offset needn't be < dur). Ships ride in
	// the edges layer, so they duck behind the islands as they pass. Skipped
	// under reduced-motion.
	var rm=window.matchMedia&&window.matchMedia('(prefers-reduced-motion: reduce)').matches;
	if(!rm){
		ships.forEach(function(s,si){
			var dur=Math.max(6,Math.min(15, s.len/46)).toFixed(1);
			var beg=(-(si*1.7)).toFixed(1);
			h+='<path id="wg-lane-'+si+'" d="'+s.d+'" fill="none" stroke="none"></path>';
			h+='<g style="pointer-events:none" opacity="0.96"><animateMotion dur="'+dur+'s" begin="'+beg+'s" repeatCount="indefinite" calcMode="linear"><mpath href="#wg-lane-'+si+'"></mpath></animateMotion>'+SHIP_SVG+'</g>';
		});
	}
	h+='<path id="wg-ghost" class="wg-edge" style="stroke:rgb(var(--c-accent) / 0.9);display:none" stroke-width="2"></path>';
	svg.innerHTML=h;
}

// ── Minimap ──────────────────────────────────────────────────────────
function renderMini(){
	var mini=document.getElementById('wg-mini');
	var cv=document.getElementById('wg-canvas');
	if(!mini||!cv) return;
	var b=graphBBox();
	var pad=60;
	b={x:b.x-pad, y:b.y-pad, w:b.w+pad*2, h:b.h+pad*2};
	var s=Math.min(mini.clientWidth/b.w, mini.clientHeight/b.h);
	var h='';
	Object.keys(NODEPOS).forEach(function(id){
		var p=NODEPOS[id]; if(!p) return;
		var life=(id==='__start'||id==='__finish');
		var w=(life?LIFE_W:NODE_W)*s, hh=(life?LIFE_H:NODE_H)*s;
		var col = life ? 'rgb(var(--c-muted) / 0.6)' : (id===selId ? 'rgb(var(--c-accent))' : 'rgb(var(--c-muted) / 0.9)');
		h+='<div style="position:absolute;left:'+((p.x-b.x)*s)+'px;top:'+((p.y-b.y)*s)+'px;width:'+Math.max(3,w)+'px;height:'+Math.max(2,hh)+'px;border-radius:2px;background:'+col+'"></div>';
	});
	var vx=(-WORLD.x/WORLD.k-b.x)*s, vy=(-WORLD.y/WORLD.k-b.y)*s;
	var vw=cv.clientWidth/WORLD.k*s, vh=cv.clientHeight/WORLD.k*s;
	h+='<div style="position:absolute;left:'+vx+'px;top:'+vy+'px;width:'+vw+'px;height:'+vh+'px;border:1.5px solid rgb(var(--c-accent) / 0.8);border-radius:3px;background:rgb(var(--c-accent) / 0.07)"></div>';
	mini.innerHTML=h;
	mini.setAttribute('data-bx',String(b.x)); mini.setAttribute('data-by',String(b.y)); mini.setAttribute('data-s',String(s));
}

// ── Palette ──────────────────────────────────────────────────────────
function renderPalette(){
	var box=document.getElementById('wg-pal-items');
	if(!box||box.childNodes.length) return;
	var h='';
	Object.keys(TYPES).forEach(function(k){
		var m=TYPES[k];
		h+='<div class="wg-pal-item rounded-lg border border-surface-border bg-surface-raised px-2.5 py-2 select-none" data-pal="'+k+'" data-pallabel="'+escX(m.label.toLowerCase())+'" title="'+escX(m.blurb)+'" style="border-left:3px solid '+m.color+'">'
		  +'<div class="flex items-center gap-2"><span class="w-5 h-5 rounded flex items-center justify-center text-[10px] font-black shrink-0" style="background:'+m.color+'22;color:'+m.color+'">'+escX(m.letter)+'</span>'
		  +'<div class="min-w-0"><div class="text-[11px] font-semibold leading-tight" style="color:'+m.color+'">'+escX(m.label)+'</div>'
		  +'<div class="text-[9px] text-muted leading-tight whitespace-nowrap lg:whitespace-normal">'+escX(m.short)+'</div></div></div></div>';
	});
	box.innerHTML=h;
	box.querySelectorAll('.wg-pal-item').forEach(function(it){
		it.addEventListener('pointerdown', function(e){
			MODE={t:'pal', type:it.getAttribute('data-pal'), startX:e.clientX, startY:e.clientY, moved:false};
			e.preventDefault();
		});
	});
}
window.wgPalFilter=function(q){
	q=(q||'').toLowerCase();
	document.querySelectorAll('#wg-pal-items .wg-pal-item').forEach(function(it){
		it.style.display=(!q||it.getAttribute('data-pallabel').indexOf(q)>=0)?'':'none';
	});
};
function palGhost(e,type){
	var g=document.getElementById('wg-pal-ghost');
	if(!g){
		g=document.createElement('div'); g.id='wg-pal-ghost';
		var m=typeMeta(type);
		g.className='rounded-lg border-2 bg-surface-raised px-3 py-2 text-[11px] font-semibold shadow-lg';
		g.style.borderColor=m.color; g.style.color=m.color;
		g.textContent=m.label;
		document.body.appendChild(g);
	}
	g.style.left=(e.clientX+10)+'px'; g.style.top=(e.clientY+8)+'px';
}

// ── Canvas interactions (single MODE state machine) ──────────────────
function bindCanvas(){
	var cv=document.getElementById('wg-canvas');
	if(!cv) return;
	cv.addEventListener('pointerdown', onCanvasDown);
	cv.addEventListener('pointerover', function(e){ if(MODE) return; var nn=e.target&&e.target.closest?e.target.closest('.wg-step'):null; if(nn) wgFocus(nn.getAttribute('data-id')); });
	cv.addEventListener('pointerout', function(e){ if(MODE) return; var nn=e.target&&e.target.closest?e.target.closest('.wg-step'):null; var to=e.relatedTarget&&e.relatedTarget.closest?e.relatedTarget.closest('.wg-step'):null; if(nn&&!to) wgClearFocus(); });
	cv.addEventListener('wheel', function(e){
		e.preventDefault();
		var r=cv.getBoundingClientRect();
		zoomAt(e.clientX-r.left, e.clientY-r.top, Math.exp(-e.deltaY*0.0016));
	}, {passive:false});
	var svg=document.getElementById('wg-edges');
	if(svg) svg.addEventListener('click', function(e){
		var del=e.target&&e.target.closest?e.target.closest('[data-del]'):null;
		if(del){ wgDeleteEdge(del.getAttribute('data-del')); return; }
		var t=e.target&&e.target.closest?e.target.closest('[data-eprop]'):null;
		if(t){ selEdge=t.getAttribute('data-ek'); selId=null; wgDrawEdges(); }
	});
	var mini=document.getElementById('wg-mini');
	if(mini) mini.addEventListener('pointerdown', function(e){
		e.stopPropagation(); e.preventDefault();
		var r=mini.getBoundingClientRect();
		var bx=parseFloat(mini.getAttribute('data-bx')), by=parseFloat(mini.getAttribute('data-by')), s=parseFloat(mini.getAttribute('data-s'));
		if(isNaN(s)||s<=0) return;
		var wx=(e.clientX-r.left)/s+bx, wy=(e.clientY-r.top)/s+by;
		WORLD.x=cv.clientWidth/2-wx*WORLD.k;
		WORLD.y=cv.clientHeight/2-wy*WORLD.k;
		applyWorld(); renderMini();
	});
}
function onCanvasDown(e){
	if(e.button!==undefined && e.button!==0) return;
	if(e.target.closest('#wg-mini')||e.target.closest('button')) return;
	var port=e.target.closest('.wg-port');
	if(port && port.getAttribute('data-port')!=='in'){
		MODE={t:'port', src:port.getAttribute('data-node'), port:port.getAttribute('data-port')};
		e.preventDefault(); return;
	}
	var node=e.target.closest('.wg-step');
	var life=e.target.closest('.wg-life');
	var el=node||life;
	if(el){
		var id=node?node.getAttribute('data-id'):life.getAttribute('data-life');
		var p=NODEPOS[id]||{x:0,y:0};
		MODE={t:'node', id:id, el:el, isStep:!!node, startX:e.clientX, startY:e.clientY, ox:p.x, oy:p.y, moved:false};
		e.preventDefault(); return;
	}
	var delB=e.target.closest&&e.target.closest('[data-del]');
	if(delB){ wgDeleteEdge(delB.getAttribute('data-del')); e.preventDefault(); return; }
	var wireHit=e.target.closest&&e.target.closest('.wg-hit, .wg-lbl');
	if(wireHit){ var nk=wireHit.getAttribute('data-ek'); if(selEdge!==nk){ selEdge=nk; selId=null; wgDrawEdges(); } e.preventDefault(); return; }
	MODE={t:'pan', startX:e.clientX, startY:e.clientY, ox:WORLD.x, oy:WORLD.y};
	var cv=document.getElementById('wg-canvas'); if(cv) cv.classList.add('wg-panning');
	e.preventDefault();
}
var _gRaf=false;
function gRedraw(){
	if(_gRaf) return;
	_gRaf=true;
	window.requestAnimationFrame(function(){ _gRaf=false; wgDrawEdges(); renderMini(); });
}
// htmx re-renders this fragment (and re-runs this script) after each save —
// swap document-level listeners instead of stacking duplicates that would
// double-fire against the new closure's window-bound functions.
if(window.__wgPM) document.removeEventListener('pointermove', window.__wgPM);
window.__wgPM=function(e){
	if(!MODE) return;
	if(MODE.t==='pan'){
		WORLD.x=MODE.ox+(e.clientX-MODE.startX);
		WORLD.y=MODE.oy+(e.clientY-MODE.startY);
		applyWorld(); renderMini();
	} else if(MODE.t==='node'){
		var dx=(e.clientX-MODE.startX)/WORLD.k, dy=(e.clientY-MODE.startY)/WORLD.k;
		if(!MODE.moved && Math.abs(dx)*WORLD.k<5 && Math.abs(dy)*WORLD.k<5) return;
		if(!MODE.moved){ MODE.moved=true; MODE.el.classList.add('wg-dragging'); }
		NODEPOS[MODE.id]={x:MODE.ox+dx, y:MODE.oy+dy};
		MODE.el.style.left=NODEPOS[MODE.id].x+'px';
		MODE.el.style.top=NODEPOS[MODE.id].y+'px';
		gRedraw();
	} else if(MODE.t==='port'){
		var g=document.getElementById('wg-ghost');
		if(g){
			var a=outAnchor(MODE.src,MODE.port);
			var w=worldFromClient(e.clientX,e.clientY);
			var dx2=Math.max(46,Math.abs(w.x-a.x)/2);
			g.setAttribute('d','M '+a.x+' '+a.y+' C '+(a.x+dx2)+' '+a.y+', '+(w.x-dx2)+' '+w.y+', '+w.x+' '+w.y);
			g.style.display='';
		}
	} else if(MODE.t==='pal'){
		MODE.moved=true;
		palGhost(e,MODE.type);
	}
};
document.addEventListener('pointermove', window.__wgPM);
if(window.__wgPU) document.removeEventListener('pointerup', window.__wgPU);
window.__wgPU=function(e){
	if(!MODE) return;
	var mode=MODE; MODE=null;
	var cv=document.getElementById('wg-canvas');
	if(cv) cv.classList.remove('wg-panning');
	if(mode.t==='node'){
		mode.el.classList.remove('wg-dragging');
		if(mode.moved){ persistPos(); gRedraw(); }
		else if(mode.isStep){ wgSelectStep(mode.id); }
		return;
	}
	if(mode.t==='port'){
		var g=document.getElementById('wg-ghost'); if(g) g.style.display='none';
		var hit=document.elementFromPoint(e.clientX,e.clientY);
		var tn=hit&&hit.closest?hit.closest('.wg-step'):null;
		var tf=hit&&hit.closest?hit.closest('.wg-life[data-life="__finish"]'):null;
		if(mode.src==='__start'){
			if(tn){
				var sid=tn.getAttribute('data-id'), si=stepIndex(sid);
				if(si>0){ pushHistory(); var it=STEPS.splice(si,1)[0]; STEPS.unshift(it); renderSteps(); }
			}
			return;
		}
		var srcStep=stepById(mode.src);
		if(!srcStep) return;
		var prop = mode.port==='reject'?'on_reject' : mode.port==='ask'?'ask' : mode.port==='approve'?'on_approve' : mode.port==='done'?'on_done' : 'on_next';
		var val=null;
		var allowSelf=(prop==='on_reject'); // reject→itself = bounded retry
		if(tn && (allowSelf || tn.getAttribute('data-id')!==mode.src)){ var ts=stepById(tn.getAttribute('data-id')); if(ts) val=ts.name; }
		else if(tf && prop!=='ask'){ val='end'; }
		if(val){
			pushHistory();
			srcStep.props=srcStep.props||{};
			if(prop==='on_next'||prop==='on_approve'){
				// Dragging a NEXT / APPROVE wire RE-POINTS it (replaces the
				// target) so you can rewire without deleting first. Build a
				// fan-out (several targets) from the step's side panel.
				var listSet=(prop==='on_next')?setNextList:setApproveList; listSet(srcStep,[val]);
			} else {
				srcStep.props[prop]=val;
			}
			selEdge=mode.src+'|'+prop+'|'+(val==='end'?'__finish':(tn?tn.getAttribute('data-id'):''));
			renderSteps();
		}
		return;
	}
	if(mode.t==='pal'){
		var pg=document.getElementById('wg-pal-ghost'); if(pg) pg.remove();
		if(!mode.moved){ wgAddStep(mode.type); return; }
		var hitCv=document.elementFromPoint(e.clientX,e.clientY);
		if(hitCv&&hitCv.closest&&hitCv.closest('#wg-canvas')){
			var wpt=worldFromClient(e.clientX,e.clientY);
			wgAddStepAt(mode.type, wpt.x-NODE_W/2, wpt.y-NODE_H/2);
		}
		return;
	}
};
document.addEventListener('pointerup', window.__wgPU);
if(window.__wgPC) document.removeEventListener('pointercancel', window.__wgPC);
window.__wgPC=function(){
	if(MODE&&MODE.t==='node'&&MODE.el) MODE.el.classList.remove('wg-dragging');
	var g=document.getElementById('wg-ghost'); if(g) g.style.display='none';
	var pg=document.getElementById('wg-pal-ghost'); if(pg) pg.remove();
	var cv=document.getElementById('wg-canvas'); if(cv) cv.classList.remove('wg-panning');
	MODE=null;
};
document.addEventListener('pointercancel', window.__wgPC);

window.wgAddStepAt=function(type,x,y){
	pushHistory();
	var id=newId();
	STEPS.push({id:id, type:type, name:uniqueName(type), props:{}});
	NODEPOS[id]={x:x, y:y};
	selId=id;
	renderSteps();
	persistPos();
};

// ── Right-hand editor panel ──────────────────────────────────────────
function renderPanel(){
	var p=document.getElementById('wg-panel');
	if(!p) return;
	if(window._panelHidden){ p.classList.add('hidden'); return; }
	if(tab!=='steps'){ p.classList.add('hidden'); return; }
	p.classList.remove('hidden');
	var n=stepById(selId);
	if(!n){
		p.innerHTML='<div class="p-6 text-center text-xs text-muted leading-relaxed">'
			+'<svg class="h-6 w-6 mx-auto mb-2 opacity-60 text-muted" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="3" width="18" height="18" rx="2"/><path d="M3 9h18M9 21V9"/></svg>'
			+'Select a step on the canvas to edit it here.<br>Drag any card to reorder the pipeline.</div>';
		return;
	}
	var m=typeMeta(n.type), i=stepIndex(n.id);
	var btn='p-1.5 rounded text-muted hover:text-ink hover:bg-surface-border disabled:opacity-25 disabled:hover:bg-transparent transition';
	var h='<div class="wg-fade p-4 space-y-3">';
	h+='<div class="flex items-center gap-2 pb-2 border-b border-surface-border">';
	h+='<span class="w-6 h-6 rounded-md flex items-center justify-center text-[11px] font-black shrink-0" style="background:'+m.color+'22;color:'+m.color+'">'+escX(m.letter)+'</span>';
	h+='<span class="flex-1 min-w-0 text-[12px] font-semibold text-ink truncate">Step '+(i+1)+' of '+STEPS.length+'</span>';
	h+='<button onclick="wgMoveStep(\''+n.id+'\',-1)" '+(i===0?'disabled':'')+' aria-label="Move up" title="Move up" class="'+btn+'">'+ICONS.up+'</button>';
	h+='<button onclick="wgMoveStep(\''+n.id+'\',1)" '+(i===STEPS.length-1?'disabled':'')+' aria-label="Move down" title="Move down" class="'+btn+'">'+ICONS.down+'</button>';
	h+='<button onclick="wgDuplicateStep(\''+n.id+'\')" aria-label="Duplicate step" title="Duplicate" class="'+btn+'">'+ICONS.copy+'</button>';
	h+='<button onclick="wgDeleteStep(\''+n.id+'\')" aria-label="Delete step" title="Delete" class="p-1.5 rounded text-muted hover:text-rose-400 hover:bg-surface-border transition">'+ICONS.trash+'</button>';
	h+='<button onclick="wgDeselect()" aria-label="Close panel" title="Close" class="'+btn+' ml-1">'
	  +'<svg class="h-3.5 w-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round"><path d="M18 6 6 18M6 6l12 12"/></svg></button>';
	h+='</div>';
	h+='<div id="wg-panel-body"></div>';
	h+='</div>';
	p.innerHTML=h;
	renderStageProps(n, document.getElementById('wg-panel-body'));
}

// ── Add / move / duplicate / delete ──────────────────────────────────
window.wgAddStep=function(type){
	// Click-to-append: place right of the last step (or at the spine start).
	var pos;
	if(STEPS.length){
		var lp=NODEPOS[STEPS[STEPS.length-1].id]||spinePos(STEPS.length-1);
		pos={x:lp.x+NODE_W+90, y:lp.y};
	} else {
		pos=spinePos(0);
	}
	wgAddStepAt(type,pos.x,pos.y);
	wgCenterOn(STEPS[STEPS.length-1].id);
};
function uniqueName(base){
	var n=base, i=2, taken={};
	STEPS.forEach(function(s){ taken[s.name]=true; });
	while(taken[n]){ n=base+'-'+i; i++; }
	return n;
}
window.wgMoveStep=function(id,dir){
	var idx=stepIndex(id); if(idx<0) return;
	var to=idx+dir; if(to<0||to>=STEPS.length) return;
	pushHistory();
	var item=STEPS.splice(idx,1)[0];
	STEPS.splice(to,0,item);
	renderSteps();
	if(selId){ var b=document.getElementById('wg-body-'+selId); if(b) renderStageProps(stepById(selId), b); }
};
window.wgDuplicateStep=function(id){
	var src=stepById(id); if(!src) return;
	pushHistory();
	var idx=stepIndex(id);
	var copy={id:newId(), type:src.type, name:uniqueName(src.name), props:clone(src.props||{})};
	STEPS.splice(idx+1,0,copy);
	var sp=NODEPOS[id];
	if(sp) NODEPOS[copy.id]={x:sp.x+36, y:sp.y+36};
	selId=copy.id;
	renderSteps();
	persistPos();
};
window.wgDeleteStep=function(id){
	pushHistory();
	STEPS=STEPS.filter(function(s){ return s.id!==id; });
	if(selId===id) selId=null;
	renderSteps();
};
// Delete one wire by its edge key. on_next / on_approve are fan-out lists —
// remove only this wire's target; every other routing prop clears outright.
window.wgDeleteEdge=function(ek){
	if(!ek) return;
	var parts=ek.split('|');
	var st=stepById(parts[0]);
	if(st&&st.props&&parts[1]&&parts[1]!=='seq'){
		pushHistory();
		if(parts[1]==='on_next'||parts[1]==='on_approve'){
			var dstName=(parts[2]==='__finish')?'end':(stepById(parts[2])?stepById(parts[2]).name:'');
			var lg=(parts[1]==='on_next')?nextList:approveList;
			var ls=(parts[1]==='on_next')?setNextList:setApproveList;
			ls(st, lg(st.props).filter(function(x){ return x!==dstName; }));
		} else {
			delete st.props[parts[1]];
		}
	}
	selEdge=null;
	renderSteps();
};

// ── Per-step field editor (renders into the panel body) ───────────────
function renderStageProps(n, el){
	if(!el || !n) return;
	var p=n.props||{};
	var ob=' onblur="wgPropBlur()"';
	// Routing target lists, computed up front so any branch can use them
	// (function declarations hoist; this assignment must come first).
	var others=STEPS.filter(function(x){ return x.id!==n.id; }).map(function(x){ return x.name; });
	var allNames=STEPS.map(function(x){ return x.name; }); // incl. self — reject→self = retry
	var h='<div class="space-y-3">';

	h+='<div class="grid grid-cols-2 gap-2">';
	h+='<div>'+fieldLabel('Name')
	  +'<input type="text" value="'+escX(n.name)+'" oninput="wgProp(\'name\',this.value)"'+ob+' class="'+inputCls()+' font-mono"></div>';
	h+='<div>'+fieldLabel('Type')
	  +'<select onchange="wgProp(\'type\',this.value)" class="'+inputCls()+'">';
	Object.keys(TYPES).forEach(function(k){
		h+='<option value="'+k+'"'+(n.type===k?' selected':'')+'>'+escX(TYPES[k].label.toLowerCase())+' — '+escX(TYPES[k].short.toLowerCase())+'</option>';
	});
	h+='</select></div></div>';

	h+=typeHelpBlock(n.type);

	if(n.type==='executor'){
		h+='<div>'+fieldLabel('Built-in capability')
		  +'<select onchange="pushHistory();wgProp(\'do\',this.value)" class="'+inputCls()+'">'
		  +'<option value=""'+((!p.do)?' selected':'')+'>— none (run the task below) —</option>'
		  +'<option value="open_pr"'+(p.do==='open_pr'?' selected':'')+'>open_pr — open / update the PR</option>'
		  +'</select>'+fieldHint('open_pr ships the change with goon\'s git host + PR template instead of a freeform task.')+'</div>';
		h+='<div>'+fieldLabel('Task (when no built-in)')
		  +'<textarea rows="5" oninput="wgProp(\'task\',this.value)"'+ob+' class="'+inputCls()+' font-mono text-[11px] resize-y leading-relaxed">'+escH(p.task||'')+'</textarea>'+varHint()
		  +fieldHint('Empty task + no built-in = the default executor: it triages the ticket into a plan and implements it. Add a line \'ASK: your question\' to let it consult an analyst.')+'</div>';
		h+='<div>'+fieldLabel('Max steps (0 = default)')
		  +'<input type="number" min="0" value="'+escX(p.max_steps||'')+'" oninput="wgProp(\'max_steps\',this.value)"'+ob+' class="'+inputCls()+'"></div>';
	} else if(n.type==='reviewer'){
		h+='<div>'+fieldLabel('Mode')
		  +'<select onchange="pushHistory();wgProp(\'mode\',this.value)" class="'+inputCls()+'">'
		  +'<option value="human"'+((!p.mode||p.mode==='human')?' selected':'')+'>human — pause, show a person the change, approve / reject</option>'
		  +'<option value="llm"'+(p.mode==='llm'?' selected':'')+'>llm — an automated model decides</option>'
		  +'</select></div>';
		h+='<div>'+fieldLabel('Review task — what to check')
		  +'<textarea rows="4" oninput="wgProp(\'task\',this.value)"'+ob+' class="'+inputCls()+' font-mono text-[11px] resize-y leading-relaxed">'+escH(p.task||'')+'</textarea>'+varHint()
		  +fieldHint('Approve advances (wire the APPROVE port); reject loops back (wire REJECT to a loop).')+'</div>';
	} else if(n.type==='notify'){
		h+='<div>'+fieldLabel('Message')
		  +'<textarea rows="3" oninput="wgProp(\'message\',this.value)"'+ob+' class="'+inputCls()+' font-mono text-[11px] resize-y leading-relaxed">'+escH(p.message||'')+'</textarea>'+varHint()
		  +fieldHint('Sent to your Telegram channel. Set On error → continue to make it optional.')+'</div>';
	} else if(n.type==='loop'){
		h+='<div class="grid grid-cols-2 gap-2">';
		h+='<div>'+fieldLabel('Max loops')
		  +'<input type="number" min="1" value="'+escX(p.max_loops||'')+'" placeholder="3" oninput="wgProp(\'max_loops\',this.value)"'+ob+' class="'+inputCls()+'"></div>';
		h+=routingSelect('on_done', p.on_done||'', 'when done → go to', others);
		h+='</div>';
		h+=fieldHint('Wire the LOOP port back to the step that starts the loop body. Each arrival here counts one iteration; after max loops the flow exits via "when done" (or the next step in list order).');
	} else {
		h+='<div>'+fieldLabel('Prompt')
		  +'<textarea rows="5" oninput="wgProp(\'prompt\',this.value)"'+ob+' class="'+inputCls()+' font-mono text-[11px] resize-y leading-relaxed">'+escH(p.prompt||'')+'</textarea>'+varHint()+'</div>';
		h+='<div>'+fieldLabel('System (optional)')
		  +'<textarea rows="2" oninput="wgProp(\'system\',this.value)"'+ob+' class="'+inputCls()+' font-mono text-[11px] resize-y leading-relaxed">'+escH(p.system||'')+'</textarea></div>';
		h+='<label class="flex items-center gap-2 cursor-pointer">'
		  +'<input type="checkbox" '+(p.json_mode?'checked':'')+' onchange="pushHistory();wgProp(\'json_mode\',this.checked)" class="rounded border-surface-border bg-surface-sunken text-accent focus:ring-accent/40">'
		  +'<span class="text-[11px] text-ink">JSON mode — force a single JSON object reply</span></label>';
		h+='<div class="grid grid-cols-3 gap-2">';
		h+='<div>'+fieldLabel('Temperature')+'<input type="number" step="0.1" min="0" max="2" value="'+escX(p.temperature!=null?p.temperature:'')+'" oninput="wgProp(\'temperature\',this.value)"'+ob+' class="'+inputCls()+'"></div>';
		h+='<div>'+fieldLabel('Max tokens')+'<input type="number" min="0" value="'+escX(p.max_tokens||'')+'" oninput="wgProp(\'max_tokens\',this.value)"'+ob+' class="'+inputCls()+'"></div>';
		h+='<div>'+fieldLabel('Output key')+'<input type="text" value="'+escX(p.output||'')+'" oninput="wgProp(\'output\',this.value)"'+ob+' placeholder="= name" class="'+inputCls()+' font-mono"></div>';
		h+='</div>';
	}

	// Advanced (collapsed): branching + model override + run-if/repeat/on_error.
	h+='<details class="border-t border-surface-border pt-2">';
	h+='<summary class="cursor-pointer text-[11px] font-semibold text-muted hover:text-ink select-none">Advanced — branching, model, conditions</summary>';
	h+='<div class="space-y-3 pt-3">';

	function routingSelect(key,val,label,list){
		var s='<div><label class="block text-[10px] text-muted mb-0.5">'+label+'</label>'
		  +'<select onchange="pushHistory();wgProp(\''+key+'\',this.value)" class="'+inputCls()+'">'
		  +'<option value=""'+(val===''?' selected':'')+'>— default (next in list) —</option>';
		(list||others).forEach(function(sn){ s+='<option value="'+escX(sn)+'"'+(val===sn?' selected':'')+'>'+escX(sn)+'</option>'; });
		s+='<option value="end"'+(val==='end'?' selected':'')+'>end pipeline</option>';
		return s+'</select></div>';
	}
	function nextMulti(label){
		var cur=nextList(p);
		var s='<div><label class="block text-[10px] text-muted mb-0.5">'+label+'</label>'
		  +'<div class="rounded-md border border-surface-border bg-surface-sunken px-2 py-1.5 space-y-1 max-h-28 overflow-y-auto">';
		others.concat(['end']).forEach(function(sn){
			var on=cur.indexOf(sn)>=0;
			s+='<label class="flex items-center gap-2 cursor-pointer text-[11px] text-ink">'
			  +'<input type="checkbox" '+(on?'checked':'')+' onchange="wgNextToggle(\''+escX(sn)+'\',this.checked)" class="rounded border-surface-border bg-surface text-accent focus:ring-accent/40">'
			  +'<span class="font-mono truncate">'+escX(sn)+'</span></label>';
		});
		s+='</div>'+fieldHint('None checked = next step in list order. Check several to fan out (branches run one after another). Or just drag wires from the port.');
		return s+'</div>';
	}
	function approveMulti(label){
		var cur=approveList(p);
		var s='<div><label class="block text-[10px] text-muted mb-0.5">'+label+'</label>'
		  +'<div class="rounded-md border border-surface-border bg-surface-sunken px-2 py-1.5 space-y-1 max-h-28 overflow-y-auto">';
		others.concat(['end']).forEach(function(sn){
			var on=cur.indexOf(sn)>=0;
			s+='<label class="flex items-center gap-2 cursor-pointer text-[11px] text-ink">'
			  +'<input type="checkbox" '+(on?'checked':'')+' onchange="wgApproveToggle(\''+escX(sn)+'\',this.checked)" class="rounded border-surface-border bg-surface text-accent focus:ring-accent/40">'
			  +'<span class="font-mono truncate">'+escX(sn)+'</span></label>';
		});
		s+='</div>'+fieldHint('Where to go when approved. Check several to fan out — e.g. open_pr + notify.');
		return s+'</div>';
	}
	var analystNames=STEPS.filter(function(x){ return x.type==='analyst'&&x.id!==n.id; }).map(function(x){ return x.name; });
	h+='<div class="text-[10px] uppercase tracking-widest text-muted font-semibold">Branching</div>';
	if(n.type==='loop'){
		h+=nextMulti('loop target(s) — the loop body');
	} else if(n.type==='reviewer'){
		h+=approveMulti('on approve → go to (fan-out)');
		h+='<div class="grid grid-cols-2 gap-2">';
		h+=routingSelect('on_reject', p.on_reject||'', 'on reject → go to', allNames);
		h+=routingSelect('ask', p.ask||'', 'ask analyst (optional)', analystNames);
		h+='</div>';
		h+='<div>'+fieldLabel('Max loops (default 3)')+'<input type="number" min="0" value="'+escX(p.max_loops||'')+'" oninput="wgProp(\'max_loops\',this.value)"'+ob+' class="'+inputCls()+'"></div>';
	} else if(n.type==='analyst'){
		h+=fieldHint('An analyst is ask-only — it has no outgoing routes. Other nodes point their ASK port at it; it answers and they re-run with the reply.');
	} else {
		h+=nextMulti('on success → go to');
		h+='<div class="grid grid-cols-2 gap-2">';
		h+=routingSelect('ask', p.ask||'', 'ask analyst (optional)', analystNames);
		h+='<div>'+fieldLabel('Max loops (default 3)')+'<input type="number" min="0" value="'+escX(p.max_loops||'')+'" oninput="wgProp(\'max_loops\',this.value)"'+ob+' class="'+inputCls()+'"></div>';
		h+='</div>';
	}

	if(n.type==='analyst'||n.type==='executor'||n.type==='reviewer'){
		h+='<div class="text-[10px] uppercase tracking-widest text-muted font-semibold pt-1">Model override</div>';
		h+='<div class="grid grid-cols-2 gap-2">';
		h+='<div><label class="block text-[10px] text-muted mb-0.5">Provider</label>'
		  +'<select onchange="pushHistory();wgProp(\'provider\',this.value)" class="'+inputCls()+'">'
		  +'<option value=""'+((!p.provider)?' selected':'')+'>— default —</option>'
		  +'<option value="openai"'+(p.provider==='openai'?' selected':'')+'>openai</option>'
		  +'<option value="anthropic"'+(p.provider==='anthropic'?' selected':'')+'>anthropic</option>'
		  +'<option value="gemini"'+(p.provider==='gemini'?' selected':'')+'>gemini</option>'
		  +'<option value="ollama"'+(p.provider==='ollama'?' selected':'')+'>ollama</option>'
		  +'</select></div>';
		h+='<div><label class="block text-[10px] text-muted mb-0.5">Model</label>'
		  +'<input type="text" value="'+escX(p.model||'')+'" oninput="wgProp(\'model\',this.value)"'+ob+' placeholder="e.g. gpt-4o" class="'+inputCls()+' font-mono text-[11px]"></div>';
		h+='</div>';
	}

	h+='<div class="text-[10px] uppercase tracking-widest text-muted font-semibold pt-1">Conditions</div>';
	h+='<div>'+fieldLabel('Run if')
	  +'<input type="text" value="'+escX(p.if||'')+'" oninput="wgProp(\'if\',this.value)"'+ob+' placeholder="{{ne (get .Stages.x &quot;tier&quot;) &quot;cold&quot;}}" class="'+inputCls()+' font-mono text-[11px]">'
	  +fieldHint('Skip this step unless the expression is truthy. Empty = always run.')+'</div>';
	h+='<div class="grid grid-cols-2 gap-2">';
	h+='<div>'+fieldLabel('Repeat')+'<input type="number" min="0" value="'+escX(p.repeat||'')+'" oninput="wgProp(\'repeat\',this.value)"'+ob+' class="'+inputCls()+'"></div>';
	h+='<div>'+fieldLabel('On error')
	  +'<select onchange="pushHistory();wgProp(\'on_error\',this.value)" class="'+inputCls()+'">'
	  +'<option value="fail"'+((!p.on_error||p.on_error==='fail')?' selected':'')+'>fail</option>'
	  +'<option value="warn"'+(p.on_error==='warn'?' selected':'')+'>warn</option>'
	  +'<option value="continue"'+(p.on_error==='continue'?' selected':'')+'>continue</option>'
	  +'</select></div>';
	h+='</div>';
	h+='</div></details>';

	h+='</div>';
	el.innerHTML=h;
}

window.wgProp=function(key,val){
	var n=stepById(selId); if(!n) return;
	setDirty(true);
	if(key==='name'){
		n.name=val;
		// Live-update the canvas node label without rebuilding the editor
		// (rebuilding would steal focus from the name field).
		var t=document.querySelector('[data-stepname="'+n.id+'"]');
		if(t) t.textContent=val||'(unnamed)';
		return;
	}
	if(key==='type'){
		pushHistory();
		n.type=val;
		renderSteps();
		var b=document.getElementById('wg-body-'+n.id); if(b) renderStageProps(n,b);
		return;
	}
	n.props=n.props||{};
	n.props[key]=val;
	// Routing changed → branch curves on the canvas must follow.
	if(key==='on_next'||key==='on_approve'||key==='on_reject'||key==='ask'||key==='on_done') wgDrawEdges();
};
window.wgPropBlur=function(){ pushHistory(); };
// Toggle one on_next fan-out target from the panel's checkbox list.
// "end" is exclusive — checking it clears the rest and vice versa.
window.wgNextToggle=function(nm,on){
	var n=stepById(selId); if(!n) return;
	pushHistory();
	var arr=nextList(n.props).filter(function(x){ return x!==nm; });
	if(on){
		if(nm==='end'){ arr=['end']; }
		else { arr=arr.filter(function(x){ return x!=='end'; }); arr.push(nm); }
	}
	setNextList(n,arr);
	renderSteps();
};
// Toggle one on_approve fan-out target (reviewer) from the panel checkbox.
window.wgApproveToggle=function(nm,on){
	var n=stepById(selId); if(!n) return;
	pushHistory();
	var arr=approveList(n.props).filter(function(x){ return x!==nm; });
	if(on){
		if(nm==='end'){ arr=['end']; }
		else { arr=arr.filter(function(x){ return x!=='end'; }); arr.push(nm); }
	}
	setApproveList(n,arr);
	renderSteps();
};

// ── Settings tab ─────────────────────────────────────────────────────
function renderSettings(el){
	if(!el) return;
	var d=_defaults||{};
	var labelsStr=(CFG.extra_labels||[]).join(', ');
	var h='<div class="max-w-xl mx-auto space-y-3">';
	h+='<div>'+fieldLabel('Name')+'<input type="text" value="'+escX(CFG.name||'')+'" oninput="wgSet(\'name\',this.value)" placeholder="'+escX(d.name||'default')+'" class="'+inputCls()+'"></div>';
	h+='<div>'+fieldLabel('Description')+'<textarea rows="2" oninput="wgSet(\'description\',this.value)" class="'+inputCls()+' resize-y leading-relaxed">'+escH(CFG.description||'')+'</textarea></div>';
	h+='<div>'+fieldLabel('Branch prefix')+'<input type="text" value="'+escX(CFG.branch_prefix||'')+'" oninput="wgSet(\'branch_prefix\',this.value)" placeholder="'+escX(d.branch_prefix||'goon/')+'" class="'+inputCls()+' font-mono"></div>';
	h+='<label class="flex items-center gap-2 cursor-pointer"><input type="checkbox" '+(CFG.auto_approve?'checked':'')+' onchange="wgSet(\'auto_approve\',this.checked)" class="rounded border-surface-border bg-surface-sunken text-accent focus:ring-accent/40"><span class="text-[11px] text-ink">Auto-approve — skip approval gates, run unattended</span></label>';
	h+='<div class="grid grid-cols-2 gap-2">';
	h+='<div>'+fieldLabel('Test command')+'<input type="text" value="'+escX(CFG.test_command||'')+'" oninput="wgSet(\'test_command\',this.value)" placeholder="auto-detect" class="'+inputCls()+' font-mono text-[11px]"></div>';
	h+='<div>'+fieldLabel('Verify runs')+'<input type="number" min="0" value="'+escX(CFG.verify_runs||'')+'" oninput="wgSet(\'verify_runs\',this.value)" placeholder="'+escX(d.verify_runs||3)+'" class="'+inputCls()+'"></div>';
	h+='</div>';
	h+='<div class="border-t border-surface-border pt-3 space-y-3">';
	h+='<div class="text-[9px] uppercase tracking-widest text-muted font-semibold">Pull Request</div>';
	h+='<div>'+fieldLabel('PR title template')+'<input type="text" value="'+escX(CFG.pr_title_template||'')+'" oninput="wgSet(\'pr_title_template\',this.value)" placeholder="empty = skip PR" class="'+inputCls()+' font-mono text-[11px]"></div>';
	h+='<div>'+fieldLabel('PR body template')+'<textarea rows="3" oninput="wgSet(\'pr_body_template\',this.value)" class="'+inputCls()+' font-mono text-[11px] resize-y leading-relaxed">'+escH(CFG.pr_body_template||'')+'</textarea></div>';
	h+='<div>'+fieldLabel('Extra labels (comma-separated)')+'<input type="text" value="'+escX(labelsStr)+'" oninput="wgSetLabels(this.value)" placeholder="goon, auto" class="'+inputCls()+' font-mono text-[11px]"></div>';
	h+='</div>';
	h+='<div class="border-t border-surface-border pt-3 space-y-2">';
	h+='<div class="text-[9px] uppercase tracking-widest text-muted font-semibold">Hooks — shell commands, one per line</div>';
	var hooks=CFG.hooks||{};
	HOOK_PHASES.forEach(function(ph){
		var cmds=(hooks[ph]||[]).join('\n');
		h+='<div>'+fieldLabel(ph)+'<textarea rows="'+(cmds?2:1)+'" oninput="wgSetHook(\''+ph+'\',this.value)" placeholder="(none)" class="'+inputCls()+' font-mono text-[11px] resize-y">'+escH(cmds)+'</textarea></div>';
	});
	h+='</div></div>';
	el.innerHTML=h;
}
window.wgSet=function(key,val){
	setDirty(true);
	if(key==='verify_runs'){ var n=parseInt(val,10); if(isNaN(n)){ delete CFG.verify_runs; } else { CFG.verify_runs=n; } return; }
	if(key==='auto_approve'){ CFG.auto_approve=!!val; return; }
	if(val===''||val==null){ delete CFG[key]; } else { CFG[key]=val; }
};
window.wgSetLabels=function(val){ setDirty(true); var a=val.split(',').map(function(s){return s.trim();}).filter(Boolean); if(a.length){ CFG.extra_labels=a; } else { delete CFG.extra_labels; } };
window.wgSetHook=function(ph,val){ setDirty(true); CFG.hooks=CFG.hooks||{}; var a=val.split('\n').map(function(s){return s.trim();}).filter(Boolean); if(a.length){ CFG.hooks[ph]=a; } else { delete CFG.hooks[ph]; } if(Object.keys(CFG.hooks).length===0) delete CFG.hooks; };

// ── Templates ────────────────────────────────────────────────────────
function populateTemplateSelect(){
	var sel=document.getElementById('wg-template-select'); if(!sel) return;
	var h='<option value="">start from template…</option>';
	(_starters||[]).forEach(function(t){ h+='<option value="'+escX(t.key)+'">'+escX(t.label)+'</option>'; });
	sel.innerHTML=h;
}
window.wgLoadTemplate=function(key){
	if(!key) return;
	var t=(_starters||[]).find(function(x){ return x.key===key; });
	if(!t) return;
	if(!confirm('Load template "'+t.label+'"? This replaces the current steps + settings (nothing is saved until you click Save).')) return;
	pushHistory();
	loadConfig(clone(t.config));
};

// ── Serialize / load ─────────────────────────────────────────────────
function stepToStage(n){
	var p=n.props||{};
	var s={ name:n.name, type:n.type };
	if(p.if) s.if=p.if;
	var rep=parseInt(p.repeat,10); if(!isNaN(rep)&&rep>0) s.repeat=rep;
	if(p.on_error&&p.on_error!=='fail') s.on_error=p.on_error;
	// on_next: single target serializes as a string (back-compat), a
	// fan-out list as an array (backend StringList accepts both).
	var nx=nextList(p);
	if(nx.length===1){ s.on_next=nx[0]; } else if(nx.length>1){ s.on_next=nx; }
	if(p.reject_if) s.reject_if=p.reject_if;
	if(p.on_reject) s.on_reject=p.on_reject;
	// on_approve: single target serializes as a string, fan-out as an array.
	var ap=approveList(p);
	if(ap.length===1){ s.on_approve=ap[0]; } else if(ap.length>1){ s.on_approve=ap; }
	if(p.ask) s.ask=p.ask;
	if(p.on_done)   s.on_done=p.on_done;
	var ml=parseInt(p.max_loops,10); if(!isNaN(ml)&&ml>0) s.max_loops=ml;
	if(p.provider) s.provider=p.provider;
	if(p.model)    s.model=p.model;
	if(n.type==='executor'){
		if(p.do){ s.do=p.do; } else if(p.task){ s.task=p.task; }
		var ms=parseInt(p.max_steps,10); if(!isNaN(ms)&&ms>0) s.max_steps=ms;
	} else if(n.type==='reviewer'){
		if(p.task) s.task=p.task;
		if(p.mode) s.mode=p.mode;
	} else if(n.type==='notify'){
		if(p.message) s.message=p.message;
	} else if(n.type==='loop'){
		// routing-only node: everything it needs is already serialized above
	} else {
		// analyst (and any prompt-style role)
		if(p.prompt) s.prompt=p.prompt;
		if(p.system) s.system=p.system;
		if(p.json_mode) s.json_mode=true;
		var tmp=parseFloat(p.temperature); if(!isNaN(tmp)&&tmp!==0) s.temperature=tmp;
		var mt=parseInt(p.max_tokens,10); if(!isNaN(mt)&&mt>0) s.max_tokens=mt;
		if(p.output) s.output=p.output;
		if(p.urls&&p.urls.length) s.urls=p.urls;
	}
	return s;
}
function buildConfig(includeStages){
	var c=clone(CFG)||{};
	if(includeStages){
		c.stages=STEPS.map(stepToStage);
		// Bake current node positions into the config so a shared workflow.json
		// opens with the same view for everyone.
		var lay={};
		STEPS.forEach(function(s){ var p=NODEPOS[s.id]; if(p&&s.name) lay[s.name]={x:Math.round(p.x),y:Math.round(p.y)}; });
		['__start','__finish'].forEach(function(k){ if(NODEPOS[k]) lay[k]={x:Math.round(NODEPOS[k].x),y:Math.round(NODEPOS[k].y)}; });
		c.layout=lay;
	} else { delete c.stages; delete c.layout; }
	return c;
}
function loadConfig(cfg){
	CFG=cfg||{};
	var stages=(CFG.stages&&CFG.stages.length)?CFG.stages:_seed;
	seededFromBuiltin=!(CFG.stages&&CFG.stages.length);
	STEPS=(stages||[]).map(function(s){ return {id:newId(), type:s.type||'executor', name:s.name||('step-'+_seq), props:clone(s)}; });
	selId=null; selEdge=null; NODEPOS={}; tab='steps';
	renderAll();
	wgHome();
	var banner=document.getElementById('wg-builtin-banner');
	var revert=document.getElementById('wg-revert-btn');
	if(banner) banner.classList.toggle('hidden', !seededFromBuiltin);
	if(revert) revert.classList.toggle('hidden', !seededFromBuiltin);
}

// ── Save ─────────────────────────────────────────────────────────────
function flashErr(t){ var m=document.getElementById('wg-save-msg'); if(m){ m.textContent=t; m.className='text-xs text-rose-700 dark:text-rose-400'; } }
function postConfig(c){
	var body=JSON.stringify(c,null,2);
	var msg=document.getElementById('wg-save-msg');
	if(msg){ msg.textContent='saving…'; msg.className='text-xs text-muted'; }
	return fetch('/api/workflow/save',{
		method:'POST', headers:{'Content-Type':'application/x-www-form-urlencoded'},
		body:'path='+encodeURIComponent(_savePath)+'&body='+encodeURIComponent(body)
	}).then(function(r){ return r.text(); }).then(function(resp){
		if(resp.indexOf('✗')!==-1){
			var t=(resp.replace(/<[^>]*>/g,'')||'save failed').trim();
			if(msg){ msg.textContent=t; msg.className='text-xs text-rose-700 dark:text-rose-400'; }
			return false;
		}
		document.body.dispatchEvent(new CustomEvent('workflowConfigChanged'));
		document.body.dispatchEvent(new CustomEvent('workflowsChanged'));
		setDirty(false);
		if(msg){ msg.textContent='✓ saved'; msg.className='text-xs text-emerald-700 dark:text-emerald-400'; }
		setTimeout(wgClose, 600);
		return true;
	}).catch(function(e){ if(msg){ msg.textContent='error: '+e; msg.className='text-xs text-rose-700 dark:text-rose-400'; } return false; });
}
window.wgSave=function(){
	if(STEPS.length===0){
		if(!confirm('No steps defined. Save as the built-in pipeline (settings + hooks only)?')) return;
		postConfig(buildConfig(false));
		return;
	}
	var seen={};
	for(var i=0;i<STEPS.length;i++){
		var n=STEPS[i], nm=(n.name||'').trim();
		if(!nm){ flashErr('step '+(i+1)+': name is required'); openStep(n.id); return; }
		if(seen[nm]){ flashErr('duplicate step name "'+nm+'"'); openStep(n.id); return; }
		seen[nm]=true;
		var pp=n.props||{};
		if(n.type==='notify'&&!(pp.message||'').trim()){ flashErr('step "'+nm+'": message is required'); openStep(n.id); return; }
		if(n.type==='reviewer'&&approveList(pp).filter(function(x){return x!=='end';}).length===0&&!(pp.on_reject||'').trim()){ flashErr('step "'+nm+'": a reviewer needs APPROVE and/or REJECT wired'); openStep(n.id); return; }
		if(n.type==='loop'&&nextList(pp).filter(function(x){return x!=='end';}).length===0&&!(pp.on_done||'').trim()){ flashErr('step "'+nm+'": wire the LOOP port to the loop body (or set "when done")'); openStep(n.id); return; }
	}
	try{ persistPos(); window._posSnap=localStorage.getItem(posStoreKey()); }catch(err){}
	postConfig(buildConfig(true));
};
function openStep(id){ if(tab!=='steps'){ tab='steps'; } selId=id; renderAll(); wgCenterOn(id); }
window.wgRevertBuiltin=function(){
	if(!confirm('Revert to goon\'s default role-graph? Saves your settings + hooks but DROPS the custom steps, restoring execute → reviewer (human) → open_pr + notify.')) return;
	postConfig(buildConfig(false));
};

// ── Open / close ─────────────────────────────────────────────────────
window.wgClose=function(){
	// Guard against losing work: anything edited since open/save asks first.
	if(_dirty && !confirm('Discard unsaved changes? Your edits are lost unless you Save first.')) return;
	setDirty(false);
	// Cancel reverts layout changes (tidy/drag) made this session.
	try{ if(window._posSnap!=null){ localStorage.setItem(posStoreKey(), window._posSnap); } else { localStorage.removeItem(posStoreKey()); } }catch(err){}
	var ov=document.getElementById('wf-graph-overlay');
	if(ov) ov.classList.add('hidden');
	var lbl=document.getElementById('wf-editor-toggle-label');
	if(lbl) lbl.textContent='edit pipeline';
};
window.goonWfEditorToggle=function(){
	var ov=document.getElementById('wf-graph-overlay');
	if(!ov) return;
	ov.classList.remove('hidden');
	var lbl=document.getElementById('wf-editor-toggle-label');
	if(lbl) lbl.textContent='close editor';
	_history=[]; updUndoBtn();
	setDirty(false);
	populateTemplateSelect();
	try{ window._posSnap=localStorage.getItem(posStoreKey()); }catch(err){ window._posSnap=null; }
	loadConfig(hasKeys(_cfg)?clone(_cfg):clone(_defaults));
};
window.goonWorkflowEditorToggle=window.goonWfEditorToggle;

if(window.__wgKeys) document.removeEventListener('keydown', window.__wgKeys);
window.__wgKeys=function(e){
	var ov=document.getElementById('wf-graph-overlay');
	if(!ov||ov.classList.contains('hidden')) return;
	var tag=e.target&&e.target.tagName?e.target.tagName:'';
	var typing=(tag==='INPUT'||tag==='TEXTAREA'||tag==='SELECT');
	if(e.key==='Escape'){
		if(selEdge){ selEdge=null; wgDrawEdges(); return; }
		wgClose();
	}
	else if((e.key==='Delete'||e.key==='Backspace') && !typing && (selEdge||selId)){
		// Delete the selected wire; if no wire is selected, delete the
		// selected node. Both work from the keyboard.
		e.preventDefault();
		if(selEdge){ wgDeleteEdge(selEdge); }
		else if(selId){ wgDeleteStep(selId); }
	}
	else if((e.ctrlKey||e.metaKey)&&(e.key==='z'||e.key==='Z')){ e.preventDefault(); wgUndo(); }
	else if((e.ctrlKey||e.metaKey)&&(e.key==='s'||e.key==='S')){ e.preventDefault(); wgSave(); }
};
document.addEventListener('keydown', window.__wgKeys);

})();
</script>`, cfgJSON, defaultsJSON, seedJSON, startersJSON, saveTarget)
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
	// Allowlist the destination — the active pipeline, the default, or any
	// automation file directly under the automations dir (slug-named JSON).
	isAuto := filepath.Dir(target) == workflow.AutomationsDir()
	allowed := target == s.opts.WorkflowPath || target == workflow.DefaultConfigFilePath() || isAuto
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
	// Semantic validation — same rules the daemon enforces at load time
	// (unique stage names, valid types, required per-type fields). Without
	// this a config that parses as JSON but has e.g. duplicate stage names
	// or an agent stage missing its task would save fine here and then
	// silently break the daemon on the next poll. Catch it now, in the
	// editor, where the user can fix it.
	if err := probe.Validate(); err != nil {
		fragErr(w, "invalid workflow: "+err.Error())
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
	// An automation save must NOT switch the active pipeline — just refresh the
	// automations fleet and leave s.opts.Workflow pointing at the board pipeline.
	if isAuto {
		w.Header().Set("HX-Trigger", "automationsChanged")
		s.events.Publish("automationsChanged")
		fragOK(w, fmt.Sprintf("saved automation — runs on its schedule (%s)", filepath.Base(target)))
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

// ── Scheduled automations (fleet + create + run / toggle / delete) ──────────
//
// Automations are extra workflows that run on a timer instead of off the
// board — digests, health checks, anything. They live one-JSON-per-file under
// the automations dir; the daemon's minute scheduler fires the due ones. This
// fleet UI lets the user create, run-now, enable/pause, edit (in the same
// island editor), and delete them without hand-writing JSON.

// fragAutomations lazy-loads the automations fleet + create panel into the
// Workflows tab.
func (s *Server) fragAutomations(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	s.renderAutomations(w)
}

// renderAutomations writes the fleet + create panel HTML. Shared by
// fragAutomations and the mutating handlers (which re-render in place), so the
// list always reflects the latest on-disk state.
func (s *Server) renderAutomations(w http.ResponseWriter) {
	autos := workflow.LoadAutomations()
	var b strings.Builder
	b.WriteString(`<div id="automations" class="rounded-xl border border-surface-border bg-surface-raised shadow-card">`)
	b.WriteString(`<div class="px-5 py-3.5 flex items-center gap-3 border-b border-surface-border">
		<svg class="h-4 w-4 text-accent shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/></svg>
		<div class="flex-1 min-w-0">
			<div class="text-sm font-semibold text-ink">Automations</div>
			<div class="text-xs text-muted">Scheduled workflows that run on their own — digests, health checks, anything. Separate from the board pipeline above.</div>
		</div>
		<button type="button" onclick="var n=document.getElementById('auto-new'); if(n) n.classList.toggle('hidden')" class="inline-flex items-center gap-1.5 rounded-md bg-accent text-white px-3 py-1.5 text-xs font-medium hover:bg-accent-strong transition">
			<svg class="h-3.5 w-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 5v14M5 12h14"/></svg>new automation</button>
	</div>`)
	b.WriteString(`<div class="divide-y divide-surface-border">`)
	if len(autos) == 0 {
		b.WriteString(`<div class="px-5 py-6 text-sm text-muted text-center">No automations yet. Click <b class="text-ink">new automation</b> — start from a template or build your own.</div>`)
	}
	for _, a := range autos {
		cfg := a.Config
		slug := workflow.AutomationSlug(cfg.Name)
		enabled := cfg.IsEnabled()
		pill := `<span class="inline-flex items-center gap-1 rounded-full bg-emerald-500/15 text-emerald-700 dark:text-emerald-400 border border-emerald-500/40 px-2 py-0.5 text-[11px] font-medium">● enabled</span>`
		toggleLabel := "pause"
		if !enabled {
			pill = `<span class="inline-flex items-center gap-1 rounded-full bg-gray-400/15 text-muted border border-surface-border px-2 py-0.5 text-[11px] font-medium">○ paused</span>`
			toggleLabel = "enable"
		}
		last := "—"
		if s.opts.Memory != nil {
			last = humanizeSince(s.opts.Memory.LastScheduledRun(cfg.Name))
		}
		b.WriteString(`<div class="px-5 py-3.5 flex items-center gap-3 flex-wrap">`)
		b.WriteString(`<div class="flex-1 min-w-0">`)
		b.WriteString(fmt.Sprintf(`<div class="flex items-center gap-2 flex-wrap"><span class="text-sm font-semibold text-ink truncate">%s</span> %s <span class="text-[11px] font-mono rounded bg-surface-sunken border border-surface-border px-1.5 py-0.5 text-muted">%s</span></div>`,
			html.EscapeString(cfg.Name), pill, html.EscapeString(cfg.Trigger.ScheduleHint())))
		b.WriteString(fmt.Sprintf(`<div class="text-xs text-muted mt-0.5 truncate">%s · %d step(s) · last run %s</div>`,
			html.EscapeString(autoFirstLine(cfg.Description)), len(cfg.Stages), html.EscapeString(last)))
		b.WriteString(`</div>`)
		b.WriteString(`<div class="flex items-center gap-1.5 shrink-0">`)
		b.WriteString(fmt.Sprintf(`<button type="button" title="Run now" class="inline-flex items-center gap-1 rounded-md border border-surface-border text-muted px-2.5 py-1.5 text-xs hover:border-emerald-500 hover:text-emerald-600 transition" hx-post="/api/automation/run" hx-vals='{"slug":"%s"}' hx-target="#auto-msg" hx-swap="innerHTML"><svg class="h-3.5 w-3.5" viewBox="0 0 24 24" fill="currentColor"><path d="M8 5v14l11-7z"/></svg>run</button>`, slug))
		b.WriteString(fmt.Sprintf(`<button type="button" title="Edit in the visual editor" class="inline-flex items-center gap-1 rounded-md border border-surface-border text-muted px-2.5 py-1.5 text-xs hover:border-accent hover:text-accent transition" hx-get="/fragments/workflow-config?file=%s" hx-target="closest [data-page]" hx-swap="innerHTML"><svg class="h-3.5 w-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M11 4H4a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7"/><path d="M18.5 2.5a2.121 2.121 0 0 1 3 3L12 15l-4 1 1-4 9.5-9.5z"/></svg>edit</button>`, slug))
		b.WriteString(fmt.Sprintf(`<button type="button" class="inline-flex items-center rounded-md border border-surface-border text-muted px-2.5 py-1.5 text-xs hover:border-accent hover:text-accent transition" hx-post="/api/automation/toggle" hx-vals='{"slug":"%s"}' hx-target="#automations" hx-swap="outerHTML">%s</button>`, slug, toggleLabel))
		b.WriteString(fmt.Sprintf(`<button type="button" class="inline-flex items-center rounded-md border border-surface-border text-muted px-2.5 py-1.5 text-xs hover:border-rose-500 hover:text-rose-600 transition" hx-post="/api/automation/delete" hx-vals='{"slug":"%s"}' hx-confirm="Delete this automation?" hx-target="#automations" hx-swap="outerHTML">delete</button>`, slug))
		b.WriteString(`</div></div>`)
	}
	b.WriteString(`</div>`)
	b.WriteString(`<div id="auto-msg" class="px-5 empty:hidden"></div>`)
	b.WriteString(s.automationCreatePanel())
	b.WriteString(`</div>`)
	_, _ = io.WriteString(w, b.String())
}

// automationCreatePanel renders the (hidden until toggled) "new automation"
// panel: one-click starter templates plus a build-your-own form.
func (s *Server) automationCreatePanel() string {
	var b strings.Builder
	b.WriteString(`<div id="auto-new" class="hidden border-t border-surface-border px-5 py-4 space-y-4">`)
	b.WriteString(`<div><div class="text-xs font-semibold text-ink mb-2">Start from a template</div><div class="flex flex-wrap gap-2">`)
	for _, st := range workflow.AutomationStarters() {
		b.WriteString(fmt.Sprintf(`<button type="button" class="text-left rounded-lg border border-surface-border bg-surface px-3 py-2 hover:border-accent hover:shadow-card transition max-w-[260px]" hx-post="/api/automation/create" hx-vals='{"starter":"%s"}' hx-target="#automations" hx-swap="outerHTML"><div class="text-xs font-semibold text-ink">%s</div><div class="text-[11px] text-muted mt-0.5">%s</div><div class="text-[10px] font-mono text-accent mt-1">%s</div></button>`,
			html.EscapeString(st.Key), html.EscapeString(st.Label), html.EscapeString(st.Desc), html.EscapeString(st.Config.Trigger.ScheduleHint())))
	}
	b.WriteString(`</div></div>`)
	b.WriteString(`<form class="border-t border-surface-border pt-4 space-y-2.5" hx-post="/api/automation/create" hx-target="#automations" hx-swap="outerHTML">`)
	b.WriteString(`<div class="text-xs font-semibold text-ink">…or build your own</div>`)
	b.WriteString(`<input name="name" required placeholder="Automation name (e.g. Morning email digest)" class="w-full px-3 py-2 text-sm rounded-md border border-surface-border bg-surface text-ink focus:border-accent focus:outline-none">`)
	b.WriteString(`<div class="flex gap-2">`)
	b.WriteString(`<select name="sched_type" class="px-3 py-2 text-sm rounded-md border border-surface-border bg-surface text-ink focus:border-accent focus:outline-none"><option value="every">every</option><option value="cron">cron</option></select>`)
	b.WriteString(`<input name="sched_value" required placeholder="15m   ·   1h   ·   daily   ·   0 9 * * 1-5" class="flex-1 px-3 py-2 text-sm rounded-md border border-surface-border bg-surface text-ink font-mono focus:border-accent focus:outline-none">`)
	b.WriteString(`</div>`)
	b.WriteString(`<div class="text-[11px] text-muted">every: a duration like <span class="font-mono">15m</span> / <span class="font-mono">1h</span>, or <span class="font-mono">hourly</span> / <span class="font-mono">daily</span>. cron: 5 fields <span class="font-mono">min hour dom mon dow</span> — e.g. <span class="font-mono">0 9 * * 1-5</span> = weekdays 9am.</div>`)
	b.WriteString(`<textarea name="task" required rows="3" placeholder="What should goon do? e.g. Read my unread email and summarise senders, subjects, and anything needing a reply." class="w-full px-3 py-2 text-sm rounded-md border border-surface-border bg-surface text-ink focus:border-accent focus:outline-none"></textarea>`)
	b.WriteString(`<input name="notify" placeholder="Notify message (optional). Use {{.Stages.run}} for the result." class="w-full px-3 py-2 text-sm rounded-md border border-surface-border bg-surface text-ink focus:border-accent focus:outline-none">`)
	b.WriteString(`<div class="flex items-center gap-2"><button type="submit" class="rounded-md bg-accent text-white px-3 py-2 text-xs font-medium hover:bg-accent-strong transition">Create automation</button><span class="text-[11px] text-muted">Builds an executor → notify graph. Edit it visually afterward for anything fancier.</span></div>`)
	b.WriteString(`</form>`)
	b.WriteString(`</div>`)
	return b.String()
}

// autoFirstLine trims a description to a single, length-capped line for the
// fleet row subtitle.
func autoFirstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len([]rune(s)) > 90 {
		s = string([]rune(s)[:90]) + "…"
	}
	if s == "" {
		s = "Scheduled automation"
	}
	return s
}

// findAutomationBySlug resolves a slug to its on-disk automation config.
func findAutomationBySlug(slug string) (workflow.WorkflowConfig, bool) {
	slug = workflow.AutomationSlug(slug)
	for _, a := range workflow.LoadAutomations() {
		if workflow.AutomationSlug(a.Config.Name) == slug {
			return a.Config, true
		}
	}
	return workflow.WorkflowConfig{}, false
}

// handleAutomationCreate saves a new automation — either from a named starter
// template or from the build-your-own form (executor → notify on a schedule).
func (s *Server) handleAutomationCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		fragErr(w, "invalid form: "+err.Error())
		return
	}
	var cfg workflow.WorkflowConfig
	if key := strings.TrimSpace(r.FormValue("starter")); key != "" {
		found := false
		for _, st := range workflow.AutomationStarters() {
			if st.Key == key {
				cfg = st.Config
				found = true
				break
			}
		}
		if !found {
			fragErr(w, "unknown template: "+key)
			return
		}
	} else {
		name := strings.TrimSpace(r.FormValue("name"))
		task := strings.TrimSpace(r.FormValue("task"))
		if name == "" || task == "" {
			fragErr(w, "name and task are required")
			return
		}
		trig := workflow.Trigger{Type: "schedule"}
		val := strings.TrimSpace(r.FormValue("sched_value"))
		if strings.TrimSpace(r.FormValue("sched_type")) == "cron" {
			if len(strings.Fields(val)) != 5 {
				fragErr(w, "cron needs 5 fields: min hour dom mon dow (e.g. 0 9 * * 1-5)")
				return
			}
			trig.Cron = val
		} else {
			if val == "" {
				fragErr(w, "enter an interval like 15m, 1h, hourly, or daily")
				return
			}
			trig.Every = val
		}
		msg := strings.TrimSpace(r.FormValue("notify"))
		if msg == "" {
			msg = "🔔 " + name + "\n{{.Stages.run}}"
		}
		enabled := true
		cfg = workflow.WorkflowConfig{
			Version:     2,
			Name:        name,
			Description: "Scheduled automation.",
			Trigger:     trig,
			Enabled:     &enabled,
			Stages: []workflow.StageConfig{
				{Name: "run", Type: workflow.RoleExecutor, OnNext: workflow.StringList{"notify"}, Task: task},
				{Name: "notify", Type: workflow.RoleNotify, Message: msg},
			},
		}
	}
	if _, err := workflow.SaveAutomation(cfg); err != nil {
		fragErr(w, "save failed: "+err.Error())
		return
	}
	s.events.Publish("automationsChanged")
	s.renderAutomations(w)
}

// handleAutomationToggle flips an automation's enabled flag.
func (s *Server) handleAutomationToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	_ = r.ParseForm()
	cfg, ok := findAutomationBySlug(r.FormValue("slug"))
	if !ok {
		fragErr(w, "automation not found")
		return
	}
	nv := !cfg.IsEnabled()
	cfg.Enabled = &nv
	if _, err := workflow.SaveAutomation(cfg); err != nil {
		fragErr(w, "save failed: "+err.Error())
		return
	}
	s.events.Publish("automationsChanged")
	s.renderAutomations(w)
}

// handleAutomationDelete removes an automation file.
func (s *Server) handleAutomationDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	_ = r.ParseForm()
	slug := workflow.AutomationSlug(r.FormValue("slug"))
	if slug == "" {
		fragErr(w, "missing automation")
		return
	}
	if err := workflow.DeleteAutomation(slug); err != nil {
		fragErr(w, "delete failed: "+err.Error())
		return
	}
	s.events.Publish("automationsChanged")
	s.renderAutomations(w)
}

// handleAutomationRun fires one automation immediately via the daemon (which
// owns the engine + lock). Requires a running daemon that implements
// AutomationRunner.
func (s *Server) handleAutomationRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	_ = r.ParseForm()
	cfg, ok := findAutomationBySlug(r.FormValue("slug"))
	if !ok {
		fragErr(w, "automation not found")
		return
	}
	runner, canRun := s.opts.Daemon.(AutomationRunner)
	if !canRun {
		fragErr(w, "start the daemon to run automations on demand (run: goon start)")
		return
	}
	if s.opts.Memory != nil && s.opts.Memory.GetStatus().Paused {
		fragErr(w, "goon is paused — resume it first, then run this automation")
		return
	}
	runner.RunAutomationNow(cfg.Name)
	fragOK(w, fmt.Sprintf("running “%s” now — watch your notify channel / logs", cfg.Name))
}

// fragSetup renders a banner only when critical configuration is missing:
// no LLM provider or no board/local tickets. Hidden once both are set.
func (s *Server) fragSetup(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	llmReady := llmConfigured()
	mem := s.opts.Memory
	st := mem.GetStatus()
	localCount := len(mem.ListLocalTickets())
	boardReady := strings.TrimSpace(getenv("GOON_BOARD")) != "" || st.BoardName != "" || localCount > 0
	if llmReady && boardReady {
		_, _ = io.WriteString(w, ``)
		return
	}
	missing := ""
	if !llmReady && !boardReady {
		missing = "an LLM provider and a ticket board"
	} else if !llmReady {
		missing = "an LLM provider"
	} else {
		missing = "a ticket board (or create a local ticket)"
	}
	fmt.Fprintf(w, `<div class="mt-4 rounded-lg border border-amber-500/40 bg-amber-500/5 p-4 text-sm text-amber-700 dark:text-amber-300">
  <strong>👋 Welcome to goon.</strong>
  Configure %s on the
  <button type="button" onclick="document.querySelector('button[data-tab=config]').click()" class="underline hover:text-accent font-medium">Configuration</button>
  tab to get started.
</div>`, missing)
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
	dotAnim2 := ""
	if st.Running && !st.Paused {
		dotAnim2 = " pulse-dot"
	}
	fmt.Fprintf(w, `<div class="flex items-center gap-3 pb-4 mb-4">
		<span class="h-2.5 w-2.5 rounded-full shrink-0 %s%s"></span>
		<div class="flex-1">
			<div class="text-[10px] uppercase tracking-widest text-muted/60">daemon</div>
			<div class="text-sm font-medium capitalize text-ink">%s</div>
		</div>
		<div class="text-right">
			<div class="text-[10px] text-muted/60">last poll</div>
			<div class="text-xs font-mono text-muted">%s</div>
		</div>
	</div>`, dotClass, dotAnim2, state, html.EscapeString(last))

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
//     deliberate state, not an error, but should stand out
//   - running → neon purple (accent) with the pulse-dot animation, so
//     the brand-purple matches the header logo glow when live
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
	// pulse-dot only when running; paused/stopped get a static dot
	dotAnim := ""
	if st.Running && !st.Paused {
		dotAnim = " pulse-dot"
	}
	glyph := ""
	fmt.Fprintf(w, `<div class="flex items-center gap-2.5" data-paused="%s" data-running="%s" title="Last poll: %s">
		<span class="h-2 w-2 rounded-full shrink-0 %s%s"></span>
		<div class="min-w-0 flex-1">
			<div class="flex items-center gap-1.5 text-[11px] font-medium %s">%s%s</div>
			<div class="text-[10px] text-muted font-mono">polled %s</div>
		</div>
	</div>`, pausedFlag, runningFlag, html.EscapeString(last), dotClass, dotAnim, labelColor, glyph, state, html.EscapeString(last))
}

// fragQuestionsBanner renders a yellow strip above the tabs when there
// are pending approvals — the user's most blocking signal. Empty body
// when nothing is pending so the page doesn't reserve vertical space.
func (s *Server) fragQuestionsBanner(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	gate := len(s.opts.Memory.PendingGateQuestions())
	learn := len(s.opts.Memory.PendingLearningQuestions())
	if gate == 0 && learn == 0 {
		_, _ = io.WriteString(w, ``)
		return
	}
	gWord := "approval"
	if gate > 1 {
		gWord = "approvals"
	}
	lWord := "question"
	if learn > 1 {
		lWord = "questions"
	}
	fmt.Fprint(w, `<div class="mt-4 flex flex-col gap-2">`)
	if gate > 0 {
		fmt.Fprintf(w, `<div class="rounded-lg border border-highlight/30 bg-highlight/8 px-4 py-3 flex flex-wrap items-center gap-3 text-sm">
			<span class="h-1.5 w-1.5 rounded-full bg-highlight shrink-0"></span>
			<div class="flex-1 min-w-0"><strong class="text-highlight font-medium">%d workflow %s</strong><span class="text-muted"> paused — waiting on approval.</span></div>
			<button type="button" onclick="if(typeof showPage==='function')showPage('workflows')" class="rounded-md bg-highlight/15 text-highlight border border-highlight/30 px-3 py-1.5 text-xs font-medium hover:bg-highlight/25 transition">Review →</button>
		</div>`, gate, gWord)
	}
	if learn > 0 {
		fmt.Fprintf(w, `<div class="rounded-lg border border-accent/20 bg-accent/6 px-4 py-3 flex flex-wrap items-center gap-3 text-sm">
			<span class="h-1.5 w-1.5 rounded-full bg-accent/80 shrink-0"></span>
			<div class="flex-1 min-w-0"><strong class="text-accent/90 font-medium">%d learning %s</strong><span class="text-muted"> — goon has questions about your project.</span></div>
			<button type="button" onclick="if(typeof showPage==='function')showPage('questions')" class="rounded-md bg-accent/12 text-accent/90 border border-accent/25 px-3 py-1.5 text-xs font-medium hover:bg-accent/20 transition">Answer →</button>
		</div>`, learn, lWord)
	}
	fmt.Fprint(w, `</div>`)
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
	// Toolbar: count + a "clear cache" escape hatch. The list is a cache
	// of what polling has seen; if a user tightens their JQL the daemon
	// reconciles within one poll, but this lets them reset instantly.
	fmt.Fprintf(w, `<div class="flex items-center justify-between gap-3 mb-3">
		<span class="text-xs text-muted">%d cached ticket%s · reflects your board filter (JIRA_JQL / GitHub query) after the next poll</span>
		<button type="button"
			hx-post="/api/tickets/clear" hx-confirm="Clear the cached ticket list? The next poll repopulates it from your current filter. Workflows are not affected."
			hx-swap="none"
			class="shrink-0 rounded-md border border-surface-border px-2.5 py-1 text-[11px] font-medium text-muted hover:border-accent hover:text-accent transition">
			Clear cached tickets
		</button>
	</div>`, len(tks), pluralS(len(tks)))
	fmt.Fprint(w, `<div class="overflow-x-auto rounded-xl border-2 border-amber-700/25 dark:border-amber-300/20 ring-1 ring-inset ring-amber-700/10 dark:ring-amber-300/10 bg-amber-50/40 dark:bg-surface-raised shadow-card">
	<table class="min-w-full text-sm">
		<thead class="sticky top-0 z-10 border-b border-amber-700/20 dark:border-amber-300/15 text-[11px] uppercase tracking-wider text-gray-500 bg-amber-50/80 dark:bg-surface">
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
	// Repo options for the per-ticket "Pick" control. Only repos with a
	// local checkout can be assigned — the execute phase needs a working
	// tree. The checkbox list is identical per row, so build it once.
	repoCheckboxes, repoOptCount := pickRepoCheckboxes()
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
			assignee = `<span class="text-muted">—</span>`
		}
		project := html.EscapeString(t.Project)
		if project == "" {
			project = `<span class="text-muted">—</span>`
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
		safeID := strings.ReplaceAll(html.EscapeString(t.Key), "/", "-")
		actionsRowID := "ta-" + safeID
		escapedKey := html.EscapeString(t.Key)

		// Inline ignore/unignore icon button — shown directly in the row
		// so the user can skip a ticket with one click, no drawer needed.
		var inlineIgnoreBtn string
		if isIgnored {
			inlineIgnoreBtn = fmt.Sprintf(
				`<form hx-post="/api/ticket/unignore" hx-target="#%s-r" hx-swap="innerHTML" class="inline">`+
					`<input type="hidden" name="key" value="%s">`+
					`<button type="submit" title="Restore into workflow" class="p-1.5 rounded text-emerald-500 hover:bg-emerald-500/10 transition">`+
					`<svg class="h-3.5 w-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><polyline points="1 4 1 10 7 10"/><path d="M3.51 15a9 9 0 1 0 .49-4.5"/></svg>`+
					`</button></form>`,
				actionsRowID, escapedKey)
		} else {
			inlineIgnoreBtn = fmt.Sprintf(
				`<form hx-post="/api/ticket/ignore" hx-target="#%s-r" hx-swap="innerHTML" class="inline">`+
					`<input type="hidden" name="key" value="%s">`+
					`<button type="submit" title="Skip in daemon workflow" class="p-1.5 rounded text-muted hover:text-amber-500 hover:bg-amber-500/10 transition">`+
					`<svg class="h-3.5 w-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="10"/><line x1="4.93" y1="4.93" x2="19.07" y2="19.07"/></svg>`+
					`</button></form>`,
				actionsRowID, escapedKey)
		}

		// Drawer toggle — three-dot icon opens comment/status/edit panel.
		drawerBtn := fmt.Sprintf(
			`<button type="button" onclick="document.getElementById('%s').classList.toggle('hidden')" `+
				`title="Comment / status / edit" class="p-1.5 rounded text-muted hover:text-accent hover:bg-accent/10 transition">`+
				`<svg class="h-3.5 w-3.5" viewBox="0 0 24 24" fill="currentColor"><circle cx="5" cy="12" r="1.5"/><circle cx="12" cy="12" r="1.5"/><circle cx="19" cy="12" r="1.5"/></svg>`+
				`</button>`,
			actionsRowID)

		actionCell := `<div class="inline-flex items-center gap-0.5">` + inlineIgnoreBtn + drawerBtn + `</div>`

		// Pick control — only for tickets goon hasn't taken yet (no open or
		// completed workflow) and that aren't ignored. The user assigns
		// repo(s) and clicks Pick to push the ticket into the workflow.
		pickBlock := ""
		if !isIgnored {
			hasWF := s.opts.Memory.HasOpenWorkflowFor(t.ID) || s.opts.Memory.HasCompletedWorkflowFor(t.ID)
			switch {
			case hasWF:
				pickBlock = ""
			case s.opts.Memory.IsPickQueued(t.ID):
				pickBlock = `<div class="flex-1 min-w-[160px] space-y-1.5"><p class="text-[10px] font-semibold uppercase tracking-wider text-muted">Pick</p>` +
					`<span class="inline-flex items-center gap-1 rounded-full bg-accent/15 text-accent px-2 py-0.5 text-[11px] font-medium">⏳ queued for goon</span></div>`
			case repoOptCount == 0:
				pickBlock = `<div class="flex-1 min-w-[160px] space-y-1.5"><p class="text-[10px] font-semibold uppercase tracking-wider text-muted">Pick</p>` +
					`<p class="text-[11px] text-muted">Map a repo to a local checkout on the <strong>Repositories</strong> tab to enable Pick.</p></div>`
			default:
				pickBlock = `<div class="flex-1 min-w-[160px] space-y-1.5">` +
					`<p class="text-[10px] font-semibold uppercase tracking-wider text-muted">Pick → run in goon</p>` +
					`<form hx-post="/api/ticket/pick" hx-target="#` + actionsRowID + `-r" hx-swap="innerHTML" class="space-y-1.5">` +
					`<input type="hidden" name="id" value="` + html.EscapeString(t.ID) + `">` +
					`<div class="space-y-1 max-h-28 overflow-y-auto pr-1">` + repoCheckboxes + `</div>` +
					`<button type="submit" class="rounded bg-accent text-surface px-3 py-1 text-xs font-semibold hover:brightness-110 transition">pick →</button>` +
					`</form>` +
					`<p class="text-[10px] text-muted">assigns the repo(s), then goon plans &amp; asks you to approve.</p>` +
					`</div>`
			}
		}

		fmt.Fprintf(w, `<tr data-ticket-row data-status="%s" class="hover:bg-gray-50 dark:hover:bg-surface-raised/60 transition%s">
			<td class="px-4 py-2.5 font-mono whitespace-nowrap align-middle">%s</td>
			<td class="px-4 py-2.5 align-middle">
				<div class="text-sm font-medium text-gray-900 dark:text-ink max-w-xl truncate">%s%s</div>
			</td>
			<td class="px-4 py-2.5 whitespace-nowrap align-middle">%s</td>
			<td class="px-4 py-2.5 text-gray-700 dark:text-gray-300 whitespace-nowrap align-middle text-xs">%s</td>
			<td class="px-4 py-2.5 font-mono text-xs text-gray-600 dark:text-gray-400 whitespace-nowrap align-middle">%s</td>
			<td class="px-4 py-2.5 text-gray-500 text-xs whitespace-nowrap align-middle">%s</td>
			<td class="px-4 py-2.5 text-right whitespace-nowrap align-middle">%s</td>
		</tr>
		<tr id="%s" data-action-row class="hidden">
			<td colspan="7" class="px-6 py-3 bg-surface-sunken/30 dark:bg-surface-sunken/20 border-b border-surface-border">
				<div class="flex flex-wrap gap-x-8 gap-y-3 text-xs">
					<div class="flex-1 min-w-[180px] space-y-1.5">
						<p class="text-[10px] font-semibold uppercase tracking-wider text-muted">Comment</p>
						<form hx-post="/api/ticket/comment" hx-target="#%s-r" hx-swap="innerHTML" hx-on::after-request="if(event.detail.successful) this.reset()" class="space-y-1.5">
							<input type="hidden" name="key" value="%s">
							<textarea name="body" rows="2" required placeholder="comment text…" class="w-full font-mono text-xs rounded border border-surface-border bg-surface px-2 py-1 focus:border-accent focus:ring-1 focus:ring-accent/30 focus:outline-none"></textarea>
							<button type="submit" class="rounded bg-accent text-surface px-3 py-1 text-xs font-semibold hover:brightness-110 transition">send →</button>
						</form>
					</div>
					<div class="flex-1 min-w-[140px] space-y-1.5">
						<p class="text-[10px] font-semibold uppercase tracking-wider text-muted">Move status</p>
						<form hx-post="/api/ticket/transition" hx-target="#%s-r" hx-swap="innerHTML" class="space-y-1.5">
							<input type="hidden" name="key" value="%s">
							<select name="status" hx-get="/api/ticket/transitions?key=%s" hx-trigger="toggle from:closest details once, load delay:200ms" hx-swap="innerHTML" class="w-full rounded border border-surface-border bg-surface px-2 py-1 focus:border-accent focus:outline-none">
								<option value="" disabled selected>loading…</option>
							</select>
							<button type="submit" class="rounded border border-accent/40 text-accent px-3 py-1 text-xs font-medium hover:bg-accent/10 transition">move →</button>
						</form>
					</div>
					<div class="flex-1 min-w-[180px] space-y-1.5">
						<p class="text-[10px] font-semibold uppercase tracking-wider text-muted">Edit field</p>
						<form hx-post="/api/ticket/edit" hx-target="#%s-r" hx-swap="innerHTML" hx-on::after-request="if(event.detail.successful) this.reset()" class="space-y-1.5">
							<input type="hidden" name="key" value="%s">
							<select name="field" class="w-full rounded border border-surface-border bg-surface px-2 py-1 focus:border-accent focus:outline-none">
								<option value="title">title</option>
								<option value="desc">description</option>
								<option value="labels">labels (a,b,c)</option>
							</select>
							<input type="text" name="value" required placeholder="new value…" class="w-full font-mono text-xs rounded border border-surface-border bg-surface px-2 py-1 focus:border-accent focus:ring-1 focus:ring-accent/30 focus:outline-none">
							<button type="submit" class="rounded border border-accent/40 text-accent px-3 py-1 text-xs font-medium hover:bg-accent/10 transition">apply →</button>
						</form>
					</div>
					%s
				</div>
				<div id="%s-r" class="mt-2 text-xs"></div>
			</td>
		</tr>`,
			html.EscapeString(strings.ToLower(t.Status)), rowOpacity, key,
			html.EscapeString(t.Title), titleBadge,
			ticketStatusPill(t.Status), assignee, project,
			html.EscapeString(humanizeSince(t.UpdatedAt)), actionCell,
			actionsRowID,
			actionsRowID, escapedKey,
			actionsRowID, escapedKey, escapedKey,
			actionsRowID, escapedKey,
			pickBlock,
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
//
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
		cls = "bg-emerald-500/15 text-emerald-700 dark:text-emerald-400 border-emerald-500/40"
	case strings.Contains(low, "block"):
		cls = "bg-rose-500/15 text-rose-700 dark:text-rose-400 border-rose-500/40"
	}
	label := status
	if label == "" {
		label = "—"
	}
	return fmt.Sprintf(`<span class="inline-flex items-center rounded-full border px-2 py-0.5 text-[11px] font-medium %s">%s</span>`,
		cls, html.EscapeString(label))
}

// pickRepoCheckboxes builds the repo checkbox list for the Tickets-tab
// "Pick" control and returns how many options exist. Only repos that have a
// local checkout in REPOSITORY.md are offered — the execute phase needs a
// working tree, so remote-only entries are skipped.
func pickRepoCheckboxes() (out string, count int) {
	ents, err := repository.Read()
	if err != nil {
		return "", 0
	}
	var b strings.Builder
	for _, e := range ents {
		if strings.TrimSpace(e.Local) == "" {
			continue
		}
		slug := html.EscapeString(e.Remote)
		b.WriteString(`<label class="flex items-center gap-1.5"><input type="checkbox" name="repos" value="` +
			slug + `" class="accent-accent"> <span class="font-mono text-[11px] text-ink">` + slug + `</span></label>`)
		count++
	}
	return b.String(), count
}

// goonGlyph renders one decorative nautical glyph from the inline
// <symbol> sprite in index.html (ship, anchor, storm, chest, flag…).
// Stroke is currentColor, so the tailwind text-* class in cls themes it
// for light AND dark automatically. Purely decorative (aria-hidden) —
// the plain text label next to it always carries the meaning.
func goonGlyph(id, cls string) string {
	return `<svg class="goon-glyph ` + cls + `" aria-hidden="true" focusable="false"><use href="#goon-i-` + id + `"></use></svg>`
}

// wfPipeline is the display order + short labels for the built-in
// engine pipeline, shared by the mini flow (workflow cards) and the
// full stage route (workflow detail).
var wfPipeline = []struct{ name, short string }{
	{"triage", "plan"},
	{"confirm_repo", "repo"},
	{"approve_plan", "plan"},
	{"execute", "exec"},
	{"test", "test"},
	{"verify", "verify"},
	{"update_memory", "memory"},
	{"open_pr", "pr"},
	{"notify", "notify"},
}

// renderMiniStageFlow renders a compact horizontal pipeline flowchart for
// a workflow card. Each stage is a small colored dot; the current stage
// is marked by a tiny ship (voyage theme) — or a flag while a gate waits
// on the user, or a storm cloud when the workflow failed. Stages after
// the current one are muted gray; completed stages are emerald.
func renderMiniStageFlow(wf memory.Workflow) string {
	state := wf.State
	// Find index of current stage.
	cur := -1
	for i, p := range wfPipeline {
		if p.name == wf.Stage {
			cur = i
			break
		}
	}
	isGate := wf.Stage == "confirm_repo" || wf.Stage == "approve_plan"

	var b strings.Builder
	b.WriteString(`<div class="mt-2 flex items-center gap-0 overflow-x-auto pb-0.5">`)
	for i, p := range wfPipeline {
		if i > 0 {
			// Connector line — green if both sides are done.
			lineCls := "bg-surface-border/60"
			if (cur >= 0 && i <= cur && state != memory.WFFailed) || state == memory.WFDone {
				lineCls = "bg-emerald-500/50"
			}
			b.WriteString(`<div class="h-px w-3 shrink-0 ` + lineCls + `"></div>`)
		}
		titleAttr := `title="` + p.name + `"`
		b.WriteString(`<div class="shrink-0 flex flex-col items-center">`)
		switch {
		case i == cur && state == memory.WFFailed:
			b.WriteString(`<span ` + titleAttr + ` class="grid place-items-center h-4 w-4 rounded-full bg-danger/15 border border-danger/50"><svg class="h-2.5 w-2.5 text-danger" viewBox="0 0 12 12" fill="none" stroke="currentColor" stroke-width="2.5"><path d="M2 10L10 2M2 2l8 8"/></svg></span>` +
				`<span class="text-[9px] text-danger/80 whitespace-nowrap leading-none mt-0.5">` + html.EscapeString(p.short) + `</span>`)
		case i == cur && state == memory.WFAwaitingApproval && isGate:
			b.WriteString(`<span ` + titleAttr + ` class="grid place-items-center h-4 w-4 rounded-full bg-highlight/15 border border-highlight/50"><svg class="h-2.5 w-2.5 text-highlight" viewBox="0 0 12 12" fill="none" stroke="currentColor" stroke-width="2"><path d="M6 3v3.5L8 8"/><circle cx="6" cy="6" r="5"/></svg></span>` +
				`<span class="text-[9px] text-amber-600 dark:text-amber-400 whitespace-nowrap leading-none mt-0.5">` + html.EscapeString(p.short) + `</span>`)
		case i == cur:
			b.WriteString(`<span ` + titleAttr + ` class="grid place-items-center h-4 w-4 rounded-full bg-accent/15 border border-accent/60"><span class="h-2 w-2 rounded-full bg-accent pulse-dot"></span></span>` +
				`<span class="text-[9px] text-accent whitespace-nowrap leading-none mt-0.5">` + html.EscapeString(p.short) + `</span>`)
		case p.name == "open_pr" && wf.PRURL != "":
			b.WriteString(`<span ` + titleAttr + ` class="grid place-items-center h-4 w-4 rounded-full bg-success/15 border border-success/50"><svg class="h-2.5 w-2.5 text-success" viewBox="0 0 12 12" fill="none" stroke="currentColor" stroke-width="2.5"><path d="M2 6l3 3 5-5"/></svg></span>`)
		case state == memory.WFDone, cur >= 0 && i < cur && state != memory.WFFailed:
			b.WriteString(`<div class="w-2 h-2 rounded-full bg-emerald-500/80" ` + titleAttr + `></div>`)
		case cur < 0 && state != memory.WFFailed && wf.Stage == "done":
			b.WriteString(`<div class="w-2 h-2 rounded-full bg-emerald-500/80" ` + titleAttr + `></div>`)
		default:
			b.WriteString(`<div class="w-2 h-2 rounded-full bg-surface-border" ` + titleAttr + `></div>`)
		}
		b.WriteString(`</div>`)
	}
	b.WriteString(`</div>`)
	return b.String()
}

// renderStageRoute renders the full "voyage" strip for the workflow
// detail panel: every pipeline stage as a labelled island dot on a
// dotted sea route. The current stage is marked by a small bobbing
// ship; a waiting gate shows a flag, a failure shows a storm cloud,
// and an opened PR shows an open chest at the end of the route.
// Decorative layer only — every label stays the plain stage name, and
// the animation is pure CSS so SSE fragment re-renders are safe.
func renderStageRoute(wf memory.Workflow) string {
	cur := -1
	for i, p := range wfPipeline {
		if p.name == wf.Stage {
			cur = i
			break
		}
	}
	isGate := wf.Stage == "confirm_repo" || wf.Stage == "approve_plan"
	allDone := wf.State == memory.WFDone || wf.Stage == "done"

	label := "pipeline complete"
	if cur >= 0 {
		label = fmt.Sprintf("pipeline stage %d of %d: %s", cur+1, len(wfPipeline), wf.Stage)
	}

	var b strings.Builder
	b.WriteString(`<div class="stage-route" role="img" aria-label="` + html.EscapeString(label) + `">`)
	b.WriteString(`<div class="relative flex items-start justify-between gap-1 overflow-x-auto pb-1"><div class="route-line"></div>`)
	for i, p := range wfPipeline {
		var node, labelCls string
		ring := `<span class="relative grid place-items-center h-[22px] w-[22px] rounded-full border-2 bg-surface `
		switch {
		case i == cur && wf.State == memory.WFFailed:
			node = ring + `border-danger/60 bg-danger/10"><svg class="h-3 w-3 text-danger" viewBox="0 0 12 12" fill="none" stroke="currentColor" stroke-width="2.5"><path d="M2 10L10 2M2 2l8 8"/></svg></span>`
			labelCls = "text-rose-600 dark:text-rose-400 font-semibold"
		case i == cur && wf.State == memory.WFAwaitingApproval && isGate:
			node = ring + `border-highlight/60 bg-highlight/10"><svg class="h-3 w-3 text-highlight" viewBox="0 0 12 12" fill="none" stroke="currentColor" stroke-width="2"><path d="M6 3v3.5L8 8"/><circle cx="6" cy="6" r="5"/></svg></span>`
			labelCls = "text-amber-700 dark:text-amber-400 font-semibold"
		case i == cur:
			node = ring + `border-accent/60 bg-accent/10"><span class="h-2 w-2 rounded-full bg-accent pulse-dot"></span></span>`
			labelCls = "text-accent font-semibold"
		case p.name == "open_pr" && wf.PRURL != "":
			node = ring + `border-success/60 bg-success/10"><svg class="h-3 w-3 text-success" viewBox="0 0 12 12" fill="none" stroke="currentColor" stroke-width="2.5"><path d="M2 6l3 3 5-5"/></svg></span>`
			labelCls = "text-emerald-700 dark:text-emerald-400"
		case allDone, cur >= 0 && i < cur && wf.State != memory.WFFailed:
			node = ring + `border-emerald-500"><span class="h-2 w-2 rounded-full bg-emerald-500"></span></span>`
			labelCls = "text-emerald-700 dark:text-emerald-400"
		default:
			node = ring + `border-surface-border"><span class="h-1.5 w-1.5 rounded-full bg-surface-border"></span></span>`
			labelCls = "text-muted"
		}
		b.WriteString(`<div class="flex flex-col items-center gap-1 shrink-0 min-w-[44px]">` + node +
			`<span class="text-[10px] leading-none ` + labelCls + `">` + html.EscapeString(p.short) + `</span></div>`)
	}
	b.WriteString(`</div></div>`)
	return b.String()
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
	// Order for legibility at scale: failures first (they need
	// attention), then awaiting-approval, then active mid-flight, then
	// terminal/done last. SliceStable preserves the newest-first order
	// within each group (ListWorkflows already returns newest first).
	sort.SliceStable(wfs, func(i, j int) bool {
		return workflowSortRank(wfs[i].State) < workflowSortRank(wfs[j].State)
	})
	if len(wfs) > 50 {
		wfs = wfs[:50]
	}

	if len(wfs) == 0 {
		_, _ = io.WriteString(w, emptyState("No workflows yet.",
			"Workflow runs appear here as soon as a ticket is picked up. Click a card to see plan progress, approvals, errors, and answer any pending question."))
		return
	}

	// Bulk-approve banner. When several workflows are stacked at the
	// confirm_repo gate, clicking each "yes" is approval fatigue — offer
	// a single "approve all repo confirmations" action that accepts
	// goon's suggestion for every one at once. Only shown when there are
	// 2+ to make the affordance worthwhile.
	// Collect workflows waiting at confirm_repo — gather ticket key + suggested repo
	// so the bulk-approve banner shows exactly what the user is agreeing to.
	// Classify confirm_repo gates into two buckets:
	//   • real suggestion  → goon picked a concrete repo; "approve all"
	//     is meaningful (accept the suggestion).
	//   • needs a pick      → goon had NO suggestion (the board project
	//     key is not a repo). Bulk-approving these would commit garbage,
	//     so we never offer it — we tell the user to pick instead.
	type repoApproval struct{ ticketKey, suggested string }
	var realApprovals []repoApproval
	needPick := 0
	for _, wf := range wfs {
		if wf.State != memory.WFAwaitingApproval || wf.Stage != "confirm_repo" {
			continue
		}
		key := wf.TicketKey
		if key == "" {
			key = wf.TicketID
		}
		suggested := ""
		if wf.PendingQuestionID != "" {
			if q, ok := s.opts.Memory.GetQuestion(wf.PendingQuestionID); ok {
				suggested = extractSuggestedRepo(q.Question)
			}
		}
		if suggested == "" {
			suggested = strings.TrimSpace(wf.Repo)
		}
		// A suggestion that equals the board project key (e.g. "EB" for
		// EB-4232) is not a real repo — treat it as "needs a pick".
		if suggested == "" || strings.EqualFold(suggested, projectKeyOf(key)) {
			needPick++
			continue
		}
		realApprovals = append(realApprovals, repoApproval{ticketKey: key, suggested: suggested})
	}
	if len(realApprovals) > 1 {
		var rows strings.Builder
		for _, ra := range realApprovals {
			fmt.Fprintf(&rows,
				`<div class="flex items-center gap-2 text-[11px]">
					<span class="font-mono text-muted/80 w-24 shrink-0 truncate">%s</span>
					<span class="text-muted">→</span>
					<span class="font-medium text-ink truncate">%s</span>
				</div>`,
				html.EscapeString(ra.ticketKey), html.EscapeString(ra.suggested))
		}
		fmt.Fprintf(w,
			`<div class="mb-4 rounded-lg border border-highlight/30 bg-highlight/6 p-4 space-y-3">
				<div class="flex items-center justify-between gap-4 flex-wrap">
					<div>
						<div class="text-sm font-medium text-ink">%d workflows have a suggested repo</div>
						<div class="text-[11px] text-muted mt-0.5">Review the suggestions below, then accept them all at once.</div>
					</div>
					<form hx-post="/api/answer-all" hx-target="#bulk-approve-result" hx-swap="innerHTML"
						hx-confirm="Accept goon's suggested repo for these %d workflows?"
						hx-disabled-elt="find button" class="m-0">
						<input type="hidden" name="stage" value="confirm_repo">
						<button type="submit"
							class="inline-flex items-center gap-1.5 rounded-md bg-emerald-600/90 hover:bg-emerald-600 text-white px-3.5 py-1.5 text-xs font-semibold transition disabled:opacity-50">
							<svg class="h-3.5 w-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><polyline points="20 6 9 17 4 12"/></svg>
							Accept all suggestions
						</button>
					</form>
				</div>
				<div class="space-y-1 rounded-md bg-surface-sunken/60 px-3 py-2">%s</div>
				<span id="bulk-approve-result" class="text-xs text-emerald-700 dark:text-emerald-400 empty:hidden"></span>
			</div>`,
			len(realApprovals), len(realApprovals), rows.String())
	}
	// Stuck workflows: awaiting an answer but their question is gone
	// (pruned / cleared), so they can't be answered from the UI. Offer a
	// one-click bulk reset so the user isn't stuck hand-resetting each.
	stuck := 0
	for _, wf := range wfs {
		if wf.State != memory.WFAwaitingApproval {
			continue
		}
		if wf.PendingQuestionID != "" {
			if q, ok := s.opts.Memory.GetQuestion(wf.PendingQuestionID); ok && q.Pending() {
				continue
			}
		}
		stuck++
	}
	if stuck > 0 {
		fmt.Fprintf(w,
			`<div class="mb-4 flex flex-wrap items-center gap-3 rounded-lg border border-rose-500/40 bg-rose-500/10 p-4">
				<div class="flex-1 min-w-0">
					<div class="text-sm font-semibold text-rose-700 dark:text-rose-300">%d workflow%s stuck on an expired question</div>
					<div class="text-[11px] text-rose-700/80 dark:text-rose-300/70 mt-0.5">These say “waiting for your answer” but the question is gone, so they can't be answered. Reset them to re-run from triage.</div>
				</div>
				<form hx-post="/api/workflow/reset-stuck" hx-target="#stuck-reset-result" hx-swap="innerHTML"
					hx-confirm="Reset %d stuck workflows and re-run them from triage?" hx-disabled-elt="find button" class="m-0">
					<button type="submit" class="rounded-lg bg-rose-500 text-white px-4 py-2 text-sm font-semibold hover:bg-rose-600 transition disabled:opacity-50 disabled:cursor-wait">↻ Reset all stuck</button>
				</form>
				<span id="stuck-reset-result" class="text-xs w-full sm:w-auto"></span>
			</div>`,
			stuck, pluralS(stuck), stuck)
	}
	// Honest message for the no-suggestion case (this is the common one
	// when triage was offline): no fake "approve all" — tell the user to
	// pick. Each card below has a fast repo picker.
	if needPick > 0 {
		fmt.Fprintf(w,
			`<div class="mb-4 rounded-lg border border-amber-500/30 bg-amber-500/5 p-4">
				<div class="text-sm font-medium text-amber-700 dark:text-amber-300">%d workflow%s need a repo</div>
				<div class="text-[11px] text-amber-700/80 dark:text-amber-300/70 mt-0.5">goon couldn't suggest one automatically (triage may have been offline, or the ticket didn't map to a known repo). Open a card below and pick a repo — the picker lists every repo from your git host with a filter. Tip: register repos on the Repositories tab so triage can suggest them next time.</div>
			</div>`,
			needPick, pluralS(needPick))
	}

	// Filter bar — state chips + free-text search. Pure client-side
	// (data-wf-state / data-wf-search on each card) so it's instant and
	// survives SSE re-renders without a round trip. Only shown when there
	// are enough cards to be worth filtering.
	if len(wfs) > 4 {
		var nFailed, nAwaiting, nActive, nDone int
		for _, wf := range wfs {
			switch workflowStateKey(wf.State) {
			case "failed":
				nFailed++
			case "awaiting":
				nAwaiting++
			case "done":
				nDone++
			default:
				nActive++
			}
		}
		chip := func(key, label string, n int) string {
			if n == 0 && key != "all" {
				return ""
			}
			return fmt.Sprintf(`<button type="button" data-wf-chip="%s" onclick="goonWFFilter('%s')"
				class="rounded-full border border-surface-border px-3 py-1 text-xs text-muted hover:border-accent hover:text-accent transition data-[active=true]:bg-accent data-[active=true]:text-surface data-[active=true]:border-accent">%s <span class="opacity-60">%d</span></button>`,
				key, key, label, n)
		}
		fmt.Fprint(w, `<div class="mb-4 flex flex-wrap items-center gap-2">`)
		fmt.Fprint(w, chip("all", "All", len(wfs)))
		fmt.Fprint(w, chip("failed", "Failed", nFailed))
		fmt.Fprint(w, chip("awaiting", "Awaiting", nAwaiting))
		fmt.Fprint(w, chip("active", "Active", nActive))
		fmt.Fprint(w, chip("done", "Done", nDone))
		fmt.Fprint(w, `<input type="text" placeholder="search ticket or title…" autocomplete="off"
			oninput="goonWFSearch(this.value)"
			class="ml-auto w-48 rounded-lg border border-surface-border bg-surface px-3 py-1 text-xs focus:border-accent focus:outline-none">`)
		fmt.Fprint(w, `</div>`)
		fmt.Fprint(w, `<script>
			window.goonWFState = window.goonWFState || {chip:'all', q:''};
			window.goonWFApply = window.goonWFApply || function(){
				var st = window.goonWFState;
				document.querySelectorAll('#wf-grid [data-wf-state]').forEach(function(el){
					var okChip = st.chip==='all' || el.getAttribute('data-wf-state')===st.chip;
					var okQ = !st.q || (el.getAttribute('data-wf-search')||'').indexOf(st.q)>=0;
					el.style.display = (okChip && okQ) ? '' : 'none';
				});
				document.querySelectorAll('[data-wf-chip]').forEach(function(b){
					b.setAttribute('data-active', b.getAttribute('data-wf-chip')===st.chip ? 'true':'false');
				});
			};
			window.goonWFFilter = function(c){ window.goonWFState.chip=c; window.goonWFApply(); };
			window.goonWFSearch = function(q){ window.goonWFState.q=(q||'').toLowerCase().trim(); window.goonWFApply(); };
			setTimeout(window.goonWFApply, 0);
		</script>`)
	}

	fmt.Fprint(w, `<div id="wf-grid" class="grid grid-cols-1 lg:grid-cols-2 gap-4">`)
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

		// History badge: when this ticket has prior attempts in
		// memory.json, surface a tiny "Nx" pill so the user knows
		// the detail view will show more than one entry.
		historyBadge := ""
		if wf.TicketID != "" {
			if n, ok := seen[wf.TicketID]; ok && n > 1 {
				historyBadge = fmt.Sprintf(`<span class="ml-1 inline-flex items-center rounded-full bg-gray-100 dark:bg-surface-sunken text-gray-600 dark:text-gray-300 px-1.5 py-0.5 text-[10px] font-mono" title="%d total attempts for this ticket">%dx</span>`, n, n)
			}
		}

		// Card chrome: state-colored left edge, with a soft glow on the
		// states that want attention (awaiting amber, failed rose, active
		// indigo). Hover lifts the card slightly — interactivity hint.
		edgeGlow := ""
		switch wf.State {
		case memory.WFAwaitingApproval:
			edgeGlow = " shadow-[0_0_12px_rgba(245,158,11,0.55)]"
		case memory.WFFailed:
			edgeGlow = " shadow-[0_0_12px_rgba(244,63,94,0.55)]"
		case memory.WFDone:
			// done rests quietly
		default:
			if total > 0 && pct < 100 {
				edgeGlow = " shadow-[0_0_12px_rgba(99,102,241,0.5)]"
			}
		}
		wfSearch := html.EscapeString(strings.ToLower(wf.TicketKey + " " + wf.Title))
		fmt.Fprintf(w, `<div data-wf-state="%s" data-wf-search="%s" class="group relative rounded-lg border border-gray-200 dark:border-surface-border bg-white dark:bg-surface-raised hover:border-accent/40 hover:-translate-y-0.5 hover:shadow-lg transition-all duration-150">
			<div class="absolute left-0 top-0 bottom-0 w-1 %s%s rounded-l-lg"></div>
			<details>
				<summary class="cursor-pointer list-none px-4 py-3 select-none">`, workflowStateKey(wf.State), wfSearch, edgeTone, edgeGlow)
		fmt.Fprintf(w, `<div class="flex items-center justify-between gap-3">
			<div class="min-w-0 flex-1">
				<div class="flex items-center gap-2 text-sm font-semibold text-gray-900 dark:text-gray-100">
					<span class="font-mono">%s</span>%s
				</div>
				<div class="mt-0.5 text-sm text-gray-600 dark:text-gray-400 truncate" title="%s">%s</div>
			</div>
			%s
		</div>`, ticket, historyBadge, title, title, stateChip)

		// Stage flowchart + meta row.
		fmt.Fprintf(w, `<div class="mt-2 text-[11px] text-muted flex items-center justify-between gap-2">
			<span class="font-mono shrink-0">%s</span>
			<span class="text-muted/50">·</span>
			<span class="shrink-0">%s</span>
		</div>`, stage, html.EscapeString(humanizeSince(wf.UpdatedAt)))
		fmt.Fprint(w, renderMiniStageFlow(wf))

		// Plan progress — slim bar + counter, only once a plan exists.
		if total > 0 {
			barTone := "bg-accent"
			if pct >= 100 {
				barTone = "bg-emerald-500"
			}
			fmt.Fprintf(w, `<div class="mt-2 flex items-center gap-2">
				<div class="h-1 flex-1 rounded-full bg-surface-sunken overflow-hidden"><div class="h-full rounded-full %s transition-all duration-300" style="width:%d%%"></div></div>
				<span class="text-[10px] font-mono text-muted shrink-0">%d/%d steps</span>
			</div>`, barTone, pct, done, total)
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
				// Clicking expands the in-card detail where the answer form
				// lives. Gate answers live here, not on the Questions tab.
				fmt.Fprintf(w, `<button type="button"
					onclick="var d=this.closest('div.group').querySelector('details'); d.open=true; this.closest('div.group').scrollIntoView({behavior:'smooth',block:'start'})"
					class="w-full flex items-center gap-2 text-left rounded-md bg-highlight/10 hover:bg-highlight/15 px-2.5 py-1.5 text-xs text-highlight transition">
					<span class="h-1.5 w-1.5 rounded-full bg-highlight shrink-0"></span>
					<span class="flex-1">waiting for your answer</span>
					<span class="font-mono text-muted">%s</span>
					<span class="font-medium shrink-0">answer ↓</span>
				</button>`, qid)
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
		<span class="ml-auto flex items-center gap-2">
			<span id="wf-reset-result-%s" class="text-[11px]"></span>
			<form hx-post="/api/workflow/reset" hx-target="#wf-reset-result-%s" hx-swap="innerHTML"
				hx-confirm="Reset this workflow and re-run the ticket from triage? The current plan, repo choice, and approvals are cleared."
				hx-disabled-elt="find button" class="m-0">
				<input type="hidden" name="wf_id" value="%s">
				<button type="submit" class="inline-flex items-center gap-1 rounded-md border border-surface-border px-2 py-1 text-[11px] text-muted hover:border-amber-500 hover:text-amber-400 transition disabled:opacity-50 disabled:cursor-wait">↻ reset from start</button>
			</form>
		</span>
	</div>`,
		html.EscapeString(humanizeSince(wf.StartedAt)),
		html.EscapeString(humanizeSince(wf.UpdatedAt)),
		html.EscapeString(wf.ID),
		html.EscapeString(domID(wf.ID)),
		html.EscapeString(domID(wf.ID)),
		html.EscapeString(wf.ID),
	)

	// Stage route — the full pipeline at a glance (voyage strip). Sits
	// right under the meta strip so "where is this workflow" is the
	// first thing the open card answers.
	fmt.Fprint(w, renderStageRoute(wf))

	if wf.Repo != "" || wf.Branch != "" || len(wf.Repos) > 0 {
		fmt.Fprint(w, `<div class="flex flex-wrap items-center gap-x-4 gap-y-2 text-xs">`)
		if wf.Repo != "" && !strings.EqualFold(wf.Repo, projectKeyOf(wf.TicketKey)) {
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

	// View changes — lazy-loaded diff of what goon did in the local repo.
	// The trust surface: review before approving the PR. Only offered once
	// a repo is selected (the handler degrades gracefully otherwise).
	if strings.TrimSpace(wf.Repo) != "" || len(wf.Repos) > 0 {
		fmt.Fprintf(w, `<details class="rounded-lg border border-surface-border bg-surface-raised/40 hover:border-accent/40 transition">
			<summary class="px-3 py-2 cursor-pointer text-xs text-muted hover:text-accent transition">⌥ view changes (git diff)</summary>
			<div class="px-3 pb-3 pt-1" hx-get="/fragments/workflow-diff?id=%s" hx-trigger="toggle from:closest details once" hx-swap="innerHTML">
				<div class="text-xs text-muted">Loading diff…</div>
			</div>
		</details>`, html.EscapeString(wf.ID))
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
			// Drop the stale "Suggested: <PROJECT>" line from older gate
			// questions — the project key isn't a repo, so it's just noise.
			qBody = stripBogusSuggestedLine(qBody, projectKeyOf(wf.TicketKey))

			fmt.Fprint(w, `<div class="rounded-xl border border-amber-500/40 bg-amber-500/5 p-4 space-y-3">
				<div class="flex items-center gap-2 text-[11px] uppercase tracking-wider text-amber-700 dark:text-amber-400 font-semibold">
					<span class="inline-block h-1.5 w-1.5 rounded-full bg-amber-500"></span>
					paused — awaiting your answer
					<span class="ml-auto font-mono normal-case tracking-normal text-muted">id `+html.EscapeString(q.ID)+`</span>
				</div>
				<div class="text-sm text-gray-800 dark:text-gray-200 whitespace-pre-line leading-relaxed">`+html.EscapeString(qBody)+`</div>`)

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
						<span class="text-[11px] text-muted">drag titles or rewrite — empty rows are dropped on save</span>
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
						<span class="font-mono text-xs text-muted w-6 text-right shrink-0">%d.</span>
						<input type="text" name="step" value="%s"
							class="flex-1 font-mono text-sm rounded-md border border-gray-200 dark:border-surface-border bg-white dark:bg-surface px-2 py-1 focus:border-accent focus:ring-1 focus:ring-accent/30 focus:outline-none">
						<button type="button" onclick="this.closest('li').remove()" title="remove step"
							class="text-xs text-muted hover:text-rose-500 transition px-2 py-1">✕</button>
					</li>`, i+1, html.EscapeString(ps.Title))
				}
				fmt.Fprintf(w, `</ol>
					<div class="flex items-center justify-between gap-2 pt-1">
						<button type="button"
							onclick="(function(ol){var li=document.createElement('li');li.className='flex items-center gap-2';li.innerHTML='<span class=\'font-mono text-xs text-muted w-6 text-right shrink-0\'>+</span><input type=\'text\' name=\'step\' placeholder=\'new step…\' class=\'flex-1 font-mono text-sm rounded-md border border-gray-200 dark:border-surface-border bg-white dark:bg-surface px-2 py-1 focus:border-accent focus:ring-1 focus:ring-accent/30 focus:outline-none\'><button type=\'button\' onclick=\'this.closest(&quot;li&quot;).remove()\' class=\'text-xs text-muted hover:text-rose-500 transition px-2 py-1\'>✕</button>';ol.appendChild(li);li.querySelector('input').focus();})(document.getElementById('plan-editor-%s'))"
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

			// One answer form wraps everything that posts to /api/answer.
			// CRITICAL: the repo picker's "use pick" button is a
			// type=submit — it only works if it lives INSIDE this form
			// (it used to be rendered outside, so it did nothing).
			fmt.Fprintf(w, `<form hx-post="/api/answer" hx-target="this" hx-swap="outerHTML" class="space-y-3">
				<input type="hidden" name="id" value="%s">`,
				html.EscapeString(q.ID))
			if pickButtons != "" {
				// Repo gate with a candidate menu: the picker (checkboxes
				// + "use pick" submit) is the ONLY action. No reject / yes /
				// send buttons — they confused users and weren't needed.
				fmt.Fprint(w, pickButtons)
				fmt.Fprint(w, `<div class="text-[11px] text-muted">Tick one or more repos above, then click “use pick”. Or use “↻ reset from start” to re-run from triage.</div>`)
			} else {
				// No repo menu (e.g. approve_plan, or a free-form gate):
				// keep the yes / no / free-form path.
				fmt.Fprint(w, `<div class="flex flex-col sm:flex-row gap-2">
					<input type="text" name="answer" autocomplete="off"
						placeholder="yes &nbsp;·&nbsp; no &nbsp;·&nbsp; change=/path/to/repo &nbsp;·&nbsp; free-form feedback"
						class="flex-1 font-mono text-sm rounded-lg border border-gray-300 dark:border-surface-border bg-white dark:bg-surface px-3 py-2 focus:border-accent focus:ring-2 focus:ring-accent/30 focus:outline-none">
					<div class="flex gap-2">
						<button type="submit" name="answer" value="yes" formnovalidate class="inline-flex items-center gap-1 rounded-lg bg-emerald-500 text-white px-3 py-2 text-sm font-semibold hover:bg-emerald-600 transition">yes</button>
						<button type="submit" name="answer" value="no" formnovalidate class="inline-flex items-center gap-1 rounded-lg border border-rose-500/40 bg-rose-500/5 text-rose-700 dark:text-rose-400 px-3 py-2 text-sm font-semibold hover:bg-rose-500/10 transition">no</button>
						<button type="submit" class="inline-flex items-center gap-1 rounded-lg bg-accent text-surface px-3 py-2 text-sm font-semibold hover:brightness-110 transition">send →</button>
					</div>
				</div>`)
			}
			fmt.Fprint(w, `</form>`)
			fmt.Fprint(w, `</div>`)
		} else {
			// Dangling reference: the workflow says it's awaiting an answer
			// but its question was answered or pruned and it never advanced
			// (classic symptom of the daemon being paused, or old state).
			// Without this the card showed NOTHING to answer — a dead end.
			// Offer a one-click reset to re-run from triage.
			fmt.Fprintf(w, `<div class="rounded-xl border border-amber-500/40 bg-amber-500/5 p-4 space-y-2">
				<div class="text-sm font-semibold text-amber-700 dark:text-amber-300">This approval can't be answered anymore</div>
				<div class="text-[12px] text-amber-700/80 dark:text-amber-300/80">The question goon was waiting on (%s) is no longer available — it was cleared or expired. Reset the workflow to re-run it from the start.</div>
				<form hx-post="/api/workflow/reset" hx-target="this" hx-swap="outerHTML"
					hx-confirm="Reset this workflow and re-run from triage?" hx-disabled-elt="find button" class="m-0 pt-1">
					<input type="hidden" name="wf_id" value="%s">
					<button type="submit" class="inline-flex items-center gap-1 rounded-lg bg-amber-500 text-surface px-3 py-2 text-sm font-semibold hover:brightness-110 transition disabled:opacity-50 disabled:cursor-wait">↻ reset &amp; re-run</button>
				</form>
			</div>`, html.EscapeString(wf.PendingQuestionID), html.EscapeString(wf.ID))
		}
	}

	// Plan steps — checklist.
	if len(wf.Plan) > 0 {
		planDone := 0
		for _, ps := range wf.Plan {
			if ps.Done {
				planDone++
			}
		}
		fmt.Fprintf(w, `<div>
			<div class="flex items-baseline justify-between mb-2">
				<h4 class="text-[11px] font-semibold uppercase tracking-wider text-gray-500">Plan (%d steps)</h4>
				<span class="text-[11px] font-mono text-muted">Step %d of %d</span>
			</div>
			<ol class="space-y-1.5">`, len(wf.Plan), planDone, len(wf.Plan))
		// Find the first undone step to mark as current.
		firstUndone := -1
		for i, ps := range wf.Plan {
			if !ps.Done {
				firstUndone = i
				break
			}
		}
		for i, ps := range wf.Plan {
			var mark, cls string
			switch {
			case ps.Done:
				mark = `<span class="shrink-0 font-mono text-emerald-700 dark:text-emerald-400 text-sm" title="done">✓</span>`
				cls = "text-gray-500 dark:text-gray-500 line-through"
			case i == firstUndone:
				mark = `<span class="shrink-0 font-mono text-accent text-sm" title="current">●</span>`
				cls = "text-gray-700 dark:text-gray-300 font-medium"
			default:
				mark = `<span class="shrink-0 font-mono text-muted text-sm" title="pending">○</span>`
				cls = "text-gray-600 dark:text-gray-400"
			}
			fmt.Fprintf(w, `<li class="flex items-start gap-2 text-sm %s">
				%s
				<div class="flex-1 min-w-0">
					<span class="font-mono text-xs text-muted mr-1">%d.</span>
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

	// Result — the task's outcome / answer (the agent's final message).
	// For non-code tickets (research, docs, summaries) this IS the
	// deliverable, so it gets a prominent emerald panel, not a footnote.
	if strings.TrimSpace(wf.Note) != "" {
		fmt.Fprintf(w, `<div class="rounded-xl border border-emerald-500/40 bg-emerald-500/5 p-4">
			<div class="flex items-center gap-2 text-[11px] uppercase tracking-wider text-emerald-700 dark:text-emerald-400 font-semibold mb-2">
				<svg class="h-3.5 w-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><polyline points="20 6 9 17 4 12"/></svg>
				result
			</div>
			<div class="text-sm text-gray-800 dark:text-gray-200 whitespace-pre-wrap leading-relaxed break-words">%s</div>
		</div>`, html.EscapeString(wf.Note))
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
					<span class="font-mono text-muted min-w-[88px]">%s</span>
					%s
					<span class="text-gray-700 dark:text-gray-300 truncate">%s</span>
				</li>`,
					html.EscapeString(humanizeSince(h.UpdatedAt)),
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
//     (live brand state — matches the daemon dot)
//   - planning phases (triaging/planning) → soft purple wash
//
// titleCaseState converts a snake_case workflow state like "awaiting_approval"
// to title-cased words with spaces like "Awaiting Approval". Does not use
// the deprecated strings.Title — instead manually capitalises each word.
func titleCaseState(s string) string {
	words := strings.Split(s, "_")
	for i, w := range words {
		if len(w) == 0 {
			continue
		}
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}

// workflowSortRank orders workflows for the list view: failures first
// (need attention), awaiting-approval next, then active mid-flight, then
// terminal/done last.
func workflowSortRank(s memory.WorkflowState) int {
	switch s {
	case memory.WFFailed:
		return 0
	case memory.WFAwaitingApproval:
		return 1
	case memory.WFDone:
		return 3
	default:
		return 2
	}
}

// workflowStateKey buckets a workflow state into one of four filter
// groups used by the Workflows-tab chips and client-side filtering.
func workflowStateKey(s memory.WorkflowState) string {
	switch s {
	case memory.WFFailed:
		return "failed"
	case memory.WFAwaitingApproval:
		return "awaiting"
	case memory.WFDone:
		return "done"
	default:
		return "active"
	}
}

func workflowStateChip(state string) string {
	cls := "bg-surface-raised text-muted border border-surface-border"
	switch state {
	case "done":
		cls = "bg-emerald-500/15 text-emerald-700 dark:text-emerald-400 border border-emerald-500/40"
	case "failed":
		cls = "bg-rose-500/15 text-rose-700 dark:text-rose-400 border border-rose-500/40"
	case "awaiting_approval":
		cls = "bg-highlight/15 text-highlight border border-highlight/40"
	case "executing", "testing", "verifying", "opening_pr", "notifying", "updating_memory":
		cls = "bg-accent/15 text-accent border border-accent/40"
	case "triaging", "planning":
		cls = "bg-accent-soft text-accent border border-accent/25"
	}
	return fmt.Sprintf(`<span class="inline-flex shrink-0 items-center rounded-full px-2 py-0.5 text-[11px] font-medium %s">%s</span>`,
		cls, html.EscapeString(titleCaseState(state)))
}

// fragQuestions renders goon's pending SELF-LEARNING questions — things goon
// wants to understand about the project or your preferences while on standby.
// Workflow approval gates (confirm_repo / approve_plan) are NOT shown here;
// they live on the Workflows tab beside the ticket they block.
func (s *Server) fragQuestions(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	pending := s.opts.Memory.PendingLearningQuestions()
	if len(pending) == 0 {
		_, _ = io.WriteString(w, emptyState("No questions right now ✓",
			"While idle, goon reviews recent changes and writes notes to LEARNED.md. When something's unclear, it asks you here."))
		return
	}
	fmt.Fprint(w, `<div class="mb-4 flex items-start gap-2.5 rounded-xl border border-sky-500/30 bg-sky-500/5 px-4 py-3">
		<svg class="h-4 w-4 text-sky-700 dark:text-sky-400 shrink-0 mt-0.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M9 18h6M10 22h4M12 2a7 7 0 0 0-4 12.7c.6.5 1 1.3 1 2.1V17h6v-.2c0-.8.4-1.6 1-2.1A7 7 0 0 0 12 2z"/></svg>
		<div class="text-xs text-muted leading-relaxed">
			<span class="text-ink font-medium">Goon is learning about your project.</span>
			Answers are saved to <code class="font-mono text-sky-700 dark:text-sky-300">LEARNED.md</code> and shape future runs. Ticket approvals live on the <button type="button" onclick="if(typeof showPage===&#39;function&#39;)showPage(&#39;workflows&#39;)" class="underline hover:text-accent">Workflows</button> tab.
		</div>
	</div>`)
	fmt.Fprint(w, `<div class="space-y-3">`)
	for i, q := range pending {
		qIDEsc := html.EscapeString(q.ID)
		seqNum := fmt.Sprintf("#%d", i+1)
		openAttr := ""
		if i == 0 {
			openAttr = " open" // first card expanded so the user can answer at once
		}
		preview := singleLineTrim(q.Question, 70)
		fmt.Fprintf(w, `<details%s data-question-id="%s" class="group relative overflow-hidden rounded-xl border border-sky-500/30 bg-surface-raised shadow-card hover:shadow-lift transition-shadow">
			<div class="absolute left-0 top-0 bottom-0 w-1 bg-sky-500/70 pointer-events-none"></div>
			<summary class="cursor-pointer list-none px-5 py-3 flex items-center gap-3 select-none">
				<span class="inline-flex items-center gap-1 rounded-full border border-sky-500/40 bg-sky-500/15 text-sky-700 dark:text-sky-300 px-2 py-0.5 text-[11px] font-semibold uppercase tracking-wider">
					<svg class="h-3 w-3" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><path d="M9 18h6M10 22h4M12 2a7 7 0 0 0-4 12.7c.6.5 1 1.3 1 2.1V17h6v-.2c0-.8.4-1.6 1-2.1A7 7 0 0 0 12 2z"/></svg>
					learning
				</span>
				<span class="flex-1 text-sm font-medium text-ink truncate">%s</span>
				<span class="text-[11px] font-mono text-muted/70 shrink-0">%s</span>
			</summary>
			<div class="border-t border-surface-border/60">
			<form hx-post="/api/answer" hx-target="closest details" hx-swap="outerHTML" class="px-5 py-4 space-y-3">
				<div class="text-sm text-ink whitespace-pre-line leading-relaxed">%s</div>
				<input type="hidden" name="id" value="%s">
				<textarea name="answer" rows="3" autocomplete="off"
					placeholder="teach goon… (saved to LEARNED.md)"
					class="w-full text-sm rounded-lg border border-surface-border bg-surface text-ink placeholder:text-muted/60 px-3 py-2 leading-relaxed focus:border-sky-400 focus:ring-2 focus:ring-sky-400/30 focus:outline-none resize-y"></textarea>
				<div class="flex items-center justify-end gap-2">
					<button type="submit" name="answer" value="(skipped)" formnovalidate
						class="inline-flex items-center gap-1 rounded-lg border border-surface-border text-muted px-3.5 py-2 text-sm font-medium hover:border-rose-400 hover:text-rose-400 transition">
						skip
					</button>
					<button type="submit"
						class="inline-flex items-center gap-1 rounded-lg bg-sky-600 text-white px-4 py-2 text-sm font-bold hover:bg-sky-500 transition shadow-card">
						<svg class="h-4 w-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><polyline points="20 6 9 17 4 12"/></svg>
						save answer
					</button>
				</div>
			</form>
			</div>
		</details>`, openAttr, qIDEsc, html.EscapeString(preview), seqNum, html.EscapeString(q.Question), html.EscapeString(q.ID))
	}
	fmt.Fprint(w, `</div>`)
}

// singleLineTrim collapses whitespace and truncates to max runes for compact
// previews (e.g. a learning question's collapsed summary line).
func singleLineTrim(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// fragQuestionsHistory renders the answered-question log (most recent first).
// Lazy-loaded once when the user switches to the History nav item.
func (s *Server) fragQuestionsHistory(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	all := s.opts.Memory.AllQuestions()
	var answered []memory.Question
	for _, q := range all {
		if !q.Pending() && q.IsLearning() {
			answered = append(answered, q)
		}
	}
	if len(answered) == 0 {
		_, _ = io.WriteString(w, emptyState("Nothing learned yet",
			"Answered learning questions are logged here (and in LEARNED.md). Approvals you've given live on the Workflows tab."))
		return
	}
	fmt.Fprintf(w, `<div class="mb-3 flex items-center gap-2">
		<span class="text-[10px] font-mono text-muted bg-surface-sunken px-2 py-0.5 rounded-full">%d answered</span>
	</div>`, len(answered))
	fmt.Fprint(w, `<div class="space-y-2">`)
	for _, q := range answered {
		gateLabel := "approval"
		ql := strings.ToLower(q.Question)
		switch {
		case strings.Contains(ql, "confirm_repo") || strings.Contains(ql, "repo path") || strings.Contains(ql, "which repo"):
			gateLabel = "confirm repo"
		case strings.Contains(ql, "approve") || strings.Contains(ql, "plan"):
			gateLabel = "approve plan"
		}
		ticketLabel := ""
		if q.TicketID != "" {
			ticketLabel = fmt.Sprintf(`<span class="font-mono text-[10px] text-muted">%s</span> ·`, html.EscapeString(q.TicketID))
		}
		answerTone := "text-emerald-700 dark:text-emerald-400"
		if strings.ToLower(strings.TrimSpace(q.Answer)) == "no" {
			answerTone = "text-rose-700 dark:text-rose-400"
		}
		when := ""
		if !q.AnsweredAt.IsZero() {
			when = humanizeSince(q.AnsweredAt)
		}
		fmt.Fprintf(w, `<div class="rounded-lg border border-surface-border bg-surface-raised px-4 py-3 space-y-1.5">
			<div class="flex items-center gap-2 flex-wrap text-[11px] text-muted">
				%s
				<span>%s</span>
				<span class="ml-auto">%s</span>
			</div>
			<div class="text-xs text-ink leading-relaxed line-clamp-2" title="%s">%s</div>
			<div class="flex items-center gap-2 pt-0.5">
				<span class="text-[10px] font-semibold %s">↳ %s</span>
			</div>
		</div>`,
			ticketLabel, html.EscapeString(gateLabel), html.EscapeString(when),
			html.EscapeString(q.Question), html.EscapeString(truncateStr(q.Question, 200)),
			answerTone, html.EscapeString(q.Answer),
		)
	}
	fmt.Fprint(w, `</div>`)
}

// truncateStr trims s to at most n runes, appending "…" if cut.
func truncateStr(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

// renderRepoPickButtons scans the question body for the numbered menu
// format that buildRepoGateQuestion in internal/workflow emits:
//
//  1. repo-a
//     * 2. repo-b              (the "*" marks the suggested one)
//  3. repo-c
//  4. owner/svc (remote)  (remote-tagged entries)
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
	showFilter := len(opts) > initialVisibleOthers

	// Unique form id so multiple pending questions on one page don't
	// collide on their JS hooks.
	mid := fmt.Sprintf("ms%d", time.Now().UnixNano())

	var sb strings.Builder
	sb.WriteString(`<div class="space-y-2 pt-1">`)
	fmt.Fprintf(&sb, `<div class="text-[11px] uppercase tracking-wider text-muted">Choose repos for this ticket:</div>`)
	if showFilter {
		fmt.Fprintf(&sb, `<input type="text" data-pick-filter="%s" placeholder="filter repos by name…" autocomplete="off"
			class="w-full rounded-lg border border-surface-border bg-surface px-3 py-1.5 text-sm focus:border-accent focus:outline-none"
			oninput="goonPickRefresh('%s')">`, mid, mid)
	}
	fmt.Fprintf(&sb, `<div class="flex flex-wrap gap-2" data-pick-group="%s">`, mid)
	for _, o := range opts {
		cls := "border-surface-border bg-surface text-muted hover:border-accent hover:text-ink"
		// Leading marker per pill. A ★ for suggested replaces the old
		// "suggested" word badge — eye-trackable in a long list, no
		// horizontal whitespace cost.
		marker := `<span class="text-xs opacity-30" aria-hidden="true">·</span>`
		if o.isSug {
			cls = "border-accent/50 bg-accent-soft text-accent hover:bg-accent hover:text-white"
			marker = `<span class="text-amber-700 dark:text-amber-400" aria-label="suggested by triage" title="suggested by triage">★</span>`
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
	fmt.Fprintf(&sb, `<div class="flex flex-wrap items-center gap-2 pt-1">
		<button type="submit" name="answer" data-pick-submit="%s" formnovalidate disabled
			class="inline-flex items-center gap-1.5 rounded-lg bg-accent text-white px-3 py-1.5 text-sm font-bold opacity-40 cursor-not-allowed transition">
			<span data-pick-label="%s">pick a repo</span>
		</button>
		<label class="inline-flex items-center gap-1.5 text-[11px] text-muted cursor-pointer select-none">
			<input type="checkbox" data-pick-remember="%s" class="h-3.5 w-3.5 accent-accent" onchange="goonPickToggle('%s')">
			remember for this project (auto-confirm next time)
		</label>
		<span class="text-[11px] text-muted" data-pick-summary="%s"></span>
	</div>`, mid, mid, mid, mid, mid)
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
				var rem = document.querySelector('input[data-pick-remember="' + mid + '"]');
				var prefix = (rem && rem.checked) ? 'remember ' : '';
				btn.value = prefix + nums.join(',');
				lbl.textContent = (nums.length === 1 ? 'use pick' : 'use ' + nums.length + ' picks') + (prefix ? ' + remember' : '') + ' →';
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
// extractSuggestedRepo parses a confirm_repo question body and returns the
// name of the suggested repo (the line prefixed with "*"). Returns "" when
// no suggestion is found (e.g. free-text or old format without a menu).
// projectKeyOf extracts the board project key from a ticket key:
// "EB-4232" → "EB". Returns "" when there's no "-" (so it never matches
// a real repo by accident). Used to detect the bogus case where a repo
// "suggestion" is really just the project key.
// stripBogusSuggestedLine removes a "Suggested: X" line when X is just
// the board project key (not a real repo) — leftover in gate questions
// stored before goon stopped suggesting project keys.
func stripBogusSuggestedLine(body, projectKey string) string {
	if projectKey == "" {
		return body
	}
	lines := strings.Split(body, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		if strings.EqualFold(strings.TrimSpace(ln), "Suggested: "+projectKey) {
			continue
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}

func projectKeyOf(ticketKey string) string {
	if i := strings.LastIndexByte(ticketKey, '-'); i > 0 {
		return ticketKey[:i]
	}
	return ""
}

// repoSuggestionFor returns a real repo suggestion for a confirm_repo
// workflow, or "" when goon has none (no triage pick, or the only value
// is the board project key — which is not a repo). Shared by the bulk
// banner and the bulk-approve guard so they agree on what's approvable.
func (s *Server) repoSuggestionFor(wf memory.Workflow) string {
	suggested := ""
	if wf.PendingQuestionID != "" {
		if q, ok := s.opts.Memory.GetQuestion(wf.PendingQuestionID); ok {
			suggested = extractSuggestedRepo(q.Question)
		}
	}
	if suggested == "" {
		suggested = strings.TrimSpace(wf.Repo)
	}
	key := wf.TicketKey
	if key == "" {
		key = wf.TicketID
	}
	if suggested == "" || strings.EqualFold(suggested, projectKeyOf(key)) {
		return ""
	}
	return suggested
}

func extractSuggestedRepo(question string) string {
	for _, raw := range strings.Split(question, "\n") {
		trimmed := strings.TrimLeft(strings.TrimRight(raw, " \t"), " \t")
		if !strings.HasPrefix(trimmed, "*") {
			continue
		}
		rest := strings.TrimSpace(trimmed[1:]) // strip the "*"
		// rest looks like "2. owner/repo" or "2. owner/repo (remote)"
		dot := strings.IndexByte(rest, '.')
		if dot > 0 {
			rest = strings.TrimSpace(rest[dot+1:])
		}
		// Strip trailing annotations like "(remote)" or "  ← suggested"
		if idx := strings.IndexByte(rest, '('); idx > 0 {
			rest = strings.TrimSpace(rest[:idx])
		}
		if rest != "" {
			return rest
		}
	}
	return ""
}

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
	return cleaned
}

// emptyState is the standardized empty-list panel — title + helpful hint.
// A dashed accent-purple border + a soft surface-raised wash signals
// "this slot is ready for content" without screaming.
func emptyState(title, hint string) string {
	return fmt.Sprintf(`<div class="py-12 text-center">
		<div class="text-sm font-medium text-ink/70">%s</div>
		<div class="mt-1.5 text-xs text-muted max-w-md mx-auto leading-relaxed">%s</div>
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
	return fmt.Sprintf(`<div class="mb-6 flex items-start justify-between gap-4 flex-wrap">
		<div>
			<h2 class="text-lg font-semibold text-ink">%s</h2>
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

// fragTabQuestions — two-column layout: left sidebar nav (Pending / History),
// right panel that shows the active list. The Pending panel auto-refreshes
// via SSE so new questions appear without a manual reload.
func (s *Server) fragTabQuestions(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	pending := s.opts.Memory.PendingLearningQuestions()
	answered := 0
	for _, q := range s.opts.Memory.AllQuestions() {
		if !q.Pending() && q.IsLearning() {
			answered++
		}
	}

	// ── Two-column layout ────────────────────────────────────────────────
	fmt.Fprint(w, `<div class="flex gap-6 min-h-[420px]">`)

	// ── Left sidebar nav ─────────────────────────────────────────────────
	fmt.Fprint(w, `<nav class="w-40 shrink-0 flex flex-col">
		<div class="mb-3 px-2 text-[10px] uppercase tracking-wider font-semibold text-muted/60">Questions</div>`)

	// Pending nav item — amber when items exist, neutral when clear
	pendingTone := "text-muted hover:text-ink hover:bg-surface-raised/60"
	pendingActive := "bg-highlight/10 text-highlight font-semibold"
	pendingBadgeTone := "bg-highlight/20 text-highlight"
	if len(pending) == 0 {
		pendingBadgeTone = "bg-surface-sunken text-muted"
	}
	pendingBadge := fmt.Sprintf(`<span class="ml-auto text-[10px] font-mono %s px-1.5 py-0.5 rounded-full shrink-0">%d</span>`, pendingBadgeTone, len(pending))
	_ = pendingTone // active by default
	fmt.Fprintf(w, `<button type="button" data-q-nav="pending" onclick="goonQNav('pending')"
		class="w-full text-left px-3 py-2 mb-0.5 rounded-lg text-sm flex items-center gap-2.5 transition %s">
		<svg class="h-3.5 w-3.5 shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
			<circle cx="12" cy="12" r="4"/>
			<path d="M12 2v2M12 20v2M4.93 4.93l1.41 1.41M17.66 17.66l1.41 1.41M2 12h2M20 12h2M4.93 19.07l1.41-1.41M17.66 6.34l1.41-1.41"/>
		</svg>
		<span>Pending</span>%s
	</button>`, pendingActive, pendingBadge)

	answeredBadge := fmt.Sprintf(`<span class="ml-auto text-[10px] font-mono bg-surface-sunken text-muted px-1.5 py-0.5 rounded-full shrink-0">%d</span>`, answered)
	fmt.Fprintf(w, `<button type="button" data-q-nav="history" onclick="goonQNav('history')"
		class="w-full text-left px-3 py-2 mb-0.5 rounded-lg text-sm flex items-center gap-2.5 transition text-muted hover:text-ink hover:bg-surface-raised/60">
		<svg class="h-3.5 w-3.5 shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
			<circle cx="12" cy="12" r="10"/>
			<polyline points="12 6 12 12 16 14"/>
		</svg>
		<span>History</span>%s
	</button>`, answeredBadge)

	fmt.Fprint(w, `</nav>`)

	// ── Right content area ───────────────────────────────────────────────
	fmt.Fprint(w, `<div class="flex-1 min-w-0">`)

	// Pending panel — default visible, auto-refreshes via SSE
	fmt.Fprint(w, `<div data-q-panel="pending">
		<div hx-get="/fragments/questions" hx-trigger="load, questionsChanged from:body" hx-swap="morph">
			<div class="space-y-2"><div class="skel h-4 w-40"></div><div class="skel h-16 w-full"></div></div>
		</div>
	</div>`)

	// History panel — hidden, lazy-loads on first switch
	fmt.Fprint(w, `<div data-q-panel="history" class="hidden">
		<div id="questions-history-container"
			hx-get="/fragments/questions-history"
			hx-trigger="revealed"
			hx-swap="innerHTML">
			<div class="text-sm text-gray-500">Loading history…</div>
		</div>
	</div>`)

	fmt.Fprint(w, `</div>`) // end right content
	fmt.Fprint(w, `</div>`) // end flex

	// ── JS ───────────────────────────────────────────────────────────────
	fmt.Fprint(w, `<script>
	(function() {
		window.goonQNav = function(target) {
			document.querySelectorAll('[data-q-panel]').forEach(function(el) {
				el.classList.toggle('hidden', el.dataset.qPanel !== target);
			});
			document.querySelectorAll('[data-q-nav]').forEach(function(btn) {
				var active = btn.dataset.qNav === target;
				if (target === 'pending') {
					btn.classList.toggle('bg-highlight/10', active);
					btn.classList.toggle('text-highlight', active);
				} else {
					btn.classList.toggle('bg-accent/10', active);
					btn.classList.toggle('text-accent', active);
				}
				btn.classList.toggle('font-semibold', active);
				btn.classList.toggle('text-muted', !active);
				if (active) btn.setAttribute('aria-current', 'page');
				else btn.removeAttribute('aria-current');
			});
			// Trigger HTMX reveal on history container so it lazy-loads once
			if (target === 'history') {
				var hc = document.getElementById('questions-history-container');
				if (hc && !hc.dataset.loaded) {
					hc.dataset.loaded = '1';
					htmx.trigger(hc, 'revealed');
				}
			}
		};
	})();
	</script>`)
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
		<div class="space-y-2"><div class="skel h-4 w-40"></div><div class="skel h-20 w-full"></div><div class="skel h-20 w-full"></div></div>
	</div>`)
	// Scheduled automations fleet — user-added workflows that run on a timer
	// (digests, health checks, anything), separate from the board pipeline above.
	fmt.Fprint(w, `<div class="mt-7" hx-get="/fragments/automations" hx-trigger="load, automationsChanged from:body" hx-swap="innerHTML">
		<div class="space-y-2"><div class="skel h-4 w-44"></div><div class="skel h-16 w-full"></div></div>
	</div>`)
}

// fragTabTickets — full ticket table + client-side filter + refresh
// button. The board mirror, unfiltered by default.
func (s *Server) fragTabTickets(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, pageHeader("Tickets",
		"Live mirror of the configured board. Click <code class=\"font-mono text-xs\">⋯ actions</code> on any row to comment, transition, or edit the ticket directly.",
		refreshButton()))
	// Filter + action bar.
	fmt.Fprint(w, `<div class="flex flex-wrap items-center gap-3 mb-3 p-3 rounded-lg bg-gray-50 dark:bg-surface-sunken border border-gray-200 dark:border-surface-border">
		<div class="relative flex-1 min-w-[200px]">
			<svg class="absolute left-3 top-1/2 -translate-y-1/2 h-4 w-4 text-muted" viewBox="0 0 20 20" fill="currentColor"><path fill-rule="evenodd" d="M9 3.5a5.5 5.5 0 100 11 5.5 5.5 0 000-11zM2 9a7 7 0 1112.452 4.391l3.328 3.329a.75.75 0 11-1.06 1.06l-3.329-3.328A7 7 0 012 9z" clip-rule="evenodd"/></svg>
			<input id="ticket-filter" type="text" placeholder="filter tickets…"
				class="w-full pl-9 pr-3 py-1.5 text-sm rounded-md border border-gray-300 dark:border-surface-border bg-white dark:bg-surface focus:border-accent focus:ring-2 focus:ring-accent/30 focus:outline-none"
				oninput="filterTickets(this.value)">
		</div>
		<div class="flex items-center gap-2 text-xs">
			<span class="text-gray-500">status:</span>
			<select id="ticket-status-filter" title="Filter tickets" onchange="filterTickets(document.getElementById('ticket-filter').value)"
				class="rounded-md border border-gray-300 dark:border-surface-border bg-white dark:bg-surface px-2 py-1 focus:border-accent focus:outline-none">
				<option value="">all</option>
				<option value="open">open</option>
				<option value="in_progress">in progress</option>
				<option value="in_review">in review</option>
				<option value="blocked">blocked</option>
				<option value="done">done</option>
			</select>
		</div>
		<div class="flex items-center gap-2 ml-auto">
			<button
				hx-get="/fragments/jira-filter"
				hx-target="#ticket-panels"
				hx-swap="innerHTML"
				class="inline-flex items-center gap-1.5 rounded-md border border-surface-border bg-surface px-3 py-1.5 text-xs font-medium hover:border-accent/50 hover:text-accent transition">
				<svg class="h-3.5 w-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="3"/><path d="M19.07 4.93a10 10 0 0 1 0 14.14M4.93 4.93a10 10 0 0 0 0 14.14"/></svg>
				Jira setup
			</button>
			<button
				onclick="document.getElementById('create-ticket-panel').classList.toggle('hidden')"
				class="inline-flex items-center gap-1.5 rounded-md border border-accent/40 bg-accent/10 px-3 py-1.5 text-xs font-medium text-accent hover:bg-accent/20 transition">
				<svg class="h-3.5 w-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><line x1="12" y1="5" x2="12" y2="19"/><line x1="5" y1="12" x2="19" y2="12"/></svg>
				create ticket
			</button>
		</div>
		<div class="w-full text-xs text-gray-500"><span id="ticket-count">—</span></div>
	</div>

	<!-- inline panels: Jira setup or create-ticket form swap in here -->
	<div id="ticket-panels"></div>

	<!-- create-ticket form (hidden by default) -->
	<div id="create-ticket-panel" class="hidden mb-3 rounded-lg border border-surface-border bg-surface p-4 space-y-3">
		<div class="flex items-start justify-between">
			<div>
				<span class="text-xs font-semibold text-accent">New task for goon</span>
				<div class="text-[11px] text-muted mt-0.5">A local task — goon picks it up and works it. No board needed; research/doc tasks don't even need a repo.</div>
			</div>
			<button onclick="document.getElementById('create-ticket-panel').classList.add('hidden')"
				class="text-[11px] text-gray-500 hover:text-accent">✕</button>
		</div>
		<form hx-post="/api/ticket/create" hx-target="#create-ticket-result" hx-swap="innerHTML"
			hx-on::after-request="if(event.detail.successful){this.reset();document.getElementById('create-ticket-panel').classList.add('hidden');}"
			class="space-y-2">
			<div>
				<label class="block text-[11px] text-muted mb-0.5">Title <span class="text-red-400">*</span></label>
				<input type="text" name="title" required placeholder="e.g. Summarize what the auth module does"
					class="w-full text-[12px] rounded border border-surface-border bg-surface-sunken px-3 py-1.5 focus:outline-none focus:border-accent/60 text-gray-200">
			</div>
			<div>
				<label class="block text-[11px] text-muted mb-0.5">Description</label>
				<textarea name="description" rows="3" placeholder="What should goon do? e.g. 'Read internal/auth and write a short overview of how login works' — add any detail or acceptance criteria."
					class="w-full text-[12px] rounded border border-surface-border bg-surface-sunken px-3 py-1.5 focus:outline-none focus:border-accent/60 text-gray-200 resize-y"></textarea>
			</div>
			<div class="flex flex-wrap gap-3">
				<div class="flex-1 min-w-[140px]">
					<label class="block text-[11px] text-muted mb-0.5">Priority</label>
					<select name="priority"
						class="w-full rounded border border-surface-border bg-surface-sunken px-2 py-1.5 text-[12px] focus:outline-none focus:border-accent/60 text-gray-200">
						<option value="">—</option>
						<option value="high">high</option>
						<option value="medium">medium</option>
						<option value="low">low</option>
					</select>
				</div>
				<div class="flex-1 min-w-[180px]">
					<label class="block text-[11px] text-muted mb-0.5">Labels <span class="text-[10px] text-muted">(comma-separated)</span></label>
					<input type="text" name="labels" placeholder="backend, bug, sprint-42"
						class="w-full text-[12px] rounded border border-surface-border bg-surface-sunken px-3 py-1.5 focus:outline-none focus:border-accent/60 text-gray-200">
				</div>
			</div>
			<div class="flex items-center gap-2 pt-1">
				<button type="submit"
					class="text-[11px] px-4 py-1.5 rounded bg-accent/20 hover:bg-accent/30 text-accent border border-accent/30 transition font-medium">
					create
				</button>
				<span id="create-ticket-result" class="text-[10px] text-gray-500 ml-1"></span>
			</div>
		</form>
	</div>

	<div hx-get="/fragments/tickets" hx-trigger="load, ticketsChanged from:body" hx-swap="morph">
		<div class="space-y-2"><div class="skel h-4 w-40"></div><div class="skel h-16 w-full"></div><div class="skel h-16 w-full"></div></div>
	</div>

	<!-- local (goon-native) tickets -->
	<div hx-get="/fragments/local-tickets" hx-trigger="load, ticketsChanged from:body" hx-swap="innerHTML">
		<div class="text-sm text-gray-500 mt-4">Loading goon tickets…</div>
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
		<div class="space-y-2"><div class="skel h-4 w-40"></div><div class="skel h-12 w-full"></div><div class="skel h-12 w-full"></div></div>
	</div>`)
}

// commafy renders an int64 with thousands separators (e.g. 1234567 →
// "1,234,567") for readable token counts.
func commafy(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// fragUsage renders the token-usage card: a grand total plus a per-model
// breakdown, sourced from the process-wide usage meter (persisted to
// ./storage/usage.json). Auto-refreshed by the dashboard.
func (s *Server) fragUsage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	meter := usage.Global()
	stats := meter.Snapshot()
	calls, prompt, completion := meter.Totals()
	total := prompt + completion

	fmt.Fprint(w, `<section class="rounded-xl border border-gray-200 dark:border-surface-border bg-white dark:bg-surface-raised p-4">
		<div class="flex items-center justify-between mb-3">
			<h3 class="text-sm font-semibold text-gray-700 dark:text-gray-300 flex items-center gap-2">
				<svg class="h-4 w-4 text-accent" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 2L2 7l10 5 10-5-10-5z"/><path d="M2 17l10 5 10-5"/><path d="M2 12l10 5 10-5"/></svg>
				Token usage
			</h3>
			<span class="text-[11px] text-muted font-mono">`+html.EscapeString(humanizeSince(meter.UpdatedAt()))+`</span>
		</div>`)

	if total == 0 {
		fmt.Fprint(w, `<div class="rounded-lg border border-dashed border-gray-300 dark:border-surface-border bg-gray-50/60 dark:bg-surface-sunken/40 p-5 text-center text-sm text-gray-500">No tokens spent yet. Counts appear here once goon makes its first model call.</div></section>`)
		return
	}

	// Grand-total strip.
	fmt.Fprintf(w, `<div class="grid grid-cols-3 gap-2 mb-3">
		<div class="rounded-lg bg-accent/5 border border-accent/20 px-3 py-2"><div class="text-[10px] uppercase tracking-wider text-gray-500">total tokens</div><div class="text-lg font-bold text-accent">%s</div></div>
		<div class="rounded-lg bg-gray-50 dark:bg-surface-sunken border border-gray-200 dark:border-surface-border px-3 py-2"><div class="text-[10px] uppercase tracking-wider text-gray-500">prompt</div><div class="text-sm font-semibold text-gray-700 dark:text-gray-300">%s</div></div>
		<div class="rounded-lg bg-gray-50 dark:bg-surface-sunken border border-gray-200 dark:border-surface-border px-3 py-2"><div class="text-[10px] uppercase tracking-wider text-gray-500">completion</div><div class="text-sm font-semibold text-gray-700 dark:text-gray-300">%s</div></div>
	</div>`, commafy(total), commafy(prompt), commafy(completion))

	// Per-model table.
	fmt.Fprintf(w, `<div class="text-[11px] text-gray-500 mb-1">%s call(s) across %d model(s)</div>`, commafy(calls), len(stats))
	fmt.Fprint(w, `<div class="overflow-hidden rounded-lg border border-gray-200 dark:border-surface-border">
		<table class="w-full text-sm">
			<thead><tr class="bg-gray-50 dark:bg-surface-sunken text-[10px] uppercase tracking-wider text-gray-500">
				<th class="text-left font-medium px-3 py-1.5">model</th>
				<th class="text-right font-medium px-3 py-1.5">calls</th>
				<th class="text-right font-medium px-3 py-1.5">prompt</th>
				<th class="text-right font-medium px-3 py-1.5">completion</th>
				<th class="text-right font-medium px-3 py-1.5">total</th>
			</tr></thead><tbody class="divide-y divide-gray-100 dark:divide-surface-border/60">`)
	for _, st := range stats {
		fmt.Fprintf(w, `<tr>
			<td class="px-3 py-1.5 font-mono text-xs text-gray-700 dark:text-gray-300 truncate max-w-[160px]">%s</td>
			<td class="px-3 py-1.5 text-right font-mono text-xs text-gray-500">%s</td>
			<td class="px-3 py-1.5 text-right font-mono text-xs text-gray-500">%s</td>
			<td class="px-3 py-1.5 text-right font-mono text-xs text-gray-500">%s</td>
			<td class="px-3 py-1.5 text-right font-mono text-xs font-semibold text-gray-700 dark:text-gray-300">%s</td>
		</tr>`,
			html.EscapeString(st.Model), commafy(st.Calls), commafy(st.PromptTokens),
			commafy(st.CompletionTokens), commafy(st.TotalTokens()))
	}
	fmt.Fprint(w, `</tbody></table></div></section>`)
}

// fragSessions renders the live-sessions card. Two sources, because "what is
// goon doing right now" spans both:
//   - in-flight model calls anywhere in THIS process (PR-review drafts, chat,
//     workflow stages, the standby reflection) via usage.ActiveActivities().
//     This is the universal view — same chokepoint as token tracking — so any
//     model work shows up, not just spawned subprocesses.
//   - queued/running child agents from the parallel pool (spawn_agents),
//     which run as separate processes.
func (s *Server) fragSessions(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<section class="rounded-xl border border-gray-200 dark:border-surface-border bg-white dark:bg-surface-raised p-4">
		<div class="flex items-center justify-between mb-3">
			<h3 class="text-sm font-semibold text-gray-700 dark:text-gray-300 flex items-center gap-2">
			<svg class="h-4 w-4 text-accent" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="3"/><path d="M12 1v4M12 19v4M4.9 4.9l2.8 2.8M16.3 16.3l2.8 2.8M1 12h4M19 12h4M4.9 19.1l2.8-2.8M16.3 7.7l2.8-2.8"/></svg>
				Live sessions
			</h3>
		</div>`)

	activities := usage.ActiveActivities()

	var poolActive []agentpool.Agent
	if pool, err := agentpool.Global(); err == nil && pool != nil {
		for _, a := range pool.List() {
			if a.State == agentpool.StateRunning || a.State == agentpool.StateQueued {
				poolActive = append(poolActive, a)
			}
		}
	}

	if len(activities) == 0 && len(poolActive) == 0 {
		fmt.Fprint(w, `<div class="rounded-lg border border-dashed border-gray-300 dark:border-surface-border bg-gray-50/60 dark:bg-surface-sunken/40 p-5 text-center text-sm text-gray-500">Idle — no model work in flight. Drafting a PR review, chatting, or a running workflow will appear here live.</div></section>`)
		return
	}

	fmt.Fprint(w, `<ul class="space-y-2">`)

	// In-flight (and just-finished, lingering) model calls in this process.
	for _, a := range activities {
		border, bg, dot, label := "border-accent/30", "bg-accent/5", "bg-accent animate-pulse-dot", "text-accent"
		state := "working"
		when := humanizeSince(a.StartedAt)
		if !a.Running() {
			border, bg, dot, label = "border-gray-200 dark:border-surface-border", "bg-gray-50/60 dark:bg-surface-sunken/40", "bg-gray-400", "text-gray-500"
			state = "done"
			when = "just now"
		}
		fmt.Fprintf(w, `<li class="rounded-lg border %s %s p-3">
			<div class="flex items-center gap-2 mb-1">
				<span class="inline-block h-2 w-2 rounded-full %s shrink-0"></span>
				<span class="text-[11px] font-semibold uppercase tracking-wider %s">%s</span>
				<span class="font-mono text-[11px] text-muted">%s</span>
				<span class="ml-auto text-[11px] text-muted">%s</span>
			</div>
			<div class="text-sm text-gray-700 dark:text-gray-300">%s</div>
		</li>`,
			border, bg, dot, label, state,
			html.EscapeString(a.Model),
			html.EscapeString(when),
			html.EscapeString(singleLineTrim(a.Label, 90)))
	}

	// Parallel child agents (subprocesses).
	for _, a := range poolActive {
		dot := "bg-sky-500 animate-pulse-dot"
		stateLabel := "agent · running"
		elapsed := humanizeSince(a.StartedAt)
		if a.State == agentpool.StateQueued {
			dot = "bg-gray-400"
			stateLabel = "agent · queued"
			elapsed = "waiting"
		}
		fmt.Fprintf(w, `<li class="rounded-lg border border-sky-500/30 bg-sky-500/5 p-3">
			<div class="flex items-center gap-2 mb-1">
				<span class="inline-block h-2 w-2 rounded-full %s shrink-0"></span>
				<span class="text-[11px] font-semibold uppercase tracking-wider text-sky-600 dark:text-sky-400">%s</span>
				<span class="font-mono text-[11px] text-muted">%s</span>
				<span class="ml-auto text-[11px] text-muted">%s</span>
			</div>
			<div class="text-sm text-gray-700 dark:text-gray-300">%s</div>
		</li>`, dot, stateLabel, html.EscapeString(a.ID), html.EscapeString(elapsed), html.EscapeString(singleLineTrim(a.Task, 90)))
	}

	fmt.Fprint(w, `</ul></section>`)
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
// llmConfigured reports whether an LLM provider looks usable from the
// current environment/config — used by the first-run readiness card.
// Ollama needs no key; every other provider needs its API key present.
func llmConfigured() bool {
	switch strings.ToLower(strings.TrimSpace(getenv("GOON_LLM_PROVIDER"))) {
	case "ollama":
		return true
	case "anthropic":
		return getenv("ANTHROPIC_API_KEY") != ""
	case "gemini":
		return getenv("GEMINI_API_KEY") != "" || getenv("GOOGLE_API_KEY") != ""
	case "openai", "":
		return getenv("OPENAI_API_KEY") != ""
	default:
		return getenv("OPENAI_API_KEY") != "" || getenv("ANTHROPIC_API_KEY") != "" ||
			getenv("GEMINI_API_KEY") != "" || getenv("GOOGLE_API_KEY") != ""
	}
}

// errorClassHint maps a daemon error class to a plain-English fix shown
// in the Home failure banner. Mirrors the daemon-side hint (kept in sync
// manually — the daemon package isn't importable here without a cycle).
func errorClassHint(class string) string {
	switch class {
	case "network":
		return "Can't reach the provider/board — check the URL and that the service (or proxy) is running."
	case "auth":
		return "Authentication failed — the API key or token looks wrong or expired. Update it in Setup."
	case "rate_limit":
		return "Rate limit or quota hit — goon backs off and retries; check your plan if it persists."
	case "model":
		return "The provider returned a server error — usually transient; goon will retry."
	case "config":
		return "A required provider/board isn't configured yet — finish Setup."
	default:
		return ""
	}
}

// healthPill renders one status chip for the Home health widget. tone is
// one of green/amber/red/gray. Each tone carries a small nautical glyph
// (ship = healthy, flag = needs attention, storm = down, anchor = idle)
// next to the unchanged plain-text state, so the meaning never relies on
// color alone.
func healthPill(label, state, tone string) string {
	var border, bg, text, glyph string
	switch tone {
	case "green":
		border, bg, text = "border-emerald-500/40", "bg-emerald-500/10", "text-emerald-700 dark:text-emerald-400"
		glyph = `<span class="h-1.5 w-1.5 rounded-full bg-emerald-500 shrink-0"></span>`
	case "amber":
		border, bg, text = "border-amber-500/40", "bg-amber-500/10", "text-amber-700 dark:text-amber-400"
		glyph = `<span class="h-1.5 w-1.5 rounded-full bg-amber-500 shrink-0"></span>`
	case "red":
		border, bg, text = "border-rose-500/40", "bg-rose-500/10", "text-rose-700 dark:text-rose-400"
		glyph = `<span class="h-1.5 w-1.5 rounded-full bg-rose-500 shrink-0"></span>`
	default:
		border, bg, text = "border-surface-border", "bg-surface-raised", "text-muted"
		glyph = `<span class="h-1.5 w-1.5 rounded-full bg-surface-border shrink-0"></span>`
	}
	return fmt.Sprintf(`<span class="inline-flex items-center gap-1.5 rounded-full border %s %s px-2.5 py-1 text-[11px] %s">
		%s%s <span class="opacity-70">%s</span></span>`,
		border, bg, text, glyph, html.EscapeString(label), html.EscapeString(state))
}

// firstTicketHint returns the contextual sub-label for the readiness
// card's final ("first ticket") step, based on what's still missing.
func firstTicketHint(llmReady, boardReady bool, st memory.DaemonStatus) string {
	if !llmReady {
		return "Add an LLM provider first, then create a task (Tickets tab) or connect a board."
	}
	if !boardReady {
		return "Create a local task on the Tickets tab (no board needed) — goon picks it up on the next poll."
	}
	if st.Paused {
		return "Everything's connected — resume the daemon (bottom-left) to start polling."
	}
	return "Everything's connected — goon is polling your board. The first ticket shows up here shortly."
}

func (s *Server) fragTabDashboard(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	mem := s.opts.Memory
	st := mem.GetStatus()
	// Only blocking workflow gates belong on the dashboard's "blocking
	// questions" surface; learning questions are non-blocking and live on
	// the Questions tab (surfaced via the banner instead).
	pending := mem.PendingGateQuestions()
	wfs := mem.ListWorkflows(50)
	tix := mem.ListTickets()

	// Readiness signals, computed once and shared by the health widget +
	// the first-run card below.
	llmReady := llmConfigured()
	// "Work to do" is satisfied by a connected board OR ≥1 local ticket —
	// a board is no longer required to get value (local tickets run too).
	localCount := len(mem.ListLocalTickets())
	boardReady := strings.TrimSpace(getenv("GOON_BOARD")) != "" || st.BoardName != "" || localCount > 0
	hostReady := strings.TrimSpace(getenv("GOON_GIT_HOST")) != "" || st.HostName != ""
	activated := len(tix) > 0 || len(wfs) > 0 || localCount > 0

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

	// --- Health widget (always on) -----------------------------------
	// Green/red at-a-glance for the four things goon needs: LLM provider,
	// board, git host (optional), daemon. Truthful — reflects both
	// configuration AND the live circuit-breaker class.
	providerProblem := st.ErrorClass == "auth" || st.ErrorClass == "network" ||
		st.ErrorClass == "rate_limit" || st.ErrorClass == "model"
	provTone, provState := "red", "not set"
	switch {
	case providerProblem:
		provTone, provState = "red", st.ErrorClass
	case llmReady:
		provTone, provState = "green", "ready"
	}
	boardTone, boardState := "red", "not set"
	if boardReady {
		boardTone, boardState = "green", "ready"
	}
	hostTone, hostState := "gray", "optional"
	if hostReady {
		hostTone, hostState = "green", "ready"
	}
	daemonTone2, daemonState2 := "gray", "stopped"
	switch {
	case st.AutoPaused:
		daemonTone2, daemonState2 = "red", "paused (auto)"
	case st.Paused:
		daemonTone2, daemonState2 = "amber", "paused"
	case st.Running:
		daemonTone2, daemonState2 = "green", "running"
	}
	fmt.Fprint(w, `<div class="mb-4 flex flex-wrap items-center gap-2">`)
	fmt.Fprint(w, healthPill("LLM", provState, provTone))
	fmt.Fprint(w, healthPill("Board", boardState, boardTone))
	fmt.Fprint(w, healthPill("Git host", hostState, hostTone))
	fmt.Fprint(w, healthPill("Daemon", daemonState2, daemonTone2))
	fmt.Fprint(w, `</div>`)

	// Provider/board failure banner. When the last poll failed (LLM proxy
	// down, board unreachable, bad key) goon records it on the daemon
	// status — surface it loudly with the error class, a plain-English
	// fix, the retry/backoff state, and whether goon paused itself.
	if strings.TrimSpace(st.LastError) != "" {
		title := "goon hit a problem"
		if st.AutoPaused {
			title = "goon paused itself after repeated failures"
		}
		classChip := ""
		if st.ErrorClass != "" {
			classChip = fmt.Sprintf(`<span class="ml-2 inline-flex items-center rounded-full border border-rose-500/40 bg-rose-500/15 px-1.5 py-0.5 text-[10px] font-mono uppercase tracking-wide">%s</span>`, html.EscapeString(st.ErrorClass))
		}
		hint := errorClassHint(st.ErrorClass)
		hintHTML := ""
		if hint != "" {
			hintHTML = fmt.Sprintf(`<div class="text-xs text-rose-700/80 dark:text-rose-300/80 mt-1">%s</div>`, html.EscapeString(hint))
		}
		// Retry / attempt line.
		retryLine := ""
		if st.AutoPaused {
			retryLine = "auto-retry stopped — resume the daemon (bottom-left) once it's fixed"
		} else if !st.NextRetryAt.IsZero() {
			rel := "soon"
			if d := time.Until(st.NextRetryAt); d > 0 {
				if d < time.Minute {
					rel = fmt.Sprintf("in %ds", int(d.Seconds()))
				} else {
					rel = fmt.Sprintf("in %dm", int(d.Minutes())+1)
				}
			}
			retryLine = fmt.Sprintf("attempt %d · next retry %s", st.ConsecutiveFails, rel)
		}
		actionBtn := `<button type="button" onclick="if(typeof showPage==='function')showPage('setup')" class="shrink-0 rounded-lg border border-rose-500/40 text-rose-700 dark:text-rose-300 px-3 py-1.5 text-xs font-semibold hover:bg-rose-500/15 transition">Open Setup →</button>`
		if st.AutoPaused {
			actionBtn = `<form hx-post="/api/daemon/resume" hx-swap="none" class="m-0 shrink-0"><button type="submit" class="rounded-lg bg-emerald-500 text-white px-3 py-1.5 text-xs font-semibold hover:bg-emerald-600 transition">Resume daemon</button></form>` + actionBtn
		}
		fmt.Fprintf(w, `<div class="mb-6 flex flex-wrap items-start gap-3 rounded-xl border border-rose-500/40 bg-rose-500/10 px-4 py-3">
			<svg class="h-5 w-5 text-rose-700 dark:text-rose-400 mt-0.5 shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><line x1="12" y1="8" x2="12" y2="12"/><line x1="12" y1="16" x2="12.01" y2="16"/></svg>
			<div class="flex-1 min-w-0">
				<div class="text-sm font-semibold text-rose-700 dark:text-rose-300">%s%s</div>
				<div class="text-xs text-rose-700/80 dark:text-rose-300/80 mt-0.5 break-words">%s</div>
				%s
				<div class="text-[11px] text-rose-600/60 dark:text-rose-300/50 mt-1">since %s%s</div>
			</div>
			<div class="flex flex-wrap gap-2">%s</div>
		</div>`,
			html.EscapeString(title), classChip,
			html.EscapeString(st.LastError),
			hintHTML,
			html.EscapeString(humanizeSince(st.LastErrorAt)),
			func() string {
				if retryLine != "" {
					return " · " + html.EscapeString(retryLine)
				}
				return ""
			}(),
			actionBtn)
	}

	// First-run readiness — guides a new user toward activation (the
	// first picked ticket) and disappears the moment goon is configured
	// and has seen a ticket, so it never nags an established instance.
	if !(llmReady && boardReady && activated) {
		step := func(done bool, label, detail string) string {
			icon := `<span class="inline-flex h-5 w-5 shrink-0 items-center justify-center rounded-full border border-surface-border text-[11px] text-muted">○</span>`
			labelCls := "text-ink"
			if done {
				icon = `<span class="inline-flex h-5 w-5 shrink-0 items-center justify-center rounded-full bg-emerald-500/20 text-emerald-700 dark:text-emerald-400 text-[11px]">✓</span>`
				labelCls = "text-muted line-through decoration-muted/40"
			}
			return fmt.Sprintf(`<li class="flex items-start gap-2.5">%s<div class="min-w-0"><div class="text-sm %s">%s</div><div class="text-[11px] text-muted">%s</div></div></li>`,
				icon, labelCls, html.EscapeString(label), html.EscapeString(detail))
		}
		fmt.Fprint(w, `<div class="mb-6 rounded-xl border border-accent/30 bg-accent/5 p-4">
			<div class="flex items-center justify-between gap-3 mb-3">
				<div class="text-sm font-semibold text-ink">Get goon running</div>
				<div class="flex items-center gap-2">
					<button type="button" onclick="if(typeof showPage==='function')showPage('tickets')" class="shrink-0 rounded-lg border border-accent/40 text-accent px-3 py-1.5 text-xs font-semibold hover:bg-accent/10 transition">+ Create a task</button>
					<button type="button" onclick="if(typeof showPage==='function')showPage('setup')" class="shrink-0 rounded-lg bg-accent text-surface px-3 py-1.5 text-xs font-semibold hover:brightness-110 transition">Open Setup →</button>
				</div>
			</div>
			<ul class="space-y-2.5">`)
		fmt.Fprint(w, step(llmReady, "Connect an LLM provider", "OpenAI, Anthropic, Gemini, or local Ollama — the only required step."))
		fmt.Fprint(w, step(boardReady, "Give goon work to do", "Connect Jira/GitHub Issues — or just create a local ticket on the Tickets tab. No board required."))
		fmt.Fprint(w, step(hostReady, "Connect a git host (optional)", "GitHub, GitLab, or Bitbucket — only needed for tickets that should open a pull request."))
		fmt.Fprint(w, step(activated, "goon picks up its first task", firstTicketHint(llmReady, boardReady, st)))
		fmt.Fprint(w, `</ul></div>`)
	}

	fmt.Fprint(w, `<div class="grid grid-cols-2 lg:grid-cols-4 gap-3 mb-6">`)
	fmt.Fprint(w, statCard("pending approvals", fmt.Sprintf("%d", len(pending)), pendingTone))
	fmt.Fprint(w, statCard("active workflows", fmt.Sprintf("%d", active), activeTone))
	fmt.Fprint(w, statCard("tickets seen", fmt.Sprintf("%d", len(tix)), "neutral"))
	fmt.Fprint(w, statCard("daemon · "+lastPoll, daemonState, daemonTone))
	fmt.Fprint(w, `</div>`)

	// --- token usage + live agent sessions (auto-refreshing cards) -----
	fmt.Fprint(w, `<div class="grid grid-cols-1 lg:grid-cols-2 gap-6 mb-6">
		<div hx-get="/fragments/usage" hx-trigger="load, every 15s" hx-swap="innerHTML"><div class="text-sm text-gray-500">Loading token usage…</div></div>
		<div hx-get="/fragments/sessions" hx-trigger="load, every 5s" hx-swap="innerHTML"><div class="text-sm text-gray-500">Loading agent sessions…</div></div>
	</div>`)

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
				<span class="text-[11px] text-muted shrink-0">%s</span>
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
			<h3 class="text-sm font-semibold text-gray-700 dark:text-gray-300">Blocking approvals</h3>
			<a href="#" onclick="showPage('workflows');return false;" class="text-xs text-gray-500 hover:text-accent transition">review →</a>
		</div>`)
	// Quick bulk-approve for repo confirmations stacked up at the gate.
	var dashApprovals []struct{ key, repo string }
	for i := range wfs {
		if wfs[i].State != memory.WFAwaitingApproval || wfs[i].Stage != "confirm_repo" {
			continue
		}
		suggested := ""
		if wfs[i].PendingQuestionID != "" {
			if q, ok := s.opts.Memory.GetQuestion(wfs[i].PendingQuestionID); ok {
				suggested = extractSuggestedRepo(q.Question)
			}
		}
		if suggested == "" {
			suggested = wfs[i].Repo
		}
		key := wfs[i].TicketKey
		if key == "" {
			key = wfs[i].TicketID
		}
		dashApprovals = append(dashApprovals, struct{ key, repo string }{key, suggested})
	}
	if len(dashApprovals) > 1 {
		var rows strings.Builder
		for _, ra := range dashApprovals {
			fmt.Fprintf(&rows,
				`<div class="flex items-center gap-2 text-[11px] py-0.5">
					<span class="font-mono text-muted/70 w-20 shrink-0 truncate">%s</span>
					<span class="text-muted/50">→</span>
					<span class="text-ink/80 truncate">%s</span>
				</div>`,
				html.EscapeString(ra.key), html.EscapeString(ra.repo))
		}
		fmt.Fprintf(w,
			`<div class="mb-3 rounded-lg border border-highlight/25 bg-highlight/6 p-3 space-y-2">
				<div class="text-[11px] font-medium text-highlight">%d repo confirmations pending</div>
				<div class="space-y-0.5">%s</div>
				<form hx-post="/api/answer-all" hx-target="#dash-bulk-result" hx-swap="innerHTML"
					hx-disabled-elt="find button" class="m-0 pt-1">
					<input type="hidden" name="stage" value="confirm_repo">
					<button type="submit"
						class="w-full rounded-md bg-emerald-600/90 hover:bg-emerald-600 text-white px-3 py-1.5 text-xs font-semibold transition">
						Approve all suggested repos
					</button>
				</form>
				<div id="dash-bulk-result" class="text-xs text-emerald-700 dark:text-emerald-400 empty:hidden"></div>
			</div>`,
			len(dashApprovals), rows.String())
	}
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
			fmt.Fprintf(w, `<div class="mt-2 text-xs text-gray-500">+%d more — <a href="#" onclick="showPage('workflows');return false;" class="text-accent hover:underline">review them</a></div>`,
				len(pending)-3)
		}
	}
	fmt.Fprint(w, `</section>`)

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
	return fmt.Sprintf(`<div class="rounded-xl border border-surface-border bg-surface-raised %s p-4">
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
				<h2 class="text-xl font-semibold tracking-tight">Setup</h2>
				<p class="mt-0.5 text-sm text-gray-500 dark:text-gray-400 max-w-2xl">
					Pick your provider and fill in credentials — goon saves to <code class="font-mono text-xs">./config.json</code> and hot-reloads instantly.
					Masked fields are already set; leave blank to keep, type to replace.
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

// ─── Jira filter panel ────────────────────────────────────────────────────────

// fragJiraFilter renders the inline Jira-filter config panel loaded by the
// "Jira setup" button in the Tickets tab.
func (s *Server) fragJiraFilter(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	jql := os.Getenv("JIRA_JQL")
	if jql == "" {
		jql = "assignee=currentUser() AND statusCategory!=Done"
	}
	fmt.Fprintf(w,
		`<div class="mt-3 rounded-lg border border-surface-border bg-surface p-4 space-y-3" id="jira-filter-panel">
			<div class="flex items-center justify-between">
				<span class="text-xs font-semibold text-accent">Jira ticket filter</span>
				<button onclick="document.getElementById('jira-filter-panel').remove()"
					class="text-[11px] text-gray-500 hover:text-accent">✕</button>
			</div>
			<p class="text-[11px] text-muted">
				Controls which Jira tickets goon pulls on each poll. Use JQL, a label, or any valid Jira filter.
			</p>
			<form hx-post="/api/ticket/jira-filter"
				hx-target="#jira-filter-result"
				hx-swap="innerHTML">
				<label class="block text-[11px] text-muted mb-1">JQL query</label>
				<input type="text" name="jql"
					value="%s"
					placeholder="assignee=currentUser() AND statusCategory!=Done"
					class="w-full text-[12px] rounded border border-surface-border bg-surface-sunken px-3 py-1.5 focus:outline-none focus:border-accent/60 font-mono text-gray-200">
				<div class="flex items-center gap-2 mt-2">
					<button type="submit"
						class="text-[11px] px-3 py-1 rounded bg-accent/20 hover:bg-accent/30 text-accent border border-accent/30 transition">
						save
					</button>
					<span id="jira-filter-result" class="text-[10px] text-gray-500 ml-1"></span>
				</div>
			</form>
			<div class="text-[11px] text-muted space-y-0.5 pt-1 border-t border-surface-border">
				<p class="font-medium text-muted">Examples</p>
				<p><code class="text-accent">assignee=currentUser() AND sprint in openSprints()</code></p>
				<p><code class="text-accent">project = ENG AND labels = backend AND status != Done</code></p>
				<p><code class="text-accent">filter=12345</code> — saved filter by id</p>
			</div>
		</div>`,
		html.EscapeString(jql))
}

// handleTicketJiraFilter saves the JQL to config.json via envstore.
func (s *Server) handleTicketJiraFilter(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := r.ParseForm(); err != nil {
		fmt.Fprint(w, `<span class="text-red-400">parse error</span>`)
		return
	}
	jql := strings.TrimSpace(r.FormValue("jql"))
	if jql == "" {
		_ = unsetConfigKey("JIRA_JQL")
		_ = os.Unsetenv("JIRA_JQL")
	} else {
		if err := setConfigKey("JIRA_JQL", jql); err != nil {
			fmt.Fprintf(w, `<span class="text-red-400">%s</span>`, html.EscapeString(err.Error()))
			return
		}
		_ = os.Setenv("JIRA_JQL", jql)
	}
	fmt.Fprint(w, `<span class="text-green-400">saved ✓ — takes effect on next poll</span>`)
}

// ─── local (goon-native) tickets ─────────────────────────────────────────────

// fragLocalTickets renders the local-ticket section injected below the board
// ticket table. Empty when no local tickets exist.
func (s *Server) fragLocalTickets(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	tks := s.opts.Memory.ListLocalTickets()
	if len(tks) == 0 {
		fmt.Fprint(w, `<p class="text-xs text-muted mt-4 px-1">No goon tickets yet — hit <strong>+ create ticket</strong> to add one.</p>`)
		return
	}

	statusColor := map[string]string{
		"open":        "text-blue-400",
		"in_progress": "text-yellow-400",
		"in_review":   "text-purple-400",
		"blocked":     "text-red-400",
		"done":        "text-green-400",
	}
	priorityBadge := map[string]string{
		"high":   `<span class="px-1 py-0.5 rounded text-[9px] bg-red-400/20 text-red-300">high</span>`,
		"medium": `<span class="px-1 py-0.5 rounded text-[9px] bg-yellow-400/20 text-yellow-300">medium</span>`,
		"low":    `<span class="px-1 py-0.5 rounded text-[9px] bg-gray-400/20 text-muted">low</span>`,
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<div class="mt-4" id="local-tickets-section">
		<h3 class="text-xs font-semibold text-muted uppercase tracking-wider mb-2 px-1">Goon tickets (%d)</h3>
		<div class="overflow-x-auto rounded-lg border border-surface-border bg-surface-raised shadow-card">
		<table class="min-w-full text-sm">
			<thead class="sticky top-0 z-10 border-b border-surface-border text-[11px] uppercase tracking-wider text-gray-500 bg-surface">
				<tr>
					<th class="px-4 py-2.5 text-left font-semibold">ID</th>
					<th class="px-4 py-2.5 text-left font-semibold">Title</th>
					<th class="px-4 py-2.5 text-left font-semibold">Status</th>
					<th class="px-4 py-2.5 text-left font-semibold">Priority</th>
					<th class="px-4 py-2.5 text-left font-semibold">Labels</th>
					<th class="px-4 py-2.5 text-left font-semibold">Created</th>
					<th class="px-4 py-2.5 text-right font-semibold">Actions</th>
				</tr>
			</thead>
			<tbody class="divide-y divide-surface-border/60">`, len(tks))

	for _, t := range tks {
		sc := statusColor[t.Status]
		if sc == "" {
			sc = "text-muted"
		}
		pb := priorityBadge[t.Priority]

		var labelHTML strings.Builder
		for _, l := range t.Labels {
			fmt.Fprintf(&labelHTML, `<span class="px-1 py-0.5 rounded text-[9px] bg-accent/10 text-accent border border-accent/20">%s</span>`, html.EscapeString(l))
		}

		rowID := "lt-" + t.ID
		fmt.Fprintf(&b,
			`<tr data-ticket-row data-status="%s" id="%s">
				<td class="px-4 py-2.5 font-mono text-[11px] text-ink whitespace-nowrap">%s</td>
				<td class="px-4 py-2.5 text-[12px] font-medium text-gray-100 max-w-xs truncate" title="%s">%s</td>
				<td class="px-4 py-2.5 whitespace-nowrap">
					<span class="text-[11px] font-medium %s">%s</span>
				</td>
				<td class="px-4 py-2.5 whitespace-nowrap">%s</td>
				<td class="px-4 py-2.5">
					<div class="flex flex-wrap gap-1">%s</div>
				</td>
				<td class="px-4 py-2.5 text-[11px] text-muted whitespace-nowrap">%s</td>
				<td class="px-4 py-2.5 text-right">
					<details class="inline relative">
						<summary class="list-none cursor-pointer text-[11px] text-muted hover:text-accent select-none">⋯ actions</summary>
						<div class="absolute right-0 z-20 mt-1 w-52 rounded-lg border border-surface-border bg-surface shadow-lg p-2 space-y-2 text-[11px]">
							<form hx-post="/api/ticket/local-status" hx-target="#%s" hx-swap="outerHTML" class="flex gap-1 items-center">
								<input type="hidden" name="id" value="%s">
								<select name="status" class="flex-1 rounded border border-surface-border bg-surface-sunken px-1 py-0.5 text-[11px] focus:outline-none">
									<option value="open"%s>open</option>
									<option value="in_progress"%s>in progress</option>
									<option value="in_review"%s>in review</option>
									<option value="blocked"%s>blocked</option>
									<option value="done"%s>done</option>
								</select>
								<button type="submit" class="px-2 py-0.5 rounded bg-accent/20 text-accent border border-accent/30 hover:bg-accent/30 transition">set</button>
							</form>
							<form hx-post="/api/ticket/local-delete" hx-target="#%s" hx-swap="outerHTML"
								hx-confirm="Delete %s?"
								hx-trigger="submit">
								<button type="submit" class="w-full text-left px-2 py-0.5 rounded text-red-400 hover:bg-red-400/10 transition">
									delete
								</button>
							</form>
						</div>
					</details>
				</td>
			</tr>`,
			t.Status, rowID,
			html.EscapeString(t.ID),
			html.EscapeString(t.Title), html.EscapeString(t.Title),
			sc, strings.ReplaceAll(t.Status, "_", " "),
			pb,
			labelHTML.String(),
			fuzzyTime(t.CreatedAt),
			// status form
			rowID, t.ID,
			sel(t.Status, "open"), sel(t.Status, "in_progress"), sel(t.Status, "in_review"), sel(t.Status, "blocked"), sel(t.Status, "done"),
			// delete form
			rowID, html.EscapeString(t.ID),
		)
	}
	b.WriteString(`</tbody></table></div></div>`)
	fmt.Fprint(w, b.String())
}

// sel returns ' selected' when cur == val, else "".
func sel(cur, val string) string {
	if cur == val {
		return " selected"
	}
	return ""
}

// handleTicketCreate creates a new local ticket from the POST form.
func (s *Server) handleTicketCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := r.ParseForm(); err != nil {
		fmt.Fprint(w, `<span class="text-red-400">parse error</span>`)
		return
	}
	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		fmt.Fprint(w, `<span class="text-red-400">title is required</span>`)
		return
	}
	desc := r.FormValue("description")
	priority := r.FormValue("priority")
	rawLabels := strings.TrimSpace(r.FormValue("labels"))
	var labels []string
	for _, l := range strings.Split(rawLabels, ",") {
		l = strings.TrimSpace(l)
		if l != "" {
			labels = append(labels, l)
		}
	}
	t, err := s.opts.Memory.AddLocalTicket(title, desc, priority, labels)
	if err != nil {
		fmt.Fprintf(w, `<span class="text-red-400">%s</span>`, html.EscapeString(err.Error()))
		return
	}
	w.Header().Set("HX-Trigger", "ticketsChanged")
	fmt.Fprintf(w, `<span class="text-green-400">created %s ✓</span>`, html.EscapeString(t.ID))
}

// handleTicketLocalDelete removes a local ticket by ID.
func (s *Server) handleTicketLocalDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := r.ParseForm(); err != nil {
		fmt.Fprint(w, `<tr><td colspan="7" class="text-red-400 px-4 py-2 text-xs">parse error</td></tr>`)
		return
	}
	id := strings.TrimSpace(r.FormValue("id"))
	if err := s.opts.Memory.DeleteLocalTicket(id); err != nil {
		fmt.Fprintf(w, `<tr><td colspan="7" class="text-red-400 px-4 py-2 text-xs">%s</td></tr>`, html.EscapeString(err.Error()))
		return
	}
	// Return empty string so hx-swap="outerHTML" removes the row.
	w.Header().Set("HX-Trigger", "ticketsChanged")
	fmt.Fprint(w, "")
}

// handleTicketLocalStatus updates the status of a local ticket and re-renders
// the row in-place via outerHTML swap.
func (s *Server) handleTicketLocalStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := r.ParseForm(); err != nil {
		fmt.Fprint(w, `<tr><td colspan="7" class="text-red-400 px-4 py-2 text-xs">parse error</td></tr>`)
		return
	}
	id := strings.TrimSpace(r.FormValue("id"))
	status := strings.TrimSpace(r.FormValue("status"))
	if err := s.opts.Memory.UpdateLocalTicketStatus(id, status); err != nil {
		fmt.Fprintf(w, `<tr><td colspan="7" class="text-red-400 px-4 py-2 text-xs">%s</td></tr>`, html.EscapeString(err.Error()))
		return
	}
	// Re-render just this ticket's row by delegating to fragLocalTickets logic.
	tks := s.opts.Memory.ListLocalTickets()
	var t *memory.LocalTicket
	for i := range tks {
		if tks[i].ID == id {
			t = &tks[i]
			break
		}
	}
	if t == nil {
		fmt.Fprint(w, "")
		return
	}
	statusColor := map[string]string{
		"open":        "text-blue-400",
		"in_progress": "text-yellow-400",
		"in_review":   "text-purple-400",
		"blocked":     "text-red-400",
		"done":        "text-green-400",
	}
	priorityBadge := map[string]string{
		"high":   `<span class="px-1 py-0.5 rounded text-[9px] bg-red-400/20 text-red-300">high</span>`,
		"medium": `<span class="px-1 py-0.5 rounded text-[9px] bg-yellow-400/20 text-yellow-300">medium</span>`,
		"low":    `<span class="px-1 py-0.5 rounded text-[9px] bg-gray-400/20 text-muted">low</span>`,
	}
	sc := statusColor[t.Status]
	if sc == "" {
		sc = "text-muted"
	}
	pb := priorityBadge[t.Priority]
	var labelHTML strings.Builder
	for _, l := range t.Labels {
		fmt.Fprintf(&labelHTML, `<span class="px-1 py-0.5 rounded text-[9px] bg-accent/10 text-accent border border-accent/20">%s</span>`, html.EscapeString(l))
	}
	rowID := "lt-" + t.ID
	fmt.Fprintf(w,
		`<tr data-ticket-row data-status="%s" id="%s">
			<td class="px-4 py-2.5 font-mono text-[11px] text-ink whitespace-nowrap">%s</td>
			<td class="px-4 py-2.5 text-[12px] font-medium text-gray-100 max-w-xs truncate" title="%s">%s</td>
			<td class="px-4 py-2.5 whitespace-nowrap">
				<span class="text-[11px] font-medium %s">%s</span>
			</td>
			<td class="px-4 py-2.5 whitespace-nowrap">%s</td>
			<td class="px-4 py-2.5">
				<div class="flex flex-wrap gap-1">%s</div>
			</td>
			<td class="px-4 py-2.5 text-[11px] text-muted whitespace-nowrap">%s</td>
			<td class="px-4 py-2.5 text-right">
				<details class="inline relative">
					<summary class="list-none cursor-pointer text-[11px] text-muted hover:text-accent select-none">⋯ actions</summary>
					<div class="absolute right-0 z-20 mt-1 w-52 rounded-lg border border-surface-border bg-surface shadow-lg p-2 space-y-2 text-[11px]">
						<form hx-post="/api/ticket/local-status" hx-target="#%s" hx-swap="outerHTML" class="flex gap-1 items-center">
							<input type="hidden" name="id" value="%s">
							<select name="status" class="flex-1 rounded border border-surface-border bg-surface-sunken px-1 py-0.5 text-[11px] focus:outline-none">
								<option value="open"%s>open</option>
								<option value="in_progress"%s>in progress</option>
								<option value="in_review"%s>in review</option>
								<option value="blocked"%s>blocked</option>
								<option value="done"%s>done</option>
							</select>
							<button type="submit" class="px-2 py-0.5 rounded bg-accent/20 text-accent border border-accent/30 hover:bg-accent/30 transition">set</button>
						</form>
						<form hx-post="/api/ticket/local-delete" hx-target="#%s" hx-swap="outerHTML"
							hx-confirm="Delete %s?"
							hx-trigger="submit">
							<button type="submit" class="w-full text-left px-2 py-0.5 rounded text-red-400 hover:bg-red-400/10 transition">
								delete
							</button>
						</form>
					</div>
				</details>
			</td>
		</tr>`,
		t.Status, rowID,
		html.EscapeString(t.ID),
		html.EscapeString(t.Title), html.EscapeString(t.Title),
		sc, strings.ReplaceAll(t.Status, "_", " "),
		pb,
		labelHTML.String(),
		fuzzyTime(t.CreatedAt),
		rowID, t.ID,
		sel(t.Status, "open"), sel(t.Status, "in_progress"), sel(t.Status, "in_review"), sel(t.Status, "blocked"), sel(t.Status, "done"),
		rowID, html.EscapeString(t.ID),
	)
}
