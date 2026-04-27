package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConfluence_Search(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/rest/api/content/search") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"id":"42","title":"Roadmap","_links":{"webui":"/spaces/X/pages/42"}}]}`))
	}))
	defer ts.Close()

	c := &Confluence{
		BaseURL:  ts.URL,
		Email:    "u",
		APIToken: "t",
		HTTP:     ts.Client(),
	}
	res, err := c.Run(context.Background(), map[string]string{
		"op":    "search",
		"query": "roadmap",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res.Stdout, "Roadmap") || !strings.Contains(res.Stdout, "/spaces/X/pages/42") {
		t.Fatalf("bad output: %q", res.Stdout)
	}
}

func TestConfluence_GetPage(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/rest/api/content/42") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{
			"id":"42","title":"Roadmap","space":{"key":"ENG"},
			"body":{"storage":{"value":"<p>Hello <b>world</b></p>"}}
		}`))
	}))
	defer ts.Close()

	c := &Confluence{BaseURL: ts.URL, Email: "u", APIToken: "t", HTTP: ts.Client()}
	res, err := c.Run(context.Background(), map[string]string{
		"op":      "get_page",
		"page_id": "42",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res.Stdout, "# Roadmap") || !strings.Contains(res.Stdout, "Hello") || !strings.Contains(res.Stdout, "world") {
		t.Fatalf("bad page render: %q", res.Stdout)
	}
}

func TestConfluence_RequiresConfig(t *testing.T) {
	c := &Confluence{}
	_, err := c.Run(context.Background(), map[string]string{"op": "search", "query": "x"})
	if err == nil || !strings.Contains(err.Error(), "CONFLUENCE_BASE_URL") {
		t.Fatalf("expected config error, got %v", err)
	}
}

func TestConfluence_UnknownOp(t *testing.T) {
	c := &Confluence{BaseURL: "http://x", Email: "u", APIToken: "t"}
	_, err := c.Run(context.Background(), map[string]string{"op": "delete_everything"})
	if err == nil || !strings.Contains(err.Error(), "unknown op") {
		t.Fatalf("expected unknown-op error, got %v", err)
	}
}
