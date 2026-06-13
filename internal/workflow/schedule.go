package workflow

// schedule.go is goon's zero-dependency scheduler matcher for automation
// triggers: a 5-field cron expression OR a simple interval ("every 30m").
// The daemon ticks every minute and asks each scheduled workflow whether it is
// Due given when it last ran.

import (
	"strconv"
	"strings"
	"time"
)

// Due reports whether a scheduled trigger should fire at `now`, given the time
// it `last` ran. board/manual triggers never fire on a schedule.
//
//   - Every: a Go duration ("30m", "1h", "24h") or "hourly"/"daily"/"weekly" —
//     fires when now-last >= the interval (and on the very first check).
//   - Cron : a 5-field expression (min hour dom month dow) — fires on the first
//     tick of a matching minute that is later than the last run.
func (t Trigger) Due(last, now time.Time) bool {
	if !strings.EqualFold(strings.TrimSpace(t.Type), "schedule") {
		return false
	}
	if e := strings.TrimSpace(t.Every); e != "" {
		d, err := parseEvery(e)
		if err != nil || d <= 0 {
			return false
		}
		return last.IsZero() || now.Sub(last) >= d
	}
	if c := strings.TrimSpace(t.Cron); c != "" {
		if !cronMatches(c, now) {
			return false
		}
		// Never fire twice inside the same minute.
		return now.Truncate(time.Minute).After(last.Truncate(time.Minute))
	}
	return false
}

// ScheduleHint is a short human description of the schedule for logs + UI.
func (t Trigger) ScheduleHint() string {
	if e := strings.TrimSpace(t.Every); e != "" {
		return "every " + e
	}
	if c := strings.TrimSpace(t.Cron); c != "" {
		return "cron: " + c
	}
	return "manual"
}

// parseEvery accepts Go durations plus a few friendly words.
func parseEvery(s string) (time.Duration, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "hourly":
		return time.Hour, nil
	case "daily":
		return 24 * time.Hour, nil
	case "weekly":
		return 7 * 24 * time.Hour, nil
	}
	return time.ParseDuration(strings.TrimSpace(s))
}

// cronMatches reports whether now satisfies a 5-field cron expression
// (minute hour day-of-month month day-of-week). day-of-week 0 or 7 = Sunday.
func cronMatches(spec string, now time.Time) bool {
	f := strings.Fields(spec)
	if len(f) != 5 {
		return false
	}
	return cronField(f[0], now.Minute(), 0, 59) &&
		cronField(f[1], now.Hour(), 0, 23) &&
		cronField(f[2], now.Day(), 1, 31) &&
		cronField(f[3], int(now.Month()), 1, 12) &&
		cronFieldDOW(f[4], int(now.Weekday()))
}

func cronField(field string, val, lo, hi int) bool {
	for _, part := range strings.Split(field, ",") {
		if cronPart(part, val, lo, hi) {
			return true
		}
	}
	return false
}

// cronFieldDOW handles day-of-week, accepting 7 as an alias for Sunday (0).
func cronFieldDOW(field string, val int) bool {
	for _, part := range strings.Split(field, ",") {
		if cronPart(part, val, 0, 6) {
			return true
		}
		if val == 0 && cronPart(part, 7, 0, 7) {
			return true
		}
	}
	return false
}

// cronPart matches one comma-segment: '*', N, 'a-b', and any with a '/step'.
func cronPart(part string, val, lo, hi int) bool {
	part = strings.TrimSpace(part)
	step := 1
	if i := strings.Index(part, "/"); i >= 0 {
		s, err := strconv.Atoi(part[i+1:])
		if err != nil || s <= 0 {
			return false
		}
		step = s
		part = part[:i]
	}
	rlo, rhi := lo, hi
	switch {
	case part == "*" || part == "":
		// keep full range
	case strings.Contains(part, "-"):
		ab := strings.SplitN(part, "-", 2)
		a, e1 := strconv.Atoi(strings.TrimSpace(ab[0]))
		b, e2 := strconv.Atoi(strings.TrimSpace(ab[1]))
		if e1 != nil || e2 != nil {
			return false
		}
		rlo, rhi = a, b
	default:
		n, err := strconv.Atoi(part)
		if err != nil {
			return false
		}
		rlo, rhi = n, n
	}
	if val < rlo || val > rhi {
		return false
	}
	if step > 1 {
		return (val-rlo)%step == 0
	}
	return true
}
