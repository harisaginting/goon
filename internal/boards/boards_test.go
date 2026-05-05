package boards

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMapStatus(t *testing.T) {
	cases := map[string]Status{
		"":            StatusUnknown,
		"To Do":       StatusOpen,
		"open":        StatusOpen,
		"In Progress": StatusInProgress,
		"In Review":   StatusInReview,
		"Code Review": StatusInReview,
		"Blocked":     StatusBlocked,
		"Done":        StatusDone,
		"Closed":      StatusDone,
		"Resolved":    StatusDone,
		"WeirdState":  StatusUnknown,
	}
	for native, want := range cases {
		if got := MapStatus(native); got != want {
			t.Errorf("MapStatus(%q) = %v, want %v", native, got, want)
		}
	}
}

func TestJira_List(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/rest/api/3/search") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if !strings.Contains(r.URL.RawQuery, "jql=") {
			t.Errorf("missing jql query: %s", r.URL.RawQuery)
		}
		if r.Header.Get("Authorization") == "" {
			t.Errorf("missing auth")
		}
		_, _ = w.Write([]byte(`{
		  "issues":[{
		    "id":"10001","key":"ENG-1",
		    "fields":{
		      "summary":"Add login","description":"Implement OAuth",
		      "status":{"name":"To Do"},"labels":["backend","auth"],
		      "project":{"key":"ENG"},
		      "assignee":{"displayName":"Harisa"},
		      "updated":"2026-04-26T10:00:00.000-0700"
		    }
		  }]
		}`))
	}))
	defer ts.Close()
	j := &Jira{
		BaseURL: ts.URL, Email: "u", APIToken: "t",
		JQL: "x=y", HTTP: ts.Client(),
	}
	got, err := j.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 ticket, got %d", len(got))
	}
	tk := got[0]
	if tk.Key != "ENG-1" || tk.Title != "Add login" || tk.Status != StatusOpen {
		t.Errorf("bad ticket: %+v", tk)
	}
	if tk.Description != "Implement OAuth" {
		t.Errorf("desc: %q", tk.Description)
	}
	if tk.Project != "ENG" || tk.Assignee != "Harisa" {
		t.Errorf("project/assignee: %+v", tk)
	}
}

func TestJira_ADF(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
		  "issues":[{
		    "id":"1","key":"X-1",
		    "fields":{
		      "summary":"t",
		      "description":{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"hello "},{"type":"text","text":"world"}]}]},
		      "status":{"name":"Open"},"project":{"key":"X"},"updated":"2026-01-01T00:00:00.000+0000"
		    }
		  }]
		}`))
	}))
	defer ts.Close()
	j := &Jira{BaseURL: ts.URL, Email: "u", APIToken: "t", JQL: "x", HTTP: ts.Client()}
	got, _ := j.List(context.Background())
	if len(got) != 1 || !strings.Contains(got[0].Description, "hello world") {
		t.Fatalf("ADF parse failed: %+v", got)
	}
}

// TestJira_List_UsesNewSearchEndpoint confirms we hit the post-deprecation
// path /rest/api/3/search/jql, not the removed /rest/api/3/search.
// (Atlassian CHANGE-2046 returned 410 GONE on the old URL.)
func TestJira_List_UsesNewSearchEndpoint(t *testing.T) {
	var hitPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPath = r.URL.Path
		_, _ = w.Write([]byte(`{"issues":[],"isLast":true}`))
	}))
	defer ts.Close()
	j := &Jira{BaseURL: ts.URL, Email: "u", APIToken: "t", JQL: "x", HTTP: ts.Client()}
	if _, err := j.List(context.Background()); err != nil {
		t.Fatalf("list: %v", err)
	}
	if hitPath != "/rest/api/3/search/jql" {
		t.Errorf("expected /rest/api/3/search/jql, got %s", hitPath)
	}
}

// TestJira_List_TruncationWarning verifies we warn (don't error) when the
// new endpoint signals more pages exist via isLast=false.
func TestJira_List_TruncationWarning(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
		  "issues":[{"id":"1","key":"X-1","fields":{"summary":"a","status":{"name":"Open"},"project":{"key":"X"},"updated":"2026-01-01T00:00:00.000+0000"}}],
		  "isLast":false,
		  "nextPageToken":"abc"
		}`))
	}))
	defer ts.Close()
	j := &Jira{BaseURL: ts.URL, Email: "u", APIToken: "t", JQL: "x", HTTP: ts.Client()}
	got, err := j.List(context.Background())
	if err != nil {
		t.Fatalf("truncation should warn, not error: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("want 1 ticket from page, got %d", len(got))
	}
}

func TestJira_Comment(t *testing.T) {
	var posted map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/comment") {
			t.Errorf("path: %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&posted)
		w.WriteHeader(201)
	}))
	defer ts.Close()
	j := &Jira{BaseURL: ts.URL, Email: "u", APIToken: "t", JQL: "x", HTTP: ts.Client()}
	if err := j.Comment(context.Background(), "ENG-1", "started"); err != nil {
		t.Fatalf("comment: %v", err)
	}
	if posted == nil {
		t.Fatal("expected comment payload")
	}
}

func TestGitHub_List(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/repos/me/myrepo/issues") {
			t.Errorf("path: %s", r.URL.Path)
		}
		if !strings.Contains(r.URL.RawQuery, "state=open") {
			t.Errorf("query: %s", r.URL.RawQuery)
		}
		if r.Header.Get("Authorization") != "Bearer TOK" {
			t.Errorf("auth: %s", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`[
		  {"number":7,"title":"Bug A","body":"reproduce me","html_url":"https://x/7","state":"open","labels":[{"name":"bug"}],"assignees":[{"login":"u1"}],"updated_at":"2026-04-26T10:00:00Z"},
		  {"number":8,"title":"PR Y","body":"","html_url":"https://x/8","state":"open","pull_request":{"url":"x"}}
		]`))
	}))
	defer ts.Close()
	g := &GitHub{Token: "TOK", Repos: []string{"me/myrepo"}, APIURL: ts.URL, HTTP: ts.Client()}
	got, err := g.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 ticket (PR filtered), got %d", len(got))
	}
	if got[0].ID != "me/myrepo#7" || got[0].Title != "Bug A" {
		t.Errorf("bad ticket: %+v", got[0])
	}
	if len(got[0].Labels) != 1 || got[0].Labels[0] != "bug" {
		t.Errorf("labels: %v", got[0].Labels)
	}
}

func TestGitHub_Transition(t *testing.T) {
	var got map[string]string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("method: %s", r.Method)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()
	g := &GitHub{Token: "TOK", APIURL: ts.URL, HTTP: ts.Client(), Repos: []string{"o/r"}}
	if err := g.Transition(context.Background(), "o/r#1", StatusDone); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if got["state"] != "closed" {
		t.Errorf("state: %v", got)
	}
}

func TestSplitGHID(t *testing.T) {
	cases := []struct {
		id     string
		repo   string
		num    int
		wantOK bool
	}{
		{"owner/repo#42", "owner/repo", 42, true},
		{"a/b#1", "a/b", 1, true},
		{"weird#x", "", 0, false},
		{"no-hash", "", 0, false},
	}
	for _, tc := range cases {
		repo, num, ok := splitGHID(tc.id)
		if ok != tc.wantOK || repo != tc.repo || num != tc.num {
			t.Errorf("splitGHID(%q) = %q,%d,%v; want %q,%d,%v",
				tc.id, repo, num, ok, tc.repo, tc.num, tc.wantOK)
		}
	}
}

func TestMockBoard(t *testing.T) {
	m := NewMock([]Ticket{{ID: "X-1", Title: "hi"}})
	got, err := m.List(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("list: %v %v", err, got)
	}
	if err := m.Comment(context.Background(), "X-1", "wip"); err != nil {
		t.Fatalf("comment: %v", err)
	}
	if err := m.Transition(context.Background(), "X-1", StatusDone); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if m.Tickets[0].Status != StatusDone {
		t.Fatalf("status not updated: %v", m.Tickets[0].Status)
	}
}

func TestNewFromEnv_None(t *testing.T) {
	t.Setenv("GOON_BOARD", "")
	if _, err := NewFromEnv(); err != ErrNoBoard {
		t.Fatalf("expected ErrNoBoard, got %v", err)
	}
}

func TestNewFromEnv_Mock(t *testing.T) {
	t.Setenv("GOON_BOARD", "mock")
	b, err := NewFromEnv()
	if err != nil {
		t.Fatalf("from env: %v", err)
	}
	if b.Name() != "mock" {
		t.Fatalf("name: %s", b.Name())
	}
}
