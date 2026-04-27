package githost

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGitHub_OpenPR(t *testing.T) {
	var got ghPRRequest
	var labelsHit bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/labels") {
			labelsHit = true
			_, _ = w.Write([]byte(`[]`))
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/pulls") {
			t.Errorf("path: %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"number":42,"html_url":"https://gh/o/r/pull/42","title":"goon: t","head":{"ref":"goon/eng-1"}}`))
	}))
	defer ts.Close()

	g := &GitHub{Token: "T", APIURL: ts.URL, HTTP: ts.Client()}
	pr, err := g.OpenPR(context.Background(), CreateOptions{
		Repo: "o/r", Title: "goon: t", Body: "did stuff",
		Head: "goon/eng-1", Base: "main", Labels: []string{"goon", "auto"},
	})
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if pr.Number != 42 || pr.URL != "https://gh/o/r/pull/42" || pr.Branch != "goon/eng-1" {
		t.Fatalf("bad PR: %+v", pr)
	}
	if got.Title != "goon: t" || got.Head != "goon/eng-1" || got.Base != "main" {
		t.Fatalf("bad payload: %+v", got)
	}
	if !labelsHit {
		t.Errorf("expected labels endpoint to be called")
	}
}

func TestGitHub_OpenPR_DefaultBase(t *testing.T) {
	var got ghPRRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"number":1,"html_url":"x","title":"t","head":{"ref":"f"}}`))
	}))
	defer ts.Close()
	g := &GitHub{Token: "T", APIURL: ts.URL, HTTP: ts.Client()}
	_, err := g.OpenPR(context.Background(), CreateOptions{Repo: "o/r", Title: "t", Head: "f"})
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if got.Base != "main" {
		t.Fatalf("default base should be 'main', got %q", got.Base)
	}
}

func TestGitLab_OpenMR(t *testing.T) {
	var got glMRRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/merge_requests") {
			t.Errorf("path: %s", r.URL.Path)
		}
		if r.Header.Get("PRIVATE-TOKEN") != "TOK" {
			t.Errorf("auth: %s", r.Header.Get("PRIVATE-TOKEN"))
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"iid":7,"web_url":"https://gl/g/p/-/mr/7","title":"goon: t","source_branch":"goon/eng-1"}`))
	}))
	defer ts.Close()

	g := &GitLab{Token: "TOK", APIURL: ts.URL, HTTP: ts.Client()}
	pr, err := g.OpenPR(context.Background(), CreateOptions{
		Repo: "group/proj", Title: "goon: t", Body: "did stuff",
		Head: "goon/eng-1", Base: "main", Labels: []string{"goon", "auto"},
	})
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if pr.Number != 7 || pr.URL != "https://gl/g/p/-/mr/7" {
		t.Fatalf("bad PR: %+v", pr)
	}
	if got.SourceBranch != "goon/eng-1" || got.Labels != "goon,auto" {
		t.Fatalf("bad payload: %+v", got)
	}
}

func TestNewFromEnv(t *testing.T) {
	t.Setenv("GOON_GIT_HOST", "")
	if _, err := NewFromEnv(); err != ErrNoHost {
		t.Errorf("expected ErrNoHost, got %v", err)
	}
	t.Setenv("GOON_GIT_HOST", "mock")
	h, err := NewFromEnv()
	if err != nil || h.Name() != "mock" {
		t.Errorf("mock: %v %v", err, h)
	}
}

func TestBitbucket_OpenPR_TokenAuth(t *testing.T) {
	var got bbPRRequest
	var auth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/repositories/myteam/myrepo/pullrequests") {
			t.Errorf("path: %s", r.URL.Path)
		}
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{
		  "id": 99,
		  "title": "goon: t",
		  "links": {"html": {"href": "https://bitbucket.org/myteam/myrepo/pull-requests/99"}},
		  "source": {"branch": {"name": "goon/eng-1"}}
		}`))
	}))
	defer ts.Close()

	b := &Bitbucket{Token: "TOK", APIURL: ts.URL, HTTP: ts.Client()}
	pr, err := b.OpenPR(context.Background(), CreateOptions{
		Repo:   "myteam/myrepo",
		Title:  "goon: t",
		Body:   "did stuff",
		Head:   "goon/eng-1",
		Base:   "main",
		Labels: []string{"goon", "auto"},
	})
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if pr.Number != 99 || pr.URL != "https://bitbucket.org/myteam/myrepo/pull-requests/99" {
		t.Fatalf("bad PR: %+v", pr)
	}
	if pr.Branch != "goon/eng-1" {
		t.Errorf("branch: %q", pr.Branch)
	}
	if got.Source.Branch.Name != "goon/eng-1" || got.Destination.Branch.Name != "main" {
		t.Errorf("payload branches: %+v", got)
	}
	if got.Title != "goon: t" {
		t.Errorf("title: %q", got.Title)
	}
	if !strings.Contains(got.Description, "Labels: goon, auto") {
		t.Errorf("labels not in description: %q", got.Description)
	}
	if auth != "Bearer TOK" {
		t.Errorf("token auth: got %q", auth)
	}
}

func TestBitbucket_OpenPR_BasicAuth(t *testing.T) {
	var auth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"id":1,"title":"t","links":{"html":{"href":"x"}},"source":{"branch":{"name":"f"}}}`))
	}))
	defer ts.Close()
	b := &Bitbucket{Username: "u@x", AppPassword: "appkey", APIURL: ts.URL, HTTP: ts.Client()}
	_, err := b.OpenPR(context.Background(), CreateOptions{Repo: "w/r", Title: "t", Head: "f"})
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if !strings.HasPrefix(auth, "Basic ") {
		t.Fatalf("expected basic auth, got %q", auth)
	}
}

func TestBitbucket_OpenPR_DefaultBase(t *testing.T) {
	var got bbPRRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"id":1,"title":"t","links":{"html":{"href":"x"}},"source":{"branch":{"name":"f"}}}`))
	}))
	defer ts.Close()
	b := &Bitbucket{Token: "TOK", APIURL: ts.URL, HTTP: ts.Client()}
	_, err := b.OpenPR(context.Background(), CreateOptions{Repo: "w/r", Title: "t", Head: "f"})
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if got.Destination.Branch.Name != "main" {
		t.Errorf("default base: %q", got.Destination.Branch.Name)
	}
}

func TestBitbucket_HTTPError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"type":"error","error":{"message":"forbidden"}}`))
	}))
	defer ts.Close()
	b := &Bitbucket{Token: "TOK", APIURL: ts.URL, HTTP: ts.Client()}
	_, err := b.OpenPR(context.Background(), CreateOptions{Repo: "w/r", Title: "t", Head: "f"})
	if err == nil || !strings.Contains(err.Error(), "bitbucket http 403") {
		t.Fatalf("expected 403 error, got %v", err)
	}
}

func TestBitbucket_NewFromEnv_RequiresAuth(t *testing.T) {
	t.Setenv("BITBUCKET_TOKEN", "")
	t.Setenv("BITBUCKET_USERNAME", "")
	t.Setenv("BITBUCKET_APP_PASSWORD", "")
	if _, err := NewBitbucketFromEnv(); err == nil {
		t.Fatal("expected error when no auth provided")
	}
	t.Setenv("BITBUCKET_TOKEN", "tok")
	if _, err := NewBitbucketFromEnv(); err != nil {
		t.Fatalf("token-only should work: %v", err)
	}
}

func TestMock_OpenPR(t *testing.T) {
	m := NewMock()
	pr, err := m.OpenPR(context.Background(), CreateOptions{
		Repo: "o/r", Title: "hello", Head: "f",
	})
	if err != nil {
		t.Fatal(err)
	}
	if pr.Title != "hello" || pr.Branch != "f" {
		t.Errorf("mock PR: %+v", pr)
	}
	if len(m.Opened) != 1 {
		t.Errorf("expected 1 recorded call")
	}
}
