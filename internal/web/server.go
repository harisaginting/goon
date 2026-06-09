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
	"github.com/harisaginting/goon/internal/llm"
	"github.com/harisaginting/goon/internal/memory"
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
	{Name: "GOOGLE_OAUTH_CLIENT_ID", Group: "agent", Hint: "Google OAuth client id (Desktop app). Then run `goon google auth` to connect Calendar/Tasks/Gmail/Logs."},
	{Name: "GOOGLE_OAUTH_CLIENT_SECRET", Sensitive: true, Group: "agent", Hint: "Google OAuth client secret."},
	{Name: "GOOGLE_OAUTH_REFRESH_TOKEN", Sensitive: true, Group: "agent", Hint: "set automatically by `goon google auth` — read-only Google access."},
	{Name: "GOOGLE_CLOUD_PROJECT", Group: "agent", Hint: "GCP project id for Cloud Logging (log search) queries."},
	{Name: "GOON_WORKSPACE_DIR", Group: "agent", Hint: `parent directory holding multiple git repos — confirm_repo gate offers them as a numbered menu`},

	// Model routing — override which provider+model is used per feature.
	// Leave empty to use GOON_LLM_PROVIDER + that provider's MODEL env var.
	{Name: "GOON_DEFAULT_MODEL", Group: "model_routing", Hint: "default model string when multiple providers are configured (e.g. gpt-4o, claude-opus-4-5)"},
	{Name: "GOON_TRIAGE_PROVIDER", Group: "model_routing", Hint: "provider for triage + planning stages (openai|anthropic|gemini|ollama)"},
	{Name: "GOON_TRIAGE_MODEL", Group: "model_routing", Hint: "model override for triage + planning"},
	{Name: "GOON_EXECUTE_PROVIDER", Group: "model_routing", Hint: "provider for the execute agent stage"},
	{Name: "GOON_EXECUTE_MODEL", Group: "model_routing", Hint: "model override for the execute agent stage"},
	{Name: "GOON_CHAT_PROVIDER", Group: "model_routing", Hint: "provider for web/Telegram chat turns"},
	{Name: "GOON_CHAT_MODEL", Group: "model_routing", Hint: "model override for chat turns"},
	{Name: "GOON_REVIEW_PROVIDER", Group: "model_routing", Hint: "provider for PR review drafts"},
	{Name: "GOON_REVIEW_MODEL", Group: "model_routing", Hint: "model override for PR review drafts"},

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
		"GOON_DEFAULT_MODEL":   "Default Model",
		"GOON_TRIAGE_PROVIDER": "Triage Provider",
		"GOON_TRIAGE_MODEL":    "Triage Model",
		"GOON_EXECUTE_PROVIDER": "Execute Provider",
		"GOON_EXECUTE_MODEL":   "Execute Model",
		"GOON_CHAT_PROVIDER":   "Chat Provider",
		"GOON_CHAT_MODEL":      "Chat Model",
		"GOON_REVIEW_PROVIDER": "Review Provider",
		"GOON_REVIEW_MODEL":    "Review Model",
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
func (s *Server) fragConfig(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// Current values for pre-selecting pickers.
	llmProvider := envEcho("GOON_LLM_PROVIDER")
	if llmProvider == "" {
		llmProvider = "openai"
	}
	board   := envEcho("GOON_BOARD")
	gitHost := envEcho("GOON_GIT_HOST")

	// Helper: render a labelled input row.
	field := func(k configKey, rowClass string) string {
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
  cfgPickLLM('` + llmProvider + `');
  cfgPickBoard('` + board + `');
  cfgPickHost('` + gitHost + `');
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

	// ── Step 5: Google Workspace (optional) ───────────────────────────────────
	gConnected := envEcho("GOOGLE_OAUTH_REFRESH_TOKEN") != ""
	gHasClient := envEcho("GOOGLE_OAUTH_CLIENT_ID") != "" && envEcho("GOOGLE_OAUTH_CLIENT_SECRET") != ""
	gProject := envEcho("GOOGLE_CLOUD_PROJECT") != ""
	var gPill string
	switch {
	case gConnected:
		gPill = `<span class="inline-flex items-center gap-1 rounded-full bg-emerald-500/15 text-emerald-700 dark:text-emerald-400 px-2.5 py-0.5 text-[11px] font-semibold">● connected</span>`
	case gHasClient:
		gPill = `<span class="inline-flex items-center gap-1 rounded-full bg-amber-500/15 text-amber-700 dark:text-amber-400 px-2.5 py-0.5 text-[11px] font-semibold">○ client set — run goon google auth</span>`
	default:
		gPill = `<span class="inline-flex items-center gap-1 rounded-full bg-surface-sunken text-muted px-2.5 py-0.5 text-[11px] font-semibold">○ not connected</span>`
	}
	logsPill := ""
	if gConnected && !gProject {
		logsPill = `<span class="inline-flex items-center gap-1 rounded-full bg-surface-sunken text-muted px-2.5 py-0.5 text-[11px]">log search needs a project id ↓</span>`
	}
	step5 := `<div class="flex items-start gap-3 mb-3">
		<span class="shrink-0 w-6 h-6 rounded-full bg-accent/15 text-accent text-[11px] font-bold flex items-center justify-center">5</span>
		<div class="flex-1"><div class="flex items-center gap-2 flex-wrap"><p class="text-sm font-semibold text-ink">Google Workspace</p>` + gPill + logsPill + `</div>
		<p class="text-xs text-muted">Ask goon about your Calendar, Tasks, Gmail &amp; Cloud Logging — read-only. Optional.</p></div></div>`
	step5 += `<div class="grid grid-cols-1 sm:grid-cols-2 gap-3">` +
		field(keyByName("GOOGLE_OAUTH_CLIENT_ID"), "") +
		field(keyByName("GOOGLE_OAUTH_CLIENT_SECRET"), "") +
		field(keyByName("GOOGLE_CLOUD_PROJECT"), "") +
		`</div>`
	step5 += `<div class="rounded-lg border border-surface-border bg-surface-sunken/50 p-3 text-[11px] text-muted space-y-1.5">
		<p class="font-semibold text-ink">Connect (one-time)</p>
		<p>1. Create an OAuth <strong>Desktop app</strong> client in Google Cloud, paste the ID + secret above, and <strong>save &amp; apply</strong>.</p>
		<p>2. In your terminal, run <code class="font-mono text-ink bg-surface px-1 py-0.5 rounded">goon google auth</code> and approve access in the browser.</p>
		<p>Optional: set <span class="font-mono text-ink">GOOGLE_CLOUD_PROJECT</span> to enable log search. Full click-by-click guide: <span class="font-mono text-ink">docs/google-workspace.md</span></p>
	</div>`
	gExamples := []string{
		"what meetings do I have today?",
		"what are my tasks?",
		"check my email from finance last week",
		"get the traceId for the login of username harisa",
	}
	step5 += `<div class="flex flex-wrap gap-1.5 pt-1">`
	for _, ex := range gExamples {
		step5 += `<span class="rounded-full border border-surface-border bg-surface px-2.5 py-1 text-[11px] text-muted">&ldquo;` + html.EscapeString(ex) + `&rdquo;</span>`
	}
	step5 += `</div>`
	fmt.Fprint(w, card(step5))

	// ── Advanced (collapsed) ──────────────────────────────────────────────────
	advKeys := []string{
		"GOON_POLL_SECONDS", "GOON_VERIFY_RUNS", "GOON_DAEMON_AUTO_START",
		"GOON_TICKET_STATUSES", "GOON_WORKSPACE_DIR", "GOON_AUTO_APPROVE",
		"GOON_AUTO_CONFIRM_REPO", "GOON_AUTO_APPROVE_PLAN", "GOON_LLM_HTTP_TIMEOUT_SEC",
		"TELEGRAM_BOT_TOKEN", "GOON_TELEGRAM_SECRET", "TELEGRAM_CHAT_ID",
		"CONFLUENCE_BASE_URL", "CONFLUENCE_EMAIL", "CONFLUENCE_API_TOKEN",
		"GOON_OBSIDIAN_VAULT", "GOON_OBSIDIAN_REPO",
		"GOON_MAX_STEPS", "GOON_LOG_LEVEL",
	}
	advHTML := `<div id="cfg-advanced" style="display:none" class="grid grid-cols-1 sm:grid-cols-2 gap-3 pt-3 border-t border-surface-border">`
	for _, name := range advKeys {
		advHTML += field(keyByName(name), "")
	}
	advHTML += `</div>`

	advSection := `<div class="rounded-xl border border-surface-border bg-surface-raised p-4">` +
		`<button type="button" id="cfg-adv-btn" onclick="cfgToggleAdv()" ` +
		`class="text-xs text-muted hover:text-ink transition font-medium">show advanced ▾</button>` +
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
func (s *Server) fragWorkflowConfig(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	cfg := s.opts.Workflow
	path := s.opts.WorkflowPath

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

	// ── Stage flowchart (compact, read-only) ─────────────────────────────
	var stageNames []string
	gateSet := map[string]bool{"confirm_repo": true, "approve_plan": true}
	if cfg != nil && len(cfg.Stages) > 0 {
		for _, st := range cfg.Stages {
			stageNames = append(stageNames, st.Name)
		}
	} else {
		stageNames = []string{"triage", "confirm_repo", "approve_plan", "execute", "test", "verify", "update_memory", "open_pr", "notify"}
	}

	fmt.Fprint(w, `<div class="px-5 pb-4 overflow-x-auto">
	<div class="flex items-center gap-0 min-w-max">`)
	for i, stage := range stageNames {
		if i > 0 {
			fmt.Fprint(w, `<div class="flex items-center shrink-0"><div class="w-5 h-px bg-surface-border"></div><svg class="h-2.5 w-2.5 text-surface-border" viewBox="0 0 10 10" fill="currentColor"><polygon points="0,2 8,5 0,8"/></svg></div>`)
		}
		isGate := gateSet[stage]
		shape := "rounded-full"
		icon := `<svg class="h-3 w-3" viewBox="0 0 24 24" fill="currentColor"><circle cx="12" cy="12" r="4"/></svg>`
		tone := "bg-surface-sunken border-surface-border text-muted"
		if isGate {
			shape = "rounded-md"
			icon = `<svg class="h-3 w-3" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="11" width="18" height="11" rx="2" ry="2"/><path d="M7 11V7a5 5 0 0 1 10 0v4"/></svg>`
			tone = "bg-amber-500/10 border-amber-500/40 text-amber-700 dark:text-amber-400"
		}
		fmt.Fprintf(w, `<div class="shrink-0 flex flex-col items-center gap-1.5"><div class="w-8 h-8 %s %s border flex items-center justify-center">%s</div><span class="text-[10px] text-muted text-center whitespace-nowrap max-w-[56px] leading-tight">%s</span></div>`,
			shape, tone, icon, html.EscapeString(stage))
	}
	fmt.Fprint(w, `</div></div>`)
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
			<button onclick="wgSave()" class="inline-flex items-center gap-1.5 rounded-md bg-accent text-black px-4 py-1.5 text-xs font-bold hover:brightness-110 transition">
				<svg class="h-3.5 w-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><path d="M19 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h11l5 5v11a2 2 0 0 1-2 2z"/><polyline points="17 21 17 13 7 13 7 21"/><polyline points="7 3 7 8 15 8"/></svg>
				Save pipeline
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

	<!-- Scrollable body -->
	<div class="flex-1 overflow-y-auto">
		<div id="wg-body" class="max-w-3xl mx-auto px-4 py-6"></div>
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
	llm: { label:'LLM', letter:'L', color:'#f59e0b', short:'One model call',
		blurb:'Runs your prompt through the model ONCE and returns the reply (optionally parsed as JSON). No file access, no tools — fast and cheap.',
		when:'Use for: planning, classifying, drafting, or turning a ticket into structured JSON the next step reads.',
		example:'Break this ticket into 3-7 steps. Reply JSON {"steps":[...]}.' },
	agent: { label:'Agent', letter:'A', color:'#38bdf8', short:'Autonomous, uses tools',
		blurb:'Runs goon\'s full agent loop: reads/edits files, runs commands, searches code, and keeps going until it finishes.',
		when:'Use for: the actual work — implement a feature, refactor, fix a bug, verify a change.',
		example:'Implement the plan for {{.Key}} and run the tests.' },
	notify: { label:'Notify', letter:'N', color:'#22c55e', short:'Send a message',
		blurb:'Sends a message to your configured Telegram channel. Stored output = the message text.',
		when:'Use for: pinging yourself mid-pipeline — e.g. after a risky step.',
		example:'Heads up: {{.Key}} passed review and is ready to ship.' },
	http: { label:'HTTP', letter:'H', color:'#a855f7', short:'Fetch a URL',
		blurb:'Fetches an https URL (GET) and captures the response text, so a later step can use it via {{.Stages.NAME}}. (POST/webhooks: use an agent step.)',
		when:'Use for: pulling in a status page, a spec, or a JSON API before reasoning about it.',
		example:'https://api.example.com/status' }
};
function typeMeta(t){ return TYPES[t] || {label:(t||'?').toUpperCase(), letter:'?', color:'#8b5cf6', blurb:'', when:'', example:''}; }

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
	var v = st.type==='agent'?p.task : st.type==='notify'?p.message : st.type==='http'?p.url : p.prompt;
	v=(v||'').replace(/\s+/g,' ').trim();
	if(v.length>72) v=v.slice(0,71)+'…';
	return v;
}

// ── Undo ─────────────────────────────────────────────────────────────
var _history=[], _MAX_HISTORY=60;
function pushHistory(){ _history.push({STEPS:clone(STEPS), selId:selId, CFG:clone(CFG)}); if(_history.length>_MAX_HISTORY) _history.shift(); updUndoBtn(); }
function updUndoBtn(){ var b=document.getElementById('wg-undo-btn'); if(b) b.disabled=(_history.length===0); }
window.wgUndo=function(){ if(!_history.length) return; var s=_history.pop(); STEPS=s.STEPS; selId=s.selId; CFG=s.CFG; renderAll(); updUndoBtn(); };

// ── Tabs + top-level render ──────────────────────────────────────────
window.wgTab=function(which){ tab=which; renderAll(); };
function setTabUI(){
	var on='text-ink border-accent bg-surface-sunken/40';
	var off='text-muted border-transparent hover:text-ink';
	var ts=document.getElementById('wg-tab-steps'), tw=document.getElementById('wg-tab-settings');
	if(ts) ts.className='px-4 py-2 text-[12px] font-semibold rounded-t-md border-b-2 transition '+(tab==='steps'?on:off);
	if(tw) tw.className='px-4 py-2 text-[12px] font-semibold rounded-t-md border-b-2 transition '+(tab==='settings'?on:off);
}
function renderAll(){ setTabUI(); if(tab==='settings'){ renderSettings(document.getElementById('wg-body')); } else { renderSteps(); } }

// ── Steps list ───────────────────────────────────────────────────────
function renderSteps(){
	var body=document.getElementById('wg-body');
	if(!body) return;
	var h='';

	if(STEPS.length===0){
		h+='<div class="rounded-xl border border-dashed border-surface-border bg-surface-raised/40 p-8 text-center">'
		  +'<div class="text-2xl mb-2">🧩</div>'
		  +'<div class="text-sm font-medium text-ink">No steps yet</div>'
		  +'<div class="text-xs text-muted mt-1 mb-4">A pipeline is a list of steps that run top to bottom. Add your first one.</div>'
		  +'</div>';
	} else {
		h+='<ol id="wg-step-list" class="space-y-2">';
		STEPS.forEach(function(st,i){ h+=stepCard(st,i); });
		h+='</ol>';
	}

	// Add-step zone (always visible — the obvious way to add multiple steps).
	h+='<div class="mt-4 rounded-xl border border-surface-border bg-surface-raised/60 p-3">'
	  +'<div class="text-[10px] uppercase tracking-widest text-muted font-semibold mb-2 px-1">Add a step</div>'
	  +'<div class="grid grid-cols-2 sm:grid-cols-4 gap-2">';
	Object.keys(TYPES).forEach(function(k){
		var m=TYPES[k];
		h+='<button type="button" onclick="wgAddStep(\''+k+'\')" title="'+escX(m.blurb)+'"'
		  +' class="text-left rounded-lg border border-surface-border bg-surface-raised px-3 py-2.5 hover:bg-surface-sunken transition" style="border-left:3px solid '+m.color+'">'
		  +'<div class="flex items-center gap-2 mb-0.5"><span class="w-5 h-5 rounded flex items-center justify-center text-[10px] font-black" style="background:'+m.color+'22;color:'+m.color+'">'+escX(m.letter)+'</span>'
		  +'<span class="text-[12px] font-semibold" style="color:'+m.color+'">'+escX(m.label)+'</span></div>'
		  +'<div class="text-[10px] text-muted leading-snug">'+escX(m.short)+'</div></button>';
	});
	h+='</div></div>';

	body.innerHTML=h;
	bindStepDrag();
	// Fill the expanded step's editor (single source of truth — callers just
	// set selId + renderSteps()). Only runs on structural re-renders, never on
	// text-input keystrokes, so it can't steal focus mid-typing.
	if(selId){ var be=document.getElementById('wg-body-'+selId); if(be) renderStageProps(stepById(selId), be); }
}

function stepCard(st,i){
	var m=typeMeta(st.type);
	var expanded=(st.id===selId);
	var preview=stepPreview(st);
	var h='<li class="wg-step rounded-xl border bg-surface-raised overflow-hidden transition '+(expanded?'border-accent/60 shadow-lg':'border-surface-border')+'" data-id="'+st.id+'">';
	// Header (click to expand)
	h+='<div class="flex items-center gap-2.5 px-2 py-2.5 cursor-pointer select-none hover:bg-surface-sunken/40" onclick="wgToggleStep(\''+st.id+'\')">';
	h+='<span class="wg-drag cursor-grab text-muted hover:text-ink px-1 select-none" data-drag="'+st.id+'" title="Drag to reorder" onclick="event.stopPropagation()">⠿</span>';
	h+='<span class="w-5 text-center text-[11px] font-mono text-muted">'+(i+1)+'</span>';
	h+='<span class="w-6 h-6 rounded-md flex items-center justify-center text-[11px] font-black shrink-0" style="background:'+m.color+'22;color:'+m.color+'">'+escX(m.letter)+'</span>';
	h+='<div class="flex-1 min-w-0">';
	h+='<div class="text-[13px] font-semibold text-ink truncate"><span data-stepname="'+st.id+'">'+escX(st.name||'(unnamed)')+'</span> <span class="text-[10px] font-normal text-muted">· '+escX(m.label.toLowerCase())+'</span></div>';
	if(preview) h+='<div class="text-[11px] text-muted truncate font-mono">'+escX(preview)+'</div>';
	h+='</div>';
	// Reorder + actions (stopPropagation so they don't toggle expand)
	h+='<div class="flex items-center gap-0.5 shrink-0" onclick="event.stopPropagation()">';
	h+='<button onclick="wgMoveStep(\''+st.id+'\',-1)" '+(i===0?'disabled':'')+' class="p-1.5 rounded text-muted hover:text-ink hover:bg-surface-border disabled:opacity-25 disabled:hover:bg-transparent" title="Move up">▲</button>';
	h+='<button onclick="wgMoveStep(\''+st.id+'\',1)" '+(i===STEPS.length-1?'disabled':'')+' class="p-1.5 rounded text-muted hover:text-ink hover:bg-surface-border disabled:opacity-25 disabled:hover:bg-transparent" title="Move down">▼</button>';
	h+='<button onclick="wgDuplicateStep(\''+st.id+'\')" class="p-1.5 rounded text-muted hover:text-accent-strong hover:bg-surface-border" title="Duplicate">⧉</button>';
	h+='<button onclick="wgDeleteStep(\''+st.id+'\')" class="p-1.5 rounded text-muted hover:text-rose-400 hover:bg-surface-border" title="Delete">🗑</button>';
	h+='<span class="px-1 text-muted text-xs">'+(expanded?'▾':'▸')+'</span>';
	h+='</div></div>';
	// Body (only built when expanded)
	if(expanded){
		h+='<div class="border-t border-surface-border px-4 py-3" id="wg-body-'+st.id+'"></div>';
	}
	h+='</li>';
	return h;
}

window.wgToggleStep=function(id){
	selId=(selId===id)?null:id;
	renderSteps();
	if(selId){
		var bodyEl=document.getElementById('wg-body-'+selId);
		if(bodyEl) renderStageProps(stepById(selId), bodyEl);
		// Scroll the expanded card into view.
		var card=document.querySelector('.wg-step[data-id="'+selId+'"]');
		if(card) card.scrollIntoView({block:'nearest', behavior:'smooth'});
	}
};

// ── Add / move / duplicate / delete ──────────────────────────────────
window.wgAddStep=function(type){
	pushHistory();
	var id=newId();
	STEPS.push({id:id, type:type, name:uniqueName(type), props:{}});
	selId=id;
	renderSteps();
	var bodyEl=document.getElementById('wg-body-'+id);
	if(bodyEl){ renderStageProps(stepById(id), bodyEl); }
	var card=document.querySelector('.wg-step[data-id="'+id+'"]');
	if(card) card.scrollIntoView({block:'center', behavior:'smooth'});
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
	selId=copy.id;
	renderSteps();
	var b=document.getElementById('wg-body-'+copy.id); if(b) renderStageProps(stepById(copy.id), b);
};
window.wgDeleteStep=function(id){
	pushHistory();
	STEPS=STEPS.filter(function(s){ return s.id!==id; });
	if(selId===id) selId=null;
	renderSteps();
};

// ── Drag-to-reorder (pointer-based, no DOM rebuild mid-drag) ──────────
var DRAG=null;
function bindStepDrag(){
	var handles=document.querySelectorAll('#wg-step-list .wg-drag');
	handles.forEach(function(hd){ hd.addEventListener('pointerdown', onDragStart); });
}
function clearDrag(){
	if(DRAG){
		var card=document.querySelector('.wg-step[data-id="'+DRAG.id+'"]');
		if(card) card.style.opacity='';
	}
	var ind=document.getElementById('wg-drop-ind'); if(ind) ind.remove();
	DRAG=null;
}
function onDragStart(e){
	e.preventDefault(); e.stopPropagation();
	var id=e.currentTarget.getAttribute('data-drag');
	DRAG={id:id, startY:e.clientY, moved:false, target:stepIndex(id)};
}
document.addEventListener('pointermove', function(e){
	if(!DRAG) return;
	if(!DRAG.moved && Math.abs(e.clientY-DRAG.startY)<5) return; // threshold
	DRAG.moved=true;
	var card=document.querySelector('.wg-step[data-id="'+DRAG.id+'"]');
	if(card) card.style.opacity='0.4';
	// Find which gap the pointer is over.
	var cards=Array.prototype.slice.call(document.querySelectorAll('#wg-step-list .wg-step'));
	var target=cards.length;
	for(var i=0;i<cards.length;i++){
		var r=cards[i].getBoundingClientRect();
		if(e.clientY < r.top + r.height/2){ target=i; break; }
	}
	DRAG.target=target;
	// Draw a drop indicator line.
	var ind=document.getElementById('wg-drop-ind');
	if(!ind){ ind=document.createElement('div'); ind.id='wg-drop-ind'; ind.style.height='2px'; ind.style.background='#8b5cf6'; ind.style.borderRadius='2px'; ind.style.margin='-1px 0'; }
	var list=document.getElementById('wg-step-list');
	if(list){
		if(target>=cards.length){ list.appendChild(ind); }
		else { list.insertBefore(ind, cards[target]); }
	}
});
document.addEventListener('pointerup', function(){
	if(DRAG && DRAG.moved){
		var from=stepIndex(DRAG.id);
		var to=DRAG.target;
		if(from>=0 && to>=0){
			if(to>from) to--; // account for removal shift
			if(to!==from){
				pushHistory();
				var item=STEPS.splice(from,1)[0];
				STEPS.splice(to,0,item);
			}
		}
		clearDrag();
		renderSteps();
		if(selId){ var b=document.getElementById('wg-body-'+selId); if(b) renderStageProps(stepById(selId), b); }
		return;
	}
	clearDrag();
});
document.addEventListener('pointercancel', clearDrag);
window.addEventListener('blur', clearDrag);

// ── Per-step field editor (renders into the expanded card body) ───────
function renderStageProps(n, el){
	if(!el || !n) return;
	var p=n.props||{};
	var ob=' onblur="wgPropBlur()"';
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

	if(n.type==='agent'){
		h+='<div>'+fieldLabel('Task')
		  +'<textarea rows="5" oninput="wgProp(\'task\',this.value)"'+ob+' class="'+inputCls()+' font-mono text-[11px] resize-y leading-relaxed">'+escH(p.task||'')+'</textarea>'+varHint()+'</div>';
		h+='<div>'+fieldLabel('Max steps (0 = default)')
		  +'<input type="number" min="0" value="'+escX(p.max_steps||'')+'" oninput="wgProp(\'max_steps\',this.value)"'+ob+' class="'+inputCls()+'"></div>';
	} else if(n.type==='notify'){
		h+='<div>'+fieldLabel('Message')
		  +'<textarea rows="3" oninput="wgProp(\'message\',this.value)"'+ob+' class="'+inputCls()+' font-mono text-[11px] resize-y leading-relaxed">'+escH(p.message||'')+'</textarea>'+varHint()
		  +fieldHint('Sent to your Telegram channel. Set On error → continue to make it optional.')+'</div>';
	} else if(n.type==='http'){
		h+='<div>'+fieldLabel('URL (https, GET)')
		  +'<input type="text" value="'+escX(p.url||'')+'" oninput="wgProp(\'url\',this.value)"'+ob+' placeholder="https://api.example.com/status" class="'+inputCls()+' font-mono text-[11px]">'+varHint()
		  +fieldHint('Response body is stored under this step — reference it later with {{.Stages.'+escH(n.name||'NAME')+'}}.')+'</div>';
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

	var others=STEPS.filter(function(x){ return x.id!==n.id; }).map(function(x){ return x.name; });
	function routingSelect(key,val,label){
		var s='<div><label class="block text-[10px] text-muted mb-0.5">'+label+'</label>'
		  +'<select onchange="pushHistory();wgProp(\''+key+'\',this.value)" class="'+inputCls()+'">'
		  +'<option value=""'+(val===''?' selected':'')+'>— default (next in list) —</option>';
		others.forEach(function(sn){ s+='<option value="'+escX(sn)+'"'+(val===sn?' selected':'')+'>'+escX(sn)+'</option>'; });
		s+='<option value="end"'+(val==='end'?' selected':'')+'>end pipeline</option>';
		return s+'</select></div>';
	}
	h+='<div class="text-[10px] uppercase tracking-widest text-muted font-semibold">Branching</div>';
	h+='<div class="grid grid-cols-2 gap-2">';
	h+=routingSelect('on_next', p.on_next||'', 'on success → go to');
	h+=routingSelect('on_reject', p.on_reject||'', 'on reject → go to');
	h+='</div>';
	h+='<div>'+fieldLabel('Reject if (template)')
	  +'<input type="text" value="'+escX(p.reject_if||'')+'" oninput="wgProp(\'reject_if\',this.value)"'+ob+' placeholder="{{eq (index .Stages.NAME &quot;ok&quot;) false}}" class="'+inputCls()+' font-mono text-[11px]">'
	  +fieldHint('When truthy, this step is rejected and routing follows "on reject". Leave empty to never reject.')+'</div>';
	h+='<div class="grid grid-cols-2 gap-2">';
	h+=routingSelect('ask_stage', p.ask_stage||'', 'ask step (sub-call)');
	h+='<div>'+fieldLabel('Max loops (default 3)')+'<input type="number" min="0" value="'+escX(p.max_loops||'')+'" oninput="wgProp(\'max_loops\',this.value)"'+ob+' class="'+inputCls()+'"></div>';
	h+='</div>';

	if(n.type==='llm'||n.type==='agent'){
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
	if(key==='name'){
		n.name=val;
		// Live-update the card header without rebuilding the open editor
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
};
window.wgPropBlur=function(){ pushHistory(); };

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
	if(key==='verify_runs'){ var n=parseInt(val,10); if(isNaN(n)){ delete CFG.verify_runs; } else { CFG.verify_runs=n; } return; }
	if(key==='auto_approve'){ CFG.auto_approve=!!val; return; }
	if(val===''||val==null){ delete CFG[key]; } else { CFG[key]=val; }
};
window.wgSetLabels=function(val){ var a=val.split(',').map(function(s){return s.trim();}).filter(Boolean); if(a.length){ CFG.extra_labels=a; } else { delete CFG.extra_labels; } };
window.wgSetHook=function(ph,val){ CFG.hooks=CFG.hooks||{}; var a=val.split('\n').map(function(s){return s.trim();}).filter(Boolean); if(a.length){ CFG.hooks[ph]=a; } else { delete CFG.hooks[ph]; } if(Object.keys(CFG.hooks).length===0) delete CFG.hooks; };

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
	if(p.on_next)   s.on_next=p.on_next;
	if(p.reject_if) s.reject_if=p.reject_if;
	if(p.on_reject) s.on_reject=p.on_reject;
	if(p.ask_stage) s.ask_stage=p.ask_stage;
	var ml=parseInt(p.max_loops,10); if(!isNaN(ml)&&ml>0) s.max_loops=ml;
	if(p.provider) s.provider=p.provider;
	if(p.model)    s.model=p.model;
	if(n.type==='agent'){
		if(p.task) s.task=p.task;
		var ms=parseInt(p.max_steps,10); if(!isNaN(ms)&&ms>0) s.max_steps=ms;
	} else if(n.type==='notify'){
		if(p.message) s.message=p.message;
	} else if(n.type==='http'){
		if(p.url) s.url=p.url;
	} else {
		if(p.prompt) s.prompt=p.prompt;
		if(p.system) s.system=p.system;
		if(p.json_mode) s.json_mode=true;
		var tmp=parseFloat(p.temperature); if(!isNaN(tmp)&&tmp!==0) s.temperature=tmp;
		var mt=parseInt(p.max_tokens,10); if(!isNaN(mt)&&mt>0) s.max_tokens=mt;
		if(p.output) s.output=p.output;
	}
	return s;
}
function buildConfig(includeStages){
	var c=clone(CFG)||{};
	if(includeStages){ c.stages=STEPS.map(stepToStage); } else { delete c.stages; }
	return c;
}
function loadConfig(cfg){
	CFG=cfg||{};
	var stages=(CFG.stages&&CFG.stages.length)?CFG.stages:_seed;
	seededFromBuiltin=!(CFG.stages&&CFG.stages.length);
	STEPS=(stages||[]).map(function(s){ return {id:newId(), type:s.type||'llm', name:s.name||('step-'+_seq), props:clone(s)}; });
	selId=null; tab='steps';
	renderAll();
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
		if(n.type==='agent'&&!(pp.task||'').trim()){ flashErr('step "'+nm+'": task is required'); openStep(n.id); return; }
		if(n.type==='llm'&&!(pp.prompt||'').trim()){ flashErr('step "'+nm+'": prompt is required'); openStep(n.id); return; }
		if(n.type==='notify'&&!(pp.message||'').trim()){ flashErr('step "'+nm+'": message is required'); openStep(n.id); return; }
		if(n.type==='http'&&!(pp.url||'').trim()){ flashErr('step "'+nm+'": url is required'); openStep(n.id); return; }
	}
	postConfig(buildConfig(true));
};
function openStep(id){ if(tab!=='steps'){ tab='steps'; } selId=id; renderSteps(); var b=document.getElementById('wg-body-'+id); if(b) renderStageProps(stepById(id), b); }
window.wgRevertBuiltin=function(){
	if(!confirm('Revert to the built-in pipeline? Saves your settings + hooks but DROPS the custom steps, restoring the gated triage → confirm_repo → approve_plan → … flow.')) return;
	postConfig(buildConfig(false));
};

// ── Open / close ─────────────────────────────────────────────────────
window.wgClose=function(){
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
	populateTemplateSelect();
	loadConfig(hasKeys(_cfg)?clone(_cfg):clone(_defaults));
};
window.goonWorkflowEditorToggle=window.goonWfEditorToggle;

document.addEventListener('keydown',function(e){
	var ov=document.getElementById('wf-graph-overlay');
	if(!ov||ov.classList.contains('hidden')) return;
	if(e.key==='Escape'){ wgClose(); }
	else if((e.ctrlKey||e.metaKey)&&(e.key==='z'||e.key==='Z')){ e.preventDefault(); wgUndo(); }
});

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
	// pulse-dot only when running; paused/stopped get a static dot
	dotAnim := ""
	if st.Running && !st.Paused {
		dotAnim = " pulse-dot"
	}
	fmt.Fprintf(w, `<div class="flex items-center gap-2.5" data-paused="%s" data-running="%s" title="Last poll: %s">
		<span class="h-2 w-2 rounded-full shrink-0 %s%s"></span>
		<div class="min-w-0 flex-1">
			<div class="text-[11px] font-medium %s">%s</div>
			<div class="text-[10px] text-muted font-mono">polled %s</div>
		</div>
	</div>`, pausedFlag, runningFlag, html.EscapeString(last), dotClass, dotAnim, labelColor, state, html.EscapeString(last))
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
	fmt.Fprint(w, `<div class="overflow-x-auto rounded-lg border border-gray-200 dark:border-surface-border bg-white dark:bg-surface-raised shadow-card">
	<table class="min-w-full text-sm">
		<thead class="sticky top-0 z-10 border-b border-gray-200 dark:border-surface-border text-[11px] uppercase tracking-wider text-gray-500 bg-gray-50/50 dark:bg-surface">
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
				`<form hx-post="/api/ticket/unignore" hx-target="#%s-r" hx-swap="innerHTML" class="inline">` +
				`<input type="hidden" name="key" value="%s">` +
				`<button type="submit" title="Restore into workflow" class="p-1.5 rounded text-emerald-500 hover:bg-emerald-500/10 transition">` +
				`<svg class="h-3.5 w-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><polyline points="1 4 1 10 7 10"/><path d="M3.51 15a9 9 0 1 0 .49-4.5"/></svg>` +
				`</button></form>`,
				actionsRowID, escapedKey)
		} else {
			inlineIgnoreBtn = fmt.Sprintf(
				`<form hx-post="/api/ticket/ignore" hx-target="#%s-r" hx-swap="innerHTML" class="inline">` +
				`<input type="hidden" name="key" value="%s">` +
				`<button type="submit" title="Skip in daemon workflow" class="p-1.5 rounded text-muted hover:text-amber-500 hover:bg-amber-500/10 transition">` +
				`<svg class="h-3.5 w-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="10"/><line x1="4.93" y1="4.93" x2="19.07" y2="19.07"/></svg>` +
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

// renderMiniStageFlow renders a compact horizontal pipeline flowchart for
// a workflow card. Each stage is a small colored dot; the current stage
// gets a slightly larger accent dot. Stages that come after the current
// one are muted gray; completed stages are emerald.
func renderMiniStageFlow(currentStage string, state memory.WorkflowState) string {
	pipeline := []struct{ name, short string }{
		{"triage", "triage"},
		{"confirm_repo", "repo"},
		{"approve_plan", "plan"},
		{"execute", "exec"},
		{"test", "test"},
		{"verify", "verify"},
		{"update_memory", "memory"},
		{"open_pr", "pr"},
		{"notify", "notify"},
	}

	// Find index of current stage.
	cur := -1
	for i, p := range pipeline {
		if p.name == currentStage {
			cur = i
			break
		}
	}

	var b strings.Builder
	b.WriteString(`<div class="mt-2 flex items-center gap-0 overflow-x-auto pb-0.5">`)
	for i, p := range pipeline {
		if i > 0 {
			// Connector line — green if both sides are done.
			lineCls := "bg-surface-border/60"
			if cur >= 0 && i <= cur && state != memory.WFFailed {
				lineCls = "bg-emerald-500/50"
			}
			b.WriteString(`<div class="h-px w-3 shrink-0 ` + lineCls + `"></div>`)
		}
		// Dot style by position relative to current.
		var dotCls, titleAttr string
		titleAttr = `title="` + p.name + `"`
		switch {
		case state == memory.WFDone:
			dotCls = "bg-emerald-500"
		case state == memory.WFFailed && i == cur:
			dotCls = "bg-rose-500 ring-1 ring-rose-400/60"
		case cur < 0 || i < cur:
			// Done stages (before current).
			if state != memory.WFFailed {
				dotCls = "bg-emerald-500/80"
			} else {
				dotCls = "bg-surface-border"
			}
		case i == cur:
			// Active stage.
			dotCls = "bg-accent ring-2 ring-accent/30 w-3 h-3"
		default:
			// Pending.
			dotCls = "bg-surface-border"
		}
		// Active stage gets a label below it.
		label := ""
		if i == cur {
			label = `<span class="text-[9px] text-accent whitespace-nowrap leading-none mt-0.5">` + html.EscapeString(p.short) + `</span>`
		}
		b.WriteString(`<div class="shrink-0 flex flex-col items-center">`)
		if label != "" {
			b.WriteString(`<div class="w-3 h-3 rounded-full ` + dotCls + `" ` + titleAttr + `></div>` + label)
		} else {
			b.WriteString(`<div class="w-2 h-2 rounded-full ` + dotCls + `" ` + titleAttr + `></div>`)
		}
		b.WriteString(`</div>`)
	}
	b.WriteString(`</div>`)
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

		// Flat card. No shadow at rest — borders carry the visual weight.
		// Hover gets the subtle lift so interactivity is still hinted.
		// The 2px left-edge color is the only chrome that depends on
		// state; everything else is plain typography.
		wfSearch := html.EscapeString(strings.ToLower(wf.TicketKey + " " + wf.Title))
		fmt.Fprintf(w, `<div data-wf-state="%s" data-wf-search="%s" class="group relative rounded-lg border border-gray-200 dark:border-surface-border bg-white dark:bg-surface-raised hover:border-gray-300 dark:hover:border-gray-700 transition-colors">
			<div class="absolute left-0 top-0 bottom-0 w-0.5 %s rounded-l-lg"></div>
			<details>
				<summary class="cursor-pointer list-none px-4 py-3 select-none">`, workflowStateKey(wf.State), wfSearch, edgeTone)
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
		fmt.Fprint(w, renderMiniStageFlow(wf.Stage, wf.State))

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
//                   (live brand state — matches the daemon dot)
//   - planning phases (triaging/planning) → soft purple wash
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
				<svg class="h-4 w-4 text-accent" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 3v18h18"/><path d="M7 16l4-6 4 3 4-7"/></svg>
				Token usage
			</h3>
			<span class="text-[11px] text-muted font-mono">` + html.EscapeString(humanizeSince(meter.UpdatedAt())) + `</span>
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
				<svg class="h-4 w-4 text-accent" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="3"/><path d="M12 2v4M12 18v4M2 12h4M18 12h4"/></svg>
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
// one of green/amber/red/gray.
func healthPill(label, state, tone string) string {
	var border, bg, text, dot string
	switch tone {
	case "green":
		border, bg, text, dot = "border-emerald-500/40", "bg-emerald-500/10", "text-emerald-700 dark:text-emerald-400", "bg-emerald-500"
	case "amber":
		border, bg, text, dot = "border-amber-500/40", "bg-amber-500/10", "text-amber-700 dark:text-amber-400", "bg-amber-500"
	case "red":
		border, bg, text, dot = "border-rose-500/40", "bg-rose-500/10", "text-rose-700 dark:text-rose-400", "bg-rose-500"
	default:
		border, bg, text, dot = "border-surface-border", "bg-surface-raised", "text-muted", "bg-surface-border"
	}
	return fmt.Sprintf(`<span class="inline-flex items-center gap-1.5 rounded-full border %s %s px-2.5 py-1 text-[11px] %s">
		<span class="inline-block h-1.5 w-1.5 rounded-full %s"></span>%s <span class="opacity-70">%s</span></span>`,
		border, bg, text, dot, html.EscapeString(label), html.EscapeString(state))
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
			<span class="text-rose-700 dark:text-rose-400 text-lg leading-none">⚠</span>
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
