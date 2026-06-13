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
	"net/url"
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
		Base:   o.Base,
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
	// Destination is the PR's target/base branch — needed so the UI can
	// show "head → base" and filter PRs by target branch.
	Destination struct {
		Branch struct {
			Name string `json:"name"`
		} `json:"branch"`
	} `json:"destination"`
	// Participants includes both reviewers and anyone who has
	// commented; we filter to role=="REVIEWER" in reviewers().
	Participants []bbParticipant `json:"participants"`
}

// bbParticipant is one entry in a Bitbucket PR's participants array.
type bbParticipant struct {
	User struct {
		DisplayName string `json:"display_name"`
		Nickname    string `json:"nickname"`
	} `json:"user"`
	Role     string `json:"role"`  // "REVIEWER" | "PARTICIPANT"
	Approved bool   `json:"approved"`
	State    string `json:"state"` // "approved" | "changes_requested" | ""
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

// reviewers extracts the PR's reviewer list from the participants
// array (a Bitbucket reviewer always appears as a participant with
// role REVIEWER).
func (d bbPRDetail) reviewers() []Reviewer {
	out := []Reviewer{}
	for _, p := range d.Participants {
		if p.Role != "REVIEWER" {
			continue
		}
		name := p.User.DisplayName
		if name == "" {
			name = p.User.Nickname
		}
		state := strings.ToLower(strings.TrimSpace(p.State))
		if state == "" {
			state = "pending"
		}
		out = append(out, Reviewer{Name: name, State: state, Approved: p.Approved})
	}
	return out
}

// ListPRs returns open PRs across the supplied repos. Fallback chain
// when repos is empty:
//
//  1. GOON_REVIEW_REPOS (comma-separated "workspace/slug,...")
//  2. discoverAccessibleRepos: ask Bitbucket for every repo the
//     authenticated user can see (capped at 20 most-recent so a user
//     with hundreds of repos doesn't trigger a 100-call fan-out per
//     /prs invocation).
//
// Bitbucket Cloud paginates list endpoints; we fetch the first page (50
// items) per repo which is plenty for an interactive review queue.
func (b *Bitbucket) ListPRs(ctx context.Context, repos []string) ([]PR, error) {
	if len(repos) == 0 {
		for _, r := range strings.Split(os.Getenv("GOON_REVIEW_REPOS"), ",") {
			r = NormalizeRepoSlug(r)
			if r != "" {
				repos = append(repos, r)
			}
		}
	} else {
		norm := make([]string, 0, len(repos))
		for _, r := range repos {
			if s := NormalizeRepoSlug(r); s != "" {
				norm = append(norm, s)
			}
		}
		repos = norm
	}
	if len(repos) == 0 {
		discovered, err := b.discoverAccessibleRepos(ctx, 20)
		if err != nil {
			return nil, fmt.Errorf("bitbucket: no repos configured and discovery failed: %w (set GOON_REVIEW_REPOS to skip discovery)", err)
		}
		repos = discovered
	}
	out := []PR{}
	for _, repo := range repos {
		endpoint := fmt.Sprintf("%s/repositories/%s/pullrequests?state=OPEN&pagelen=20", b.APIURL, repo)
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
				Base:   it.Destination.Branch.Name,
				Author: it.authorName(),
				State:  strings.ToLower(it.State),
				Body:   it.Description,
				Repo:   repo,
			})
		}
	}
	return out, nil
}

// discoverAccessibleRepos asks Bitbucket Cloud for every repository
// the authenticated user is a member of, sorted by most-recently-
// updated, and returns up to `limit` slugs in "workspace/slug" form.
// Internal helper for the /prs fallback — for the rich Repo objects
// (used by the repo-picker UI), call ListRepos instead.
func (b *Bitbucket) discoverAccessibleRepos(ctx context.Context, limit int) ([]string, error) {
	repos, err := b.listAccessibleRepos(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(repos))
	for _, r := range repos {
		out = append(out, r.Slug)
	}
	return out, nil
}

// ListRepos satisfies the RepoLister interface. Returns up to 100 of
// the most-recently-updated repos the authenticated principal can see
// in "workspace/slug" form, with full Repo metadata (URL, description,
// default branch, private flag). The picker UI uses this so the user
// can tick which ones go into GOON_REVIEW_REPOS.
func (b *Bitbucket) ListRepos(ctx context.Context) ([]Repo, error) {
	return b.listAccessibleRepos(ctx, 100)
}

// bbRepoEntry is the subset of fields we read from
// /repositories/{workspace} response objects. Reused across both the
// workspace-scoped path and the per-workspace fallback so we only
// have to maintain one shape.
type bbRepoEntry struct {
	FullName    string `json:"full_name"`
	Name        string `json:"name"`
	Description string `json:"description"`
	IsPrivate   bool   `json:"is_private"`
	Mainbranch  struct {
		Name string `json:"name"`
	} `json:"mainbranch"`
	Links struct {
		HTML struct {
			Href string `json:"href"`
		} `json:"html"`
		Clone []struct {
			Name string `json:"name"`
			Href string `json:"href"`
		} `json:"clone"`
	} `json:"links"`
}

func (e bbRepoEntry) toRepo() Repo {
	cloneURL := e.Links.HTML.Href
	for _, c := range e.Links.Clone {
		if c.Name == "https" && c.Href != "" {
			cloneURL = c.Href
			break
		}
	}
	return Repo{
		Slug:          e.FullName,
		Name:          e.Name,
		URL:           cloneURL,
		DefaultBranch: e.Mainbranch.Name,
		Description:   e.Description,
		Private:       e.IsPrivate,
	}
}

// listAccessibleRepos enumerates every repo the authenticated principal
// can see, capped at `limit`. The global /repositories?role=member
// endpoint we used to call was deprecated in CHANGE-2770 (HTTP 410), so
// we now go two-step:
//
//   1. GET /workspaces?pagelen=100        → list accessible workspaces
//      (OR fall back to BITBUCKET_WORKSPACE if /workspaces is 401/403,
//       which happens for repo-scoped access tokens).
//   2. For each workspace W:
//        GET /repositories/{W}?pagelen=100&sort=-updated_on
//      → list repos. Stop once we hit `limit` total.
//
// Sorted by most-recently-updated workspace+repo first, which matches
// what users expect to see at the top of the picker.
func (b *Bitbucket) listAccessibleRepos(ctx context.Context, limit int) ([]Repo, error) {
	if limit <= 0 {
		limit = 100
	}

	workspaces, err := b.listWorkspaces(ctx)
	if err != nil {
		// Fall back to a single explicit workspace if the user
		// pre-configured one — covers tokens that can't see /workspaces.
		if ws := strings.TrimSpace(os.Getenv("BITBUCKET_WORKSPACE")); ws != "" {
			workspaces = []string{ws}
		} else {
			return nil, fmt.Errorf("bitbucket list workspaces: %w (set BITBUCKET_WORKSPACE to a specific workspace slug to bypass workspace discovery)", err)
		}
	}
	if len(workspaces) == 0 {
		return nil, fmt.Errorf("bitbucket: no workspaces visible to the token (set BITBUCKET_WORKSPACE to a known slug or use a token with workspace access)")
	}

	out := make([]Repo, 0, limit)
	for _, ws := range workspaces {
		if len(out) >= limit {
			break
		}
		endpoint := fmt.Sprintf("%s/repositories/%s?pagelen=100&sort=-updated_on", b.APIURL, ws)
		raw, err := b.do(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			// Skip workspaces the token can't access (e.g. when
			// /workspaces returned more than the token's actual scope).
			continue
		}
		var resp struct {
			Values []bbRepoEntry `json:"values"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			continue
		}
		for _, r := range resp.Values {
			if r.FullName == "" {
				continue
			}
			out = append(out, r.toRepo())
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// listWorkspaces returns the slugs of every workspace the token can see.
//
// Strategy (two-step because Bitbucket token scopes vary):
//  1. GET /workspaces — works for OAuth + app-password + workspace tokens.
//  2. GET /user/permissions/workspaces — alternative that some repo-scoped
//     tokens can access when /workspaces returns 403/410.
//
// For tokens that can hit neither endpoint, callers should fall back to
// the BITBUCKET_WORKSPACE env var.
func (b *Bitbucket) listWorkspaces(ctx context.Context) ([]string, error) {
	// --- primary: /workspaces -------------------------------------------------
	raw, err := b.do(ctx, http.MethodGet, fmt.Sprintf("%s/workspaces?pagelen=100", b.APIURL), nil)
	if err == nil {
		var resp struct {
			Values []struct {
				Slug string `json:"slug"`
			} `json:"values"`
		}
		if jerr := json.Unmarshal(raw, &resp); jerr != nil {
			return nil, fmt.Errorf("bitbucket workspaces decode: %w", jerr)
		}
		out := make([]string, 0, len(resp.Values))
		for _, w := range resp.Values {
			if w.Slug != "" {
				out = append(out, w.Slug)
			}
		}
		return out, nil
	}
	primaryErr := err

	// --- fallback: /user/permissions/workspaces (works for access tokens) ----
	raw2, err2 := b.do(ctx, http.MethodGet, fmt.Sprintf("%s/user/permissions/workspaces?pagelen=100", b.APIURL), nil)
	if err2 == nil {
		var resp2 struct {
			Values []struct {
				Workspace struct {
					Slug string `json:"slug"`
				} `json:"workspace"`
			} `json:"values"`
		}
		if jerr := json.Unmarshal(raw2, &resp2); jerr != nil {
			return nil, fmt.Errorf("bitbucket workspaces decode: %w", jerr)
		}
		out := make([]string, 0, len(resp2.Values))
		for _, w := range resp2.Values {
			if w.Workspace.Slug != "" {
				out = append(out, w.Workspace.Slug)
			}
		}
		return out, nil
	}

	// Both endpoints failed — return the primary error so callers can log it.
	return nil, primaryErr
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
		Branch: d.Source.Branch.Name, Base: d.Destination.Branch.Name, Author: d.authorName(),
		State: strings.ToLower(d.State), Body: d.Description, Repo: repo,
		Reviewers: d.reviewers(),
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

// bbSetAuth applies the correct Authorization header based on which
// credentials are available:
//
//	Token only              → Bearer <token>   (workspace / repo access token)
//	Username + Token        → Basic base64(username:token)  (token-as-password)
//	Username + AppPassword  → Basic base64(username:app_password)
func bbSetAuth(req *http.Request, token, username, appPassword string) {
	switch {
	case token != "" && username != "":
		// Token used as a password — some orgs issue these for CI.
		req.Header.Set("Authorization", "Basic "+
			base64.StdEncoding.EncodeToString([]byte(username+":"+token)))
	case token != "":
		// Workspace or repository access token — must use Bearer.
		req.Header.Set("Authorization", "Bearer "+token)
	case username != "" && appPassword != "":
		req.Header.Set("Authorization", "Basic "+
			base64.StdEncoding.EncodeToString([]byte(username+":"+appPassword)))
	}
}

// fetchDiff GETs a Bitbucket diff URL with text/plain Accept and the same
// auth as the JSON endpoints.
func (b *Bitbucket) fetchDiff(ctx context.Context, urlStr string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "text/plain")
	bbSetAuth(req, b.Token, b.Username, b.AppPassword)
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
	bbSetAuth(req, b.Token, b.Username, b.AppPassword)
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

// --- ReviewRequester implementation ----------------------------------------

// currentUserUUID returns the authenticated account's UUID, used to
// filter pull requests by reviewer.
func (b *Bitbucket) currentUserUUID(ctx context.Context) (string, error) {
	raw, err := b.do(ctx, http.MethodGet, b.APIURL+"/user", nil)
	if err != nil {
		return "", fmt.Errorf("bitbucket current user: %w", err)
	}
	var u struct {
		UUID string `json:"uuid"`
	}
	if err := json.Unmarshal(raw, &u); err != nil {
		return "", fmt.Errorf("bitbucket current user decode: %w", err)
	}
	if u.UUID == "" {
		return "", errors.New("bitbucket: /user returned an empty uuid " +
			"(a repo-scoped token can't see /user — use a workspace token to detect review requests)")
	}
	return u.UUID, nil
}

// ReviewRequestedPRs implements ReviewRequester. Bitbucket Cloud has no
// cross-repo "PRs awaiting my review" endpoint, so we resolve the repo
// set (GOON_REVIEW_REPOS, else workspace discovery) and query each repo
// for open PRs that list the current user as a reviewer. A repo whose
// query fails (token can't read it, or the host rejects the reviewers
// filter) is skipped rather than failing the whole batch.
func (b *Bitbucket) ReviewRequestedPRs(ctx context.Context) ([]PR, error) {
	uuid, err := b.currentUserUUID(ctx)
	if err != nil {
		return nil, err
	}
	var repos []string
	for _, r := range strings.Split(os.Getenv("GOON_REVIEW_REPOS"), ",") {
		if r = NormalizeRepoSlug(r); r != "" {
			repos = append(repos, r)
		}
	}
	if len(repos) == 0 {
		discovered, derr := b.discoverAccessibleRepos(ctx, 20)
		if derr != nil {
			return nil, fmt.Errorf("bitbucket: no repos configured and discovery failed: %w "+
				"(set GOON_REVIEW_REPOS to skip discovery)", derr)
		}
		repos = discovered
	}
	out := []PR{}
	for _, repo := range repos {
		q := fmt.Sprintf(`state="OPEN" AND reviewers.uuid="%s"`, uuid)
		endpoint := fmt.Sprintf("%s/repositories/%s/pullrequests?pagelen=50&q=%s",
			b.APIURL, repo, url.QueryEscape(q))
		raw, err := b.do(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			logx.Warn("bitbucket.review_requested_skip", "repo", repo, "error", err.Error())
			continue
		}
		var resp struct {
			Values []bbPRDetail `json:"values"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			logx.Warn("bitbucket.review_requested_decode", "repo", repo, "error", err.Error())
			continue
		}
		for _, it := range resp.Values {
			out = append(out, PR{
				Number: it.ID,
				URL:    it.Links.HTML.Href,
				Title:  it.Title,
				Branch: it.Source.Branch.Name,
				Base:   it.Destination.Branch.Name,
				Author: it.authorName(),
				State:  strings.ToLower(it.State),
				Body:   it.Description,
				Repo:   repo,
			})
		}
	}
	return out, nil
}
