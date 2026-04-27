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
	OnList   func() ([]Ticket, error)
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
