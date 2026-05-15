// Package memory holds short-term in-process state and a long-term JSON file
// at ./storage/memory.json (override with GOON_MEMORY_PATH; the storage
// root itself is overridable via GOON_STORAGE_DIR).
package memory

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/harisaginting/goon/internal/storage"
)

// Interaction is one user turn + outcome.
type Interaction struct {
	When     time.Time `json:"when"`
	Input    string    `json:"input"`
	ToolUsed string    `json:"tool_used,omitempty"`
	Command  string    `json:"command,omitempty"`
	OK       bool      `json:"ok"`
	Output   string    `json:"output,omitempty"`
}

// Question is something the agent decided it cannot proceed without and is
// asking the user to answer via `goon train`.
type Question struct {
	ID         string    `json:"id"` // stable id (e.g. "q-1730000000-1")
	When       time.Time `json:"when"`
	TicketID   string    `json:"ticket_id,omitempty"`
	WorkflowID string    `json:"workflow_id,omitempty"`
	Question   string    `json:"question"`
	Answer     string    `json:"answer,omitempty"`
	AnsweredAt time.Time `json:"answered_at,omitempty"`
}

// Pending reports whether the question is still awaiting an answer.
func (q Question) Pending() bool { return q.Answer == "" }

// AllRepos returns every repo associated with the workflow as a
// single slice, preferring the multi-pick Repos field when present
// and falling back to the legacy single Repo otherwise. Returns nil
// when neither is set.
func (w Workflow) AllRepos() []string {
	if len(w.Repos) > 0 {
		out := make([]string, len(w.Repos))
		copy(out, w.Repos)
		return out
	}
	if w.Repo != "" {
		return []string{w.Repo}
	}
	return nil
}

// PlanStep is one item inside a Workflow's plan.
type PlanStep struct {
	Index int    `json:"index"`
	Title string `json:"title"`
	Done  bool   `json:"done"`
	Note  string `json:"note,omitempty"`
}

// WorkflowState describes where a Workflow is in its lifecycle.
type WorkflowState string

const (
	WFTriaging         WorkflowState = "triaging"
	WFPlanning         WorkflowState = "planning"
	WFAwaitingApproval WorkflowState = "awaiting_approval"
	WFExecuting        WorkflowState = "executing"
	WFTesting          WorkflowState = "testing"
	WFVerifying        WorkflowState = "verifying"
	WFUpdatingMemory   WorkflowState = "updating_memory"
	WFOpeningPR        WorkflowState = "opening_pr"
	WFNotifying        WorkflowState = "notifying"
	WFDone             WorkflowState = "done"
	WFBlocked          WorkflowState = "blocked"
	WFFailed           WorkflowState = "failed"
)

// Workflow tracks a single ticket's run from triage through PR.
//
// When State == WFAwaitingApproval, Stage records which gate fired
// ("confirm_repo", "approve_plan", …) and PendingQuestionID points at the
// memory.Question the daemon is waiting for the user to answer. On the
// next poll cycle the daemon resumes the workflow from Stage once the
// question has an answer.
type Workflow struct {
	ID                string            `json:"id"`
	TicketID          string            `json:"ticket_id"`
	TicketKey         string            `json:"ticket_key,omitempty"`
	Title             string            `json:"title,omitempty"`
	StartedAt         time.Time         `json:"started_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
	State             WorkflowState     `json:"state"`
	Stage             string            `json:"stage,omitempty"`
	PendingQuestionID string            `json:"pending_question_id,omitempty"`
	Approvals         map[string]string `json:"approvals,omitempty"`
	Repo              string            `json:"repo,omitempty"`
	// Repos is the full set of target repos for a workflow, including
	// the primary Repo above as the first element. Populated when the
	// confirm_repo gate runs in multi-pick mode (workspace dir or git
	// host list). For backward compatibility every existing workflow
	// keeps Repo populated; new ones set both. AllRepos() returns the
	// canonical list across either field.
	Repos             []string          `json:"repos,omitempty"`
	Branch            string            `json:"branch,omitempty"`
	Plan              []PlanStep        `json:"plan,omitempty"`
	PRURL             string            `json:"pr_url,omitempty"`
	VerifyRuns        int               `json:"verify_runs"`
	Note              string            `json:"note,omitempty"`
	Error             string            `json:"error,omitempty"`
}

// TicketSnapshot is what we last saw for a ticket — used to dedupe polls and
// power the web UI's recent-activity view.
type TicketSnapshot struct {
	ID        string    `json:"id"`
	Source    string    `json:"source"`
	Key       string    `json:"key,omitempty"`
	Title     string    `json:"title,omitempty"`
	URL       string    `json:"url,omitempty"`
	Status    string    `json:"status,omitempty"`
	Assignee  string    `json:"assignee,omitempty"`
	Labels    []string  `json:"labels,omitempty"`
	Project   string    `json:"project,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
	LastSeen  time.Time `json:"last_seen"`
}

// ChatAuth records a Telegram chat that has authenticated against the bot
// (by sending `/auth <secret>` once). The chat ID is then trusted for every
// subsequent message until the user runs `/logout` or an operator removes
// the entry. Persisted in memory.json so authentication survives restarts.
type ChatAuth struct {
	ChatID       int64     `json:"chat_id"`
	Username     string    `json:"username,omitempty"`
	DisplayName  string    `json:"display_name,omitempty"`
	AuthorizedAt time.Time `json:"authorized_at"`
	LastSeen     time.Time `json:"last_seen,omitempty"`
}

// DaemonStatus is a tiny live-status snapshot for the UI / `goon status`.
//
// Paused is the source of truth for the pause/resume control surface:
// CLI (`goon pause` / `goon resume`), web UI (POST /api/daemon/pause),
// and Telegram bot (`/pause` / `/resume`) all flip this single flag.
// The daemon's poll loop reads it every tick (via memory.Reload) and
// skips pollAndRun when true. Already-running workflows are unaffected
// — Pause is the equivalent of "stop picking up new work," not "kill
// in-flight tasks."
type DaemonStatus struct {
	Running        bool      `json:"running"`
	Paused         bool      `json:"paused,omitempty"`
	StartedAt      time.Time `json:"started_at,omitempty"`
	LastPoll       time.Time `json:"last_poll,omitempty"`
	LastTicket     string    `json:"last_ticket,omitempty"`
	ActiveWorkflow string    `json:"active_workflow,omitempty"`
	BoardName      string    `json:"board_name,omitempty"`
	HostName       string    `json:"host_name,omitempty"`
	PID            int       `json:"pid,omitempty"`
	WebAddr        string    `json:"web_addr,omitempty"`
}

// Memory is the persistent store on disk + a slice of recent in-memory items.
type Memory struct {
	mu          sync.Mutex
	path        string
	store       storeFile
	lockWarnOnce sync.Once
}

type storeFile struct {
	History      []Interaction             `json:"history"`
	Counts       map[string]int            `json:"command_counts"`
	Questions    []Question                `json:"questions,omitempty"`
	Workflows    []Workflow                `json:"workflows,omitempty"`
	Tickets      map[string]TicketSnapshot `json:"tickets,omitempty"`
	Status       DaemonStatus              `json:"status,omitempty"`
	NextQID      int64                     `json:"next_qid,omitempty"`
	TelegramAuth []ChatAuth                `json:"telegram_auth,omitempty"`

	// RepoChoices remembers the resolved repo path per project key
	// (Jira project, GitHub "owner/repo"). Populated when the
	// confirm_repo gate succeeds — the next ticket from the same
	// project skips the gate's "where do I work?" branch and uses
	// the learned path. Env GOON_REPO_MAP exact matches still win
	// over learned values; learned wins over the "*" wildcard
	// fallback so a single explicit confirmation overrides a vague
	// default.
	RepoChoices map[string]string `json:"repo_choices,omitempty"`
}

// New opens (or creates) the memory file. If path is empty it defaults to
// <storage.Root()>/memory.json (i.e. ./storage/memory.json by default,
// or whatever GOON_STORAGE_DIR points to).
//
// The old fallback to ~/.goon/memory.json was removed when we moved to
// per-project storage; users wanting global state can set
// GOON_STORAGE_DIR or GOON_MEMORY_PATH explicitly.
func New(path string) (*Memory, error) {
	if path == "" {
		path = storage.Path("memory.json")
	}
	// Catch the easy first-run footgun: GOON_MEMORY_PATH pointed at a
	// directory (most often the notes dir at ./storage/memory). The
	// stdlib's "is a directory" error doesn't tell the user which env
	// var to fix, and the names differ by one character, so spell it
	// out explicitly. Stat-first means we never call os.ReadFile on a
	// directory and can produce an actionable message.
	if info, statErr := os.Stat(path); statErr == nil && info.IsDir() {
		def := storage.Path("memory.json")
		notesDir := storage.Path("memory")
		return nil, fmt.Errorf(
			"memory: configured path %q is a directory, not a JSON file.\n"+
				"  GOON_MEMORY_PATH must point at a .json file (default: %s).\n"+
				"  The active notes directory is configured via GOON_MEMORY_DIR (default: %s).\n"+
				"  Likely fix: unset GOON_MEMORY_PATH, or set it to %q.",
			path, def, notesDir, path+".json")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	m := &Memory{path: path, store: storeFile{
		Counts:      map[string]int{},
		Tickets:     map[string]TicketSnapshot{},
		RepoChoices: map[string]string{},
	}}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &m.store)
		if m.store.Counts == nil {
			m.store.Counts = map[string]int{}
		}
		if m.store.Tickets == nil {
			m.store.Tickets = map[string]TicketSnapshot{}
		}
		if m.store.RepoChoices == nil {
			m.store.RepoChoices = map[string]string{}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("memory: read %s: %w", path, err)
	}
	return m, nil
}

// Disabled returns a no-op memory used when persistence is unavailable.
func Disabled() *Memory {
	return &Memory{path: "", store: storeFile{
		Counts:  map[string]int{},
		Tickets: map[string]TicketSnapshot{},
	}}
}

// Append records an interaction.
func (m *Memory) Append(i Interaction) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if i.When.IsZero() {
		i.When = time.Now()
	}
	m.store.History = append(m.store.History, i)
	if len(m.store.History) > 200 {
		m.store.History = m.store.History[len(m.store.History)-200:]
	}
	if i.Command != "" {
		m.store.Counts[i.Command]++
	}
	m.flush()
}

// RecentSummary returns at most n recent interactions.
func (m *Memory) RecentSummary(n int) []Interaction {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.store.History) == 0 {
		return nil
	}
	if n > len(m.store.History) {
		n = len(m.store.History)
	}
	out := make([]Interaction, n)
	copy(out, m.store.History[len(m.store.History)-n:])
	return out
}

// FrequentCommands returns the top-k most frequent commands.
func (m *Memory) FrequentCommands(k int) []string {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	type kv struct {
		k string
		v int
	}
	all := make([]kv, 0, len(m.store.Counts))
	for cmd, n := range m.store.Counts {
		all = append(all, kv{cmd, n})
	}
	// Simple selection sort — small k.
	for i := 0; i < len(all); i++ {
		for j := i + 1; j < len(all); j++ {
			if all[j].v > all[i].v {
				all[i], all[j] = all[j], all[i]
			}
		}
	}
	if k > len(all) {
		k = len(all)
	}
	out := make([]string, k)
	for i := 0; i < k; i++ {
		out[i] = all[i].k
	}
	return out
}

// --- Question API ----------------------------------------------------------

// maxQuestions caps the question history. Re-plan loops + months of
// uptime would otherwise grow the queue unbounded. We keep the most
// recent N (mix of pending + answered) — pending always wins eviction
// because the user might still need to see them.
const maxQuestions = 500

// AskQuestion records a new pending question and returns its id.
// Older answered questions are evicted when the cap is exceeded.
func (m *Memory) AskQuestion(q Question) string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.store.NextQID++
	if q.When.IsZero() {
		q.When = time.Now()
	}
	if q.ID == "" {
		q.ID = fmt.Sprintf("q-%d", m.store.NextQID)
	}
	m.store.Questions = append(m.store.Questions, q)
	m.pruneQuestions()
	m.flush()
	return q.ID
}

// pruneQuestions caps the question slice. Caller must hold m.mu.
// Eviction order: oldest answered first; if everything is pending,
// keep all (operators need to see them).
func (m *Memory) pruneQuestions() {
	if len(m.store.Questions) <= maxQuestions {
		return
	}
	// Drop oldest answered first.
	kept := make([]Question, 0, maxQuestions)
	answered := make([]Question, 0, len(m.store.Questions))
	for _, q := range m.store.Questions {
		if q.Pending() {
			kept = append(kept, q)
		} else {
			answered = append(answered, q)
		}
	}
	// Sort answered by AnsweredAt asc; keep newest until we hit cap.
	for i := 1; i < len(answered); i++ {
		for j := i; j > 0 && answered[j].AnsweredAt.Before(answered[j-1].AnsweredAt); j-- {
			answered[j], answered[j-1] = answered[j-1], answered[j]
		}
	}
	for len(kept)+len(answered) > maxQuestions && len(answered) > 0 {
		answered = answered[1:] // drop oldest answered
	}
	kept = append(kept, answered...)
	m.store.Questions = kept
}

// PendingQuestions returns questions still awaiting an answer.
func (m *Memory) PendingQuestions() []Question {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Question{}
	for _, q := range m.store.Questions {
		if q.Pending() {
			out = append(out, q)
		}
	}
	return out
}

// AllQuestions returns the full question log (most recent first).
func (m *Memory) AllQuestions() []Question {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Question, len(m.store.Questions))
	copy(out, m.store.Questions)
	// Reverse for most-recent-first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// AnswerQuestion records an answer for a pending question. Returns false if
// the id is unknown or already answered.
func (m *Memory) AnswerQuestion(id, answer string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.store.Questions {
		if m.store.Questions[i].ID == id && m.store.Questions[i].Pending() {
			m.store.Questions[i].Answer = answer
			m.store.Questions[i].AnsweredAt = time.Now()
			m.flush()
			return true
		}
	}
	return false
}

// FindAnswer looks up an answer for a previously-asked question with the
// same TicketID + Question text. Useful for the daemon picking up where it
// left off after `goon train`.
func (m *Memory) FindAnswer(ticketID, question string) (string, bool) {
	if m == nil {
		return "", false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, q := range m.store.Questions {
		if q.TicketID == ticketID && q.Question == question && !q.Pending() {
			return q.Answer, true
		}
	}
	return "", false
}

// --- Workflow API ----------------------------------------------------------

// UpsertWorkflow inserts or replaces a workflow by id.
func (m *Memory) UpsertWorkflow(w Workflow) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	w.UpdatedAt = time.Now()
	for i := range m.store.Workflows {
		if m.store.Workflows[i].ID == w.ID {
			m.store.Workflows[i] = w
			m.flush()
			return
		}
	}
	if w.StartedAt.IsZero() {
		w.StartedAt = w.UpdatedAt
	}
	m.store.Workflows = append(m.store.Workflows, w)
	if len(m.store.Workflows) > 200 {
		m.store.Workflows = m.store.Workflows[len(m.store.Workflows)-200:]
	}
	m.flush()
}

// GetWorkflow returns a workflow by id.
func (m *Memory) GetWorkflow(id string) (Workflow, bool) {
	if m == nil {
		return Workflow{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, w := range m.store.Workflows {
		if w.ID == id {
			return w, true
		}
	}
	return Workflow{}, false
}

// ListWorkflows returns workflows most-recent-first, limited to n.
func (m *Memory) ListWorkflows(n int) []Workflow {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	all := make([]Workflow, len(m.store.Workflows))
	copy(all, m.store.Workflows)
	// reverse
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}
	if n > 0 && n < len(all) {
		all = all[:n]
	}
	return all
}

// HistoryWorkflowsFor returns every workflow record for a ticket, newest
// first. The web UI's detail view uses this so users can see the full
// history of attempts (failed triage, replans, re-runs) for one ticket.
func (m *Memory) HistoryWorkflowsFor(ticketID string) []Workflow {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Workflow
	for _, w := range m.store.Workflows {
		if w.TicketID == ticketID {
			out = append(out, w)
		}
	}
	// Sort newest first by UpdatedAt with a stable fallback to StartedAt.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			a, b := out[j-1], out[j]
			if a.UpdatedAt.After(b.UpdatedAt) ||
				(a.UpdatedAt.Equal(b.UpdatedAt) && a.StartedAt.After(b.StartedAt)) {
				break
			}
			out[j-1], out[j] = b, a
		}
	}
	return out
}

// GetQuestion looks up a Question by id. Returns ok=false when absent.
// Used by the workflow-detail view to render the pending question
// inline (with an answer form) so users don't have to flip tabs.
func (m *Memory) GetQuestion(id string) (Question, bool) {
	if m == nil {
		return Question{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, q := range m.store.Questions {
		if q.ID == id {
			return q, true
		}
	}
	return Question{}, false
}

// OpenWorkflowFor returns the active (non-terminal) workflow for a ticket,
// if one exists. The most recently-updated open workflow wins so the daemon
// resumes the latest run after a crash + restart.
func (m *Memory) OpenWorkflowFor(ticketID string) (Workflow, bool) {
	if m == nil {
		return Workflow{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var best Workflow
	found := false
	for _, w := range m.store.Workflows {
		if w.TicketID != ticketID {
			continue
		}
		if terminal(w.State) {
			continue
		}
		if !found || w.UpdatedAt.After(best.UpdatedAt) {
			best = w
			found = true
		}
	}
	return best, found
}

// ResumableWorkflow returns the most recently-updated open workflow that is
// awaiting approval AND whose pending question has been answered. The daemon
// calls this every tick before picking a fresh ticket so paused workflows
// resume as soon as the user replies via `goon train` or the web UI.
//
// Returns ok=false if nothing is ready to resume.
func (m *Memory) ResumableWorkflow() (Workflow, bool) {
	if m == nil {
		return Workflow{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	answered := map[string]bool{}
	for _, q := range m.store.Questions {
		if !q.Pending() {
			answered[q.ID] = true
		}
	}
	var best Workflow
	found := false
	for _, w := range m.store.Workflows {
		// Eligible for resume if EITHER:
		//  - awaiting approval AND the question now has an answer
		//  - re-plan path: state=WFTriaging with a recorded
		//    replan_feedback approval (set when the user rejected
		//    the previous plan with feedback). The daemon should
		//    re-enter triage on the next tick instead of waiting
		//    for an answered question.
		eligible := false
		switch {
		case w.State == WFAwaitingApproval && w.PendingQuestionID != "" && answered[w.PendingQuestionID]:
			eligible = true
		case w.State == WFTriaging && w.Approvals != nil && w.Approvals["replan_feedback"] != "":
			eligible = true
		}
		if !eligible {
			continue
		}
		if !found || w.UpdatedAt.After(best.UpdatedAt) {
			best = w
			found = true
		}
	}
	return best, found
}

// HasOpenWorkflowFor returns true if there is a non-terminal workflow for
// the given ticket id.
func (m *Memory) HasOpenWorkflowFor(ticketID string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, w := range m.store.Workflows {
		if w.TicketID == ticketID && !terminal(w.State) {
			return true
		}
	}
	return false
}

// HasCompletedWorkflowFor returns true if a successful (Done) workflow
// already exists for the ticket — used by the daemon to avoid redoing work.
func (m *Memory) HasCompletedWorkflowFor(ticketID string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, w := range m.store.Workflows {
		if w.TicketID == ticketID && w.State == WFDone {
			return true
		}
	}
	return false
}

func terminal(s WorkflowState) bool {
	return s == WFDone || s == WFFailed
}

// --- Ticket snapshot API ---------------------------------------------------

// maxTicketSnapshots caps how many ticket snapshots we keep on disk. The
// daemon writes a snapshot for every ticket it sees on every poll tick;
// without a cap, a busy backlog grows memory.json unboundedly. 500 is
// enough to cover several months of unique tickets per project before
// the oldest get evicted, which is plenty for dedupe purposes.
const maxTicketSnapshots = 500

// SeenTicket updates the last-seen snapshot for a ticket id. When the
// ticket map exceeds maxTicketSnapshots, the oldest entries (by
// LastSeen) are dropped to keep memory.json bounded.
func (m *Memory) SeenTicket(s TicketSnapshot) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.store.Tickets == nil {
		m.store.Tickets = map[string]TicketSnapshot{}
	}
	if s.LastSeen.IsZero() {
		s.LastSeen = time.Now()
	}
	m.store.Tickets[s.ID] = s
	if len(m.store.Tickets) > maxTicketSnapshots {
		pruneOldestTickets(m.store.Tickets, maxTicketSnapshots)
	}
	m.flush()
}

// pruneOldestTickets evicts entries with the oldest LastSeen until the
// map size is at most cap. O(n log n) per call; only invoked when the
// map crosses the threshold, so amortized cost stays low.
func pruneOldestTickets(m map[string]TicketSnapshot, cap int) {
	if len(m) <= cap {
		return
	}
	type kv struct {
		id   string
		seen time.Time
	}
	all := make([]kv, 0, len(m))
	for id, t := range m {
		all = append(all, kv{id, t.LastSeen})
	}
	// Insertion sort — list rarely exceeds the cap by more than a few.
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && all[j].seen.Before(all[j-1].seen); j-- {
			all[j], all[j-1] = all[j-1], all[j]
		}
	}
	drop := len(all) - cap
	for i := 0; i < drop; i++ {
		delete(m, all[i].id)
	}
}

// ListTickets returns all known ticket snapshots.
func (m *Memory) ListTickets() []TicketSnapshot {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]TicketSnapshot, 0, len(m.store.Tickets))
	for _, t := range m.store.Tickets {
		out = append(out, t)
	}
	return out
}

// GetTicket fetches a ticket snapshot by id or by Key (case-insensitive
// match on Key, exact match on ID). Returns ok=false when neither matches.
// Useful for the Telegram bot's `/ticket <id-or-key>` lookup — users
// usually have the human-friendly Key on hand, not the wire ID.
func (m *Memory) GetTicket(idOrKey string) (TicketSnapshot, bool) {
	if m == nil {
		return TicketSnapshot{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.store.Tickets[idOrKey]; ok {
		return t, true
	}
	want := strings.ToLower(strings.TrimSpace(idOrKey))
	for _, t := range m.store.Tickets {
		if strings.ToLower(t.Key) == want || strings.ToLower(t.ID) == want {
			return t, true
		}
	}
	return TicketSnapshot{}, false
}

// WorkflowsForTicket returns every workflow record (open + completed +
// failed) for the ticket id, most-recently-updated first. Used by the
// Telegram bot's detail view to show what goon did or is doing.
func (m *Memory) WorkflowsForTicket(ticketID string) []Workflow {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Workflow{}
	for _, w := range m.store.Workflows {
		if w.TicketID == ticketID {
			out = append(out, w)
		}
	}
	// Sort by UpdatedAt descending (insertion sort — workflows-per-ticket
	// is small).
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].UpdatedAt.After(out[j-1].UpdatedAt); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// --- Telegram chat auth API ------------------------------------------------

// maxTelegramAuth caps the number of authorized Telegram chats kept in
// memory.json. Goon's auth model is single-user, so going above this is
// almost always a misconfiguration (replayed /auth from automation, a
// shared bot, etc.) — capping here protects memory.json from drifting
// to megabytes.
const maxTelegramAuth = 100

// AuthorizeChat marks chatID as a trusted Telegram conversation. Idempotent
// — repeated calls update display fields and refresh AuthorizedAt. When
// the auth list exceeds maxTelegramAuth, the oldest entries (by
// AuthorizedAt) are evicted so the file stays bounded.
func (m *Memory) AuthorizeChat(chatID int64, username, displayName string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for i := range m.store.TelegramAuth {
		if m.store.TelegramAuth[i].ChatID == chatID {
			m.store.TelegramAuth[i].Username = username
			m.store.TelegramAuth[i].DisplayName = displayName
			m.store.TelegramAuth[i].AuthorizedAt = now
			m.store.TelegramAuth[i].LastSeen = now
			m.flush()
			return
		}
	}
	m.store.TelegramAuth = append(m.store.TelegramAuth, ChatAuth{
		ChatID:       chatID,
		Username:     username,
		DisplayName:  displayName,
		AuthorizedAt: now,
		LastSeen:     now,
	})
	if len(m.store.TelegramAuth) > maxTelegramAuth {
		m.store.TelegramAuth = pruneOldestAuth(m.store.TelegramAuth, maxTelegramAuth)
	}
	m.flush()
}

// pruneOldestAuth drops entries with the oldest AuthorizedAt until the
// slice length is at most cap.
func pruneOldestAuth(in []ChatAuth, cap int) []ChatAuth {
	if len(in) <= cap {
		return in
	}
	out := make([]ChatAuth, len(in))
	copy(out, in)
	// Insertion sort by AuthorizedAt asc — small slice, so simple wins.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].AuthorizedAt.Before(out[j-1].AuthorizedAt); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out[len(out)-cap:]
}

// PruneStaleAuth drops authorized chats whose AuthorizedAt is older than
// maxAge. Returns the number of entries removed. Currently unused by the
// daemon but exposed so an admin command (or a periodic sweep) can tidy
// up forgotten Telegram authorizations without manual edits.
func (m *Memory) PruneStaleAuth(maxAge time.Duration) int {
	if m == nil || maxAge <= 0 {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoff := time.Now().Add(-maxAge)
	kept := m.store.TelegramAuth[:0]
	dropped := 0
	for _, a := range m.store.TelegramAuth {
		if a.AuthorizedAt.Before(cutoff) {
			dropped++
			continue
		}
		kept = append(kept, a)
	}
	if dropped > 0 {
		m.store.TelegramAuth = kept
		m.flush()
	}
	return dropped
}

// IsChatAuthorized reports whether chatID has previously authenticated.
func (m *Memory) IsChatAuthorized(chatID int64) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.store.TelegramAuth {
		if c.ChatID == chatID {
			return true
		}
	}
	return false
}

// TouchChat records when an authorized chat last sent a message. Best-effort:
// silently does nothing for unauthorized chats.
func (m *Memory) TouchChat(chatID int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.store.TelegramAuth {
		if m.store.TelegramAuth[i].ChatID == chatID {
			m.store.TelegramAuth[i].LastSeen = time.Now()
			m.flush()
			return
		}
	}
}

// RevokeChat removes chatID from the allowlist (logout). Returns true when
// an entry was removed.
func (m *Memory) RevokeChat(chatID int64) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, c := range m.store.TelegramAuth {
		if c.ChatID == chatID {
			m.store.TelegramAuth = append(m.store.TelegramAuth[:i], m.store.TelegramAuth[i+1:]...)
			m.flush()
			return true
		}
	}
	return false
}

// AuthorizedChats returns the current list of trusted chats (most recently
// seen first).
func (m *Memory) AuthorizedChats() []ChatAuth {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ChatAuth, len(m.store.TelegramAuth))
	copy(out, m.store.TelegramAuth)
	// Sort by LastSeen desc (insertion sort is fine — list is small).
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].LastSeen.After(out[j-1].LastSeen); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// --- Daemon status API -----------------------------------------------------

// SetStatus replaces the live daemon status block.
func (m *Memory) SetStatus(s DaemonStatus) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.store.Status = s
	m.flush()
}

// GetStatus returns the current daemon status.
func (m *Memory) GetStatus() DaemonStatus {
	if m == nil {
		return DaemonStatus{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.store.Status
}

// SetPaused flips the daemon's pause flag inside the persisted status
// block. Used by `goon pause` / `goon resume`, the web UI's pause
// button, and the Telegram bot's /pause /resume commands. The daemon's
// poll loop reads this every tick (after Reload) and skips pollAndRun
// when true. Idempotent — calling SetPaused(true) on a paused daemon
// is a no-op.
func (m *Memory) SetPaused(paused bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.store.Status.Paused = paused
	m.flush()
}

// IsPaused reports whether the daemon's pause flag is set.
func (m *Memory) IsPaused() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.store.Status.Paused
}

// --- Repo-choice memory API ------------------------------------------------

// RecordRepoChoice persists project→repo so the next ticket from the
// same project doesn't re-ask the confirm_repo gate. Called from
// workflow.phaseConfirmRepo right after the user (or auto-approve)
// confirms the path. Empty project or repo strings are ignored — we
// only remember concrete choices.
func (m *Memory) RecordRepoChoice(project, repo string) {
	if m == nil {
		return
	}
	if strings.TrimSpace(project) == "" || strings.TrimSpace(repo) == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.store.RepoChoices == nil {
		m.store.RepoChoices = map[string]string{}
	}
	if existing, ok := m.store.RepoChoices[project]; ok && existing == repo {
		return // no-op write avoids touching disk on every poll
	}
	m.store.RepoChoices[project] = repo
	m.flush()
}

// LookupRepoChoice returns the previously-confirmed repo path for the
// project, if one was recorded. Used by the workflow engine to skip
// the confirm_repo gate when a learned choice already exists for the
// ticket's project.
func (m *Memory) LookupRepoChoice(project string) (string, bool) {
	if m == nil {
		return "", false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.store.RepoChoices[project]
	return v, ok
}

// ForgetRepoChoice drops the learned mapping for project (if any).
// Useful when a user wants to re-route a project to a new repo —
// CLI: `goon repo forget ENG`.
func (m *Memory) ForgetRepoChoice(project string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.store.RepoChoices[project]; !ok {
		return false
	}
	delete(m.store.RepoChoices, project)
	m.flush()
	return true
}

// RepoChoices returns a copy of the current learned mapping. Stable
// keys/values; callers can mutate the result freely.
func (m *Memory) RepoChoices() map[string]string {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]string, len(m.store.RepoChoices))
	for k, v := range m.store.RepoChoices {
		out[k] = v
	}
	return out
}

// flush writes the in-memory store to disk. Before writing, it merges any
// external changes made by other processes (e.g. `goon train` answering a
// question while the daemon is running) so we don't clobber them.
//
// Caller must hold m.mu.
//
// Multi-process safety: a sibling lock file (path+".lock") is locked
// exclusively via flock(2) for the duration of the read-merge-write
// sequence, so two `goon` processes writing the same memory.json never
// interleave.
func (m *Memory) flush() {
	if m.path == "" {
		return
	}
	unlock, err := lockFile(m.path + ".lock")
	if err == nil {
		defer unlock()
	} else {
		// Lock acquisition failed (NFS, permission, locked-out FS, etc.).
		// Warn ONCE per Memory instance so users on unsupported filesystems
		// know they've lost multi-process safety. Then proceed — the
		// single-process case still works correctly because m.mu serializes
		// in-process writers.
		m.lockWarnOnce.Do(func() {
			fmt.Fprintf(os.Stderr,
				"goon: warning: could not lock %s.lock: %v\n"+
					"goon: concurrent writers from a second goon process may interleave writes.\n",
				m.path, err)
		})
	}
	// Re-read the file under the lock to pick up external mutations.
	if data, err := os.ReadFile(m.path); err == nil {
		var disk storeFile
		if err := json.Unmarshal(data, &disk); err == nil {
			m.store = mergeStores(disk, m.store)
		}
	}
	// Re-apply caps after merge — a freshly-merged store can exceed the
	// caps when disk holds entries we don't have in memory yet (e.g.
	// after a fresh process loads an old, unbounded memory.json that
	// pre-dates this pruning logic).
	if len(m.store.Tickets) > maxTicketSnapshots {
		pruneOldestTickets(m.store.Tickets, maxTicketSnapshots)
	}
	if len(m.store.TelegramAuth) > maxTelegramAuth {
		m.store.TelegramAuth = pruneOldestAuth(m.store.TelegramAuth, maxTelegramAuth)
	}
	data, err := json.MarshalIndent(m.store, "", "  ")
	if err != nil {
		return
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, m.path)
}

// mergeStores merges two storeFile snapshots into a unified view.
//
// Ownership rules:
//   - Status, History, Counts — the daemon owns these (only one daemon
//     at a time), so the in-memory copy wins.
//   - Workflows — primarily daemon-owned, but CLI tools like
//     `goon train` and `goon repo` are SEPARATE processes that load
//     Memory at startup with whatever Workflows existed then. Without
//     a per-id merge, those CLI processes' flush would silently
//     clobber any newer Workflow updates the daemon wrote since.
//     Merge by id, preferring the more recent UpdatedAt.
//   - Tickets — prefer the more recent LastSeen so concurrent writers
//     don't lose snapshot data.
//   - Questions — both daemon (via ask_user) and CLI (`goon train`)
//     write these. Merge by id: prefer the version with an answer;
//     if both have answers, prefer the more recent answered_at.
//   - NextQID — take the max so concurrent writers don't collide.
func mergeStores(disk, mem storeFile) storeFile {
	out := mem // start from in-memory; we'll overlay disk-side question changes

	if len(disk.Questions) == 0 && len(mem.Questions) == 0 {
		// nothing to merge
	} else {
		byID := map[string]Question{}
		order := []string{}

		// Index disk first.
		for _, q := range disk.Questions {
			byID[q.ID] = q
			order = append(order, q.ID)
		}
		// Overlay mem, picking the more authoritative answer.
		for _, q := range mem.Questions {
			if existing, ok := byID[q.ID]; ok {
				byID[q.ID] = preferAnswered(existing, q)
			} else {
				byID[q.ID] = q
				order = append(order, q.ID)
			}
		}

		out.Questions = make([]Question, 0, len(order))
		for _, id := range order {
			out.Questions = append(out.Questions, byID[id])
		}
	}

	if disk.NextQID > out.NextQID {
		out.NextQID = disk.NextQID
	}

	// Workflows: merge by id, prefer newer UpdatedAt. Without this,
	// a `goon train answer` (separate process) loaded Memory with
	// stale Workflows then flushed back its in-memory copy,
	// silently rolling back any daemon-side workflow progress that
	// happened in the interim.
	if len(disk.Workflows) > 0 {
		byID := map[string]Workflow{}
		order := []string{}
		for _, w := range disk.Workflows {
			byID[w.ID] = w
			order = append(order, w.ID)
		}
		for _, w := range out.Workflows {
			if existing, ok := byID[w.ID]; ok {
				if w.UpdatedAt.After(existing.UpdatedAt) {
					byID[w.ID] = w
				}
			} else {
				byID[w.ID] = w
				order = append(order, w.ID)
			}
		}
		merged := make([]Workflow, 0, len(order))
		for _, id := range order {
			merged = append(merged, byID[id])
		}
		// Apply the same cap that UpsertWorkflow uses, by-UpdatedAt.
		if len(merged) > 200 {
			// Sort newest first, drop tail. Insertion sort — N stays small.
			for i := 1; i < len(merged); i++ {
				for j := i; j > 0 && merged[j].UpdatedAt.After(merged[j-1].UpdatedAt); j-- {
					merged[j], merged[j-1] = merged[j-1], merged[j]
				}
			}
			merged = merged[:200]
		}
		out.Workflows = merged
	}

	// Tickets: prefer the more recent LastSeen so external `goon train`
	// snapshots aren't clobbered.
	if len(disk.Tickets) > 0 {
		if out.Tickets == nil {
			out.Tickets = map[string]TicketSnapshot{}
		}
		for id, dt := range disk.Tickets {
			mt, ok := out.Tickets[id]
			if !ok || dt.LastSeen.After(mt.LastSeen) {
				out.Tickets[id] = dt
			}
		}
	}

	// RepoChoices: mem wins absolutely. The previous merge-disk-back
	// implementation silently undid `goon repo forget` — the CLI
	// process loaded Memory (had ENG), called ForgetRepoChoice
	// (removed ENG from mem), then flushed, and the merge re-added
	// ENG from disk. Result: forget appeared to succeed but the
	// next read found the entry alive.
	//
	// RepoChoices is only ever written by the engine (daemon
	// in-process) or by `goon repo` CLI; there's no parallel-write
	// scenario where disk has additions mem doesn't know about.
	// Last writer wins is the right policy here. `out` already
	// holds mem's RepoChoices via `out := mem` at the top.

	return out
}

// preferAnswered returns whichever of a or b has an answer (or, if both, the
// one answered more recently).
func preferAnswered(a, b Question) Question {
	switch {
	case !a.Pending() && b.Pending():
		return a
	case a.Pending() && !b.Pending():
		return b
	case !a.Pending() && !b.Pending():
		if b.AnsweredAt.After(a.AnsweredAt) {
			return b
		}
		return a
	default:
		// both pending — prefer the one with the older creation timestamp,
		// matching what the user expects to see in the queue.
		if !a.When.IsZero() && (b.When.IsZero() || a.When.Before(b.When)) {
			return a
		}
		return b
	}
}

// Reload pulls the latest store from disk into memory, merging anything we
// haven't flushed yet. Used by the daemon at the start of each poll cycle so
// it sees user answers as soon as they're written.
//
// RepoChoices is treated specially: disk wins absolutely. The daemon
// always flushes its own writes immediately (RecordRepoChoice → flush),
// so by Reload time, any daemon-side additions are already on disk.
// Anything missing from disk that mem still holds is therefore a stale
// entry — typically because a separate `goon repo forget` deleted it.
// Without this snapshot, mergeStores' mem-wins policy would silently
// resurrect forgotten entries on the daemon's next flush.
func (m *Memory) Reload() {
	if m == nil || m.path == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	data, err := os.ReadFile(m.path)
	if err != nil {
		return
	}
	var disk storeFile
	if err := json.Unmarshal(data, &disk); err != nil {
		return
	}
	if disk.Counts == nil {
		disk.Counts = map[string]int{}
	}
	if disk.Tickets == nil {
		disk.Tickets = map[string]TicketSnapshot{}
	}
	if disk.RepoChoices == nil {
		disk.RepoChoices = map[string]string{}
	}
	m.store = mergeStores(disk, m.store)
	// Snapshot RepoChoices from disk after the merge — see fn doc.
	m.store.RepoChoices = disk.RepoChoices
}
