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
		return nil, fmt.Errorf("bitbucket http %d: %s", resp.StatusCode, truncate(string(raw), 400))
	}
	return raw, nil
}
