// Package boards is goon's pluggable ticket source.
//
// A Board returns a list of "open" tickets the daemon should pick up. The
// concrete implementation (Jira, GitHub Issues, …) is selected via env var
// GOON_BOARD.
package boards

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Status is the engineering lifecycle a ticket can be in. Boards translate
// their native states to these values via simple keyword matching.
type Status string

const (
	StatusUnknown    Status = "unknown"
	StatusOpen       Status = "open"
	StatusInProgress Status = "in_progress"
	StatusInReview   Status = "in_review"
	StatusBlocked    Status = "blocked"
	StatusDone       Status = "done"
)

// Ticket is the canonical normalized ticket goon's workflow consumes.
type Ticket struct {
	ID          string    `json:"id"`     // unique stable id, e.g. "ENG-123" or "github:owner/repo#42"
	Source      string    `json:"source"` // "jira" | "github"
	Key         string    `json:"key"`    // human-friendly key (e.g. "ENG-123" or "#42")
	Title       string    `json:"title"`
	Description string    `json:"description"`
	URL         string    `json:"url"`
	Status      Status    `json:"status"`
	Labels      []string  `json:"labels,omitempty"`
	Assignee    string    `json:"assignee,omitempty"`
	Project     string    `json:"project,omitempty"` // jira project key or github "owner/repo"
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
}

// Board is the abstract ticket source.
type Board interface {
	Name() string
	List(ctx context.Context) ([]Ticket, error)
	Get(ctx context.Context, id string) (Ticket, error)
	// Comment posts a comment on the ticket. Used by goon to log progress.
	Comment(ctx context.Context, id, body string) error
	// Transition moves the ticket to a goon-known Status. Best-effort —
	// boards that don't support it return nil.
	Transition(ctx context.Context, id string, s Status) error
}

// Searcher is an optional companion interface for boards that support
// ad-hoc queries (Jira's JQL, GitHub's `is:issue` syntax). The chat
// agent uses this to fetch live data mid-conversation instead of
// answering from a cached snapshot. Boards that don't implement it
// degrade gracefully — the chat falls back to filtering memory.
//
// query is intentionally board-native (e.g. JQL for Jira) — the LLM
// emits it directly. limit caps results; pass 0 for the board's
// default. Returning ErrSearchUnsupported lets callers detect "this
// board can't search" cleanly.
type Searcher interface {
	Search(ctx context.Context, query string, limit int) ([]Ticket, error)
}

// ErrSearchUnsupported is returned by boards that don't implement
// Searcher when callers reach them through the optional path. Lets the
// chat agent fall back to memory-only answers without a type assertion.
var ErrSearchUnsupported = errors.New("board does not support ad-hoc search")

// TicketPatch is the diff used by Updater.Update. Each pointer
// field encodes "leave this alone (nil) vs set this value (non-nil
// pointer to new value)". Pointer-to-empty-string is a deliberate
// clear. Labels uses a slice rather than a pointer because Go
// already has a "nil slice = leave alone" idiom; pass an empty
// non-nil slice (`[]string{}`) to clear labels.
type TicketPatch struct {
	Title       *string
	Description *string
	Labels      []string
}

// Updater is an optional companion interface for boards that allow
// editing ticket fields (summary, description, labels). Jira
// implements it; GitHub and the mock board implement it too. Boards
// that don't can ignore — the chat agent surfaces "not supported"
// to the user.
type Updater interface {
	Update(ctx context.Context, id string, patch TicketPatch) error
}

// ErrUpdateUnsupported is the analogue of ErrSearchUnsupported for
// the Updater path.
var ErrUpdateUnsupported = errors.New("board does not support ticket update")

// TransitionResolver is an optional companion interface for boards
// whose workflow has custom statuses that goon's five-value Status
// enum cannot represent — Jira boards routinely have "Ready to Test",
// "In QA", "UAT", "Selected for Development", etc. It lets the chat
// agent list a ticket's REAL transitions and move it by the board's
// own status name, instead of bucketing through MapStatus (which, for
// example, collapses "Ready to Test" to "open" because the name
// contains the substring "ready").
//
// Boards that don't implement it degrade gracefully: the chat agent
// falls back to the canonical MapStatus + Transition path.
type TransitionResolver interface {
	// ListTransitions returns the status names the ticket can move to
	// right now, exactly as the board names them.
	ListTransitions(ctx context.Context, id string) ([]string, error)
	// TransitionByName moves the ticket to the status whose name best
	// matches `name`, against the board's real workflow. It returns the
	// actual status name applied — so callers report the truth, not the
	// user's wording. On no match it returns an error listing the
	// available status names.
	TransitionByName(ctx context.Context, id, name string) (string, error)
}

// NewFromEnv selects and constructs the board adapter from environment
// variables. Returns ErrNoBoard when no board is configured, so the daemon
// can degrade gracefully.
func NewFromEnv() (Board, error) {
	name := strings.ToLower(strings.TrimSpace(os.Getenv("GOON_BOARD")))
	if name == "" {
		return nil, ErrNoBoard
	}
	switch name {
	case "jira":
		return NewJiraFromEnv()
	case "github":
		return NewGitHubFromEnv()
	case "mock":
		return NewMock(nil), nil
	default:
		return nil, fmt.Errorf("unknown GOON_BOARD %q (want jira|github|mock)", name)
	}
}

// ErrNoBoard signals "no board configured". The daemon treats this as "idle".
var ErrNoBoard = errors.New("no board configured (set GOON_BOARD=jira|github)")

// MapStatus does a fuzzy match on a board's native status string.
func MapStatus(native string) Status {
	s := strings.ToLower(native)
	switch {
	case s == "":
		return StatusUnknown
	case contains(s, "done", "closed", "resolved", "merged"):
		return StatusDone
	case contains(s, "review"):
		return StatusInReview
	case contains(s, "block"):
		return StatusBlocked
	case contains(s, "progress", "doing", "in dev"):
		return StatusInProgress
	case contains(s, "open", "todo", "to do", "backlog", "ready"):
		return StatusOpen
	}
	return StatusUnknown
}

func contains(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// Mock is a deterministic in-memory board, used in tests and `GOON_BOARD=mock`.
type Mock struct {
	Tickets  []Ticket
	Comments []string
	Transit  []string
	// Transitions is the set of status names the mock board reports as
	// available — ListTransitions returns it, TransitionByName matches
	// against it.
	Transitions []string
	// Searches records every query passed to Search (newest at the end)
	// — handy in tests to verify the chat agent actually queried the
	// board rather than hallucinating an answer from cached state.
	Searches []string
	// Updates records every patch passed to Update so tests can assert
	// the chat agent edited tickets correctly. Stored as
	// "<key>: title=<title?> desc=<desc?> labels=<labels?>".
	Updates  []string
	OnList   func() ([]Ticket, error)
	OnSearch func(query string, limit int) ([]Ticket, error)
}

// NewMock constructs a mock board prefilled with the given tickets.
func NewMock(t []Ticket) *Mock { return &Mock{Tickets: t} }

func (*Mock) Name() string { return "mock" }
func (m *Mock) List(_ context.Context) ([]Ticket, error) {
	if m.OnList != nil {
		return m.OnList()
	}
	out := make([]Ticket, len(m.Tickets))
	copy(out, m.Tickets)
	return out, nil
}
func (m *Mock) Get(_ context.Context, id string) (Ticket, error) {
	for _, t := range m.Tickets {
		if t.ID == id {
			return t, nil
		}
	}
	return Ticket{}, fmt.Errorf("mock board: ticket %q not found", id)
}
func (m *Mock) Comment(_ context.Context, id, body string) error {
	m.Comments = append(m.Comments, id+": "+body)
	return nil
}
func (m *Mock) Transition(_ context.Context, id string, s Status) error {
	m.Transit = append(m.Transit, id+"->"+string(s))
	for i := range m.Tickets {
		if m.Tickets[i].ID == id {
			m.Tickets[i].Status = s
		}
	}
	return nil
}

// ListTransitions implements TransitionResolver for the mock board.
func (m *Mock) ListTransitions(_ context.Context, _ string) ([]string, error) {
	out := make([]string, len(m.Transitions))
	copy(out, m.Transitions)
	return out, nil
}

// TransitionByName implements TransitionResolver for the mock board:
// it matches name against m.Transitions (normalised exact, then
// containment) and records the move in m.Transit.
func (m *Mock) TransitionByName(_ context.Context, id, name string) (string, error) {
	w := normStatus(name)
	for _, s := range m.Transitions {
		if normStatus(s) == w {
			m.Transit = append(m.Transit, id+"->"+s)
			return s, nil
		}
	}
	for _, s := range m.Transitions {
		n := normStatus(s)
		if n != "" && w != "" && (strings.Contains(n, w) || strings.Contains(w, n)) {
			m.Transit = append(m.Transit, id+"->"+s)
			return s, nil
		}
	}
	return "", fmt.Errorf("no status matches %q — available: %s", name, strings.Join(m.Transitions, ", "))
}

// Search implements Searcher for the mock board. It records every
// query in m.Searches (so tests can assert what the chat agent asked
// for) and returns a configurable result via OnSearch — falling back
// to all tickets when no override is set.
func (m *Mock) Search(_ context.Context, query string, limit int) ([]Ticket, error) {
	m.Searches = append(m.Searches, query)
	if m.OnSearch != nil {
		return m.OnSearch(query, limit)
	}
	out := make([]Ticket, len(m.Tickets))
	copy(out, m.Tickets)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Update implements Updater for the mock board. Applies the patch
// to the in-memory ticket slice in addition to logging it, so a
// subsequent Get/List call sees the change.
func (m *Mock) Update(_ context.Context, id string, patch TicketPatch) error {
	parts := []string{}
	if patch.Title != nil {
		parts = append(parts, "title="+*patch.Title)
	}
	if patch.Description != nil {
		parts = append(parts, "desc="+*patch.Description)
	}
	if patch.Labels != nil {
		parts = append(parts, "labels="+strings.Join(patch.Labels, ","))
	}
	m.Updates = append(m.Updates, id+": "+strings.Join(parts, " "))
	for i := range m.Tickets {
		if m.Tickets[i].ID == id {
			if patch.Title != nil {
				m.Tickets[i].Title = *patch.Title
			}
			if patch.Description != nil {
				m.Tickets[i].Description = *patch.Description
			}
			if patch.Labels != nil {
				m.Tickets[i].Labels = patch.Labels
			}
		}
	}
	return nil
}
