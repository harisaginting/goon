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
	Author string `json:"author,omitempty"`
	State  string `json:"state,omitempty"`  // "open" | "closed" | "merged"
	Body   string `json:"body,omitempty"`
	Repo   string `json:"repo,omitempty"`   // "owner/repo"
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

// PRReviewer is a companion interface for hosts that support reading and
// reviewing existing PRs (used by the Telegram bot's /prs and /review flows).
//
// Hosts that don't implement it gracefully degrade: the bot reports "PR
// review not supported on this host" instead of crashing. Currently only
// GitHub implements PRReviewer; gitlab/bitbucket can grow into it.
//
// Repo argument format is host-native ("owner/repo" for GitHub).
type PRReviewer interface {
	Host
	// ListPRs returns open PRs across one or more repos. When repos is
	// empty, hosts may consult an env var (GOON_REVIEW_REPOS) to fall
	// back to a configured list, or return an empty slice.
	ListPRs(ctx context.Context, repos []string) ([]PR, error)
	// GetPRDetails returns the PR including body + diff text. Diff is
	// returned as the unified diff (git format-patch friendly).
	GetPRDetails(ctx context.Context, repo string, number int) (PR, string, error)
	// CommentPR posts a top-level review comment.
	CommentPR(ctx context.Context, repo string, number int, body string) error
	// ApprovePR submits an approval review.
	ApprovePR(ctx context.Context, repo string, number int, body string) error
	// RequestChangesPR submits a "request changes" review.
	RequestChangesPR(ctx context.Context, repo string, number int, body string) error
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
	Opened       []CreateOptions
	NextPR       PR
	OpenPRs      []PR              // returned by ListPRs
	Diffs        map[int]string    // GetPRDetails returns this for the PR number
	Comments     []MockComment
	Approved     []int
	ChangesAsked []MockComment
}

// MockComment is the structure recorded for posted comments / reviews.
type MockComment struct {
	Repo   string
	Number int
	Body   string
}

// NewMock returns a Mock prefilled with a PR #1.
func NewMock() *Mock {
	return &Mock{
		NextPR: PR{Number: 1, URL: "https://example/pr/1", Title: "x", Branch: "x"},
		Diffs:  map[int]string{},
	}
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

// ListPRs returns the prefilled list (or filtered to repos when supplied).
func (m *Mock) ListPRs(_ context.Context, repos []string) ([]PR, error) {
	if len(repos) == 0 {
		out := make([]PR, len(m.OpenPRs))
		copy(out, m.OpenPRs)
		return out, nil
	}
	want := map[string]bool{}
	for _, r := range repos {
		want[r] = true
	}
	out := []PR{}
	for _, pr := range m.OpenPRs {
		if want[pr.Repo] {
			out = append(out, pr)
		}
	}
	return out, nil
}

// GetPRDetails returns the matching PR record + the prefilled diff.
func (m *Mock) GetPRDetails(_ context.Context, repo string, number int) (PR, string, error) {
	for _, pr := range m.OpenPRs {
		if pr.Number == number && (repo == "" || pr.Repo == repo) {
			diff := m.Diffs[number]
			return pr, diff, nil
		}
	}
	return PR{}, "", fmt.Errorf("mock: PR #%d not found in %s", number, repo)
}

// CommentPR records a top-level review comment.
func (m *Mock) CommentPR(_ context.Context, repo string, number int, body string) error {
	m.Comments = append(m.Comments, MockComment{Repo: repo, Number: number, Body: body})
	return nil
}

// ApprovePR records an approval.
func (m *Mock) ApprovePR(_ context.Context, repo string, number int, body string) error {
	_ = body
	_ = repo
	m.Approved = append(m.Approved, number)
	return nil
}

// RequestChangesPR records a request-changes review.
func (m *Mock) RequestChangesPR(_ context.Context, repo string, number int, body string) error {
	m.ChangesAsked = append(m.ChangesAsked, MockComment{Repo: repo, Number: number, Body: body})
	return nil
}
