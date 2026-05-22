// Package agentctx builds the shared "what does goon know right now"
// context block that's injected into every chat surface (Telegram bot,
// web UI chat panel, and any future REPL). Composes two layers:
//
//   1. Live runtime state from internal/memory — tickets, workflows,
//      pending questions, daemon status, learned project→repo cache.
//   2. Durable knowledge from internal/notes — SOUL.md body inline,
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
	"github.com/harisaginting/goon/internal/skills"
)

// Build returns the full context — state + knowledge + skills —
// concatenated. Safe to call with a nil/Disabled Memory; the
// function degrades to "operating without context" rather than
// panicking. notesDir overrides the default discovery (mostly
// useful in tests); when empty, notes.New("") falls back to
// $GOON_MEMORY_DIR / storage.
//
// Character / voice content used to ship from a separate
// personal.md file; it's now folded into SOUL.md and surfaces
// through BuildKnowledge, so the "character" block is no longer
// emitted separately. One always-loaded context file = one mental
// model.
func Build(mem *memory.Memory, notesDir string) string {
	var sb strings.Builder
	sb.WriteString(BuildState(mem))
	if kb := BuildKnowledge(notesDir); kb != "" {
		sb.WriteByte('\n')
		sb.WriteString(kb)
	}
	if sk := BuildSkills(""); sk != "" {
		sb.WriteByte('\n')
		sb.WriteString(sk)
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

// BuildKnowledge renders the active markdown-notes block — SOUL.md
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
	if soul := store.Soul(); strings.TrimSpace(soul) != "" {
		fmt.Fprintf(&sb, "[knowledge: SOUL.md — always-loaded notes from %s/%s]\n",
			store.Path(), notes.SoulFilename)
		sb.WriteString(snippet(strings.TrimSpace(soul), 4000))
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
		// Hide files that have their own dedicated render surface
		// elsewhere:
		//   SOUL.md      → rendered above as the always-loaded block
		//   PINNED.md    → legacy alias of SOUL, read-only
		//   REPOSITORY.md → its own table view in the dashboard; also
		//                  surfaces in the confirm_repo menu so it
		//                  doesn't need to clutter the topic index
		//   HISTORY.md   → chronological log, rendered separately
		if n == notes.SoulFilename || n == "PINNED.md" || n == "REPOSITORY.md" || n == "HISTORY.md" {
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

// KnowledgeIndex returns one entry per non-soul note with its
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
		// Exclude SOUL / PINNED (read-only alias) / REPOSITORY /
		// HISTORY — each has a dedicated render surface elsewhere
		// and would otherwise clutter the "topic notes" index.
		if n == notes.SoulFilename || n == "PINNED.md" || n == "REPOSITORY.md" || n == "HISTORY.md" {
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

// Soul returns the SOUL.md body verbatim (or "" if absent). Falls
// back to the legacy PINNED.md when SOUL.md is missing so installs
// mid-rename still render their always-loaded knowledge. Exposed so
// UI surfaces can render it as styled markdown without pulling it
// out of the BuildKnowledge string.
func Soul(notesDir string) string {
	store, err := notes.New(notesDir)
	if err != nil {
		return ""
	}
	return store.Soul()
}

// Pinned is a deprecated alias for Soul.
//
// Deprecated: use Soul.
func Pinned(notesDir string) string { return Soul(notesDir) }

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

// WriteNote replaces or creates a memory note. Returns the resolved
// path on success — useful for confirmation messages.
func WriteNote(notesDir, name, body string) (string, error) {
	store, err := notes.New(notesDir)
	if err != nil {
		return "", err
	}
	if err := store.Write(name, body); err != nil {
		return "", err
	}
	p, _ := store.Resolve(name)
	return p, nil
}

// DeleteNote removes a memory note. Returns os.ErrNotExist when
// absent so the caller can render a friendly message.
func DeleteNote(notesDir, name string) error {
	store, err := notes.New(notesDir)
	if err != nil {
		return err
	}
	return store.Delete(name)
}

// --- Skills (specialist procedures / role definitions) ---------------------
//
// Skills are markdown files stored under ./storage/skills/ that
// codify HOW-tos, roles, and procedures — distinct from memory
// (which carries facts). They're listed in the GOON STATE block by
// name + headline so the LLM can ask the user to apply one ("want me
// to use the writer skill?") but they are NOT auto-injected.

// BuildSkills returns the skills block for the system prompt — a
// short index of every skill with a one-line headline. Empty when no
// skills exist (so callers can omit the block cleanly).
func BuildSkills(skillsDir string) string {
	store, err := skills.New(skillsDir)
	if err != nil {
		return ""
	}
	names, err := store.List()
	if err != nil || len(names) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "[skills: %d available — specialist procedures the user can ask you to apply]\n", len(names))
	const maxIndex = 30
	shown := names
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
	if len(names) > maxIndex {
		fmt.Fprintf(&sb, "  …%d more\n", len(names)-maxIndex)
	}
	return sb.String()
}

// SkillsIndex mirrors KnowledgeIndex but for the skills store.
func SkillsIndex(skillsDir string) []IndexEntry {
	store, err := skills.New(skillsDir)
	if err != nil {
		return nil
	}
	names, err := store.List()
	if err != nil {
		return nil
	}
	out := make([]IndexEntry, 0, len(names))
	for _, n := range names {
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

// ReadSkill returns one skill's full body.
func ReadSkill(skillsDir, name string) (string, error) {
	store, err := skills.New(skillsDir)
	if err != nil {
		return "", err
	}
	return store.Read(name)
}

// WriteSkill creates or replaces a skill.
func WriteSkill(skillsDir, name, body string) (string, error) {
	store, err := skills.New(skillsDir)
	if err != nil {
		return "", err
	}
	if err := store.Write(name, body); err != nil {
		return "", err
	}
	p, _ := store.Resolve(name)
	return p, nil
}

// DeleteSkill removes a skill.
func DeleteSkill(skillsDir, name string) error {
	store, err := skills.New(skillsDir)
	if err != nil {
		return err
	}
	return store.Delete(name)
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
