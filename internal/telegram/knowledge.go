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

// cmdSkills is the Telegram-side manager for the skills store (the
// specialist sibling of memory/knowledge). Sub-commands:
//
//	/skills                — list every skill with one-line headlines
//	/skills list           — same as bare /skills
//	/skills read <name>    — print one skill's full body
//	/skills write <name> <body…>  — create or replace
//	/skills delete <name>  — remove
//
// Mirrors the web UI's CRUD endpoints; both go through agentctx so
// path safety + the storage location stay consistent.
func (b *Bot) cmdSkills(ctx context.Context, chatID int64, args []string) {
	sub := "list"
	if len(args) > 0 {
		sub = strings.ToLower(args[0])
		args = args[1:]
	}
	switch sub {
	case "list", "":
		idx := agentctx.SkillsIndex("")
		if len(idx) == 0 {
			_ = b.Send(ctx, chatID, "No skills yet. Create one with: /skills write <name> <body>")
			return
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "🛠 Skills (%d):\n", len(idx))
		for _, e := range idx {
			if e.Headline != "" {
				fmt.Fprintf(&sb, "  • %s — %s\n", e.Name, e.Headline)
			} else {
				fmt.Fprintf(&sb, "  • %s\n", e.Name)
			}
		}
		sb.WriteString("\nRead a skill with: /skills read <name>")
		b.SendChunked(ctx, chatID, sb.String())
	case "read":
		if len(args) < 1 {
			_ = b.Send(ctx, chatID, "usage: /skills read <name>")
			return
		}
		body, err := agentctx.ReadSkill("", args[0])
		if err != nil {
			_ = b.Send(ctx, chatID, "✗ "+err.Error())
			return
		}
		b.SendChunked(ctx, chatID, "📄 "+args[0]+"\n\n"+strings.TrimSpace(body))
	case "write":
		if len(args) < 2 {
			_ = b.Send(ctx, chatID, "usage: /skills write <name> <body>")
			return
		}
		name := args[0]
		body := strings.Join(args[1:], " ")
		if _, err := agentctx.WriteSkill("", name, body); err != nil {
			_ = b.Send(ctx, chatID, "✗ "+err.Error())
			return
		}
		_ = b.Send(ctx, chatID, "✓ saved skill "+name)
	case "delete", "rm":
		if len(args) < 1 {
			_ = b.Send(ctx, chatID, "usage: /skills delete <name>")
			return
		}
		if err := agentctx.DeleteSkill("", args[0]); err != nil {
			_ = b.Send(ctx, chatID, "✗ "+err.Error())
			return
		}
		_ = b.Send(ctx, chatID, "✓ deleted skill "+args[0])
	default:
		_ = b.Send(ctx, chatID, "unknown sub-command: "+sub+"\nusage: /skills list|read|write|delete")
	}
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
