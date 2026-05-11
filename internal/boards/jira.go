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

	"github.com/harisaginting/goon/internal/atlassian"
	"github.com/harisaginting/goon/internal/logx"
	"github.com/harisaginting/goon/internal/util"
)

// Jira reads tickets from Atlassian Cloud's REST API v3.
//
// Configure via env. Either set the per-product vars (JIRA_*) OR the shared
// Atlassian vars (ATLASSIAN_*) — the per-product vars win when both are set.
//
//	JIRA_BASE_URL or ATLASSIAN_BASE_URL    e.g. https://acme.atlassian.net
//	JIRA_EMAIL    or ATLASSIAN_EMAIL       e.g. you@acme.com
//	JIRA_API_TOKEN or ATLASSIAN_API_TOKEN  id.atlassian.com/manage-profile/security/api-tokens
//	JIRA_JQL                                JQL filter (defaults to assigned + open)
type Jira struct {
	BaseURL  string
	Email    string
	APIToken string
	JQL      string
	HTTP     *http.Client
}

// NewJiraFromEnv reads config from env and returns a Jira board.
func NewJiraFromEnv() (*Jira, error) {
	c := atlassian.Jira()
	jql := os.Getenv("JIRA_JQL")
	if jql == "" {
		jql = `assignee=currentUser() AND statusCategory!=Done ORDER BY updated DESC`
	}
	if !c.Filled() {
		return nil, errors.New("jira: set JIRA_BASE_URL/JIRA_EMAIL/JIRA_API_TOKEN (or shared ATLASSIAN_BASE_URL/ATLASSIAN_EMAIL/ATLASSIAN_API_TOKEN)")
	}
	return &Jira{
		BaseURL:  c.BaseURL,
		Email:    c.Email,
		APIToken: c.APIToken,
		JQL:      jql,
		HTTP:     logx.InstrumentClient("jira", &http.Client{Timeout: 20 * time.Second}),
	}, nil
}

// Name returns "jira".
func (*Jira) Name() string { return "jira" }

// jiraSearchResponse matches the shape of /rest/api/3/search/jql.
//
// The new endpoint replaced /rest/api/3/search (which Atlassian removed,
// returns 410 GONE — see CHANGE-2046). Pagination is now cursor-based:
// `nextPageToken` carries forward to fetch the next page; `isLast` tells you
// when there are no more results. The legacy `total`/`startAt`/`maxResults`
// fields are gone.
type jiraSearchResponse struct {
	Issues        []jiraIssue `json:"issues"`
	IsLast        bool        `json:"isLast"`
	NextPageToken string      `json:"nextPageToken,omitempty"`
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

// jiraPageSize is the per-page cap. The /rest/api/3/search/jql endpoint
// allows up to 5000. We pick 50 because:
//   - The daemon picks one ticket per poll TICK, but every ticket in
//     the page is recorded via memory.SeenTicket — that's what the
//     user-facing inventory (/tickets, web Tickets tab, chat context)
//     surfaces. With pageSize=10, a user with 30 open tickets only
//     ever sees the first 10, and the chat answer to "list my open
//     tickets" was truncated. 50 covers most real teams in one page.
//   - We deliberately do NOT auto-paginate further. If a user has
//     >50 matches, they should tighten JIRA_JQL — fetching every page
//     on every 5-minute tick would burn API quota.
const jiraPageSize = 50

// List runs the configured JQL and returns matching tickets. Caps at
// jiraPageSize results per page; logs a warning to stderr if the result was
// truncated (more pages exist) so users notice their backlog overflows the
// poll window.
//
// Note: we deliberately do NOT auto-paginate. The daemon polls every 5
// minutes and only picks one ticket per poll, so fetching every page on
// every tick wastes API quota. Users who want broader coverage should
// tighten JIRA_JQL.
func (j *Jira) List(ctx context.Context) ([]Ticket, error) {
	q := url.Values{}
	q.Set("jql", j.JQL)
	q.Set("fields", "summary,description,status,labels,project,assignee,updated")
	q.Set("maxResults", fmt.Sprintf("%d", jiraPageSize))
	// /rest/api/3/search was removed by Atlassian (returns 410 GONE).
	// /rest/api/3/search/jql is the replacement; same JQL, same fields,
	// cursor-based pagination via isLast / nextPageToken.
	u := j.BaseURL + "/rest/api/3/search/jql?" + q.Encode()
	body, err := j.do(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	var sr jiraSearchResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		return nil, fmt.Errorf("jira decode: %w", err)
	}
	// Truncated set? The new API signals this via isLast=false (or a
	// non-empty nextPageToken). Either is enough to warn.
	if !sr.IsLast || sr.NextPageToken != "" {
		fmt.Fprintf(os.Stderr,
			"jira: warning: matched more than %d issues — older tickets may never be picked. "+
				"Tighten JIRA_JQL.\n",
			len(sr.Issues))
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
		return nil, fmt.Errorf("jira http %d: %s", resp.StatusCode, util.Truncate(string(raw), 400))
	}
	return raw, nil
}
