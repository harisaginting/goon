// Package agentctx builds the shared "what does goon know right now"
// context block that's injected into every chat surface (Telegram bot,
// web UI chat panel, and any future REPL). Composes two layers:
//
//   1. Live runtime state from internal/memory — tickets, workflows,
//      pending questions, daemon status, learned project→repo cache.
//   2. Durable knowledge from internal/notes — PINNED.md body inline,
//      plus an index of every topic note with its first-line headline.
//
// Keeping this in its own package avoids forcing telegram and web to
// depend on each other or duplicate the rendering logic. Both surfaces
// inject the same prompt; the LLM sees identical context regardless
// of which channel the user types from.
package agentctx

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/harisaginting/goon/internal/memory"
	"github.com/harisaginting/goon/internal/notes"
)

// Build returns the full context — state + knowledge — concatenated.
// Safe to call with a nil/Disabled Memory; the function degrades to
// "operating without context" rather than panicking. notesDir
// overrides the default discovery (mostly useful in tests); when
// empty, notes.New("") falls back to $GOON_MEMORY_DIR / storage.
func Build(mem *memory.Memory, notesDir string) string {
	var sb strings.Builder
	sb.WriteString(BuildState(mem))
	if kb := BuildKnowledge(notesDir); kb != "" {
		sb.WriteByte('\n')
		sb.WriteString(kb)
	}
	return sb.String()
}

// BuildState renders the live-runtime block from passive memory.
func BuildState(mem *memory.Memory) string {
	if mem == nil {
		return "GOON STATE: (memory unavailable — operating without live context)"
	}
	var sb strings.Builder
	sb.WriteString("GOON STATE — live snapshot of this user's goon instance:\n\n")

	st := mem.GetStatus()
	sb.WriteString("[daemon]\n")
	fmt.Fprintf(&sb, "  running: %v\n", st.Running)
	if st.Paused {
		sb.WriteString("  paused:  yes (no new tickets are picked up — /resume to continue)\n")
	}
	if st.BoardName != "" {
		fmt.Fprintf(&sb, "  board:   %s\n", st.BoardName)
	}
	if st.HostName != "" {
		fmt.Fprintf(&sb, "  host:    %s\n", st.HostName)
	}
	if !st.LastPoll.IsZero() {
		fmt.Fprintf(&sb, "  last poll: %s ago\n", humanizeAge(time.Since(st.LastPoll)))
	}
	if st.LastTicket != "" {
		fmt.Fprintf(&sb, "  last ticket: %s\n", st.LastTicket)
	}

	pending := mem.PendingQuestions()
	if len(pending) > 0 {
		fmt.Fprintf(&sb, "\n[pending questions: %d]\n", len(pending))
		for i, q := range pending {
			if i >= 5 {
				fmt.Fprintf(&sb, "  …and %d more\n", len(pending)-5)
				break
			}
			tid := q.TicketID
			if tid == "" {
				tid = "—"
			}
			fmt.Fprintf(&sb, "  %s (%s): %s\n", q.ID, tid, snippet(q.Question, 200))
		}
		sb.WriteString("  Answer command: /answer <id> <text>\n")
	} else {
		sb.WriteString("\n[pending questions: none]\n")
	}

	tickets := mem.ListTickets()
	if len(tickets) > 0 {
		sort.SliceStable(tickets, func(i, j int) bool {
			return tickets[i].LastSeen.After(tickets[j].LastSeen)
		})
		// Cap raised from 15 → 30 so chat answers to "list my open
		// tickets" don't truncate. The state block stays under
		// ~10KB even at the cap.
		const maxTickets = 30
		shown := tickets
		if len(shown) > maxTickets {
			shown = shown[:maxTickets]
		}
		fmt.Fprintf(&sb, "\n[tickets: %d total, %d most-recent shown]\n", len(tickets), len(shown))
		for _, t := range shown {
			status := t.Status
			if status == "" {
				status = "?"
			}
			// Include assignee + labels + project so the model can
			// answer "tickets assigned to me", "tickets labelled X",
			// "tickets in project Y" without guessing.
			line := fmt.Sprintf("  %s [%s]", t.Key, status)
			if t.Assignee != "" {
				line += " assignee=" + t.Assignee
			}
			if t.Project != "" {
				line += " project=" + t.Project
			}
			if len(t.Labels) > 0 {
				line += " labels=" + strings.Join(t.Labels, ",")
			}
			line += " " + snippet(t.Title, 80)
			sb.WriteString(line)
			sb.WriteByte('\n')
		}
		if len(tickets) > maxTickets {
			fmt.Fprintf(&sb, "  …%d more not shown — suggest user run /tickets for the full inventory\n",
				len(tickets)-maxTickets)
		}
	} else {
		sb.WriteString("\n[tickets: none — daemon hasn't picked any up yet]\n")
	}

	wfs := mem.ListWorkflows(8)
	if len(wfs) > 0 {
		fmt.Fprintf(&sb, "\n[recent workflows: %d shown]\n", len(wfs))
		for _, w := range wfs {
			done := 0
			for _, s := range w.Plan {
				if s.Done {
					done++
				}
			}
			stage := w.Stage
			if stage == "" {
				stage = "—"
			}
			fmt.Fprintf(&sb, "  %s (%s) state=%s stage=%s plan=%d/%d",
				w.ID, w.TicketKey, w.State, stage, done, len(w.Plan))
			if w.PRURL != "" {
				fmt.Fprintf(&sb, " pr=%s", w.PRURL)
			}
			if w.PendingQuestionID != "" {
				fmt.Fprintf(&sb, " awaiting=%s", w.PendingQuestionID)
			}
			sb.WriteByte('\n')
		}
	}

	choices := mem.RepoChoices()
	if len(choices) > 0 {
		fmt.Fprintf(&sb, "\n[learned repos: %d]\n", len(choices))
		keys := make([]string, 0, len(choices))
		for k := range choices {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&sb, "  %s → %s\n", k, choices[k])
		}
	}

	return sb.String()
}

// BuildKnowledge renders the active markdown-notes block — PINNED.md
// body inline plus an index of every topic note with its first
// non-empty line as a headline. notesDir overrides notes.New's default
// discovery; "" uses $GOON_MEMORY_DIR / storage.
//
// Returns "" when the notes store is unreadable or empty, so callers
// can omit the block entirely.
func BuildKnowledge(notesDir string) string {
	store, err := notes.New(notesDir)
	if err != nil {
		return ""
	}
	var sb strings.Builder
	if pinned := store.Pinned(); strings.TrimSpace(pinned) != "" {
		fmt.Fprintf(&sb, "[knowledge: PINNED.md — always-loaded notes from %s/%s]\n",
			store.Path(), notes.PinnedFilename)
		sb.WriteString(snippet(strings.TrimSpace(pinned), 4000))
		sb.WriteString("\n")
	}
	names, err := store.List()
	if err != nil || len(names) == 0 {
		if sb.Len() == 0 {
			return ""
		}
		return sb.String()
	}
	idx := make([]string, 0, len(names))
	for _, n := range names {
		if n == notes.PinnedFilename {
			continue
		}
		idx = append(idx, n)
	}
	if len(idx) > 0 {
		fmt.Fprintf(&sb, "\n[knowledge: %d topic note(s) — full body via `/memory read <name>`]\n", len(idx))
		const maxIndex = 30
		shown := idx
		if len(shown) > maxIndex {
			shown = shown[:maxIndex]
		}
		for _, n := range shown {
			body, err := store.Read(n)
			headline := ""
			if err == nil {
				for _, line := range strings.SplitN(strings.TrimSpace(body), "\n", 2) {
					line = strings.TrimSpace(strings.TrimLeft(line, "#- "))
					if line != "" {
						headline = snippet(line, 80)
						break
					}
				}
			}
			if headline == "" {
				fmt.Fprintf(&sb, "  %s\n", n)
			} else {
				fmt.Fprintf(&sb, "  %s — %s\n", n, headline)
			}
		}
		if len(idx) > maxIndex {
			fmt.Fprintf(&sb, "  …%d more (run /memory list to see them all)\n", len(idx)-maxIndex)
		}
	}
	return sb.String()
}

// KnowledgeIndex returns the topic-note names + their first-line
// headlines. Used by UI surfaces that render the index visually
// (not as a prompt block). Empty slice when no notes exist.
type IndexEntry struct {
	Name     string
	Headline string
}

// KnowledgeIndex returns one entry per non-pinned note with its
// first-line headline. Independent of BuildKnowledge so callers can
// render the index in whatever shape they want (table, list, …).
func KnowledgeIndex(notesDir string) []IndexEntry {
	store, err := notes.New(notesDir)
	if err != nil {
		return nil
	}
	names, err := store.List()
	if err != nil {
		return nil
	}
	out := make([]IndexEntry, 0, len(names))
	for _, n := range names {
		if n == notes.PinnedFilename {
			continue
		}
		entry := IndexEntry{Name: n}
		body, err := store.Read(n)
		if err == nil {
			for _, line := range strings.SplitN(strings.TrimSpace(body), "\n", 2) {
				line = strings.TrimSpace(strings.TrimLeft(line, "#- "))
				if line != "" {
					entry.Headline = snippet(line, 200)
					break
				}
			}
		}
		out = append(out, entry)
	}
	return out
}

// Pinned returns the PINNED.md body verbatim (or "" if absent).
// Exposed so UI surfaces can render it as styled markdown without
// pulling it out of the BuildKnowledge string.
func Pinned(notesDir string) string {
	store, err := notes.New(notesDir)
	if err != nil {
		return ""
	}
	return store.Pinned()
}

// ReadNote returns one note's full body (or empty + error on miss).
// Thin wrapper so web/telegram surfaces don't need to import notes
// directly.
func ReadNote(notesDir, name string) (string, error) {
	store, err := notes.New(notesDir)
	if err != nil {
		return "", err
	}
	return store.Read(name)
}

func humanizeAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
}

func snippet(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
