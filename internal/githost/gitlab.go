package githost

import (
	"bytes"
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
)

// GitLab opens merge requests via the GitLab REST API.
type GitLab struct {
	Token  string
	APIURL string // e.g. https://gitlab.com/api/v4
	HTTP   *http.Client
}

// NewGitLabFromEnv reads GITLAB_TOKEN + optional GITLAB_API_URL.
func NewGitLabFromEnv() (*GitLab, error) {
	tok := os.Getenv("GITLAB_TOKEN")
	if tok == "" {
		return nil, errors.New("gitlab host: set GITLAB_TOKEN")
	}
	api := os.Getenv("GITLAB_API_URL")
	if api == "" {
		api = "https://gitlab.com/api/v4"
	}
	return &GitLab{
		Token:  tok,
		APIURL: strings.TrimRight(api, "/"),
		HTTP:   &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// Name returns "gitlab".
func (*GitLab) Name() string { return "gitlab" }

type glMRRequest struct {
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	Labels       string `json:"labels,omitempty"`
}

type glMRResponse struct {
	IID          int    `json:"iid"`
	WebURL       string `json:"web_url"`
	Title        string `json:"title"`
	SourceBranch string `json:"source_branch"`
}

// OpenPR creates a merge request. Repo here is "namespace/project" — we
// URL-encode it because GitLab's API uses the encoded path as the project id.
func (g *GitLab) OpenPR(ctx context.Context, o CreateOptions) (PR, error) {
	if o.Base == "" {
		o.Base = "main"
	}
	body, _ := json.Marshal(glMRRequest{
		SourceBranch: o.Head,
		TargetBranch: o.Base,
		Title:        o.Title,
		Description:  o.Body,
		Labels:       strings.Join(o.Labels, ","),
	})
	endpoint := fmt.Sprintf("%s/projects/%s/merge_requests",
		g.APIURL, url.PathEscape(o.Repo))
	respBody, err := g.do(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return PR{}, err
	}
	var mr glMRResponse
	if err := json.Unmarshal(respBody, &mr); err != nil {
		return PR{}, fmt.Errorf("gitlab decode: %w", err)
	}
	return PR{Number: mr.IID, URL: mr.WebURL, Title: mr.Title, Branch: mr.SourceBranch}, nil
}

func (g *GitLab) do(ctx context.Context, method, urlStr string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, urlStr, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("PRIVATE-TOKEN", g.Token)
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
		return nil, fmt.Errorf("gitlab http %d: %s", resp.StatusCode, truncate(string(raw), 400))
	}
	return raw, nil
}
