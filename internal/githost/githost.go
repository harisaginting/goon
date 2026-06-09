// Package githost is goon's pluggable Git hosting layer for opening pull /
// merge requests after a workflow finishes.
package githost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Reviewer is one person on a PR's review list, with their latest
// review state when the host reports it.
type Reviewer struct {
	Name     string `json:"name"`
	State    string `json:"state,omitempty"` // "approved" | "changes_requested" | "commented" | "pending"
	Approved bool   `json:"approved,omitempty"`
}

// PR is the canonical pull-request descriptor used by goon's workflow.
type PR struct {
	Number    int        `json:"number"`
	URL       string     `json:"url"`
	Title     string     `json:"title"`
	Branch    string     `json:"branch"`            // head / source branch
	Base      string     `json:"base,omitempty"`    // target / base branch (e.g. "main")
	Author    string     `json:"author,omitempty"`
	State     string     `json:"state,omitempty"` // "open" | "closed" | "merged"
	Body      string     `json:"body,omitempty"`
	Repo      string     `json:"repo,omitempty"`      // "owner/repo"
	Reviewers []Reviewer `json:"reviewers,omitempty"` // populated by GetPRDetails
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

// Repo is one repository hosted on the git provider, surfaced by
// RepoLister so users can pick from a fetched list at the
// confirm_repo gate (no typing required, no local checkout needed).
type Repo struct {
	Slug          string `json:"slug"`           // "owner/name" — host-native id
	Name          string `json:"name,omitempty"` // human-friendly display name (often same as basename of slug)
	URL           string `json:"url,omitempty"`  // HTTPS clone URL
	DefaultBranch string `json:"default_branch,omitempty"`
	Description   string `json:"description,omitempty"`
	Private       bool   `json:"private,omitempty"`
}

// RepoLister is an optional companion interface for hosts that can
// enumerate the user's accessible repos. The confirm_repo gate uses
// this to present a numbered menu so users can multi-pick by number
// instead of typing paths. Hosts that don't implement it gracefully
// degrade — the gate falls back to local workspace dir + free-text
// input.
type RepoLister interface {
	Host
	ListRepos(ctx context.Context) ([]Repo, error)
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

// Notification is one actionable item from the authenticated user's
// git-host inbox — currently a review request or an @-mention. Hosts
// surface only items relevant to the current user; see Notifier.
type Notification struct {
	ID        string    `json:"id"`               // host-unique; used for dedup
	Kind      string    `json:"kind"`             // "review_requested" | "mention"
	Title     string    `json:"title"`
	Repo      string    `json:"repo,omitempty"`   // "owner/repo" when known
	URL       string    `json:"url,omitempty"`    // browser URL when derivable
	Reason    string    `json:"reason,omitempty"` // the host's own reason string, verbatim
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// ReviewRequester is an optional companion interface for hosts that can
// list the PRs/MRs where the authenticated user is a requested reviewer.
// Used by `goon review-prs` and the Telegram bot's auto-review loop.
//
// Hosts that don't implement it degrade gracefully — callers report
// "review-request listing not supported on this host". GitHub, GitLab
// and Bitbucket all implement it.
type ReviewRequester interface {
	Host
	ReviewRequestedPRs(ctx context.Context) ([]PR, error)
}

// Notifier is an optional companion interface for hosts that expose the
// authenticated user's notification inbox, filtered to review requests
// and @-mentions. GitHub (/notifications) and GitLab (/todos) implement
// it; Bitbucket Cloud has no inbox API and deliberately does not, so a
// type assertion to Notifier fails and callers degrade gracefully.
type Notifier interface {
	Host
	Notifications(ctx context.Context) ([]Notification, error)
}

// NormalizeRepoSlug turns whatever the user pasted for a "repo"
// into the canonical "owner/name" form the host adapters expect.
//
// Inputs we accept and what they reduce to:
//
//	owner/name                                  → owner/name      (already canonical)
//	https://github.com/owner/name               → owner/name
//	https://github.com/owner/name.git           → owner/name
//	https://github.com/owner/name/pull/42       → owner/name
//	git@github.com:owner/name.git               → owner/name
//	https://bitbucket.org/workspace/slug        → workspace/slug
//	https://bitbucket.org/workspace/slug/src/   → workspace/slug
//	@owner/name (or owner/name with whitespace) → owner/name
//
// Anything we can't parse is returned trimmed and lower-cased to
// "owner/name" if it has exactly two non-empty path segments —
// otherwise returned as-is so the host can decide.
func NormalizeRepoSlug(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return s
	}
	// SSH style: git@host:owner/name(.git)?
	if i := strings.Index(s, "@"); i >= 0 && strings.Contains(s, ":") && !strings.Contains(s, "://") {
		// strip "git@host:" prefix
		if colon := strings.Index(s, ":"); colon > i {
			s = s[colon+1:]
		}
	}
	// HTTPS / HTTP style: drop scheme + host.
	if idx := strings.Index(s, "://"); idx >= 0 {
		rest := s[idx+3:]
		if slash := strings.Index(rest, "/"); slash >= 0 {
			s = rest[slash+1:]
		} else {
			s = ""
		}
	}
	// Drop trailing .git
	s = strings.TrimSuffix(s, ".git")
	// Trim leading/trailing slashes.
	s = strings.Trim(s, "/")
	// Keep only the first two path segments — "owner/repo/pull/42"
	// reduces to "owner/repo".
	parts := strings.Split(s, "/")
	if len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
		return parts[0] + "/" + parts[1]
	}
	return s
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
	// Repos backs the Mock's ListRepos implementation so tests can
	// pre-seed an expected menu and assert on the chosen subset.
	Repos []Repo
	// ReviewPRs backs ReviewRequestedPRs; Notifs backs Notifications.
	ReviewPRs []PR
	Notifs    []Notification
}

// ListRepos returns the prefilled mock repo list.
func (m *Mock) ListRepos(_ context.Context) ([]Repo, error) {
	out := make([]Repo, len(m.Repos))
	copy(out, m.Repos)
	return out, nil
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

// ReviewRequestedPRs returns the prefilled review-request list.
func (m *Mock) ReviewRequestedPRs(_ context.Context) ([]PR, error) {
	out := make([]PR, len(m.ReviewPRs))
	copy(out, m.ReviewPRs)
	return out, nil
}

// Notifications returns the prefilled notification list.
func (m *Mock) Notifications(_ context.Context) ([]Notification, error) {
	out := make([]Notification, len(m.Notifs))
	copy(out, m.Notifs)
	return out, nil
}
