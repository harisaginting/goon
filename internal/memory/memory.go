// Package memory holds short-term in-process state and a long-term JSON file
// at ~/.goon/memory.json (override with GOON_MEMORY_PATH).
package memory

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
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
	WFTriaging  WorkflowState = "triaging"
	WFPlanning  WorkflowState = "planning"
	WFExecuting WorkflowState = "executing"
	WFTesting   WorkflowState = "testing"
	WFVerifying WorkflowState = "verifying"
	WFOpeningPR WorkflowState = "opening_pr"
	WFNotifying WorkflowState = "notifying"
	WFDone      WorkflowState = "done"
	WFBlocked   WorkflowState = "blocked"
	WFFailed    WorkflowState = "failed"
)

// Workflow tracks a single ticket's run from triage through PR.
type Workflow struct {
	ID         string        `json:"id"`
	TicketID   string        `json:"ticket_id"`
	TicketKey  string        `json:"ticket_key,omitempty"`
	Title      string        `json:"title,omitempty"`
	StartedAt  time.Time     `json:"started_at"`
	UpdatedAt  time.Time     `json:"updated_at"`
	State      WorkflowState `json:"state"`
	Repo       string        `json:"repo,omitempty"`
	Branch     string        `json:"branch,omitempty"`
	Plan       []PlanStep    `json:"plan,omitempty"`
	PRURL      string        `json:"pr_url,omitempty"`
	VerifyRuns int           `json:"verify_runs"`
	Note       string        `json:"note,omitempty"`
	Error      string        `json:"error,omitempty"`
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
	UpdatedAt time.Time `json:"updated_at,omitempty"`
	LastSeen  time.Time `json:"last_seen"`
}

// DaemonStatus is a tiny live-status snapshot for the UI / `goon status`.
type DaemonStatus struct {
	Running        bool      `json:"running"`
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
	History   []Interaction             `json:"history"`
	Counts    map[string]int            `json:"command_counts"`
	Questions []Question                `json:"questions,omitempty"`
	Workflows []Workflow                `json:"workflows,omitempty"`
	Tickets   map[string]TicketSnapshot `json:"tickets,omitempty"`
	Status    DaemonStatus              `json:"status,omitempty"`
	NextQID   int64                     `json:"next_qid,omitempty"`
}

// New opens (or creates) the memory file. If path is empty it defaults to
// ~/.goon/memory.json.
func New(path string) (*Memory, error) {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(home, ".goon", "memory.json")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	m := &Memory{path: path, store: storeFile{
		Counts:  map[string]int{},
		Tickets: map[string]TicketSnapshot{},
	}}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &m.store)
		if m.store.Counts == nil {
			m.store.Counts = map[string]int{}
		}
		if m.store.Tickets == nil {
			m.store.Tickets = map[string]TicketSnapshot{}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
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

// AskQuestion records a new pending question and returns its id.
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
	m.flush()
	return q.ID
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

// SeenTicket updates the last-seen snapshot for a ticket id.
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
	m.flush()
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
//   - Status, History, Counts, Workflows, Tickets — the daemon owns these
//     (only one daemon at a time), so the in-memory copy wins.
//   - Questions — both daemon (via ask_user) and CLI (`goon train`) write
//     these. Merge by id: prefer the version with an answer; if both have
//     answers, prefer the more recent answered_at.
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
	m.store = mergeStores(disk, m.store)
}
