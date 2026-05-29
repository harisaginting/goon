package githost

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBitbucket_GetPRDetails_Reviewers(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/diff") {
			_, _ = w.Write([]byte("diff --git a/x b/x\n"))
			return
		}
		_, _ = w.Write([]byte(`{
		  "id": 629, "title": "t", "description": "d", "state": "OPEN",
		  "links": {"html": {"href": "https://bb/pr/629"}},
		  "author": {"display_name": "Wind"},
		  "source": {"branch": {"name": "feat/x"}},
		  "participants": [
		    {"user": {"display_name": "Alice"}, "role": "REVIEWER", "approved": true, "state": "approved"},
		    {"user": {"display_name": "Bob"}, "role": "REVIEWER", "approved": false, "state": ""},
		    {"user": {"display_name": "Carol"}, "role": "PARTICIPANT", "approved": false, "state": ""}
		  ]
		}`))
	}))
	defer ts.Close()
	b := &Bitbucket{Token: "TOK", APIURL: ts.URL, HTTP: ts.Client()}
	pr, _, err := b.GetPRDetails(context.Background(), "ws/repo", 629)
	if err != nil {
		t.Fatalf("GetPRDetails: %v", err)
	}
	if len(pr.Reviewers) != 2 {
		t.Fatalf("want 2 reviewers (PARTICIPANT excluded), got %d: %+v", len(pr.Reviewers), pr.Reviewers)
	}
	if pr.Reviewers[0].Name != "Alice" || !pr.Reviewers[0].Approved || pr.Reviewers[0].State != "approved" {
		t.Errorf("reviewer 0: %+v", pr.Reviewers[0])
	}
	if pr.Reviewers[1].Name != "Bob" || pr.Reviewers[1].State != "pending" {
		t.Errorf("reviewer 1 (empty state should normalize to pending): %+v", pr.Reviewers[1])
	}
}

func TestGitHub_GetPRDetails_Reviewers(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/reviews") {
			_, _ = w.Write([]byte(`[
			  {"user":{"login":"alice"},"state":"APPROVED"},
			  {"user":{"login":"carol"},"state":"CHANGES_REQUESTED"}
			]`))
			return
		}
		// PR detail and diff share the same path — branch on Accept.
		if r.Header.Get("Accept") == "application/vnd.github.v3.diff" {
			_, _ = w.Write([]byte("diff --git a/x b/x\n"))
			return
		}
		_, _ = w.Write([]byte(`{
		  "number": 42, "html_url": "https://gh/pr/42", "title": "t",
		  "state": "open", "body": "b", "head": {"ref": "feat/x"},
		  "user": {"login": "author"},
		  "requested_reviewers": [{"login": "bob"}]
		}`))
	}))
	defer ts.Close()
	g := &GitHub{Token: "T", APIURL: ts.URL, HTTP: ts.Client()}
	pr, _, err := g.GetPRDetails(context.Background(), "o/r", 42)
	if err != nil {
		t.Fatalf("GetPRDetails: %v", err)
	}
	if len(pr.Reviewers) != 3 {
		t.Fatalf("want 3 reviewers, got %d: %+v", len(pr.Reviewers), pr.Reviewers)
	}
	byName := map[string]Reviewer{}
	for _, rv := range pr.Reviewers {
		byName[rv.Name] = rv
	}
	if !byName["alice"].Approved || byName["alice"].State != "approved" {
		t.Errorf("alice should be approved: %+v", byName["alice"])
	}
	if byName["carol"].State != "changes_requested" {
		t.Errorf("carol should be changes_requested: %+v", byName["carol"])
	}
	if byName["bob"].State != "pending" {
		t.Errorf("bob (requested, no review yet) should be pending: %+v", byName["bob"])
	}
}

func TestGitLab_GetPRDetails_Reviewers(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/approvals"):
			_, _ = w.Write([]byte(`{"approved_by":[{"user":{"username":"alice"}}]}`))
		case strings.HasSuffix(r.URL.Path, "/diffs"):
			_, _ = w.Write([]byte(`[]`))
		default:
			_, _ = w.Write([]byte(`{"iid":7,"web_url":"https://gitlab.com/g/p/-/merge_requests/7",
			  "title":"t","state":"opened","source_branch":"feat/x","author":{"username":"author"},
			  "reviewers":[{"username":"alice","name":"Alice"},{"username":"bob","name":"Bob"}]}`))
		}
	}))
	defer ts.Close()
	g := &GitLab{Token: "TOK", APIURL: ts.URL, HTTP: ts.Client()}
	pr, _, err := g.GetPRDetails(context.Background(), "g/p", 7)
	if err != nil {
		t.Fatalf("GetPRDetails: %v", err)
	}
	if len(pr.Reviewers) != 2 {
		t.Fatalf("want 2 reviewers, got %d: %+v", len(pr.Reviewers), pr.Reviewers)
	}
	if pr.Reviewers[0].Name != "Alice" || !pr.Reviewers[0].Approved {
		t.Errorf("alice should be approved: %+v", pr.Reviewers[0])
	}
	if pr.Reviewers[1].Name != "Bob" || pr.Reviewers[1].State != "pending" {
		t.Errorf("bob should be pending: %+v", pr.Reviewers[1])
	}
}
