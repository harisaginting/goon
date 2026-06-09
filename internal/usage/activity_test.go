package usage

import (
	"context"
	"testing"
)

func TestActivityLifecycle(t *testing.T) {
	id := StartActivity("PR review draft", "claude-sonnet")
	found := false
	for _, a := range ActiveActivities() {
		if a.ID == id {
			found = true
			if a.Label != "PR review draft" || a.Model != "claude-sonnet" {
				t.Errorf("activity = %+v", a)
			}
		}
	}
	if !found {
		t.Fatal("started activity not listed as active")
	}
	EndActivity(id)
	for _, a := range ActiveActivities() {
		if a.ID == id {
			t.Fatal("activity still active after EndActivity")
		}
	}
}

func TestActivityNormalisesBlanks(t *testing.T) {
	id := StartActivity("", "")
	defer EndActivity(id)
	var got Activity
	for _, a := range ActiveActivities() {
		if a.ID == id {
			got = a
		}
	}
	if got.Label != "model call" || got.Model != "unknown" {
		t.Errorf("blanks not normalised: %+v", got)
	}
}

// TestActivityLingers verifies a finished activity stays visible (so the
// dashboard doesn't flicker between an agent loop's calls) but is marked
// not-running.
func TestActivityLingers(t *testing.T) {
	id := StartActivity("workflow ENG-9", "gpt-4o")
	EndActivity(id)
	var got Activity
	found := false
	for _, a := range ActiveActivities() {
		if a.ID == id {
			got, found = a, true
		}
	}
	if !found {
		t.Fatal("finished activity should linger in ActiveActivities, but it's gone")
	}
	if got.Running() {
		t.Error("lingering activity should report Running()==false")
	}
	if got.EndedAt.IsZero() {
		t.Error("lingering activity should have EndedAt set")
	}
}

func TestActivityLabelContext(t *testing.T) {
	if LabelFrom(context.Background()) != "" {
		t.Error("expected empty label on a bare context")
	}
	ctx := WithLabel(context.Background(), "workflow ENG-1")
	if got := LabelFrom(ctx); got != "workflow ENG-1" {
		t.Errorf("LabelFrom = %q, want 'workflow ENG-1'", got)
	}
	// nil-safe.
	if LabelFrom(nil) != "" {
		t.Error("LabelFrom(nil) should be empty")
	}
}
