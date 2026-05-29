package githost

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGitLab_RepoFromWebURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://gitlab.com/group/project/-/merge_requests/42", "group/project"},
		{"https://gitlab.com/group/sub/project/-/merge_requests/1", "group/sub/project"},
		{"https://gl.example.com/g/p/-/merge_requests/9", "g/p"},
		{"https://gitlab.com/group/project", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := glRepoFromWebURL(c.in); got != c.want {
			t.Errorf("glRepoFromWebURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestGitLab_NormalizeState(t *testing.T) {
	cases := []struct{ in, want string }{
		{"opened", "open"},
		{"merged", "merged"},
		{"closed", "closed"},
		{"locked", "closed"},
		{"weird", "weird"},
	}
	for _, c := range cases {
		if got := glNormalizeState(c.in); got != c.want {
			t.Errorf("glNormalizeState(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestGitHub_NotificationHTMLURL(t *testing.T) {
	g := &GitHub{APIURL: "https://api.github.com"}
	if got := g.notificationHTMLURL("https://api.github.com/repos/o/n/pulls/42"); got != "https://github.com/o/n/pull/42" {
		t.Errorf("pull URL: %q", got)
	}
	if got := g.notificationHTMLURL("https://api.github.com/repos/o/n/issues/7"); got != "https://github.com/o/n/issues/7" {
		t.Errorf("issue URL: %q", got)
	}
	if g.notificationHTMLURL("") != "" {
		t.Error("empty input should give empty URL")
	}
}

func TestGitHub_ReviewRequestedPRs(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/search/issues") {
			t.Errorf("path: %s", r.URL.Path)
		}
		if q := r.URL.Query().Get("q"); !strings.Contains(q, "review-requested:@me") {
			t.Errorf("query missing review-requested qualifier: %q", q)
		}
		_, _ = w.Write([]byte(`{"total_count":1,"items":[
		  {"number":12,"html_url":"https://gh/o/r/pull/12","title":"Fix bug",
		   "state":"open","body":"b","user":{"login":"alice"},
		   "repository_url":"https://api.github.com/repos/o/r"}
		]}`))
	}))
	defer ts.Close()
	g := &GitHub{Token: "T", APIURL: ts.URL, HTTP: ts.Client()}
	prs, err := g.ReviewRequestedPRs(context.Background())
	if err != nil {
		t.Fatalf("ReviewRequestedPRs: %v", err)
	}
	if len(prs) != 1 {
		t.Fatalf("want 1 PR, got %d", len(prs))
	}
	if prs[0].Number != 12 || prs[0].Repo != "o/r" || prs[0].Author != "alice" {
		t.Errorf("PR: %+v", prs[0])
	}
}

func TestGitHub_Notifications(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/notifications") {
			t.Errorf("path: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`[
		  {"id":"1","reason":"review_requested","updated_at":"2024-01-02T03:04:05Z",
		   "subject":{"title":"Review this","url":"x","type":"PullRequest"},
		   "repository":{"full_name":"o/r"}},
		  {"id":"2","reason":"mention","updated_at":"2024-01-01T00:00:00Z",
		   "subject":{"title":"You were mentioned","url":"","type":"Issue"},
		   "repository":{"full_name":"o/r"}},
		  {"id":"3","reason":"subscribed","updated_at":"2024-01-03T00:00:00Z",
		   "subject":{"title":"ignored","url":"","type":"Issue"},
		   "repository":{"full_name":"o/r"}}
		]`))
	}))
	defer ts.Close()
	g := &GitHub{Token: "T", APIURL: ts.URL, HTTP: ts.Client()}
	notifs, err := g.Notifications(context.Background())
	if err != nil {
		t.Fatalf("Notifications: %v", err)
	}
	if len(notifs) != 2 {
		t.Fatalf("want 2 actionable notifications (subscribed filtered out), got %d", len(notifs))
	}
	if notifs[0].Kind != "review_requested" || notifs[0].ID != "1" {
		t.Errorf("first notif: %+v", notifs[0])
	}
	if notifs[1].Kind != "mention" {
		t.Errorf("second notif kind: %q", notifs[1].Kind)
	}
}

func TestGitLab_ReviewRequestedPRs(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/user"):
			_, _ = w.Write([]byte(`{"username":"alice"}`))
		case strings.HasSuffix(r.URL.Path, "/merge_requests"):
			if r.URL.Query().Get("reviewer_username") != "alice" {
				t.Errorf("reviewer_username: %q", r.URL.Query().Get("reviewer_username"))
			}
			_, _ = w.Write([]byte(`[
			  {"iid":5,"web_url":"https://gitlab.com/g/p/-/merge_requests/5","title":"MR five",
			   "description":"d","state":"opened","source_branch":"feat/x","author":{"username":"bob"}}
			]`))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer ts.Close()
	g := &GitLab{Token: "TOK", APIURL: ts.URL, HTTP: ts.Client()}
	prs, err := g.ReviewRequestedPRs(context.Background())
	if err != nil {
		t.Fatalf("ReviewRequestedPRs: %v", err)
	}
	if len(prs) != 1 || prs[0].Number != 5 || prs[0].Repo != "g/p" {
		t.Fatalf("PRs: %+v", prs)
	}
	if prs[0].State != "open" {
		t.Errorf("state should normalize to 'open', got %q", prs[0].State)
	}
}

func TestGitLab_GetPRDetails(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/merge_requests/8/diffs"):
			_, _ = w.Write([]byte(`[
			  {"old_path":"a.go","new_path":"a.go","diff":"@@ -1 +1 @@\n-old\n+new\n",
			   "new_file":false,"deleted_file":false,"renamed_file":false}
			]`))
		case strings.HasSuffix(r.URL.Path, "/merge_requests/8"):
			_, _ = w.Write([]byte(`{"iid":8,"web_url":"https://gitlab.com/g/p/-/merge_requests/8",
			  "title":"T","description":"d","state":"opened","source_branch":"feat/y",
			  "author":{"username":"carol"}}`))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer ts.Close()
	g := &GitLab{Token: "TOK", APIURL: ts.URL, HTTP: ts.Client()}
	pr, diff, err := g.GetPRDetails(context.Background(), "g/p", 8)
	if err != nil {
		t.Fatalf("GetPRDetails: %v", err)
	}
	if pr.Number != 8 || pr.Repo != "g/p" || pr.Branch != "feat/y" {
		t.Errorf("PR: %+v", pr)
	}
	if !strings.Contains(diff, "diff --git a/a.go b/a.go") || !strings.Contains(diff, "+new") {
		t.Errorf("reconstructed diff:\n%s", diff)
	}
}

func TestGitLab_Notifications(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/todos") {
			t.Errorf("path: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`[
		  {"id":101,"action_name":"review_requested","target_url":"https://gl/mr/1",
		   "target":{"title":"Review MR"},"project":{"path_with_namespace":"g/p"},
		   "created_at":"2024-05-01T00:00:00Z"},
		  {"id":102,"action_name":"directly_addressed","target_url":"https://gl/i/2",
		   "target":{"title":"Ping"},"project":{"path_with_namespace":"g/p"},
		   "created_at":"2024-05-02T00:00:00Z"},
		  {"id":103,"action_name":"build_failed","target_url":"x",
		   "target":{"title":"ignored"},"project":{"path_with_namespace":"g/p"},
		   "created_at":"2024-05-03T00:00:00Z"}
		]`))
	}))
	defer ts.Close()
	g := &GitLab{Token: "TOK", APIURL: ts.URL, HTTP: ts.Client()}
	notifs, err := g.Notifications(context.Background())
	if err != nil {
		t.Fatalf("Notifications: %v", err)
	}
	if len(notifs) != 2 {
		t.Fatalf("want 2 actionable to-dos (build_failed filtered out), got %d", len(notifs))
	}
	if notifs[0].ID != "101" || notifs[0].Kind != "review_requested" {
		t.Errorf("first to-do: %+v", notifs[0])
	}
	if notifs[1].Kind != "mention" {
		t.Errorf("directly_addressed should map to 'mention', got %q", notifs[1].Kind)
	}
}

func TestBitbucket_ReviewRequestedPRs(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/user"):
			_, _ = w.Write([]byte(`{"uuid":"{abc-123}"}`))
		case strings.Contains(r.URL.Path, "/pullrequests"):
			if q := r.URL.Query().Get("q"); !strings.Contains(q, "reviewers.uuid") {
				t.Errorf("query missing reviewers.uuid filter: %q", q)
			}
			_, _ = w.Write([]byte(`{"values":[
			  {"id":4,"title":"Review me","description":"d","state":"OPEN",
			   "links":{"html":{"href":"https://bb/pr/4"}},
			   "author":{"display_name":"Dana"},"source":{"branch":{"name":"feat/z"}}}
			]}`))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer ts.Close()
	t.Setenv("GOON_REVIEW_REPOS", "team/repo")
	b := &Bitbucket{Token: "TOK", APIURL: ts.URL, HTTP: ts.Client()}
	prs, err := b.ReviewRequestedPRs(context.Background())
	if err != nil {
		t.Fatalf("ReviewRequestedPRs: %v", err)
	}
	if len(prs) != 1 || prs[0].Number != 4 || prs[0].Repo != "team/repo" {
		t.Fatalf("PRs: %+v", prs)
	}
	if prs[0].Author != "Dana" {
		t.Errorf("author: %q", prs[0].Author)
	}
}
