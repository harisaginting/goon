package agentctx

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/harisaginting/goon/internal/boards"
	"github.com/harisaginting/goon/internal/memory"
)

// ErrNoBoard signals that a refresh was requested but no board adapter
// is wired in. Callers (chat, /refresh command, web button) translate
// this into a user-facing message.
var ErrNoBoard = errors.New("no board configured")

// RefreshTickets calls the configured board's List and copies every
// returned ticket into memory.SeenTicket. Used by:
//
//   - the Telegram bot's `/refresh` command,
//   - the web UI's "refresh tickets" button (POST /api/refresh),
//   - the chat handlers when the snapshot is older than refreshStale.
//
// Returns the number of tickets seen + any board error. Best-effort
// from memory's perspective — SeenTicket failures are silent because
// memory.Disabled() is a valid mode.
func RefreshTickets(ctx context.Context, mem *memory.Memory, board boards.Board) (int, error) {
	if board == nil {
		return 0, ErrNoBoard
	}
	listCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tickets, err := board.List(listCtx)
	if err != nil {
		return 0, fmt.Errorf("board list: %w", err)
	}
	now := time.Now()
	bySource := map[string][]string{}
	for _, t := range tickets {
		if mem == nil {
			continue
		}
		mem.SeenTicket(memory.TicketSnapshot{
			ID: t.ID, Source: t.Source, Key: t.Key,
			Title: t.Title, URL: t.URL, Status: string(t.Status),
			Assignee:  t.Assignee,
			Labels:    t.Labels,
			Project:   t.Project,
			UpdatedAt: t.UpdatedAt, LastSeen: now,
		})
		bySource[t.Source] = append(bySource[t.Source], t.ID)
	}
	// Reconcile so a tightened filter (e.g. assignee=currentUser) drops
	// tickets that fell out of the result instead of leaving stale,
	// now-excluded rows in the table. Skip a source whose page looks
	// truncated (>=50) to avoid dropping legitimate page-2 matches.
	if mem != nil {
		for src, ids := range bySource {
			if len(ids) < 50 {
				mem.ReconcileTickets(src, ids)
			}
		}
	}
	// Bump the daemon's LastPoll so /status reflects the manual
	// refresh. Don't touch Running/Paused — that's the daemon's job.
	if mem != nil {
		st := mem.GetStatus()
		st.LastPoll = now
		mem.SetStatus(st)
	}
	return len(tickets), nil
}

// MaybeRefreshStale runs RefreshTickets only when the last poll is
// older than maxAge. Used inside the chat handlers so every chat turn
// triggers at most one network call (and only when memory is actually
// stale). Returns (refreshed bool, count int, err error).
func MaybeRefreshStale(ctx context.Context, mem *memory.Memory, board boards.Board, maxAge time.Duration) (bool, int, error) {
	if board == nil || mem == nil {
		return false, 0, nil
	}
	st := mem.GetStatus()
	if !st.LastPoll.IsZero() && time.Since(st.LastPoll) < maxAge {
		return false, 0, nil
	}
	n, err := RefreshTickets(ctx, mem, board)
	return true, n, err
}
