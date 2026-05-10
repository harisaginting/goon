package githost

import (
	"bytes"
	"context"
	"encoding/base64"
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

// Bitbucket opens pull requests against Bitbucket Cloud (REST API v2).
//
// Repo format is "workspace/repo_slug".
//
// Auth (in priority order):
//
//	BITBUCKET_TOKEN              workspace or repo access token (Bearer)
//	BITBUCKET_USERNAME +         atlassian id + app password (Basic)
//	BITBUCKET_APP_PASSWORD
//
// API URL defaults to https://api.bitbucket.org/2.0; override with
// BITBUCKET_API_URL for proxies. Bitbucket Server (self-hosted) is NOT
// supported — its REST API has a different shape.
type Bitbucket struct {
	APIURL      string
	Token       string
	Username    string
	AppPassword string
	HTTP        *http.Client
}

// NewBitbucketFromEnv reads auth and API URL from env.
func NewBitbucketFromEnv() (*Bitbucket, error) {
	api := os.Getenv("BITBUCKET_API_URL")
	if api == "" {
		api = "https://api.bitbucket.org/2.0"
	}
	tok := os.Getenv("BITBUCKET_TOKEN")
	user := os.Getenv("BITBUCKET_USERNAME")
	pw := os.Getenv("BITBUCKET_APP_PASSWORD")
	if tok == "" && (user == "" || pw == "") {
		return nil, errors.New(
			"bitbucket host: set BITBUCKET_TOKEN, or BITBUCKET_USERNAME + BITBUCKET_APP_PASSWORD")
	}
	return &Bitbucket{
		APIURL:      strings.TrimRight(api, "/"),
		Token:       tok,
		Username:    user,
		AppPassword: pw,
		HTTP:        logx.InstrumentClient("bitbucket", &http.Client{Timeout: 30 * time.Second}),
	}, nil
}

// Name returns "bitbucket".
func (*Bitbucket) Name() string { return "bitbucket" }

type bbBranchRef struct {
	Branch struct {
		Name string `json:"name"`
	} `json:"branch"`
}

type bbPRRequest struct {
	Title       string      `json:"title"`
	Description string      `json:"description,omitempty"`
	Source      bbBranchRef `json:"source"`
	Destination bbBranchRef `json:"destination"`
	CloseSource bool        `json:"close_source_branch,omitempty"`
}

type bbPRResponse struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
	Links struct {
		HTML struct {
			Href string `json:"href"`
		} `json:"html"`
	} `json:"links"`
	Source struct {
		Branch struct {
			Name string `json:"name"`
		} `json:"branch"`
	} `json:"source"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// OpenPR creates a pull request. Bitbucket Cloud has no first-class PR
// labels, so when labels are passed they're prepended to the description as
// a small metadata header — kept consistent with the other adapters.
func (b *Bitbucket) OpenPR(ctx context.Context, o CreateOptions) (PR, error) {
	if o.Base == "" {
		o.Base = "main"
	}
	body := o.Body
	if len(o.Labels) > 0 {
		body = fmt.Sprintf("Labels: %s\n\n%s", strings.Join(o.Labels, ", "), body)
	}
	req := bbPRRequest{
		Title:       o.Title,
		Description: body,
	}
	req.Source.Branch.Name = o.Head
	req.Destination.Branch.Name = o.Base

	buf, _ := json.Marshal(req)
	endpoint := fmt.Sprintf("%s/repositories/%s/pullrequests", b.APIURL, o.Repo)
	respBody, err := b.do(ctx, http.MethodPost, endpoint, bytes.NewReader(buf))
	if err != nil {
		return PR{}, err
	}
	var pr bbPRResponse
	if err := json.Unmarshal(respBody, &pr); err != nil {
		return PR{}, fmt.Errorf("bitbucket decode: %w", err)
	}
	if pr.Error != nil {
		return PR{}, fmt.Errorf("bitbucket: %s", pr.Error.Message)
	}
	return PR{
		Number: pr.ID,
		URL:    pr.Links.HTML.Href,
		Title:  pr.Title,
		Branch: pr.Source.Branch.Name,
	}, nil
}

// --- PRReviewer implementation ---------------------------------------------

// bbPRDetail mirrors the subset of the Bitbucket Cloud PR JSON we consume.
type bbPRDetail struct {
	ID          int    `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	State       string `json:"state"`
	Links       struct {
		HTML struct {
			Href string `json:"href"`
		} `json:"html"`
	} `json:"links"`
	Author struct {
		DisplayName string `json:"display_name"`
		Username    string `json:"username"`
		Nickname    string `json:"nickname"`
	} `json:"author"`
	Source struct {
		Branch struct {
			Name string `json:"name"`
		} `json:"branch"`
	} `json:"source"`
}

// authorName returns the friendliest non-empty author label.
func (d bbPRDetail) authorName() string {
	if d.Author.DisplayName != "" {
		return d.Author.DisplayName
	}
	if d.Author.Username != "" {
		return d.Author.Username
	}
	return d.Author.Nickname
}

// ListPRs returns open PRs across the supplied repos. When repos is empty,
// falls back to GOON_REVIEW_REPOS (comma-separated "workspace/slug,...").
// Bitbucket Cloud paginates list endpoints; we fetch the first page (50
// items) per repo which is plenty for an interactive review queue.
func (b *Bitbucket) ListPRs(ctx context.Context, repos []string) ([]PR, error) {
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
		endpoint := fmt.Sprintf("%s/repositories/%s/pullrequests?state=OPEN&pagelen=50", b.APIURL, repo)
		raw, err := b.do(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return out, fmt.Errorf("bitbucket list %s: %w", repo, err)
		}
		var resp struct {
			Values []bbPRDetail `json:"values"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			return out, fmt.Errorf("bitbucket list %s decode: %w", repo, err)
		}
		for _, it := range resp.Values {
			out = append(out, PR{
				Number: it.ID,
				URL:    it.Links.HTML.Href,
				Title:  it.Title,
				Branch: it.Source.Branch.Name,
				Author: it.authorName(),
				State:  strings.ToLower(it.State),
				Body:   it.Description,
				Repo:   repo,
			})
		}
	}
	return out, nil
}

// GetPRDetails returns the PR + the unified diff text. The diff endpoint
// returns text/plain instead of JSON so we use a separate fetcher.
func (b *Bitbucket) GetPRDetails(ctx context.Context, repo string, number int) (PR, string, error) {
	metaURL := fmt.Sprintf("%s/repositories/%s/pullrequests/%d", b.APIURL, repo, number)
	raw, err := b.do(ctx, http.MethodGet, metaURL, nil)
	if err != nil {
		return PR{}, "", fmt.Errorf("bitbucket get pr: %w", err)
	}
	var d bbPRDetail
	if err := json.Unmarshal(raw, &d); err != nil {
		return PR{}, "", fmt.Errorf("bitbucket decode pr: %w", err)
	}
	pr := PR{
		Number: d.ID, Title: d.Title, URL: d.Links.HTML.Href,
		Branch: d.Source.Branch.Name, Author: d.authorName(),
		State: strings.ToLower(d.State), Body: d.Description, Repo: repo,
	}
	diff, err := b.fetchDiff(ctx, fmt.Sprintf("%s/diff", metaURL))
	if err != nil {
		return pr, "", err
	}
	return pr, diff, nil
}

// CommentPR posts a top-level review comment on the PR.
func (b *Bitbucket) CommentPR(ctx context.Context, repo string, number int, body string) error {
	if strings.TrimSpace(body) == "" {
		return errors.New("bitbucket comment: body is empty")
	}
	payload, _ := json.Marshal(map[string]any{
		"content": map[string]string{"raw": body},
	})
	endpoint := fmt.Sprintf("%s/repositories/%s/pullrequests/%d/comments", b.APIURL, repo, number)
	_, err := b.do(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	return err
}

// ApprovePR approves the PR. When body is non-empty it's posted as a
// preceding comment so the approval has visible context (Bitbucket's
// /approve endpoint takes no body).
func (b *Bitbucket) ApprovePR(ctx context.Context, repo string, number int, body string) error {
	if strings.TrimSpace(body) != "" {
		if err := b.CommentPR(ctx, repo, number, body); err != nil {
			return err
		}
	}
	endpoint := fmt.Sprintf("%s/repositories/%s/pullrequests/%d/approve", b.APIURL, repo, number)
	_, err := b.do(ctx, http.MethodPost, endpoint, nil)
	return err
}

// RequestChangesPR submits a request-changes review. The body (the reason)
// is posted as a comment first since Bitbucket's /request-changes endpoint
// has no body.
func (b *Bitbucket) RequestChangesPR(ctx context.Context, repo string, number int, body string) error {
	if strings.TrimSpace(body) != "" {
		if err := b.CommentPR(ctx, repo, number, body); err != nil {
			return err
		}
	}
	endpoint := fmt.Sprintf("%s/repositories/%s/pullrequests/%d/request-changes", b.APIURL, repo, number)
	_, err := b.do(ctx, http.MethodPost, endpoint, nil)
	return err
}

// fetchDiff GETs a Bitbucket diff URL with text/plain Accept and the same
// auth as the JSON endpoints.
func (b *Bitbucket) fetchDiff(ctx context.Context, urlStr string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "text/plain")
	switch {
	case b.Token != "":
		req.Header.Set("Authorization", "Bearer "+b.Token)
	case b.Username != "" && b.AppPassword != "":
		req.Header.Set("Authorization", "Basic "+
			base64.StdEncoding.EncodeToString([]byte(b.Username+":"+b.AppPassword)))
	}
	resp, err := b.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("bitbucket diff http %d: %s", resp.StatusCode, util.Truncate(string(raw), 400))
	}
	return string(raw), nil
}

func (b *Bitbucket) do(ctx context.Context, method, urlStr string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, urlStr, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	switch {
	case b.Token != "":
		req.Header.Set("Authorization", "Bearer "+b.Token)
	case b.Username != "" && b.AppPassword != "":
		req.Header.Set("Authorization", "Basic "+
			base64.StdEncoding.EncodeToString([]byte(b.Username+":"+b.AppPassword)))
	}
	resp, err := b.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("bitbucket http %d: %s", resp.StatusCode, util.Truncate(string(raw), 400))
	}
	return raw, nil
}
