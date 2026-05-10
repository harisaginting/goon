package boards

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/harisaginting/goon/internal/logx"
	"github.com/harisaginting/goon/internal/util"
)

// GitHub uses GitHub Issues as the ticket board.
//
// Configure via env:
//
//	GITHUB_TOKEN     PAT or fine-grained token with `issues:read`, `issues:write`
//	GITHUB_REPOS     comma-separated "owner/repo" list to poll
//	GITHUB_LABEL     optional label filter (default: "")
//	GITHUB_ASSIGNEE  optional assignee filter (default: "@me" — requires `read:user`)
//	GITHUB_API_URL   override for GitHub Enterprise (default: https://api.github.com)
type GitHub struct {
	Token    string
	Repos    []string
	Label    string
	Assignee string
	APIURL   string
	HTTP     *http.Client
}

// NewGitHubFromEnv constructs a GitHub board from environment variables.
func NewGitHubFromEnv() (*GitHub, error) {
	tok := os.Getenv("GITHUB_TOKEN")
	if tok == "" {
		return nil, errors.New("github board: set GITHUB_TOKEN")
	}
	rawRepos := strings.TrimSpace(os.Getenv("GITHUB_REPOS"))
	if rawRepos == "" {
		return nil, errors.New("github board: set GITHUB_REPOS=owner/repo[,owner/repo...]")
	}
	repos := []string{}
	for _, r := range strings.Split(rawRepos, ",") {
		if r = strings.TrimSpace(r); r != "" {
			repos = append(repos, r)
		}
	}
	api := os.Getenv("GITHUB_API_URL")
	if api == "" {
		api = "https://api.github.com"
	}
	return &GitHub{
		Token:    tok,
		Repos:    repos,
		Label:    os.Getenv("GITHUB_LABEL"),
		Assignee: os.Getenv("GITHUB_ASSIGNEE"),
		APIURL:   strings.TrimRight(api, "/"),
		HTTP:     logx.InstrumentClient("github-board", &http.Client{Timeout: 20 * time.Second}),
	}, nil
}

// Name returns "github".
func (*GitHub) Name() string { return "github" }

type ghIssue struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Labels  []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Assignees []struct {
		Login string `json:"login"`
	} `json:"assignees"`
	UpdatedAt     time.Time `json:"updated_at"`
	PullRequest   any       `json:"pull_request,omitempty"` // present means this is a PR, skip
	RepositoryURL string    `json:"repository_url,omitempty"`
}

// ghPageSize is the per-repo page size; bumped to GitHub's max-100 from
// the previous 30. Pagination via Link header is not implemented yet —
// we log a warning when a repo returns exactly the page size, since
// that's the signal that more results exist.
const ghPageSize = 100

// List queries each configured repo for open issues matching the filters.
// Caps each repo at ghPageSize results; logs a warning when truncated so
// users notice their backlog is bigger than one page.
func (g *GitHub) List(ctx context.Context) ([]Ticket, error) {
	var all []Ticket
	for _, repo := range g.Repos {
		q := url.Values{}
		q.Set("state", "open")
		q.Set("per_page", fmt.Sprintf("%d", ghPageSize))
		if g.Label != "" {
			q.Set("labels", g.Label)
		}
		if g.Assignee != "" {
			q.Set("assignee", g.Assignee)
		}
		u := fmt.Sprintf("%s/repos/%s/issues?%s", g.APIURL, repo, q.Encode())
		body, err := g.do(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, fmt.Errorf("github list %s: %w", repo, err)
		}
		var raw []ghIssue
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, fmt.Errorf("github decode %s: %w", repo, err)
		}
		for _, is := range raw {
			if is.PullRequest != nil {
				continue // /issues includes PRs; skip them
			}
			all = append(all, g.toTicket(repo, is))
		}
		if len(raw) >= ghPageSize {
			fmt.Fprintf(os.Stderr,
				"github: warning: %s returned %d issues (page size %d) — more pages exist. "+
					"Tighten GITHUB_LABEL or GITHUB_ASSIGNEE.\n",
				repo, len(raw), ghPageSize)
		}
	}
	return all, nil
}

// Get fetches a single ticket. ID format: "owner/repo#NN".
func (g *GitHub) Get(ctx context.Context, id string) (Ticket, error) {
	repo, num, ok := splitGHID(id)
	if !ok {
		return Ticket{}, fmt.Errorf("github get: bad id %q (want owner/repo#N)", id)
	}
	u := fmt.Sprintf("%s/repos/%s/issues/%d", g.APIURL, repo, num)
	body, err := g.do(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Ticket{}, err
	}
	var is ghIssue
	if err := json.Unmarshal(body, &is); err != nil {
		return Ticket{}, fmt.Errorf("github decode: %w", err)
	}
	return g.toTicket(repo, is), nil
}

// Comment posts a comment on the issue.
func (g *GitHub) Comment(ctx context.Context, id, body string) error {
	repo, num, ok := splitGHID(id)
	if !ok {
		return fmt.Errorf("github comment: bad id %q", id)
	}
	u := fmt.Sprintf("%s/repos/%s/issues/%d/comments", g.APIURL, repo, num)
	payload, _ := json.Marshal(map[string]string{"body": body})
	_, err := g.do(ctx, http.MethodPost, u, strings.NewReader(string(payload)))
	return err
}

// Transition uses GitHub's state field. Done → close; Open/InProgress → open.
func (g *GitHub) Transition(ctx context.Context, id string, s Status) error {
	repo, num, ok := splitGHID(id)
	if !ok {
		return fmt.Errorf("github transition: bad id %q", id)
	}
	state := "open"
	if s == StatusDone {
		state = "closed"
	}
	u := fmt.Sprintf("%s/repos/%s/issues/%d", g.APIURL, repo, num)
	payload, _ := json.Marshal(map[string]string{"state": state})
	_, err := g.do(ctx, http.MethodPatch, u, strings.NewReader(string(payload)))
	return err
}

func (g *GitHub) toTicket(repo string, is ghIssue) Ticket {
	labels := make([]string, 0, len(is.Labels))
	for _, l := range is.Labels {
		labels = append(labels, l.Name)
	}
	assignee := ""
	if len(is.Assignees) > 0 {
		assignee = is.Assignees[0].Login
	}
	st := StatusOpen
	if is.State == "closed" {
		st = StatusDone
	}
	return Ticket{
		ID:          fmt.Sprintf("%s#%d", repo, is.Number),
		Source:      "github",
		Key:         fmt.Sprintf("#%d", is.Number),
		Title:       is.Title,
		Description: is.Body,
		URL:         is.HTMLURL,
		Status:      st,
		Labels:      labels,
		Assignee:    assignee,
		Project:     repo,
		UpdatedAt:   is.UpdatedAt,
	}
}

func splitGHID(id string) (repo string, num int, ok bool) {
	hash := strings.LastIndexByte(id, '#')
	if hash < 0 {
		return "", 0, false
	}
	repo = id[:hash]
	if _, err := fmt.Sscanf(id[hash+1:], "%d", &num); err != nil {
		return "", 0, false
	}
	return repo, num, true
}

func (g *GitHub) do(ctx context.Context, method, url string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+g.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("github http %d: %s", resp.StatusCode, util.Truncate(string(raw), 400))
	}
	return raw, nil
}
