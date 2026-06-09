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

	"github.com/harisaginting/goon/internal/logx"
	"github.com/harisaginting/goon/internal/util"
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
		HTTP:   logx.InstrumentClient("gitlab", &http.Client{Timeout: 30 * time.Second}),
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
		return nil, fmt.Errorf("gitlab http %d: %s", resp.StatusCode, util.Truncate(string(raw), 400))
	}
	return raw, nil
}

// --- PRReviewer implementation ---------------------------------------------

// glMR mirrors the subset of GitLab merge-request JSON we consume.
type glMR struct {
	IID          int    `json:"iid"`
	WebURL       string `json:"web_url"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	State        string `json:"state"`
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	Author       struct {
		Username string `json:"username"`
	} `json:"author"`
	Reviewers []struct {
		Username string `json:"username"`
		Name     string `json:"name"`
	} `json:"reviewers"`
}

// collectReviewers builds a merge request's reviewer list, marking those
// who have approved. The /approvals call is best-effort.
func (g *GitLab) collectReviewers(ctx context.Context, projEscaped string, iid int, mr glMR) []Reviewer {
	approved := map[string]bool{}
	raw, err := g.do(ctx, http.MethodGet,
		fmt.Sprintf("%s/projects/%s/merge_requests/%d/approvals", g.APIURL, projEscaped, iid), nil)
	if err == nil {
		var ap struct {
			ApprovedBy []struct {
				User struct {
					Username string `json:"username"`
				} `json:"user"`
			} `json:"approved_by"`
		}
		if json.Unmarshal(raw, &ap) == nil {
			for _, a := range ap.ApprovedBy {
				if a.User.Username != "" {
					approved[a.User.Username] = true
				}
			}
		}
	}
	out := make([]Reviewer, 0, len(mr.Reviewers))
	for _, rv := range mr.Reviewers {
		name := rv.Name
		if name == "" {
			name = rv.Username
		}
		r := Reviewer{Name: name, State: "pending"}
		if approved[rv.Username] {
			r.State = "approved"
			r.Approved = true
		}
		out = append(out, r)
	}
	return out
}

// toPR converts a GitLab MR into the host-agnostic PR descriptor. The
// repo slug ("group/project") is recovered from the web URL because the
// MR payload only carries a numeric project_id.
func (mr glMR) toPR() PR {
	return PR{
		Number: mr.IID,
		URL:    mr.WebURL,
		Title:  mr.Title,
		Branch: mr.SourceBranch,
		Base:   mr.TargetBranch,
		Author: mr.Author.Username,
		State:  glNormalizeState(mr.State),
		Body:   mr.Description,
		Repo:   glRepoFromWebURL(mr.WebURL),
	}
}

// glNormalizeState maps GitLab's MR states onto goon's vocabulary.
func glNormalizeState(s string) string {
	switch s {
	case "opened":
		return "open"
	case "merged":
		return "merged"
	case "closed", "locked":
		return "closed"
	default:
		return s
	}
}

// glRepoFromWebURL extracts the "group/project" slug from an MR web URL
// like "https://gitlab.com/group/sub/project/-/merge_requests/42".
func glRepoFromWebURL(webURL string) string {
	i := strings.Index(webURL, "/-/merge_requests/")
	if i < 0 {
		return ""
	}
	path := webURL[:i]
	if j := strings.Index(path, "://"); j >= 0 {
		rest := path[j+3:]
		if k := strings.IndexByte(rest, '/'); k >= 0 {
			return strings.Trim(rest[k+1:], "/")
		}
	}
	return ""
}

// ListPRs implements PRReviewer. With an explicit repo list it queries
// each project's open MRs; with none it falls back to GOON_REVIEW_REPOS
// and finally to the MRs awaiting the current user's review.
func (g *GitLab) ListPRs(ctx context.Context, repos []string) ([]PR, error) {
	if len(repos) == 0 {
		for _, r := range strings.Split(os.Getenv("GOON_REVIEW_REPOS"), ",") {
			if r = strings.TrimSpace(r); r != "" {
				repos = append(repos, r)
			}
		}
	}
	if len(repos) == 0 {
		return g.ReviewRequestedPRs(ctx)
	}
	out := []PR{}
	for _, repo := range repos {
		repo = strings.TrimSpace(repo)
		endpoint := fmt.Sprintf("%s/projects/%s/merge_requests?state=opened&per_page=50",
			g.APIURL, url.PathEscape(repo))
		raw, err := g.do(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return out, fmt.Errorf("gitlab list %s: %w", repo, err)
		}
		var mrs []glMR
		if err := json.Unmarshal(raw, &mrs); err != nil {
			return out, fmt.Errorf("gitlab list %s decode: %w", repo, err)
		}
		for _, mr := range mrs {
			pr := mr.toPR()
			if pr.Repo == "" {
				pr.Repo = repo
			}
			out = append(out, pr)
		}
	}
	return out, nil
}

// GetPRDetails implements PRReviewer — returns the MR plus a
// reconstructed unified diff.
func (g *GitLab) GetPRDetails(ctx context.Context, repo string, number int) (PR, string, error) {
	proj := url.PathEscape(strings.TrimSpace(repo))
	endpoint := fmt.Sprintf("%s/projects/%s/merge_requests/%d", g.APIURL, proj, number)
	raw, err := g.do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return PR{}, "", fmt.Errorf("gitlab get mr: %w", err)
	}
	var mr glMR
	if err := json.Unmarshal(raw, &mr); err != nil {
		return PR{}, "", fmt.Errorf("gitlab decode mr: %w", err)
	}
	pr := mr.toPR()
	if pr.Repo == "" {
		pr.Repo = strings.TrimSpace(repo)
	}
	pr.Reviewers = g.collectReviewers(ctx, proj, number, mr)
	diff, err := g.fetchMRDiff(ctx, proj, number)
	if err != nil {
		return pr, "", err
	}
	return pr, diff, nil
}

// fetchMRDiff pulls an MR's per-file changes and reconstructs a unified
// diff readable by the review model. GitLab returns the hunks split per
// file; we re-add the "diff --git" / "---" / "+++" headers.
func (g *GitLab) fetchMRDiff(ctx context.Context, projEscaped string, iid int) (string, error) {
	endpoint := fmt.Sprintf("%s/projects/%s/merge_requests/%d/diffs?per_page=100",
		g.APIURL, projEscaped, iid)
	raw, err := g.do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("gitlab mr diff: %w", err)
	}
	var changes []struct {
		OldPath     string `json:"old_path"`
		NewPath     string `json:"new_path"`
		Diff        string `json:"diff"`
		NewFile     bool   `json:"new_file"`
		DeletedFile bool   `json:"deleted_file"`
		RenamedFile bool   `json:"renamed_file"`
	}
	if err := json.Unmarshal(raw, &changes); err != nil {
		return "", fmt.Errorf("gitlab mr diff decode: %w", err)
	}
	var sb strings.Builder
	for _, c := range changes {
		fmt.Fprintf(&sb, "diff --git a/%s b/%s\n", c.OldPath, c.NewPath)
		switch {
		case c.NewFile:
			sb.WriteString("(new file)\n")
		case c.DeletedFile:
			sb.WriteString("(deleted file)\n")
		case c.RenamedFile:
			fmt.Fprintf(&sb, "(renamed %s -> %s)\n", c.OldPath, c.NewPath)
		}
		fmt.Fprintf(&sb, "--- a/%s\n+++ b/%s\n", c.OldPath, c.NewPath)
		sb.WriteString(c.Diff)
		if !strings.HasSuffix(c.Diff, "\n") {
			sb.WriteString("\n")
		}
	}
	return sb.String(), nil
}

// CommentPR posts a top-level note on the merge request.
func (g *GitLab) CommentPR(ctx context.Context, repo string, number int, body string) error {
	if strings.TrimSpace(body) == "" {
		return errors.New("gitlab comment: body is empty")
	}
	endpoint := fmt.Sprintf("%s/projects/%s/merge_requests/%d/notes",
		g.APIURL, url.PathEscape(strings.TrimSpace(repo)), number)
	payload, _ := json.Marshal(map[string]string{"body": body})
	_, err := g.do(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	return err
}

// ApprovePR approves the merge request. A non-empty body is posted as a
// note first so the approval carries visible context (GitLab's /approve
// endpoint takes no message).
func (g *GitLab) ApprovePR(ctx context.Context, repo string, number int, body string) error {
	if strings.TrimSpace(body) != "" {
		if err := g.CommentPR(ctx, repo, number, body); err != nil {
			return err
		}
	}
	endpoint := fmt.Sprintf("%s/projects/%s/merge_requests/%d/approve",
		g.APIURL, url.PathEscape(strings.TrimSpace(repo)), number)
	_, err := g.do(ctx, http.MethodPost, endpoint, nil)
	return err
}

// RequestChangesPR records a change request. GitLab's REST API has no
// "request changes" review event, so the reason is posted as a note —
// the same visible outcome (a blocking comment) without a dedicated
// endpoint.
func (g *GitLab) RequestChangesPR(ctx context.Context, repo string, number int, body string) error {
	if strings.TrimSpace(body) == "" {
		body = "Changes requested."
	}
	return g.CommentPR(ctx, repo, number, "**Changes requested**\n\n"+body)
}

// --- ReviewRequester implementation ----------------------------------------

// currentUsername returns the authenticated user's GitLab username,
// needed to filter merge requests by reviewer.
func (g *GitLab) currentUsername(ctx context.Context) (string, error) {
	raw, err := g.do(ctx, http.MethodGet, g.APIURL+"/user", nil)
	if err != nil {
		return "", fmt.Errorf("gitlab current user: %w", err)
	}
	var u struct {
		Username string `json:"username"`
	}
	if err := json.Unmarshal(raw, &u); err != nil {
		return "", fmt.Errorf("gitlab current user decode: %w", err)
	}
	if u.Username == "" {
		return "", errors.New("gitlab: /user returned an empty username")
	}
	return u.Username, nil
}

// ReviewRequestedPRs implements ReviewRequester — every open MR where the
// authenticated user is a requested reviewer, across all projects.
func (g *GitLab) ReviewRequestedPRs(ctx context.Context) ([]PR, error) {
	me, err := g.currentUsername(ctx)
	if err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf("%s/merge_requests?state=opened&scope=all&reviewer_username=%s&per_page=100",
		g.APIURL, url.QueryEscape(me))
	raw, err := g.do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("gitlab review-requested mrs: %w", err)
	}
	var mrs []glMR
	if err := json.Unmarshal(raw, &mrs); err != nil {
		return nil, fmt.Errorf("gitlab review-requested mrs decode: %w", err)
	}
	out := make([]PR, 0, len(mrs))
	for _, mr := range mrs {
		out = append(out, mr.toPR())
	}
	return out, nil
}

// --- Notifier implementation -----------------------------------------------

// Notifications implements Notifier via GitLab's to-do list. Only
// review-request and mention to-dos are surfaced — the actionable
// subset goon forwards to Telegram.
func (g *GitLab) Notifications(ctx context.Context) ([]Notification, error) {
	raw, err := g.do(ctx, http.MethodGet, g.APIURL+"/todos?state=pending&per_page=50", nil)
	if err != nil {
		return nil, fmt.Errorf("gitlab todos: %w", err)
	}
	var todos []struct {
		ID         int64  `json:"id"`
		ActionName string `json:"action_name"`
		TargetURL  string `json:"target_url"`
		Target     struct {
			Title string `json:"title"`
		} `json:"target"`
		Project struct {
			PathWithNamespace string `json:"path_with_namespace"`
		} `json:"project"`
		CreatedAt time.Time `json:"created_at"`
	}
	if err := json.Unmarshal(raw, &todos); err != nil {
		return nil, fmt.Errorf("gitlab todos decode: %w", err)
	}
	out := []Notification{}
	for _, t := range todos {
		kind := ""
		switch t.ActionName {
		case "review_requested":
			kind = "review_requested"
		case "mentioned", "directly_addressed":
			kind = "mention"
		default:
			continue
		}
		out = append(out, Notification{
			ID:        fmt.Sprintf("%d", t.ID),
			Kind:      kind,
			Title:     t.Target.Title,
			Repo:      t.Project.PathWithNamespace,
			URL:       t.TargetURL,
			Reason:    t.ActionName,
			UpdatedAt: t.CreatedAt,
		})
	}
	return out, nil
}
