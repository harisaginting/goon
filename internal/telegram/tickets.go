package telegram

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/harisaginting/goon/internal/memory"
)

// cmdListTickets handles `/tickets`. Shows every ticket goon has seen
// (cached in memory.json's Tickets snapshot) sorted most-recently-seen
// first, annotated with the current workflow state if any. Designed to
// fit on a phone screen — one line per ticket plus an indented status.
//
// Optional filter: `/tickets <substr>` matches case-insensitively against
// key+title so a user with many tickets can scope quickly.
func (b *Bot) cmdListTickets(ctx context.Context, chatID int64, args []string) {
	tickets := b.opts.Memory.ListTickets()
	if len(tickets) == 0 {
		_ = b.Send(ctx, chatID,
			"no tickets yet — the daemon hasn't seen any. "+
				"Run `/status` to check daemon state.")
		return
	}

	// Optional substring filter
	if len(args) > 0 {
		needle := strings.ToLower(strings.Join(args, " "))
		filtered := tickets[:0]
		for _, t := range tickets {
			if strings.Contains(strings.ToLower(t.Key+" "+t.Title), needle) {
				filtered = append(filtered, t)
			}
		}
		tickets = filtered
		if len(tickets) == 0 {
			_ = b.Send(ctx, chatID, "no tickets matching "+strings.Join(args, " "))
			return
		}
	}

	sort.SliceStable(tickets, func(i, j int) bool {
		return tickets[i].LastSeen.After(tickets[j].LastSeen)
	})
	const maxList = 30
	more := 0
	if len(tickets) > maxList {
		more = len(tickets) - maxList
		tickets = tickets[:maxList]
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%d ticket(s):\n\n", len(tickets))
	for _, t := range tickets {
		statusPill := ticketStatusPill(t.Status)
		fmt.Fprintf(&sb, "%s  %s  %s\n", statusPill, t.Key, snippet(t.Title, 56))
		// annotate with workflow status if any
		wfs := b.opts.Memory.WorkflowsForTicket(t.ID)
		if len(wfs) > 0 {
			latest := wfs[0]
			fmt.Fprintf(&sb, "    %s\n", workflowOneLiner(latest))
		} else {
			fmt.Fprintf(&sb, "    no workflow run yet\n")
		}
		if !t.LastSeen.IsZero() {
			fmt.Fprintf(&sb, "    seen %s ago\n", humanizeDuration(time.Since(t.LastSeen)))
		}
	}
	if more > 0 {
		fmt.Fprintf(&sb, "\n…%d more not shown. Filter: `/tickets <substring>`\n", more)
	}
	sb.WriteString("\nDetails: /ticket <key>")
	b.SendChunked(ctx, chatID, sb.String())
}

// cmdTicketDetail handles `/ticket <id-or-key>`. Returns the ticket
// metadata plus every workflow run we've recorded for it (most recent
// first). The detail pane is what the user reaches for to answer
// "what's goon doing right now on ENG-42?"
func (b *Bot) cmdTicketDetail(ctx context.Context, chatID int64, args []string) {
	if len(args) == 0 {
		_ = b.Send(ctx, chatID, "usage: /ticket <id-or-key>")
		return
	}
	idOrKey := args[0]
	t, ok := b.opts.Memory.GetTicket(idOrKey)
	if !ok {
		// Maybe the user passed a workflow id. Be helpful.
		if w, found := b.opts.Memory.GetWorkflow(idOrKey); found {
			t = memory.TicketSnapshot{
				ID: w.TicketID, Key: w.TicketKey, Title: w.Title,
			}
		} else {
			_ = b.Send(ctx, chatID,
				"ticket not found. Try /tickets to list what goon has seen.")
			return
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%s — %s\n\n", t.Key, t.Title)
	if t.Source != "" {
		fmt.Fprintf(&sb, "source:    %s\n", t.Source)
	}
	if t.Status != "" {
		fmt.Fprintf(&sb, "status:    %s\n", t.Status)
	}
	if t.URL != "" {
		fmt.Fprintf(&sb, "url:       %s\n", t.URL)
	}
	if !t.LastSeen.IsZero() {
		fmt.Fprintf(&sb, "last seen: %s ago (%s)\n",
			humanizeDuration(time.Since(t.LastSeen)),
			t.LastSeen.Format(time.RFC3339))
	}

	wfs := b.opts.Memory.WorkflowsForTicket(t.ID)
	if len(wfs) == 0 {
		sb.WriteString("\n(no workflow run yet — the daemon hasn't picked this up)\n")
		b.SendChunked(ctx, chatID, sb.String())
		return
	}

	for i, w := range wfs {
		if i == 0 {
			sb.WriteString("\n── current workflow ──\n")
		} else {
			fmt.Fprintf(&sb, "\n── older run (#%d) ──\n", i)
		}
		fmt.Fprintf(&sb, "id:       %s\n", w.ID)
		fmt.Fprintf(&sb, "state:    %s\n", w.State)
		if w.Stage != "" {
			fmt.Fprintf(&sb, "stage:    %s\n", w.Stage)
		}
		if w.Repo != "" {
			fmt.Fprintf(&sb, "repo:     %s\n", w.Repo)
		}
		if w.Branch != "" {
			fmt.Fprintf(&sb, "branch:   %s\n", w.Branch)
		}
		if w.PRURL != "" {
			fmt.Fprintf(&sb, "pr:       %s\n", w.PRURL)
		}
		if !w.StartedAt.IsZero() {
			fmt.Fprintf(&sb, "started:  %s ago\n", humanizeDuration(time.Since(w.StartedAt)))
		}
		if !w.UpdatedAt.IsZero() && w.UpdatedAt != w.StartedAt {
			fmt.Fprintf(&sb, "updated:  %s ago\n", humanizeDuration(time.Since(w.UpdatedAt)))
		}
		if len(w.Approvals) > 0 {
			sb.WriteString("approvals:\n")
			// stable order: confirm_repo, then approve_plan, then anything else
			order := []string{"confirm_repo", "approve_plan"}
			seen := map[string]bool{}
			for _, k := range order {
				if v, ok := w.Approvals[k]; ok {
					fmt.Fprintf(&sb, "  - %s: %s\n", k, v)
					seen[k] = true
				}
			}
			for k, v := range w.Approvals {
				if !seen[k] {
					fmt.Fprintf(&sb, "  - %s: %s\n", k, v)
				}
			}
		}
		if len(w.Plan) > 0 {
			sb.WriteString("plan:\n")
			done := 0
			for _, s := range w.Plan {
				if s.Done {
					done++
				}
				mark := "✗"
				if s.Done {
					mark = "✓"
				}
				fmt.Fprintf(&sb, "  %s %s\n", mark, snippet(s.Title, 80))
				if s.Note != "" {
					fmt.Fprintf(&sb, "      note: %s\n", snippet(s.Note, 120))
				}
			}
			fmt.Fprintf(&sb, "  (%d/%d steps done)\n", done, len(w.Plan))
		}
		if w.PendingQuestionID != "" {
			fmt.Fprintf(&sb, "pending question: %s\n", w.PendingQuestionID)
			fmt.Fprintf(&sb, "  → reply with: /answer %s <yes|no|...>\n", w.PendingQuestionID)
		}
		if w.Error != "" {
			fmt.Fprintf(&sb, "error: %s\n", snippet(w.Error, 300))
		}
		// Limit to most-recent 3 runs to keep the message readable
		if i >= 2 && len(wfs) > 3 {
			fmt.Fprintf(&sb, "\n…and %d older run(s) not shown\n", len(wfs)-3)
			break
		}
	}

	b.SendChunked(ctx, chatID, sb.String())
}

// ticketStatusPill renders a one-emoji indicator for a ticket's status,
// matching what users see in `goon status` so the two surfaces feel like
// the same product.
func ticketStatusPill(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "open", "todo", "to do", "backlog", "ready", "":
		return "🟦"
	case "in_progress", "in progress", "doing", "in dev":
		return "🟨"
	case "in_review", "in review", "review":
		return "🟪"
	case "blocked":
		return "🟥"
	case "done", "closed", "resolved", "merged":
		return "✅"
	}
	return "⬜"
}

// workflowOneLiner condenses a workflow record into a single line for the
// /tickets list view. Communicates state at a glance.
func workflowOneLiner(w memory.Workflow) string {
	switch w.State {
	case memory.WFAwaitingApproval:
		return fmt.Sprintf("⏸ paused at %s — /answer %s ...", w.Stage, w.PendingQuestionID)
	case memory.WFDone:
		if w.PRURL != "" {
			return "✓ done — " + w.PRURL
		}
		return "✓ done"
	case memory.WFFailed:
		return "✗ failed: " + snippet(w.Error, 60)
	default:
		// Active / running states all condense to "running at <stage>".
		stage := w.Stage
		if stage == "" {
			stage = string(w.State)
		}
		return "▶ running at " + stage
	}
}

// humanizeDuration produces a compact "5m" / "2h" / "3d" string for the
// most common cases. Sub-minute resolutions round up to "<1m" since the
// daemon polls every 5 minutes anyway.
func humanizeDuration(d time.Duration) string {
	if d < time.Minute {
		return "<1m"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}
