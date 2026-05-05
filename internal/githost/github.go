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
		return nil, fmt.Errorf("github http %d: %s", resp.StatusCode, truncate(string(raw), 400))
	}
	return raw, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
