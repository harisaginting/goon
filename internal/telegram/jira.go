package telegram

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/harisaginting/goon/internal/boards"
)

// jiraHelp prints the /jira command surface. Kept in one place so the
// bare /jira and unknown-subcommand paths can both surface it.
const jiraHelp = `Direct Jira actions (no LLM needed):

Quick queries — no JQL required:
  /jira mine [PROJ]               tickets assigned to me (not done)
  /jira reported [PROJ]           tickets I reported (not done)
  /jira open [PROJ]               every open ticket (everyone)
  /jira blocked [PROJ]            tickets in blocked status
  /jira recent [PROJ]             updated in the last 7 days

Or top-level shortcut:  /mine

Other actions:
  /jira get <KEY>                  fetch one ticket with description
  /jira comment <KEY> <body…>      post a comment on the ticket
  /jira move <KEY> <status>        transition (open|in_progress|in_review|blocked|done)
  /jira edit <KEY> title <…>       set ticket summary
  /jira edit <KEY> desc <…>        set ticket description
  /jira edit <KEY> labels a,b,c    replace labels
  /jira search <JQL>               raw JQL — power-user escape hatch
  /jira refresh                    pull a fresh snapshot from the board

Every action goes straight to Jira via the configured token. No agent
loop — fast and predictable.`

// cmdJira routes the /jira subcommands. Pure CRUD against the
// configured board — works without any LLM provider configured.
func (b *Bot) cmdJira(ctx context.Context, chatID int64, args []string) {
	if len(args) == 0 {
		_ = b.Send(ctx, chatID, jiraHelp)
		return
	}
	sub := strings.ToLower(args[0])
	rest := args[1:]

	switch sub {
	case "help":
		_ = b.Send(ctx, chatID, jiraHelp)
	// One-word canned queries — the 95% case. Each takes an optional
	// project key to scope the search.
	case "mine", "me":
		b.runCannedQuery(ctx, chatID, rest, "mine",
			"assignee = currentUser() AND statusCategory != Done ORDER BY updated DESC")
	case "reported", "created":
		b.runCannedQuery(ctx, chatID, rest, "reported",
			"reporter = currentUser() AND statusCategory != Done ORDER BY updated DESC")
	case "open":
		b.runCannedQuery(ctx, chatID, rest, "open",
			"statusCategory != Done ORDER BY updated DESC")
	case "blocked":
		b.runCannedQuery(ctx, chatID, rest, "blocked",
			"status = Blocked ORDER BY updated DESC")
	case "recent":
		b.runCannedQuery(ctx, chatID, rest, "recent",
			"updated >= -7d AND statusCategory != Done ORDER BY updated DESC")
	// Power-user escape hatch.
	case "search", "find":
		b.cmdJiraSearch(ctx, chatID, rest)
	// Single-ticket actions.
	case "get", "show", "view":
		b.cmdJiraGet(ctx, chatID, rest)
	case "comment":
		b.cmdJiraComment(ctx, chatID, rest)
	case "move", "transition":
		b.cmdJiraMove(ctx, chatID, rest)
	case "edit", "update":
		b.cmdJiraEdit(ctx, chatID, rest)
	case "refresh":
		b.cmdRefresh(ctx, chatID)
	default:
		_ = b.Send(ctx, chatID, "unknown /jira subcommand: "+sub+"\n\n"+jiraHelp)
	}
}

// runCannedQuery composes a JQL string from a baseClause plus an
// optional project filter (taken from the first remaining arg), then
// hands off to the same search path as /jira search. label is the
// header shown to the user so they know which preset ran.
func (b *Bot) runCannedQuery(ctx context.Context, chatID int64, args []string, label, baseClause string) {
	jql := baseClause
	project := ""
	if len(args) > 0 {
		project = strings.TrimSpace(strings.ToUpper(args[0]))
	}
	if project != "" {
		jql = "project = " + project + " AND " + baseClause
	}
	if b.opts.Stdout != nil {
		fmt.Fprintf(b.opts.Stdout, "telegram_bot.jira.canned label=%s jql=%q\n", label, jql)
	}
	b.cmdJiraSearch(ctx, chatID, []string{jql})
}

// cmdJiraSearch runs a JQL query against the board if it implements
// boards.Searcher. Returns up to 20 tickets formatted for Telegram.
func (b *Bot) cmdJiraSearch(ctx context.Context, chatID int64, args []string) {
	if b.opts.Board == nil {
		_ = b.Send(ctx, chatID, "✗ no board configured")
		return
	}
	searcher, ok := b.opts.Board.(boards.Searcher)
	if !ok {
		_ = b.Send(ctx, chatID, "✗ the configured board does not support search")
		return
	}
	jql := strings.TrimSpace(strings.Join(args, " "))
	if jql == "" {
		_ = b.Send(ctx, chatID, "usage: /jira search <JQL>\nexample: /jira search 'assignee = currentUser() AND statusCategory != Done'")
		return
	}
	searchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tickets, err := searcher.Search(searchCtx, jql, 20)
	if err != nil {
		_ = b.Send(ctx, chatID, "✗ search failed: "+err.Error())
		return
	}
	if len(tickets) == 0 {
		_ = b.Send(ctx, chatID, "no matches for: "+jql)
		return
	}
	// Header message — just the summary, no per-ticket detail. Each
	// ticket gets its own message with a tap-to-view button so the
	// user doesn't have to copy keys.
	_ = b.Send(ctx, chatID, fmt.Sprintf("🔍 %d match(es) for:\n  %s", len(tickets), oneLine(jql, 200)))

	// Telegram caps inline keyboards / message volume. Cap the
	// interactive list at 15 so we don't spam the chat. The user can
	// narrow with a project filter for more.
	limit := len(tickets)
	if limit > 15 {
		limit = 15
	}
	for i := 0; i < limit; i++ {
		t := tickets[i]
		assignee := "—"
		if t.Assignee != "" {
			assignee = t.Assignee
		}
		line := fmt.Sprintf("%s — %s\n[%s] · %s", t.Key, oneLine(t.Title, 120), t.Status, assignee)
		// Single "open" row per ticket. Tap → detailed view with
		// full action keyboard underneath (comment/move/edit).
		row := [][]InlineButton{{{Text: "🔍 view " + t.Key, CallbackData: "v:" + t.Key}}}
		if t.URL != "" {
			row[0] = append(row[0], InlineButton{Text: "↗ browser", URL: t.URL})
		}
		_ = b.SendWithButtons(ctx, chatID, line, row)
	}
	if len(tickets) > limit {
		_ = b.Send(ctx, chatID, fmt.Sprintf("(+%d more not shown — narrow with /mine <PROJECT> or /jira search ...)", len(tickets)-limit))
	}
}

// cmdJiraGet fetches a single ticket with full description and
// attaches the per-ticket action keyboard (Comment / Move / Edit).
func (b *Bot) cmdJiraGet(ctx context.Context, chatID int64, args []string) {
	if b.opts.Board == nil {
		_ = b.Send(ctx, chatID, "✗ no board configured")
		return
	}
	if len(args) < 1 {
		_ = b.Send(ctx, chatID, "usage: /jira get <KEY>")
		return
	}
	getCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	t, err := b.opts.Board.Get(getCtx, args[0])
	if err != nil {
		_ = b.Send(ctx, chatID, "✗ get failed: "+err.Error())
		return
	}
	body := formatTicketDetail(t)
	kb := ticketActionKeyboard(t.Key)
	if err := b.SendWithButtons(ctx, chatID, body, kb); err != nil {
		// Fallback to plain text if Telegram rejects the keyboard
		// (very unlikely with our payload sizes).
		b.SendChunked(ctx, chatID, body)
	}
}

// cmdJiraComment posts a comment on a ticket. Comment is part of the
// base Board interface so this works on every board adapter.
func (b *Bot) cmdJiraComment(ctx context.Context, chatID int64, args []string) {
	if b.opts.Board == nil {
		_ = b.Send(ctx, chatID, "✗ no board configured")
		return
	}
	if len(args) < 2 {
		_ = b.Send(ctx, chatID, "usage: /jira comment <KEY> <body…>")
		return
	}
	key := args[0]
	body := strings.TrimSpace(strings.Join(args[1:], " "))
	if body == "" {
		_ = b.Send(ctx, chatID, "✗ empty comment body")
		return
	}
	cCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := b.opts.Board.Comment(cCtx, key, body); err != nil {
		_ = b.Send(ctx, chatID, "✗ comment failed: "+err.Error())
		return
	}
	_ = b.Send(ctx, chatID, "✓ commented on "+key+" — "+oneLine(body, 80))
}

// cmdJiraMove transitions a ticket to one of goon's canonical
// statuses (open, in_progress, in_review, blocked, done). The board
// adapter fuzzy-matches against the project's workflow transitions.
func (b *Bot) cmdJiraMove(ctx context.Context, chatID int64, args []string) {
	if b.opts.Board == nil {
		_ = b.Send(ctx, chatID, "✗ no board configured")
		return
	}
	if len(args) < 2 {
		_ = b.Send(ctx, chatID, "usage: /jira move <KEY> <status>\nstatus: open | in_progress | in_review | blocked | done")
		return
	}
	key := args[0]
	statusRaw := strings.ToLower(strings.TrimSpace(args[1]))
	target := boards.MapStatus(statusRaw)
	if target == boards.StatusUnknown {
		_ = b.Send(ctx, chatID, "✗ unknown status: "+args[1]+"\nuse one of: open, in_progress, in_review, blocked, done")
		return
	}
	tCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := b.opts.Board.Transition(tCtx, key, target); err != nil {
		_ = b.Send(ctx, chatID, "✗ transition failed: "+err.Error())
		return
	}
	_ = b.Send(ctx, chatID, fmt.Sprintf("✓ %s → %s", key, target))
}

// cmdJiraEdit covers title / description / labels updates. Routes
// to boards.Updater (Jira implements it; other boards may not).
//
// Syntax:
//
//	/jira edit <KEY> title <new title>
//	/jira edit <KEY> desc  <new description>
//	/jira edit <KEY> labels a,b,c    (empty list to clear)
func (b *Bot) cmdJiraEdit(ctx context.Context, chatID int64, args []string) {
	if b.opts.Board == nil {
		_ = b.Send(ctx, chatID, "✗ no board configured")
		return
	}
	updater, ok := b.opts.Board.(boards.Updater)
	if !ok {
		_ = b.Send(ctx, chatID, "✗ the configured board does not support edits")
		return
	}
	if len(args) < 3 {
		_ = b.Send(ctx, chatID, "usage: /jira edit <KEY> title|desc|labels <value…>")
		return
	}
	key := args[0]
	field := strings.ToLower(args[1])
	value := strings.TrimSpace(strings.Join(args[2:], " "))

	patch := boards.TicketPatch{}
	switch field {
	case "title", "summary":
		t := value
		patch.Title = &t
	case "desc", "description", "body":
		d := value
		patch.Description = &d
	case "labels", "label", "tags":
		if value == "" || value == "-" {
			patch.Labels = []string{} // clear
		} else {
			parts := strings.Split(value, ",")
			out := make([]string, 0, len(parts))
			for _, p := range parts {
				p = strings.TrimSpace(p)
				if p != "" {
					out = append(out, p)
				}
			}
			patch.Labels = out
		}
	default:
		_ = b.Send(ctx, chatID, "✗ unknown field: "+field+"\nuse one of: title, desc, labels")
		return
	}
	uCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := updater.Update(uCtx, key, patch); err != nil {
		_ = b.Send(ctx, chatID, "✗ edit failed: "+err.Error())
		return
	}
	_ = b.Send(ctx, chatID, "✓ updated "+key+" · "+field)
}

// oneLine trims a string to a single line of at most n runes for
// compact Telegram rendering. Local copy so jira.go has no shared
// dependency on chat.go's helpers.
func oneLine(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.TrimSpace(s)
	if n > 0 && len([]rune(s)) > n {
		r := []rune(s)
		return string(r[:n-1]) + "…"
	}
	return s
}
