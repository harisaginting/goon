package usage

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Activity is one unit of model work — an LLM call executing (or just
// finished) somewhere in this process (a workflow stage, a PR-review draft, a
// chat turn, the standby reflection, …). Unlike per-model token totals,
// activities are ephemeral and never persisted.
//
// Finished activities linger for a short window (see lingerWindow) so the
// dashboard doesn't flicker between the back-to-back LLM calls of an agent
// loop. EndedAt is zero while the call is in flight.
type Activity struct {
	ID        int64     `json:"id"`
	Label     string    `json:"label"`
	Model     string    `json:"model"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
}

// Running reports whether the call is still in flight (not yet ended).
func (a Activity) Running() bool { return a.EndedAt.IsZero() }

// lingerWindow is how long a finished activity stays visible after EndActivity,
// smoothing the gaps between an agent loop's successive LLM calls.
const lingerWindow = 8 * time.Second

var (
	actMu      sync.Mutex
	actSeq     int64
	activities = map[int64]Activity{}
)

// purgeLocked drops finished activities older than lingerWindow. Caller holds
// actMu.
func purgeLocked(now time.Time) {
	for id, a := range activities {
		if !a.EndedAt.IsZero() && now.Sub(a.EndedAt) > lingerWindow {
			delete(activities, id)
		}
	}
}

// StartActivity registers an in-flight model call and returns its id. Pair it
// with a deferred EndActivity(id). An empty label/model is normalised so the
// dashboard never shows a blank row.
func StartActivity(label, model string) int64 {
	if label == "" {
		label = "model call"
	}
	if model == "" {
		model = "unknown"
	}
	now := time.Now()
	actMu.Lock()
	defer actMu.Unlock()
	purgeLocked(now)
	actSeq++
	id := actSeq
	activities[id] = Activity{ID: id, Label: label, Model: model, StartedAt: now}
	return id
}

// EndActivity marks an activity finished. It stays visible for lingerWindow
// (to avoid dashboard flicker between an agent loop's calls) and is purged
// later. Safe to call with an unknown id.
func EndActivity(id int64) {
	actMu.Lock()
	if a, ok := activities[id]; ok {
		a.EndedAt = time.Now()
		activities[id] = a
	}
	actMu.Unlock()
}

// ActiveActivities returns running + recently-finished model calls (running
// first, then by start time). Finished entries past the linger window are
// pruned here so the set never grows unbounded.
func ActiveActivities() []Activity {
	now := time.Now()
	actMu.Lock()
	defer actMu.Unlock()
	purgeLocked(now)
	out := make([]Activity, 0, len(activities))
	for _, a := range activities {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Running() != out[j].Running() {
			return out[i].Running() // running ones first
		}
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	return out
}

// labelKey is the private context key under which an activity label is stored.
type labelKey struct{}

// WithLabel returns a context carrying a human-readable label describing what
// the model work is for ("PR review draft", "workflow ENG-1 triage", …). The
// provider layer reads it when it registers an in-flight activity, so callers
// only have to set it once at an entry point.
func WithLabel(ctx context.Context, label string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, labelKey{}, label)
}

// LabelFrom extracts the activity label set by WithLabel, or "" if none.
func LabelFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(labelKey{}).(string); ok {
		return v
	}
	return ""
}
