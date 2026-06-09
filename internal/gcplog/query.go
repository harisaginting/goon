package gcplog

import (
	"strings"
	"time"
)

// Build composes a Cloud Logging filter from the structured chat args. The
// common asks map cleanly:
//   - trace set      → everything for that trace id (within the window)
//   - user + query   → that user's matching events (e.g. login/register)
//   - query only     → free-text search, OR a raw filter if it looks like one
// A severity floor and a time window (hours, default 24) are always ANDed on.
func Build(query, trace, user, severity string, hours int) string {
	var parts []string
	if t := strings.TrimSpace(trace); t != "" {
		parts = append(parts, byTrace(t))
	} else {
		if q := strings.TrimSpace(query); q != "" {
			if looksLikeFilter(q) {
				parts = append(parts, "("+q+")")
			} else {
				parts = append(parts, quoted(q))
			}
		}
		if u := strings.TrimSpace(user); u != "" {
			parts = append(parts, quoted(u))
		}
	}
	parts = append(parts, severityFloor(severity), sinceHours(hours))
	return and(parts...)
}

// byTrace matches the trace field by substring, so a bare trace id matches
// the full "projects/PID/traces/<id>" resource form Cloud Logging stores.
func byTrace(id string) string { return `trace:"` + escapeQuotes(id) + `"` }

// quoted is a global free-text search across the whole entry.
func quoted(s string) string { return `"` + escapeQuotes(s) + `"` }

func severityFloor(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	return "severity>=" + s
}

func sinceHours(n int) string {
	if n <= 0 {
		n = 24
	}
	since := time.Now().Add(-time.Duration(n) * time.Hour).UTC().Format(time.RFC3339)
	return `timestamp>="` + since + `"`
}

func and(parts ...string) string {
	var keep []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, " AND ")
}

func escapeQuotes(s string) string {
	return strings.ReplaceAll(s, `"`, `\"`)
}

// looksLikeFilter heuristically detects when the user (or LLM) handed us a
// raw Cloud Logging filter instead of plain search text, so we pass it
// through rather than wrapping it in quotes.
func looksLikeFilter(q string) bool {
	low := strings.ToLower(q)
	for _, tok := range []string{
		"severity", "timestamp", "jsonpayload", "textpayload",
		"resource.", "labels.", "loginid", "trace=", "trace:", " and ", " or ",
		">=", "<=", "!=",
	} {
		if strings.Contains(low, tok) {
			return true
		}
	}
	return false
}
