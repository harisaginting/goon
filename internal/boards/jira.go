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

// Search runs an arbitrary JQL query and returns matching tickets.
// Used by the chat agent to answer live questions ("show me tickets
// assigned to bob in project ENG that are not done") without needing
// the daemon's cached snapshot to already contain them.
//
// Limits: capped at jiraPageSize per page (50); we deliberately do not
// auto-paginate (same rationale as List). If the user wants more,
// they should narrow the JQL.
//
// limit==0 means "default" (jiraPageSize). limit values above
// jiraPageSize are clamped down to jiraPageSize.
func (j *Jira) Search(ctx context.Context, jql string, limit int) ([]Ticket, error) {
	jql = strings.TrimSpace(jql)
	if jql == "" {
		return nil, fmt.Errorf("jira search: empty JQL")
	}
	if limit <= 0 || limit > jiraPageSize {
		limit = jiraPageSize
	}
	q := url.Values{}
	q.Set("jql", jql)
	q.Set("fields", "summary,description,status,labels,project,assignee,updated")
	q.Set("maxResults", fmt.Sprintf("%d", limit))
	u := j.BaseURL + "/rest/api/3/search/jql?" + q.Encode()
	body, err := j.do(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("jira search: %w", err)
	}
	var sr jiraSearchResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		return nil, fmt.Errorf("jira search decode: %w", err)
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

// jiraTransition is one workflow transition currently available for an
// issue.
type jiraTransition struct {
	ID     string // transition id, used in the POST
	Name   string // the transition's own name ("Start Progress")
	ToName string // the status it moves the issue TO ("In Progress")
}

// fetchTransitions GETs the workflow transitions currently available
// for an issue. The set is workflow-defined and depends on the issue's
// current status.
func (j *Jira) fetchTransitions(ctx context.Context, id string) ([]jiraTransition, error) {
	u := j.BaseURL + "/rest/api/3/issue/" + url.PathEscape(id) + "/transitions"
	body, err := j.do(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("jira list transitions %s: %w", id, err)
	}
	var resp struct {
		Transitions []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			To   struct {
				Name string `json:"name"`
			} `json:"to"`
		} `json:"transitions"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("jira transitions decode: %w", err)
	}
	out := make([]jiraTransition, 0, len(resp.Transitions))
	for _, t := range resp.Transitions {
		out = append(out, jiraTransition{ID: t.ID, Name: t.Name, ToName: t.To.Name})
	}
	return out, nil
}

// applyTransition POSTs the chosen transition id to move the issue.
func (j *Jira) applyTransition(ctx context.Context, id, transitionID string) error {
	u := j.BaseURL + "/rest/api/3/issue/" + url.PathEscape(id) + "/transitions"
	buf, _ := json.Marshal(map[string]any{
		"transition": map[string]any{"id": transitionID},
	})
	_, err := j.do(ctx, http.MethodPost, u, bytes.NewReader(buf))
	return err
}

// Transition moves a ticket to a goon-canonical Status — used by the
// daemon's workflow, which thinks in the five-value lifecycle. It picks
// the transition whose target state (or own name) maps to s via
// MapStatus. The chat agent uses the richer TransitionByName instead,
// which matches the board's real status names.
func (j *Jira) Transition(ctx context.Context, id string, s Status) error {
	trs, err := j.fetchTransitions(ctx, id)
	if err != nil {
		return err
	}
	if len(trs) == 0 {
		return fmt.Errorf("jira: no transitions available for %s", id)
	}
	pick, found := jiraTransition{}, false
	for _, t := range trs {
		if MapStatus(t.ToName) == s {
			pick, found = t, true
			break
		}
	}
	if !found {
		for _, t := range trs {
			if MapStatus(t.Name) == s {
				pick, found = t, true
				break
			}
		}
	}
	if !found {
		var avail []string
		for _, t := range trs {
			avail = append(avail, fmt.Sprintf("%q → %q", t.Name, t.ToName))
		}
		return fmt.Errorf("jira: no transition on %s maps to status %q (available: %s)",
			id, s, strings.Join(avail, ", "))
	}
	if err := j.applyTransition(ctx, id, pick.ID); err != nil {
		return fmt.Errorf("jira transition %s via %q: %w", id, pick.Name, err)
	}
	return nil
}

// ListTransitions implements TransitionResolver — the real status names
// this issue can move to right now, exactly as Jira names them.
func (j *Jira) ListTransitions(ctx context.Context, id string) ([]string, error) {
	trs, err := j.fetchTransitions(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(trs))
	for _, t := range trs {
		out = append(out, t.ToName)
	}
	return out, nil
}

// TransitionByName implements TransitionResolver — moves the issue to
// the status whose name best matches `name`, against the project's
// actual workflow. No MapStatus bucketing, so "Ready to Test" resolves
// to the real "Ready to Test" status instead of collapsing to "open".
// Returns the real target status name that was applied.
func (j *Jira) TransitionByName(ctx context.Context, id, name string) (string, error) {
	trs, err := j.fetchTransitions(ctx, id)
	if err != nil {
		return "", err
	}
	if len(trs) == 0 {
		return "", fmt.Errorf("jira: %s has no available transitions (it may be in a terminal status)", id)
	}
	pick, ok := matchTransition(trs, name)
	if !ok {
		avail := make([]string, 0, len(trs))
		for _, t := range trs {
			avail = append(avail, t.ToName)
		}
		return "", fmt.Errorf("no status on %s matches %q — available statuses: %s",
			id, name, strings.Join(avail, ", "))
	}
	if err := j.applyTransition(ctx, id, pick.ID); err != nil {
		return "", fmt.Errorf("jira transition %s via %q: %w", id, pick.Name, err)
	}
	return pick.ToName, nil
}

// matchTransition picks the transition that best matches a user-typed
// status name. Order: exact match on the target status name, exact on
// the transition name, then containment either way — all after
// normalising to lowercase alphanumerics, so "ready to test",
// "Ready To Test" and "ready-to-test" are equivalent.
func matchTransition(trs []jiraTransition, want string) (jiraTransition, bool) {
	w := normStatus(want)
	if w == "" {
		return jiraTransition{}, false
	}
	for _, t := range trs {
		if normStatus(t.ToName) == w {
			return t, true
		}
	}
	for _, t := range trs {
		if normStatus(t.Name) == w {
			return t, true
		}
	}
	for _, t := range trs {
		n := normStatus(t.ToName)
		if n != "" && (strings.Contains(n, w) || strings.Contains(w, n)) {
			return t, true
		}
	}
	for _, t := range trs {
		n := normStatus(t.Name)
		if n != "" && (strings.Contains(n, w) || strings.Contains(w, n)) {
			return t, true
		}
	}
	return jiraTransition{}, false
}

// normStatus lowercases s and strips everything but [a-z0-9], so status
// names compare without caring about spaces, case or punctuation.
func normStatus(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Update edits a ticket's mutable fields. Only fields whose pointer
// is non-nil are sent — passing &"" explicitly clears a field; nil
// leaves it untouched. We send via PUT /rest/api/3/issue/{key} with
// the standard fields map. Description goes as ADF since the API
// rejects plain strings.
func (j *Jira) Update(ctx context.Context, id string, patch TicketPatch) error {
	if patch.Title == nil && patch.Description == nil && patch.Labels == nil {
		return nil // nothing to do
	}
	fields := map[string]any{}
	if patch.Title != nil {
		fields["summary"] = *patch.Title
	}
	if patch.Description != nil {
		// Wrap the plain string in a minimal ADF document — single
		// paragraph holding one text node. Matches what we do in
		// Comment so the round-trip stays consistent.
		fields["description"] = map[string]any{
			"type":    "doc",
			"version": 1,
			"content": []map[string]any{{
				"type": "paragraph",
				"content": []map[string]any{{
					"type": "text",
					"text": *patch.Description,
				}},
			}},
		}
	}
	if patch.Labels != nil {
		// Jira accepts a plain []string for labels — empty slice
		// clears them. patch.Labels==nil leaves them alone (handled
		// above by skipping this branch).
		fields["labels"] = patch.Labels
	}
	payload := map[string]any{"fields": fields}
	buf, _ := json.Marshal(payload)
	u := j.BaseURL + "/rest/api/3/issue/" + url.PathEscape(id)
	if _, err := j.do(ctx, http.MethodPut, u, bytes.NewReader(buf)); err != nil {
		return fmt.Errorf("jira update %s: %w", id, err)
	}
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
