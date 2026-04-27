// Package githost is goon's pluggable Git hosting layer for opening pull /
// merge requests after a workflow finishes.
package githost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// PR is the canonical pull-request descriptor used by goon's workflow.
type PR struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	Title  string `json:"title"`
	Branch string `json:"branch"`
}

// CreateOptions holds the inputs to OpenPR.
type CreateOptions struct {
	Repo   string // "owner/repo" for GitHub, "group/proj" for GitLab
	Title  string
	Body   string
	Head   string // source branch
	Base   string // target branch (default "main"/"master")
	Draft  bool
	Labels []string
}

// Host abstracts the PR/MR provider.
type Host interface {
	Name() string
	OpenPR(ctx context.Context, opts CreateOptions) (PR, error)
}

// NewFromEnv selects an adapter by GOON_GIT_HOST.
func NewFromEnv() (Host, error) {
	name := strings.ToLower(strings.TrimSpace(os.Getenv("GOON_GIT_HOST")))
	if name == "" {
		return nil, ErrNoHost
	}
	switch name {
	case "github":
		return NewGitHubFromEnv()
	case "gitlab":
		return NewGitLabFromEnv()
	case "bitbucket":
		return NewBitbucketFromEnv()
	case "mock":
		return NewMock(), nil
	default:
		return nil, fmt.Errorf("unknown GOON_GIT_HOST %q (want github|gitlab|bitbucket|mock)", name)
	}
}

// ErrNoHost signals "no git host configured" — workflow should skip the PR step.
var ErrNoHost = errors.New("no git host configured (set GOON_GIT_HOST=github|gitlab|bitbucket)")

// Mock records calls without making HTTP requests. Used in tests.
type Mock struct {
	Opened []CreateOptions
	NextPR PR
}

// NewMock returns a Mock prefilled with a PR #1.
func NewMock() *Mock {
	return &Mock{NextPR: PR{Number: 1, URL: "https://example/pr/1", Title: "x", Branch: "x"}}
}

// Name returns "mock".
func (*Mock) Name() string { return "mock" }

// OpenPR records the call and returns NextPR.
func (m *Mock) OpenPR(_ context.Context, opts CreateOptions) (PR, error) {
	m.Opened = append(m.Opened, opts)
	pr := m.NextPR
	pr.Title = opts.Title
	pr.Branch = opts.Head
	return pr, nil
}
