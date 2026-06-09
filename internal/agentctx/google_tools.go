package agentctx

// google_tools.go wires read-only Google Workspace tools into the chat
// loop: today's calendar and your task list. They read their own OAuth
// config from the environment (via `goon google auth`), so casual chat
// can reach them whenever Google is connected — no board/host needed.

import (
	"context"
	"os"
	"strings"

	"github.com/harisaginting/goon/internal/gcplog"
	"github.com/harisaginting/goon/internal/google"
)

// timeFmt renders a message/event time compactly for chat lists.
const gmailTimeFmt = "2006-01-02 15:04"

// googleConfigured reports whether goon is connected to Google.
func googleConfigured() bool { return google.Configured() }

// gcpLogConfigured reports whether log search is usable: Google connected
// AND a target project set.
func gcpLogConfigured() bool {
	return google.Configured() && strings.TrimSpace(os.Getenv("GOOGLE_CLOUD_PROJECT")) != ""
}

// execCalendarToday lists the user's events for today (primary calendar).
func execCalendarToday(ctx context.Context, _ ToolCall) (string, string) {
	cl, err := google.NewFromEnv()
	if err != nil {
		return "TOOL ERROR: " + err.Error(), "calendar_today rejected (not connected)"
	}
	cctx, cancel := context.WithTimeout(ctx, chatToolBudget)
	defer cancel()
	events, err := cl.EventsToday(cctx)
	if err != nil {
		return "TOOL ERROR: calendar_today failed: " + err.Error() + ". Tell the user what went wrong.",
			"calendar_today failed"
	}
	if len(events) == 0 {
		return "CALENDAR (today): no events. Tell the user their day is clear.", "calendar_today ok (0)"
	}
	var b strings.Builder
	b.WriteString("CALENDAR — today's events:\n")
	for _, e := range events {
		when := e.Start.Format("15:04")
		if e.AllDay {
			when = "all-day"
		} else if !e.End.IsZero() {
			when += "–" + e.End.Format("15:04")
		}
		b.WriteString("- " + when + "  " + e.Summary)
		if e.Location != "" {
			b.WriteString("  @ " + e.Location)
		}
		if len(e.Attendees) > 0 {
			b.WriteString("  (with " + strings.Join(e.Attendees, ", ") + ")")
		}
		b.WriteString("\n")
	}
	return clampForChat(b.String(), maxChatToolResult) + "\n\nAnswer the user in prose (times are local).",
		"calendar_today ok"
}

// execTasksList lists the user's open Google Tasks (default list).
func execTasksList(ctx context.Context, _ ToolCall) (string, string) {
	cl, err := google.NewFromEnv()
	if err != nil {
		return "TOOL ERROR: " + err.Error(), "tasks_list rejected (not connected)"
	}
	cctx, cancel := context.WithTimeout(ctx, chatToolBudget)
	defer cancel()
	items, err := cl.TasksList(cctx, "")
	if err != nil {
		return "TOOL ERROR: tasks_list failed: " + err.Error() + ". Tell the user what went wrong.",
			"tasks_list failed"
	}
	if len(items) == 0 {
		return "TASKS: none open. Tell the user they have no open tasks.", "tasks_list ok (0)"
	}
	var b strings.Builder
	b.WriteString("GOOGLE TASKS — open:\n")
	for _, t := range items {
		b.WriteString("- " + t.Title)
		if t.HasDue {
			b.WriteString("  (due " + t.Due.Format("2006-01-02") + ")")
		}
		b.WriteString("\n")
	}
	return clampForChat(b.String(), maxChatToolResult) + "\n\nAnswer the user in prose.",
		"tasks_list ok"
}

// execGmailSearch lists messages matching a Gmail query (e.g.
// "from:finance newer_than:7d"). Returns id/from/subject/date/snippet rows.
func execGmailSearch(ctx context.Context, c ToolCall) (string, string) {
	cl, err := google.NewFromEnv()
	if err != nil {
		return "TOOL ERROR: " + err.Error(), "gmail_search rejected (not connected)"
	}
	q := strings.TrimSpace(c.Query)
	cctx, cancel := context.WithTimeout(ctx, chatToolBudget)
	defer cancel()
	msgs, err := cl.SearchMessages(cctx, q, c.Limit)
	if err != nil {
		return "TOOL ERROR: gmail_search failed: " + err.Error() + ". Tell the user what went wrong.",
			"gmail_search failed"
	}
	if len(msgs) == 0 {
		return "GMAIL: no messages match. Tell the user nothing matched their query.", "gmail_search ok (0)"
	}
	var b strings.Builder
	b.WriteString("GMAIL — search results (use gmail_get with an id to read one in full):\n")
	for _, m := range msgs {
		when := ""
		if !m.Date.IsZero() {
			when = m.Date.Format(gmailTimeFmt) + "  "
		}
		b.WriteString("- id=" + m.ID + "  " + when + "from " + m.From + "\n")
		b.WriteString("    " + m.Subject + "\n")
		if m.Snippet != "" {
			b.WriteString("    " + m.Snippet + "\n")
		}
	}
	return clampForChat(b.String(), maxChatToolResult) +
			"\n\nSummarise for the user in prose (sender · subject · gist). Offer to open one in full if useful.",
		"gmail_search ok"
}

// execGmailGet fetches one message in full and returns its decoded body.
func execGmailGet(ctx context.Context, c ToolCall) (string, string) {
	cl, err := google.NewFromEnv()
	if err != nil {
		return "TOOL ERROR: " + err.Error(), "gmail_get rejected (not connected)"
	}
	id := strings.TrimSpace(c.ID)
	if id == "" {
		return "TOOL ERROR: gmail_get needs an \"id\" (from a gmail_search result). Re-emit with id set.",
			"gmail_get rejected (no id)"
	}
	cctx, cancel := context.WithTimeout(ctx, chatToolBudget)
	defer cancel()
	m, err := cl.GetMessage(cctx, id)
	if err != nil {
		return "TOOL ERROR: gmail_get failed: " + err.Error() + ". Tell the user what went wrong.",
			"gmail_get failed"
	}
	var b strings.Builder
	b.WriteString("GMAIL MESSAGE:\n")
	b.WriteString("From: " + m.From + "\n")
	b.WriteString("Subject: " + m.Subject + "\n")
	if !m.Date.IsZero() {
		b.WriteString("Date: " + m.Date.Format(gmailTimeFmt) + "\n")
	}
	b.WriteString("\n")
	body := m.Body
	if strings.TrimSpace(body) == "" {
		body = m.Snippet
	}
	b.WriteString(body)
	return clampForChat(b.String(), maxChatToolResult) +
			"\n\nAnswer the user's question about this email in prose; quote sparingly.",
		"gmail_get ok"
}

// execGCPLogSearch queries Google Cloud Logging (Log Explorer) and returns
// compact rows: timestamp · severity · trace · message.
func execGCPLogSearch(ctx context.Context, c ToolCall) (string, string) {
	project := strings.TrimSpace(os.Getenv("GOOGLE_CLOUD_PROJECT"))
	if project == "" {
		return "TOOL ERROR: GOOGLE_CLOUD_PROJECT is not set — run `goon config set GOOGLE_CLOUD_PROJECT <id>` so goon knows which project's logs to read.",
			"gcp_log_search rejected (no project)"
	}
	cl, err := google.NewFromEnv()
	if err != nil {
		return "TOOL ERROR: " + err.Error(), "gcp_log_search rejected (not connected)"
	}
	filter := gcplog.Build(c.Query, c.Trace, c.User, c.Severity, c.Hours)
	cctx, cancel := context.WithTimeout(ctx, chatToolBudget)
	defer cancel()
	entries, err := gcplog.Search(cctx, cl, project, filter, c.Limit)
	if err != nil {
		return "TOOL ERROR: gcp_log_search failed: " + err.Error() + ". Tell the user what went wrong (e.g. the Cloud Logging API may need enabling, or the project id is wrong).",
			"gcp_log_search failed"
	}
	if len(entries) == 0 {
		return "CLOUD LOGGING: no entries match (filter: " + filter + "). Tell the user nothing matched; suggest widening the time window or query.",
			"gcp_log_search ok (0)"
	}
	var b strings.Builder
	b.WriteString("CLOUD LOGGING — matches (newest first; filter: " + filter + "):\n")
	for _, e := range entries {
		ts := ""
		if !e.Timestamp.IsZero() {
			ts = e.Timestamp.Local().Format(gmailTimeFmt) + "  "
		}
		sev := e.Severity
		if sev == "" {
			sev = "DEFAULT"
		}
		b.WriteString("- " + ts + "[" + sev + "]")
		if e.Trace != "" {
			b.WriteString("  trace=" + e.Trace)
		}
		b.WriteString("\n    " + e.Message + "\n")
	}
	return clampForChat(b.String(), maxChatToolResult) +
			"\n\nAnswer the user in prose. If they asked for a traceId, state it plainly; if they asked when something happened, give the timestamp.",
		"gcp_log_search ok"
}
