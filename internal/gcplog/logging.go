// Package gcplog is a zero-dependency read client for Google Cloud
// Logging (the "Log Explorer"). It reuses internal/google's OAuth'd
// *Client for its Bearer token and calls the entries:list REST endpoint
// directly with encoding/json — no google.golang.org/api SDK, per goon's
// stdlib-only rule.
package gcplog

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/harisaginting/goon/internal/google"
)

const entriesListURL = "https://logging.googleapis.com/v2/entries:list"

// Entry is one log line flattened for chat display.
type Entry struct {
	Timestamp time.Time
	Severity  string
	Trace     string
	Message   string
	LogName   string
}

type listRequest struct {
	ResourceNames []string `json:"resourceNames"`
	Filter        string   `json:"filter,omitempty"`
	OrderBy       string   `json:"orderBy,omitempty"`
	PageSize      int      `json:"pageSize,omitempty"`
}

type rawEntry struct {
	Timestamp    string          `json:"timestamp"`
	Severity     string          `json:"severity"`
	Trace        string          `json:"trace"`
	SpanID       string          `json:"spanId"`
	TextPayload  string          `json:"textPayload"`
	JSONPayload  json.RawMessage `json:"jsonPayload"`
	ProtoPayload json.RawMessage `json:"protoPayload"`
	LogName      string          `json:"logName"`
}

type listResponse struct {
	Entries       []rawEntry `json:"entries"`
	NextPageToken string     `json:"nextPageToken"`
}

// Search runs an entries:list against one project with the given Cloud
// Logging filter, newest first, capped at limit rows.
func Search(ctx context.Context, cl *google.Client, project, filter string, limit int) ([]Entry, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	req := listRequest{
		ResourceNames: []string{"projects/" + strings.TrimSpace(project)},
		Filter:        filter,
		OrderBy:       "timestamp desc",
		PageSize:      limit,
	}
	var resp listResponse
	if err := cl.PostJSON(ctx, entriesListURL, req, &resp); err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(resp.Entries))
	for _, e := range resp.Entries {
		ent := Entry{
			Severity: e.Severity,
			Trace:    shortTrace(e.Trace),
			LogName:  shortLogName(e.LogName),
			Message:  messageOf(e),
		}
		if t, err := time.Parse(time.RFC3339, e.Timestamp); err == nil {
			ent.Timestamp = t
		}
		out = append(out, ent)
	}
	return out, nil
}

// messageOf picks the most human-readable text from an entry: textPayload
// first, then common message-ish keys in jsonPayload, else the compact JSON.
func messageOf(e rawEntry) string {
	if s := strings.TrimSpace(e.TextPayload); s != "" {
		return oneLine(s)
	}
	if len(e.JSONPayload) > 0 {
		var m map[string]any
		if err := json.Unmarshal(e.JSONPayload, &m); err == nil {
			for _, k := range []string{"message", "msg", "event", "summary", "description"} {
				if v, ok := m[k]; ok {
					if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
						return oneLine(s)
					}
				}
			}
		}
		return oneLine(string(e.JSONPayload))
	}
	if len(e.ProtoPayload) > 0 {
		return oneLine(string(e.ProtoPayload))
	}
	return ""
}

// shortTrace reduces "projects/PID/traces/abc123" to "abc123".
func shortTrace(t string) string {
	t = strings.TrimSpace(t)
	if i := strings.LastIndex(t, "/"); i >= 0 {
		return t[i+1:]
	}
	return t
}

// shortLogName reduces "projects/PID/logs/run.googleapis.com%2Fstdout" to
// the decoded leaf "stdout".
func shortLogName(n string) string {
	n = strings.TrimSpace(n)
	if i := strings.LastIndex(n, "/logs/"); i >= 0 {
		n = n[i+len("/logs/"):]
	}
	n = strings.ReplaceAll(n, "%2F", "/")
	if i := strings.LastIndex(n, "/"); i >= 0 {
		n = n[i+1:]
	}
	return n
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return strings.TrimSpace(s)
}
