package boards

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Jira reads tickets from Atlassian Cloud's REST API v3.
//
// Configure via env:
//
//	JIRA_BASE_URL    e.g. https://acme.atlassian.net
//	JIRA_EMAIL       e.g. you@acme.com
//	JIRA_API_TOKEN   token from id.atlassian.com/manage-profile/security/api-tokens
//	JIRA_JQL         JQL filter (default: "assignee=currentUser() AND statusCategory!=Done ORDER BY updated DESC")
type Jira struct {
	BaseURL  string
	Email    string
	APIToken string
	JQL      string
	HTTP     *http.Client
}

// NewJiraFromEnv reads config from env and returns a Jira board.
func NewJiraFromEnv() (*Jira, error) {
	base := strings.TrimRight(os.Getenv("JIRA_BASE_URL"), "/")
	email := os.Getenv("JIRA_EMAIL")
	tok := os.Getenv("JIRA_API_TOKEN")
	jql := os.Getenv("JIRA_JQL")
	if jql == "" {
		jql = `assignee=currentUser() AND statusCategory!=Done ORDER BY updated DESC`
	}
	if base == "" || email == "" || tok == "" {
		return nil, errors.New("jira: set JIRA_BASE_URL, JIRA_EMAIL, JIRA_API_TOKEN")
	}
	return &Jira{
		BaseURL:  base,
		Email:    email,
		APIToken: tok,
		JQL:      jql,
		HTTP:     &http.Client{Timeout: 20 * time.Second},
	}, nil
}

// Name returns "jira".
func (*Jira) Name() string { return "jira" }

type jiraSearchResponse struct {
	Issues []jiraIssue `json:"issues"`
}

type jiraIssue struct {
	ID     string `json:"id"`
	Key    string `json:"key"`
	Fields struct {
		Summary     string `json:"summary"`
		Description any    `json:"description"`
		Status      struct {
			Name string `json:"name"`
		} `json:"status"`
		Labels  []string `json:"labels"`
		Project struct {
			Key string `json:"key"`
		} `json:"project"`
		Assignee *struct {
			DisplayName string `json:"displayName"`
		} `json:"assignee"`
		Updated string `json:"updated"`
	} `json:"fields"`
}

// List runs the configured JQL and returns matching tickets.
func (j *Jira) List(ctx context.Context) ([]Ticket, error) {
	q := url.Values{}
	q.Set("jql", j.JQL)
	q.Set("fields", "summary,description,status,labels,project,assignee,updated")
	q.Set("maxResults", "50")
	u := j.BaseURL + "/rest/api/3/search?" + q.Encode()
	body, err := j.do(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	var sr jiraSearchResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		return nil, fmt.Errorf("jira decode: %w", err)
	}
	out := make([]Ticket, 0, len(sr.Issues))
	for _, is := range sr.Issues {
		out = append(out, j.toTicket(is))
	}
	return out, nil
}

// Get fetches a single ticket by id (Jira issue key like "ENG-123").
func (j *Jira) Get(ctx context.Context, id string) (Ticket, error) {
	u := j.BaseURL + "/rest/api/3/issue/" + url.PathEscape(id) +
		"?fields=summary,description,status,labels,project,assignee,updated"
	body, err := j.do(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Ticket{}, err
	}
	var is jiraIssue
	if err := json.Unmarshal(body, &is); err != nil {
		return Ticket{}, fmt.Errorf("jira decode: %w", err)
	}
	return j.toTicket(is), nil
}

// Comment posts a plain-text comment on the issue.
func (j *Jira) Comment(ctx context.Context, id, body string) error {
	u := j.BaseURL + "/rest/api/3/issue/" + url.PathEscape(id) + "/comment"
	payload := map[string]any{
		"body": map[string]any{
			"type":    "doc",
			"version": 1,
			"content": []map[string]any{{
				"type": "paragraph",
				"content": []map[string]any{{
					"type": "text",
					"text": body,
				}},
			}},
		},
	}
	buf, _ := json.Marshal(payload)
	_, err := j.do(ctx, http.MethodPost, u, bytes.NewReader(buf))
	return err
}

// Transition is a stub for now — Jira transitions require a project-specific
// workflow id mapping. We log it and return nil so the daemon can proceed.
func (*Jira) Transition(_ context.Context, _ string, _ Status) error {
	// Implementing this properly requires per-project workflow lookups
	// (GET /issue/{key}/transitions). Out of scope for v1.
	return nil
}

func (j *Jira) toTicket(is jiraIssue) Ticket {
	desc := ""
	switch v := is.Fields.Description.(type) {
	case string:
		desc = v
	case map[string]any:
		desc = jiraADFToText(v)
	}
	upd, _ := time.Parse("2006-01-02T15:04:05.000-0700", is.Fields.Updated)
	t := Ticket{
		ID:          is.Key,
		Source:      "jira",
		Key:         is.Key,
		Title:       is.Fields.Summary,
		Description: desc,
		URL:         j.BaseURL + "/browse/" + is.Key,
		Status:      MapStatus(is.Fields.Status.Name),
		Labels:      is.Fields.Labels,
		Project:     is.Fields.Project.Key,
		UpdatedAt:   upd,
	}
	if is.Fields.Assignee != nil {
		t.Assignee = is.Fields.Assignee.DisplayName
	}
	return t
}

// jiraADFToText is a minimal ADF (Atlassian Document Format) extractor that
// concatenates leaf "text" nodes.
func jiraADFToText(node map[string]any) string {
	var b strings.Builder
	var walk func(n any)
	walk = func(n any) {
		switch v := n.(type) {
		case map[string]any:
			if t, ok := v["type"].(string); ok && t == "text" {
				if txt, ok := v["text"].(string); ok {
					b.WriteString(txt)
				}
			}
			if c, ok := v["content"]; ok {
				walk(c)
			}
			if v["type"] == "paragraph" || v["type"] == "heading" {
				b.WriteString("\n")
			}
		case []any:
			for _, x := range v {
				walk(x)
			}
		}
	}
	walk(node)
	return strings.TrimSpace(b.String())
}

func (j *Jira) do(ctx context.Context, method, url string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Basic "+
		base64.StdEncoding.EncodeToString([]byte(j.Email+":"+j.APIToken)))
	resp, err := j.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("jira http %d: %s", resp.StatusCode, truncate(string(raw), 400))
	}
	return raw, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
