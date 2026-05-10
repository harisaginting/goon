package githost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/harisaginting/goon/internal/logx"
	"github.com/harisaginting/goon/internal/util"
)

// GitHub is the GitHub.com / GitHub Enterprise PR creator.
type GitHub struct {
	Token  string
	APIURL string
	HTTP   *http.Client
}

// NewGitHubFromEnv reads GITHUB_TOKEN + optional GITHUB_API_URL.
func NewGitHubFromEnv() (*GitHub, error) {
	tok := os.Getenv("GITHUB_TOKEN")
	if tok == "" {
		return nil, errors.New("github host: set GITHUB_TOKEN")
	}
	api := os.Getenv("GITHUB_API_URL")
	if api == "" {
		api = "https://api.github.com"
	}
	return &GitHub{
		Token:  tok,
		APIURL: strings.TrimRight(api, "/"),
		HTTP:   logx.InstrumentClient("github-host", &http.Client{Timeout: 30 * time.Second}),
	}, nil
}

// Name returns "github".
func (*GitHub) Name() string { return "github" }

type ghPRRequest struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Head  string `json:"head"`
	Base  string `json:"base"`
	Draft bool   `json:"draft,omitempty"`
}

type ghPRResponse struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	Title   string `json:"title"`
	Head    struct {
		Ref string `json:"ref"`
	} `json:"head"`
}

// OpenPR creates a pull request and (best-effort) attaches labels.
func (g *GitHub) OpenPR(ctx context.Context, o CreateOptions) (PR, error) {
	if o.Base == "" {
		o.Base = "main"
	}
	body, _ := json.Marshal(ghPRRequest{
		Title: o.Title, Body: o.Body, Head: o.Head, Base: o.Base, Draft: o.Draft,
	})
	url := fmt.Sprintf("%s/repos/%s/pulls", g.APIURL, o.Repo)
	respBody, err := g.do(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return PR{}, err
	}
	var pr ghPRResponse
	if err := json.Unmarshal(respBody, &pr); err != nil {
		return PR{}, fmt.Errorf("github decode: %w", err)
	}

	if len(o.Labels) > 0 {
		labelsURL := fmt.Sprintf("%s/repos/%s/issues/%d/labels", g.APIURL, o.Repo, pr.Number)
		lp, _ := json.Marshal(map[string]any{"labels": o.Labels})
		_, _ = g.do(ctx, http.MethodPost, labelsURL, bytes.NewReader(lp)) // best-effort
	}

	return PR{Number: pr.Number, URL: pr.HTMLURL, Title: pr.Title, Branch: pr.Head.Ref}, nil
}

// --- PRReviewer implementation ---------------------------------------------

// ListPRs returns open PRs across the supplied repos. When repos is empty,
// falls back to GOON_REVIEW_REPOS (comma-separated "owner/repo,owner/repo").
func (g *GitHub) ListPRs(ctx context.Context, repos []string) ([]PR, error) {
	if len(repos) == 0 {
		for _, r := range strings.Split(os.Getenv("GOON_REVIEW_REPOS"), ",") {
			r = strings.TrimSpace(r)
			if r != "" {
				repos = append(repos, r)
			}
		}
	}
	out := []PR{}
	for _, repo := range repos {
		url := fmt.Sprintf("%s/repos/%s/pulls?state=open&per_page=50", g.APIURL, repo)
		raw, err := g.do(ctx, http.MethodGet, url, nil)
		if err != nil {
			return out, fmt.Errorf("github list %s: %w", repo, err)
		}
		var arr []ghPRListItem
		if err := json.Unmarshal(raw, &arr); err != nil {
			return out, fmt.Errorf("github list %s decode: %w", repo, err)
		}
		for _, it := range arr {
			out = append(out, PR{
				Number: it.Number,
				URL:    it.HTMLURL,
				Title:  it.Title,
				Branch: it.Head.Ref,
				Author: it.User.Login,
				State:  it.State,
				Body:   it.Body,
				Repo:   repo,
			})
		}
	}
	return out, nil
}

// GetPRDetails returns the PR + the unified diff body (via Accept:
// application/vnd.github.v3.diff).
func (g *GitHub) GetPRDetails(ctx context.Context, repo string, number int) (PR, string, error) {
	prURL := fmt.Sprintf("%s/repos/%s/pulls/%d", g.APIURL, repo, number)
	raw, err := g.do(ctx, http.MethodGet, prURL, nil)
	if err != nil {
		return PR{}, "", fmt.Errorf("github get pr: %w", err)
	}
	var meta ghPRListItem
	if err := json.Unmarshal(raw, &meta); err != nil {
		return PR{}, "", fmt.Errorf("github decode pr: %w", err)
	}
	pr := PR{
		Number: meta.Number, URL: meta.HTMLURL, Title: meta.Title,
		Branch: meta.Head.Ref, Author: meta.User.Login, State: meta.State,
		Body: meta.Body, Repo: repo,
	}
	// Fetch the diff via the special Accept header.
	diff, err := g.fetchDiff(ctx, prURL)
	if err != nil {
		return pr, "", err
	}
	return pr, diff, nil
}

// CommentPR posts an issue-style comment on the PR (which is also an issue
// in GitHub's data model).
func (g *GitHub) CommentPR(ctx context.Context, repo string, number int, body string) error {
	if strings.TrimSpace(body) == "" {
		return errors.New("github comment: body is empty")
	}
	url := fmt.Sprintf("%s/repos/%s/issues/%d/comments", g.APIURL, repo, number)
	payload, _ := json.Marshal(map[string]string{"body": body})
	_, err := g.do(ctx, http.MethodPost, url, bytes.NewReader(payload))
	return err
}

// ApprovePR submits an APPROVE review.
func (g *GitHub) ApprovePR(ctx context.Context, repo string, number int, body string) error {
	return g.submitReview(ctx, repo, number, "APPROVE", body)
}

// RequestChangesPR submits a REQUEST_CHANGES review (used for /decline).
func (g *GitHub) RequestChangesPR(ctx context.Context, repo string, number int, body string) error {
	return g.submitReview(ctx, repo, number, "REQUEST_CHANGES", body)
}

func (g *GitHub) submitReview(ctx context.Context, repo string, number int, event, body string) error {
	url := fmt.Sprintf("%s/repos/%s/pulls/%d/reviews", g.APIURL, repo, number)
	payload, _ := json.Marshal(map[string]string{
		"event": event,
		"body":  body,
	})
	_, err := g.do(ctx, http.MethodPost, url, bytes.NewReader(payload))
	return err
}

func (g *GitHub) fetchDiff(ctx context.Context, prURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, prURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github.v3.diff")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+g.Token)
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("github diff http %d: %s", resp.StatusCode, util.Truncate(string(raw), 400))
	}
	return string(raw), nil
}

// ghPRListItem mirrors only the GitHub fields we care about.
type ghPRListItem struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	Title   string `json:"title"`
	State   string `json:"state"`
	Body    string `json:"body"`
	Head    struct {
		Ref string `json:"ref"`
	} `json:"head"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
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
