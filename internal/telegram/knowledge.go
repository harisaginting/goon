package telegram

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/harisaginting/goon/internal/agentctx"
)

// cmdKnowledge renders goon's active-memory layer for the user:
// PINNED.md body inline (full or capped) plus an index of every topic
// note with a one-line headline. The user pulls a specific note's
// full body via `/memory read <name>`.
//
// This is the user-facing equivalent of what the chat handler
// silently injects into every LLM prompt — but printed verbatim so
// the user can see what goon knows without asking a question first.
func (b *Bot) cmdKnowledge(ctx context.Context, chatID int64) {
	var sb strings.Builder
	pinned := agentctx.Pinned("")
	if strings.TrimSpace(pinned) != "" {
		sb.WriteString("📌 PINNED.md (auto-loaded into every agent run):\n")
		sb.WriteString("─────────────────────────────────────────────\n")
		sb.WriteString(strings.TrimSpace(pinned))
		sb.WriteString("\n─────────────────────────────────────────────\n\n")
	} else {
		sb.WriteString("📌 PINNED.md: (empty — `goon memory edit PINNED.md` to seed it)\n\n")
	}
	idx := agentctx.KnowledgeIndex("")
	if len(idx) == 0 {
		sb.WriteString("📚 Topic notes: (none yet)\n")
		sb.WriteString("Notes get written here as workflows run their update_memory phase.\n")
	} else {
		fmt.Fprintf(&sb, "📚 Topic notes (%d):\n", len(idx))
		for _, e := range idx {
			if e.Headline != "" {
				fmt.Fprintf(&sb, "  • %s — %s\n", e.Name, e.Headline)
			} else {
				fmt.Fprintf(&sb, "  • %s\n", e.Name)
			}
		}
		sb.WriteString("\nPull the full body with: /memory read <name>\n")
		sb.WriteString("Grep across notes with: /memory search <query>\n")
	}
	b.SendChunked(ctx, chatID, sb.String())
}

// cmdRefresh forces a live poll of the configured board and updates
// memory so subsequent /tickets, /ticket, and chat answers see the
// latest snapshot. Bounded by a 30s context.
func (b *Bot) cmdRefresh(ctx context.Context, chatID int64) {
	if b.opts.Board == nil {
		_ = b.Send(ctx, chatID,
			"✗ no board configured. Set GOON_BOARD + the matching auth in `.env`, then `goon stop && goon start`.")
		return
	}
	_ = b.Send(ctx, chatID, "→ pulling fresh snapshot from the board…")
	refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	n, err := agentctx.RefreshTickets(refreshCtx, b.opts.Memory, b.opts.Board)
	if err != nil {
		if errors.Is(err, agentctx.ErrNoBoard) {
			_ = b.Send(ctx, chatID, "✗ no board configured.")
			return
		}
		_ = b.Send(ctx, chatID, "✗ board refresh failed: "+err.Error())
		return
	}
	_ = b.Send(ctx, chatID,
		fmt.Sprintf("✓ pulled %d ticket(s) from the board. /tickets to see them.", n))
}
