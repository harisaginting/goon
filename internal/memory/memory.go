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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/harisaginting/goon/internal/notes"
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

// Question kinds. Kind distinguishes a workflow approval gate (blocks a
// ticket; surfaced in the Workflows tab) from a self-learning question goon
// raised while reflecting on standby (surfaced in the Questions tab). An
// empty Kind is treated as a gate for backwards-compat with older
// memory.json files written before this field existed.
const (
	QuestionKindGate     = "gate"
	QuestionKindLearning = "learning"
)

// Question is something goon is asking the user to answer via `goon train`,
// the web UI, or Telegram. Two flavours, distinguished by Kind:
//   - gate     — a workflow can't proceed without an answer (confirm_repo,
//     approve_plan, or an agent that called ask_user mid-ticket).
//   - learning — goon's daily standby reflection wants to understand
//     something about the project/itself; answers are saved to LEARNED.md.
type Question struct {
	ID         string    `json:"id"` // stable id (e.g. "q-1730000000-1")
	When       time.Time `json:"when"`
	Kind       string    `json:"kind,omitempty"` // "gate" (default) | "learning"
	TicketID   string    `json:"ticket_id,omitempty"`
	WorkflowID string    `json:"workflow_id,omitempty"`
	Question   string    `json:"question"`
	Answer     string    `json:"answer,omitempty"`
	AnsweredAt time.Time `json:"answered_at,omitempty"`
}

// IsLearning reports whether this is a self-learning question (Kind explicitly
// "learning"). Everything else — including the empty/legacy Kind — counts as
// a workflow gate.
func (q Question) IsLearning() bool { return q.Kind == QuestionKindLearning }

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
	// NeedsRepo is the triage classification: nil = legacy workflow
	// (pre-feature, treated as needs_repo=true); pointer-to-true =
	// classified as code work; pointer-to-false = pure research /
	// docs / comms ticket — skip the confirm_repo gate, skip the
	// test + open_pr phases. Plan still runs via the agent loop but
	// without any git operations. Persisted in memory.json so a
	// paused workflow resumes with the same classification.
	NeedsRepo         *bool             `json:"needs_repo,omitempty"`
	Branch            string            `json:"branch,omitempty"`
	Plan              []PlanStep        `json:"plan,omitempty"`
	PRURL             string            `json:"pr_url,omitempty"`
	VerifyRuns        int               `json:"verify_runs"`
	Note              string            `json:"note,omitempty"`
	Error             string            `json:"error,omitempty"`
}

// WorkflowNeedsRepo reports whether a workflow requires a git repo to
// execute. Returns true for legacy workflows (NeedsRepo == nil) so
// pre-feature records keep behaving like before — when the field is
// absent we assume "yes, this is code work." Centralised so phase
// implementations don't each re-derive the nil-vs-false semantics.
func WorkflowNeedsRepo(w Workflow) bool {
	if w.NeedsRepo == nil {
		return true
	}
	return *w.NeedsRepo
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

// ReviewMark records the state of the last PR review goon drafted.
// DiffHash is a fingerprint of the diff that was reviewed, so a PR is
// re-reviewed only when its diff actually changes.
type ReviewMark struct {
	DiffHash string    `json:"diff_hash"`
	When     time.Time `json:"when"`
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
	// LastError records the most recent poll-level failure (provider or
	// board unreachable / misconfigured) so the UI can show one clear
	// banner instead of leaving the user to guess why nothing moves.
	// Cleared on the next successful poll.
	LastError   string    `json:"last_error,omitempty"`
	LastErrorAt time.Time `json:"last_error_at,omitempty"`
	// Circuit-breaker state. ConsecutiveFails counts back-to-back
	// infrastructural poll failures; NextRetryAt is the backoff window's
	// end; ErrorClass is the last failure's class (network/auth/
	// rate_limit/model/config/other); AutoPaused is set when the daemon
	// paused *itself* (vs a user pause) so the UI can say so and a manual
	// resume can clear the breaker.
	ConsecutiveFails int       `json:"consecutive_fails,omitempty"`
	NextRetryAt      time.Time `json:"next_retry_at,omitempty"`
	ErrorClass       string    `json:"error_class,omitempty"`
	AutoPaused       bool      `json:"auto_paused,omitempty"`
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

	// LastReflectAt is when the daemon last ran its standby self-learning
	// reflection. Used to throttle reflection to roughly once per
	// ReflectInterval (default daily) while idle. Persisted so a restart
	// doesn't trigger an immediate extra reflection.
	LastReflectAt time.Time `json:"last_reflect_at,omitempty"`

	// ScheduledRuns records when each scheduled automation (by workflow name)
	// last fired, so the daemon's scheduler doesn't double-run inside a minute
	// and the UI can show last/next run.
	ScheduledRuns map[string]time.Time `json:"scheduled_runs,omitempty"`

	// RepoChoices is a legacy per-project repo cache kept only for
	// backwards-compat with old memory.json files. It is no longer
	// consulted for repo selection — that's driven per-ticket by triage +
	// REPOSITORY.md + the confirm_repo gate. Safe to ignore; retained so
	// unmarshalling an existing memory.json doesn't drop the field.
	RepoChoices map[string]string `json:"repo_choices,omitempty"`

	// ReviewSeen / NotifSeen back the PR-review and notification dedup
	// used by `goon review-prs` / `goon notifications` and the Telegram
	// bot's auto loop. ReviewSeen is keyed "host:repo#number" and stores
	// a hash of the diff last drafted, so a PR is re-reviewed only when
	// its diff changes. NotifSeen is keyed "host:id" and just records
	// which inbox items have already been forwarded.
	ReviewSeen map[string]ReviewMark `json:"review_seen,omitempty"`
	NotifSeen  map[string]time.Time  `json:"notif_seen,omitempty"`

	// Ignored tracks ticket keys the user has explicitly opted out of
	// the daemon workflow. The daemon's ticket-picker filters this
	// set, so an ignored ticket never gets a workflow opened against
	// it (no triage, no confirm_repo, nothing). Per-key timestamp is
	// "ignored at" — useful for showing the user when they last
	// opted out + for any future "auto-unclaim after N days" policy.
	Ignored map[string]time.Time `json:"ignored,omitempty"`

	// LocalTickets are user-created tickets stored entirely in memory.json
	// (source="goon"). They participate in the daemon poll loop alongside
	// Jira/GitHub tickets but never require an external connection. The
	// NextLocalID counter monotonically increments so IDs are stable.
	LocalTickets []LocalTicket `json:"local_tickets,omitempty"`

	// PickQueue holds ticket IDs the user explicitly queued from the
	// Tickets tab ("Pick" button). The daemon drains these BEFORE its
	// recency-based auto-pick, so a manual pick runs next. FIFO.
	PickQueue []string `json:"pick_queue,omitempty"`

	// TicketRepos is the per-ticket repo assignment made at pick time
	// (local checkout paths). confirm_repo honors these verbatim and
	// skips the gate — the user already chose which repo(s) to use.
	TicketRepos map[string][]string `json:"ticket_repos,omitempty"`
	NextLocalID  int           `json:"next_local_id,omitempty"`
}

// LocalTicket is a user-created ticket stored locally in memory.json.
// Fields mirror boards.Ticket so it can be fed to the workflow engine
// as source="goon" without any external board connection.
type LocalTicket struct {
	ID          string    `json:"id"`          // e.g. "GOON-3"
	Title       string    `json:"title"`
	Description string    `json:"description,omitempty"`
	Status      string    `json:"status"`      // open | in_progress | in_review | blocked | done
	Labels      []string  `json:"labels,omitempty"`
	Priority    string    `json:"priority,omitempty"` // high | medium | low
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
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
		ReviewSeen:  map[string]ReviewMark{},
		NotifSeen:   map[string]time.Time{},
		Ignored:     map[string]time.Time{},
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
		if m.store.ReviewSeen == nil {
			m.store.ReviewSeen = map[string]ReviewMark{}
		}
		if m.store.NotifSeen == nil {
			m.store.NotifSeen = map[string]time.Time{}
		}
		if m.store.Ignored == nil {
			m.store.Ignored = map[string]time.Time{}
		}
		if m.store.ScheduledRuns == nil {
			m.store.ScheduledRuns = map[string]time.Time{}
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

// LastScheduledRun returns when the named automation last ran (zero if never).
func (m *Memory) LastScheduledRun(name string) time.Time {
	if m == nil {
		return time.Time{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.store.ScheduledRuns == nil {
		return time.Time{}
	}
	return m.store.ScheduledRuns[name]
}

// MarkScheduledRun records that the named automation fired at t.
func (m *Memory) MarkScheduledRun(name string, t time.Time) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.store.ScheduledRuns == nil {
		m.store.ScheduledRuns = map[string]time.Time{}
	}
	m.store.ScheduledRuns[name] = t
	m.flush()
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

// PendingLearningQuestions returns pending self-learning questions only —
// what the Questions tab shows ("goon's questions for you").
func (m *Memory) PendingLearningQuestions() []Question {
	out := []Question{}
	for _, q := range m.PendingQuestions() {
		if q.IsLearning() {
			out = append(out, q)
		}
	}
	return out
}

// PendingGateQuestions returns pending workflow-gate questions only — what
// the Workflows tab surfaces (confirm_repo / approve_plan / agent ask_user).
func (m *Memory) PendingGateQuestions() []Question {
	out := []Question{}
	for _, q := range m.PendingQuestions() {
		if !q.IsLearning() {
			out = append(out, q)
		}
	}
	return out
}

// LastReflect returns when standby self-learning last ran (zero if never).
func (m *Memory) LastReflect() time.Time {
	if m == nil {
		return time.Time{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.store.LastReflectAt
}

// SetLastReflect records the standby-reflection timestamp and flushes.
func (m *Memory) SetLastReflect(t time.Time) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.store.LastReflectAt = t
	m.flush()
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
	var learned *Question
	ok := false
	for i := range m.store.Questions {
		if m.store.Questions[i].ID == id && m.store.Questions[i].Pending() {
			m.store.Questions[i].Answer = answer
			m.store.Questions[i].AnsweredAt = time.Now()
			m.flush()
			if m.store.Questions[i].IsLearning() {
				q := m.store.Questions[i]
				learned = &q
			}
			ok = true
			break
		}
	}
	m.mu.Unlock()
	// Persist answered learning questions into LEARNED.md so the knowledge
	// is durable and the agent sees it on future runs. Done outside the lock
	// (file I/O) and best-effort — a notes hiccup must not fail the answer.
	if learned != nil {
		persistLearnedAnswer(*learned)
	}
	return ok
}

// persistLearnedAnswer appends an answered learning question to LEARNED.md.
// Best-effort: errors are swallowed (the answer is already recorded in
// memory.json regardless).
func persistLearnedAnswer(q Question) {
	store, err := notes.New("")
	if err != nil {
		return
	}
	entry := fmt.Sprintf("\n### %s\nQ: %s\nA: %s\n",
		time.Now().Format("2006-01-02 15:04"),
		strings.TrimSpace(q.Question),
		strings.TrimSpace(q.Answer))
	_ = store.Append(notes.LearnedFilename, entry)
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

// ReconcileTickets removes cached ticket snapshots for one board source
// that the latest poll no longer returns — so tightening JIRA_JQL /
// GITHUB_* immediately clears tickets that fell out of the filter, instead
// of leaving stale ones (e.g. assigned to someone else) lingering in the
// UI and chat context until they age out of the 500-snapshot cap.
//
// Safety: only the named source is touched (other boards + local tickets
// are left alone), and any ticket that still has a workflow on record is
// kept so an in-flight or resumable run is never orphaned. Returns the
// number of snapshots removed.
func (m *Memory) ReconcileTickets(source string, keepIDs []string) int {
	if m == nil || strings.TrimSpace(source) == "" {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.store.Tickets) == 0 {
		return 0
	}
	keep := make(map[string]bool, len(keepIDs))
	for _, id := range keepIDs {
		keep[id] = true
	}
	hasWF := make(map[string]bool)
	for _, w := range m.store.Workflows {
		if w.TicketID != "" {
			hasWF[w.TicketID] = true
		}
	}
	removed := 0
	for id, t := range m.store.Tickets {
		if t.Source != source || keep[id] || hasWF[id] {
			continue
		}
		delete(m.store.Tickets, id)
		removed++
	}
	if removed > 0 {
		m.flush()
	}
	return removed
}

// ClearTickets wipes the entire cached ticket inventory (all sources). The
// next poll repopulates it from the live board using the current filter.
// Workflows, questions and other state are untouched. A manual "reset to
// my current filter" escape hatch for the web UI. Returns the count removed.
func (m *Memory) ClearTickets() int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n := len(m.store.Tickets)
	if n > 0 {
		m.store.Tickets = map[string]TicketSnapshot{}
		m.flush()
	}
	return n
}

// RequestPick queues a ticket for manual execution with a pre-assigned set
// of repos (local checkout paths). The daemon runs it on the next tick,
// ahead of recency-based auto-pick; confirm_repo honors the repos and skips
// the gate. Re-queuing the same ticket just refreshes its repo assignment.
func (m *Memory) RequestPick(ticketID string, repos []string) {
	if m == nil || strings.TrimSpace(ticketID) == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.store.TicketRepos == nil {
		m.store.TicketRepos = map[string][]string{}
	}
	m.store.TicketRepos[ticketID] = repos
	found := false
	for _, id := range m.store.PickQueue {
		if id == ticketID {
			found = true
			break
		}
	}
	if !found {
		m.store.PickQueue = append(m.store.PickQueue, ticketID)
	}
	m.flush()
}

// NextPick returns the head of the pick queue (the ticket id + its assigned
// repos) without removing it. ok is false when the queue is empty.
func (m *Memory) NextPick() (ticketID string, repos []string, ok bool) {
	if m == nil {
		return "", nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.store.PickQueue) == 0 {
		return "", nil, false
	}
	id := m.store.PickQueue[0]
	return id, append([]string(nil), m.store.TicketRepos[id]...), true
}

// ClearPick removes a ticket from the pick queue (after the daemon has
// started running it). The repo assignment in TicketRepos is left in place
// so confirm_repo can still read it during that run.
func (m *Memory) ClearPick(ticketID string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.store.PickQueue[:0]
	for _, id := range m.store.PickQueue {
		if id != ticketID {
			out = append(out, id)
		}
	}
	m.store.PickQueue = out
	m.flush()
}

// AssignedRepos returns the repos a user assigned to a ticket at pick time.
// ok is false when the ticket was never manually picked.
func (m *Memory) AssignedRepos(ticketID string) (repos []string, ok bool) {
	if m == nil {
		return nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.store.TicketRepos[ticketID]
	if !ok || len(r) == 0 {
		return nil, false
	}
	return append([]string(nil), r...), true
}

// IsPickQueued reports whether a ticket is currently waiting in the manual
// pick queue (used by the UI to show a "queued" badge instead of the form).
func (m *Memory) IsPickQueued(ticketID string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range m.store.PickQueue {
		if id == ticketID {
			return true
		}
	}
	return false
}

// IgnoreTicket marks a ticket key as opted-out of the daemon
// workflow. The daemon's ticket-picker filters this set, so an
// ignored ticket never gets a workflow opened against it. The
// per-key timestamp is "ignored at" — used for future "auto-
// unclaim after N days" policies and surfaced in the UI.
func (m *Memory) IgnoreTicket(key string) {
	if m == nil || strings.TrimSpace(key) == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.store.Ignored == nil {
		m.store.Ignored = map[string]time.Time{}
	}
	m.store.Ignored[key] = time.Now()
	m.flush()
}

// UnignoreTicket removes the opt-out so the ticket can be picked up
// by the next poll cycle. No-op when the key wasn't ignored.
func (m *Memory) UnignoreTicket(key string) {
	if m == nil || strings.TrimSpace(key) == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.store.Ignored == nil {
		return
	}
	delete(m.store.Ignored, key)
	m.flush()
}

// IsTicketIgnored returns true when the daemon should skip this key.
// Used both by the daemon's pickNextTicket filter and by the web UI
// to render the muted/ignored badge on each row.
func (m *Memory) IsTicketIgnored(key string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.store.Ignored == nil {
		return false
	}
	_, ok := m.store.Ignored[key]
	return ok
}

// IgnoredTickets returns a copy of the current ignore-set keys.
// Useful for the daemon to filter a fetched ticket list in O(1)
// per key without holding the memory lock for the whole iteration.
func (m *Memory) IgnoredTickets() map[string]time.Time {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.store.Ignored) == 0 {
		return nil
	}
	out := make(map[string]time.Time, len(m.store.Ignored))
	for k, v := range m.store.Ignored {
		out[k] = v
	}
	return out
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

// --- PR review / notification dedup API ------------------------------------

// maxReviewMarks / maxNotifSeen cap the dedup maps so memory.json stays
// bounded across months of uptime.
const (
	maxReviewMarks = 500
	maxNotifSeen   = 2000
)

// ReviewMarkFor returns the recorded review state for a PR key
// ("host:repo#number"). ok is false when goon has never drafted a
// review for that PR.
func (m *Memory) ReviewMarkFor(key string) (ReviewMark, bool) {
	if m == nil {
		return ReviewMark{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.store.ReviewSeen[key]
	return r, ok
}

// RecordReview stores the diff hash goon last drafted a review from, so
// the next pass skips the PR until its diff changes.
func (m *Memory) RecordReview(key, diffHash string) {
	if m == nil || key == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.store.ReviewSeen == nil {
		m.store.ReviewSeen = map[string]ReviewMark{}
	}
	m.store.ReviewSeen[key] = ReviewMark{DiffHash: diffHash, When: time.Now()}
	if len(m.store.ReviewSeen) > maxReviewMarks {
		pruneOldestReviewMarks(m.store.ReviewSeen, maxReviewMarks)
	}
	m.flush()
}

// NotificationSeen reports whether a notification key ("host:id") has
// already been forwarded.
func (m *Memory) NotificationSeen(key string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.store.NotifSeen[key]
	return ok
}

// MarkNotificationSeen records that a notification has been forwarded.
func (m *Memory) MarkNotificationSeen(key string) {
	if m == nil || key == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.store.NotifSeen == nil {
		m.store.NotifSeen = map[string]time.Time{}
	}
	m.store.NotifSeen[key] = time.Now()
	if len(m.store.NotifSeen) > maxNotifSeen {
		pruneOldestNotifSeen(m.store.NotifSeen, maxNotifSeen)
	}
	m.flush()
}

// pruneOldestReviewMarks evicts the oldest entries (by When) until the
// map size is at most cap.
func pruneOldestReviewMarks(mp map[string]ReviewMark, cap int) {
	if len(mp) <= cap {
		return
	}
	type kv struct {
		k string
		t time.Time
	}
	all := make([]kv, 0, len(mp))
	for k, v := range mp {
		all = append(all, kv{k, v.When})
	}
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && all[j].t.Before(all[j-1].t); j-- {
			all[j], all[j-1] = all[j-1], all[j]
		}
	}
	for i := 0; i < len(all)-cap; i++ {
		delete(mp, all[i].k)
	}
}

// pruneOldestNotifSeen evicts the oldest entries until the map size is
// at most cap.
func pruneOldestNotifSeen(mp map[string]time.Time, cap int) {
	if len(mp) <= cap {
		return
	}
	type kv struct {
		k string
		t time.Time
	}
	all := make([]kv, 0, len(mp))
	for k, v := range mp {
		all = append(all, kv{k, v})
	}
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && all[j].t.Before(all[j-1].t); j-- {
			all[j], all[j-1] = all[j-1], all[j]
		}
	}
	for i := 0; i < len(all)-cap; i++ {
		delete(mp, all[i].k)
	}
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
	// Resuming (paused=false) always clears the circuit breaker so a
	// manual resume actually takes effect immediately — otherwise a
	// lingering NextRetryAt backoff window or AutoPaused flag would make
	// the daemon ignore the resume until the window elapsed.
	if !paused {
		m.store.Status.AutoPaused = false
		m.store.Status.ConsecutiveFails = 0
		m.store.Status.NextRetryAt = time.Time{}
		m.store.Status.ErrorClass = ""
		m.store.Status.LastError = ""
		m.store.Status.LastErrorAt = time.Time{}
	}
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

// RepoChoiceFor returns the repo a user explicitly chose to REMEMBER for
// a project (via the confirm_repo "remember for this project" opt-in), if
// any. Distinct from the removed auto-learn behaviour: this map is only
// written on an explicit user opt-in, so it never silently forces tickets
// down the same path.
func (m *Memory) RepoChoiceFor(project string) (string, bool) {
	if m == nil {
		return "", false
	}
	project = strings.TrimSpace(project)
	if project == "" {
		return "", false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.store.RepoChoices[project]
	return v, ok && strings.TrimSpace(v) != ""
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
	if len(m.store.ReviewSeen) > maxReviewMarks {
		pruneOldestReviewMarks(m.store.ReviewSeen, maxReviewMarks)
	}
	if len(m.store.NotifSeen) > maxNotifSeen {
		pruneOldestNotifSeen(m.store.NotifSeen, maxNotifSeen)
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

	// LastReflectAt — prefer the more recent so a concurrent CLI flush
	// doesn't roll back the daemon's standby-reflection throttle.
	if disk.LastReflectAt.After(out.LastReflectAt) {
		out.LastReflectAt = disk.LastReflectAt
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

	// ReviewSeen / NotifSeen: union, prefer the newer timestamp. Both
	// the daemon's bot loop and a standalone `goon review-prs` /
	// `goon notifications` write these, so neither side may clobber the
	// other's additions.
	if len(disk.ReviewSeen) > 0 {
		if out.ReviewSeen == nil {
			out.ReviewSeen = map[string]ReviewMark{}
		}
		for k, dv := range disk.ReviewSeen {
			if mv, ok := out.ReviewSeen[k]; !ok || dv.When.After(mv.When) {
				out.ReviewSeen[k] = dv
			}
		}
	}
	if len(disk.NotifSeen) > 0 {
		if out.NotifSeen == nil {
			out.NotifSeen = map[string]time.Time{}
		}
		for k, dt := range disk.NotifSeen {
			if mt, ok := out.NotifSeen[k]; !ok || dt.After(mt) {
				out.NotifSeen[k] = dt
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

// ─── local tickets ────────────────────────────────────────────────────────────

// AddLocalTicket creates a new local-only ticket, assigns the next GOON-N ID,
// persists to memory.json, and returns the created ticket.
func (m *Memory) AddLocalTicket(title, description, priority string, labels []string) (LocalTicket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.store.NextLocalID++
	now := time.Now().UTC()
	t := LocalTicket{
		ID:          fmt.Sprintf("GOON-%d", m.store.NextLocalID),
		Title:       title,
		Description: description,
		Status:      "open",
		Labels:      labels,
		Priority:    priority,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	m.store.LocalTickets = append(m.store.LocalTickets, t)
	m.flush()
	return t, nil
}

// ListLocalTickets returns a copy of all local tickets, most-recently-updated first.
func (m *Memory) ListLocalTickets() []LocalTicket {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]LocalTicket, len(m.store.LocalTickets))
	copy(out, m.store.LocalTickets)
	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out
}

// UpdateLocalTicketStatus sets the status of the ticket with the given ID and flushes.
func (m *Memory) UpdateLocalTicketStatus(id, status string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.store.LocalTickets {
		if m.store.LocalTickets[i].ID == id {
			m.store.LocalTickets[i].Status = status
			m.store.LocalTickets[i].UpdatedAt = time.Now().UTC()
			m.flush()
			return nil
		}
	}
	return fmt.Errorf("local ticket %q not found", id)
}

// DeleteLocalTicket removes the local ticket with the given ID and flushes.
func (m *Memory) DeleteLocalTicket(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	tks := m.store.LocalTickets[:0]
	found := false
	for _, t := range m.store.LocalTickets {
		if t.ID == id {
			found = true
			continue
		}
		tks = append(tks, t)
	}
	if !found {
		return fmt.Errorf("local ticket %q not found", id)
	}
	m.store.LocalTickets = tks
	m.flush()
	return nil
}
