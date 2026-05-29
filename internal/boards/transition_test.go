package boards

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestMatchTransition is the regression test for the bug where
// "ready to test" was bucketed to StatusOpen (it contains "ready") and
// the ticket was moved to Backlog instead.
func TestMatchTransition(t *testing.T) {
	trs := []jiraTransition{
		{ID: "11", Name: "Back to Backlog", ToName: "Backlog"},
		{ID: "21", Name: "Start Work", ToName: "In Progress"},
		{ID: "31", Name: "Ready for Testing", ToName: "Ready to Test"},
		{ID: "41", Name: "Done", ToName: "Done"},
	}
	cases := []struct{ in, wantTo string }{
		{"ready to test", "Ready to Test"}, // must NOT pick Backlog
		{"Ready To Test", "Ready to Test"},
		{"ready-to-test", "Ready to Test"},
		{"in progress", "In Progress"},
		{"backlog", "Backlog"},
		{"done", "Done"},
		{"Start Work", "In Progress"}, // match on the transition name
	}
	for _, c := range cases {
		got, ok := matchTransition(trs, c.in)
		if !ok {
			t.Errorf("matchTransition(%q): no match, want %q", c.in, c.wantTo)
			continue
		}
		if got.ToName != c.wantTo {
			t.Errorf("matchTransition(%q) → %q, want %q", c.in, got.ToName, c.wantTo)
		}
	}
	if got, ok := matchTransition(trs, "completely unrelated xyz"); ok {
		t.Errorf("matchTransition matched garbage → %q", got.ToName)
	}
}

func TestJira_TransitionByName(t *testing.T) {
	var postedID string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"transitions":[
			  {"id":"11","name":"Back to Backlog","to":{"name":"Backlog"}},
			  {"id":"31","name":"Ready for Testing","to":{"name":"Ready to Test"}}
			]}`))
			return
		}
		var body struct {
			Transition struct {
				ID string `json:"id"`
			} `json:"transition"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		postedID = body.Transition.ID
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()
	j := &Jira{BaseURL: ts.URL, Email: "e", APIToken: "t", HTTP: ts.Client()}

	applied, err := j.TransitionByName(context.Background(), "EB-4978", "ready to test")
	if err != nil {
		t.Fatalf("TransitionByName: %v", err)
	}
	if applied != "Ready to Test" {
		t.Errorf("applied status = %q, want %q", applied, "Ready to Test")
	}
	if postedID != "31" {
		t.Errorf("posted transition id = %q, want 31 (Ready to Test) — not 11 (Backlog)", postedID)
	}
}

func TestJira_TransitionByName_NoMatch(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"transitions":[{"id":"11","name":"x","to":{"name":"Backlog"}}]}`))
	}))
	defer ts.Close()
	j := &Jira{BaseURL: ts.URL, Email: "e", APIToken: "t", HTTP: ts.Client()}
	_, err := j.TransitionByName(context.Background(), "EB-1", "ready to test")
	if err == nil || !strings.Contains(err.Error(), "Backlog") {
		t.Fatalf("expected an error listing available statuses, got %v", err)
	}
}

func TestJira_ListTransitions(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"transitions":[
		  {"id":"11","name":"a","to":{"name":"Backlog"}},
		  {"id":"31","name":"b","to":{"name":"Ready to Test"}}
		]}`))
	}))
	defer ts.Close()
	j := &Jira{BaseURL: ts.URL, Email: "e", APIToken: "t", HTTP: ts.Client()}
	names, err := j.ListTransitions(context.Background(), "EB-1")
	if err != nil {
		t.Fatalf("ListTransitions: %v", err)
	}
	if len(names) != 2 || names[0] != "Backlog" || names[1] != "Ready to Test" {
		t.Errorf("names = %v, want [Backlog, Ready to Test]", names)
	}
}
