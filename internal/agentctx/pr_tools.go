package agentctx

// pr_tools.go implements the pull-request tools for the chat loop:
// pr_get / pr_list / pr_comment / pr_approve / pr_request_changes.
//
// They are thin wrappers over the githost adapters, so plain-text chat
// can answer "who's reviewing PR X" and act on PRs across GitHub,
// GitLab and Bitbucket — whatever GOON_GIT_HOST is configured to.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/harisaginting/goon/internal/githost"
	"github.com/harisaginting/goon/internal/llm"
	"github.com/harisaginting/goon/internal/review"
)

// prReviewer extracts the PRReviewer capability from the configured
// host. The second return is a TOOL ERROR message (surfaced to the
// LLM) when PR review isn't available.
func prReviewer(host githost.Host) (githost.PRReviewer, string) {
	if host == nil {
		return nil, "TOOL ERROR: no git host is configured (set GOON_GIT_HOST). Tell the user PR access isn't available."
	}
	r, ok := host.(githost.PRReviewer)
	if !ok {
		return nil, fmt.Sprintf("TOOL ERROR: the %q git host adapter does not support pull-request review.", host.Name())
	}
	return r, ""
}

// execPRGet reads a PR's metadata + reviewer list.
func execPRGet(ctx context.Context, host githost.Host, c ToolCall) (string, string) {
	rev, errMsg := prReviewer(host)
	if rev == nil {
		return errMsg, "pr_get skipped (no PR support)"
	}
	repo, number, err := parsePRReference(c.PR)
	if err != nil {
		return "TOOL ERROR: pr_get — " + err.Error() + ` Pass a full PR URL or "owner/repo#number" as "pr".`,
			"pr_get rejected (bad ref)"
	}
	gctx, cancel := context.WithTimeout(ctx, chatToolBudget)
	defer cancel()
	pr, diff, err := rev.GetPRDetails(gctx, repo, number)
	if err != nil {
		return fmt.Sprintf("TOOL ERROR: pr_get %s#%d failed: %s. Tell the user what went wrong.", repo, number, err.Error()),
			fmt.Sprintf("pr_get %s#%d failed: %v", repo, number, err)
	}
	return formatPRDetail(pr, len(diff)), fmt.Sprintf("pr_get %s#%d ok", repo, number)
}

// execPRList lists PRs — open in a repo, or awaiting the user's review.
func execPRList(ctx context.Context, host githost.Host, c ToolCall) (string, string) {
	rev, errMsg := prReviewer(host)
	if rev == nil {
		return errMsg, "pr_list skipped (no PR support)"
	}
	lctx, cancel := context.WithTimeout(ctx, chatToolBudget)
	defer cancel()

	var (
		prs   []githost.PR
		err   error
		scope string
	)
	if strings.EqualFold(strings.TrimSpace(c.Filter), "review-requested") {
		rr, ok := host.(githost.ReviewRequester)
		if !ok {
			return fmt.Sprintf("TOOL ERROR: the %q git host can't list review-requested PRs.", host.Name()),
				"pr_list skipped (no review-requester)"
		}
		prs, err = rr.ReviewRequestedPRs(lctx)
		scope = "awaiting your review"
	} else {
		var repos []string
		if r := strings.TrimSpace(c.Repo); r != "" {
			repos = []string{r}
		}
		prs, err = rev.ListPRs(lctx, repos)
		scope = "open"
	}
	if err != nil {
		return "TOOL ERROR: pr_list failed: " + err.Error() + ". Tell the user what went wrong.",
			fmt.Sprintf("pr_list failed: %v", err)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "PR LIST (%s): %d pull request(s)\n", scope, len(prs))
	if len(prs) == 0 {
		sb.WriteString("(none)\n")
	} else {
		for _, p := range prs {
			fmt.Fprintf(&sb, "  %s#%d [%s] by %s — %s\n",
				p.Repo, p.Number, safeStr(p.State, "open"), safeStr(p.Author, "—"), oneLine(p.Title))
			if p.URL != "" {
				fmt.Fprintf(&sb, "    url: %s\n", p.URL)
			}
		}
	}
	sb.WriteString("\nAnswer the user in prose using this list.")
	return sb.String(), fmt.Sprintf("pr_list ok (%s, %d hits)", scope, len(prs))
}

// execPRComment posts a comment on a PR.
func execPRComment(ctx context.Context, host githost.Host, c ToolCall) (string, string) {
	rev, errMsg := prReviewer(host)
	if rev == nil {
		return errMsg, "pr_comment skipped (no PR support)"
	}
	repo, number, err := parsePRReference(c.PR)
	if err != nil {
		return "TOOL ERROR: pr_comment — " + err.Error(), "pr_comment rejected (bad ref)"
	}
	body := strings.TrimSpace(c.Body)
	if body == "" {
		return `TOOL ERROR: pr_comment needs a non-empty "body".`, "pr_comment rejected (no body)"
	}
	actCtx, cancel := context.WithTimeout(ctx, chatToolBudget)
	defer cancel()
	if err := rev.CommentPR(actCtx, repo, number, body); err != nil {
		return fmt.Sprintf("TOOL ERROR: pr_comment on %s#%d failed: %s.", repo, number, err.Error()),
			fmt.Sprintf("pr_comment %s#%d failed: %v", repo, number, err)
	}
	return fmt.Sprintf("ACTION OK: commented on %s#%d. Confirm to the user in prose.", repo, number),
		fmt.Sprintf("pr_comment %s#%d ok", repo, number)
}

// execPRApprove approves a PR.
func execPRApprove(ctx context.Context, host githost.Host, c ToolCall) (string, string) {
	rev, errMsg := prReviewer(host)
	if rev == nil {
		return errMsg, "pr_approve skipped (no PR support)"
	}
	repo, number, err := parsePRReference(c.PR)
	if err != nil {
		return "TOOL ERROR: pr_approve — " + err.Error(), "pr_approve rejected (bad ref)"
	}
	body := strings.TrimSpace(c.Body)
	if body == "" {
		body = "Approved via goon."
	}
	actCtx, cancel := context.WithTimeout(ctx, chatToolBudget)
	defer cancel()
	if err := rev.ApprovePR(actCtx, repo, number, body); err != nil {
		return fmt.Sprintf("TOOL ERROR: pr_approve on %s#%d failed: %s.", repo, number, err.Error()),
			fmt.Sprintf("pr_approve %s#%d failed: %v", repo, number, err)
	}
	return fmt.Sprintf("ACTION OK: approved %s#%d. Confirm to the user in prose.", repo, number),
		fmt.Sprintf("pr_approve %s#%d ok", repo, number)
}

// execPRRequestChanges submits a request-changes review on a PR.
func execPRRequestChanges(ctx context.Context, host githost.Host, c ToolCall) (string, string) {
	rev, errMsg := prReviewer(host)
	if rev == nil {
		return errMsg, "pr_request_changes skipped (no PR support)"
	}
	repo, number, err := parsePRReference(c.PR)
	if err != nil {
		return "TOOL ERROR: pr_request_changes — " + err.Error(), "pr_request_changes rejected (bad ref)"
	}
	body := strings.TrimSpace(c.Body)
	if body == "" {
		return `TOOL ERROR: pr_request_changes needs a "body" explaining what must change.`,
			"pr_request_changes rejected (no body)"
	}
	actCtx, cancel := context.WithTimeout(ctx, chatToolBudget)
	defer cancel()
	if err := rev.RequestChangesPR(actCtx, repo, number, body); err != nil {
		return fmt.Sprintf("TOOL ERROR: pr_request_changes on %s#%d failed: %s.", repo, number, err.Error()),
			fmt.Sprintf("pr_request_changes %s#%d failed: %v", repo, number, err)
	}
	return fmt.Sprintf("ACTION OK: requested changes on %s#%d. Confirm to the user in prose.", repo, number),
		fmt.Sprintf("pr_request_changes %s#%d ok", repo, number)
}

// execPRReview drafts an LLM review for a PR and hands it back together
// with explicit instructions for the assistant: show the review to the
// user verbatim, ask "post this?", and on confirmation post the EXACT
// text via pr_comment. This is the chat equivalent of /review followed
// by /comment, in one natural conversation turn.
func execPRReview(ctx context.Context, host githost.Host, llmProv llm.Provider, c ToolCall) (string, string) {
	rev, errMsg := prReviewer(host)
	if rev == nil {
		return errMsg, "pr_review skipped (no PR support)"
	}
	if llmProv == nil {
		return "TOOL ERROR: pr_review needs an LLM provider — none is configured.",
			"pr_review skipped (no llm)"
	}
	repo, number, err := parsePRReference(c.PR)
	if err != nil {
		return "TOOL ERROR: pr_review — " + err.Error() + ` Pass a full PR URL or "owner/repo#number" as "pr".`,
			"pr_review rejected (bad ref)"
	}
	// Generous timeout — covers fetching a large diff over a slow link
	// AND the model call. Earlier 90s sometimes timed out on multi-100-KB
	// diffs; 3 minutes gives real PRs room to breathe.
	rctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	pr, diff, err := rev.GetPRDetails(rctx, repo, number)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Sprintf("TOOL ERROR: pr_review timed out fetching the diff for %s#%d after 3 minutes — the diff is likely very large or the host is slow. Tell the user this exact reason and offer to focus on a specific file path or to retry later. Do NOT recommend running /review as a workaround — it shares the same backing engine and will hit the same wall.", repo, number),
				fmt.Sprintf("pr_review %s#%d fetch timeout", repo, number)
		}
		return fmt.Sprintf("TOOL ERROR: pr_review fetch %s#%d failed: %s. Tell the user the exact error.", repo, number, err.Error()),
			fmt.Sprintf("pr_review %s#%d fetch failed: %v", repo, number, err)
	}
	if strings.TrimSpace(diff) == "" {
		return fmt.Sprintf("TOOL ERROR: pr_review — %s#%d has an empty diff (nothing to review).", repo, number),
			fmt.Sprintf("pr_review %s#%d empty diff", repo, number)
	}
	draft, err := review.DraftReview(rctx, llmProv, pr, diff)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Sprintf("TOOL ERROR: pr_review fetched the diff for %s#%d (%d bytes) but the model didn't finish reviewing within 3 minutes. Tell the user this exact reason; offer to focus the review on a specific area.", repo, number, len(diff)),
				fmt.Sprintf("pr_review %s#%d llm timeout", repo, number)
		}
		return "TOOL ERROR: pr_review model call failed: " + err.Error() + ". Report the exact error to the user.",
			fmt.Sprintf("pr_review %s#%d llm failed: %v", repo, number, err)
	}
	ref := fmt.Sprintf("%s#%d", repo, number)
	return fmt.Sprintf(`PR REVIEW DRAFT for %s — %s
%s

——— BEGIN REVIEW ———
%s
——— END REVIEW ———

INSTRUCTIONS (you, the assistant):
  1. Reply in prose to the user. Show the review verbatim — everything
     between BEGIN and END above — but do NOT include the BEGIN/END
     fences themselves.
  2. End your reply with this one short follow-up question:
     "Would you like me to post this as a comment on the PR?"
  3. If on the NEXT turn the user confirms (yes / sure / post it / ok /
     👍 / etc.), call pr_comment with:
       "pr":   %q
       "body": the EXACT review text between BEGIN and END above — do
               not paraphrase, summarise, shorten or edit a single
               character. The body must match what you showed the user.
  4. If they decline, acknowledge briefly and stop.`, ref, pr.Title, pr.URL, draft, ref),
		fmt.Sprintf("pr_review %s ok", ref)
}

// formatPRDetail renders a PR for the LLM to answer from.
func formatPRDetail(pr githost.PR, diffBytes int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "PR DETAIL: %s#%d\n", pr.Repo, pr.Number)
	fmt.Fprintf(&sb, "title: %s\n", oneLine(pr.Title))
	if pr.URL != "" {
		fmt.Fprintf(&sb, "url: %s\n", pr.URL)
	}
	fmt.Fprintf(&sb, "author: %s\n", safeStr(pr.Author, "—"))
	fmt.Fprintf(&sb, "state: %s\n", safeStr(pr.State, "—"))
	if pr.Branch != "" {
		fmt.Fprintf(&sb, "branch: %s\n", pr.Branch)
	}
	if len(pr.Reviewers) == 0 {
		sb.WriteString("reviewers: (none assigned)\n")
	} else {
		sb.WriteString("reviewers:\n")
		for _, rv := range pr.Reviewers {
			fmt.Fprintf(&sb, "  - %s [%s]\n", rv.Name, safeStr(rv.State, "pending"))
		}
	}
	fmt.Fprintf(&sb, "diff size: %d bytes\n", diffBytes)
	sb.WriteString("\nAnswer the user in prose from this. For a full AI code review of the diff, point them at the /review command.")
	return sb.String()
}

// parsePRReference accepts a PR/MR URL or an "owner/repo#number" form
// and returns the repo slug + PR number.
func parsePRReference(s string) (string, int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", 0, fmt.Errorf("empty PR reference")
	}
	if strings.Contains(s, "://") {
		repo, number := parsePRURL(s)
		if repo == "" || number == 0 {
			return "", 0, fmt.Errorf("could not parse the PR URL %q", s)
		}
		return repo, number, nil
	}
	// "owner/repo#number"
	if i := strings.LastIndex(s, "#"); i > 0 {
		repo := strings.TrimSpace(s[:i])
		number := leadingInt(strings.TrimSpace(s[i+1:]))
		if repo != "" && number > 0 {
			return repo, number, nil
		}
	}
	// "owner/repo number" (whitespace-separated)
	if f := strings.Fields(s); len(f) >= 2 {
		repo := f[0]
		number := leadingInt(f[len(f)-1])
		if strings.Contains(repo, "/") && number > 0 {
			return repo, number, nil
		}
	}
	return "", 0, fmt.Errorf("could not parse %q as a PR reference", s)
}

// parsePRURL extracts the repo slug + number from a PR/MR browser URL.
// Handles GitHub (/pull/N), GitLab (/-/merge_requests/N) and Bitbucket
// (/pull-requests/N), including GitLab's nested group paths.
func parsePRURL(u string) (string, int) {
	path := u
	if i := strings.Index(path, "://"); i >= 0 {
		rest := path[i+3:]
		j := strings.IndexByte(rest, '/')
		if j < 0 {
			return "", 0
		}
		path = rest[j:]
	}
	path = strings.TrimPrefix(path, "/")
	for _, marker := range []string{"/-/merge_requests/", "/pull-requests/", "/merge_requests/", "/pulls/", "/pull/"} {
		if i := strings.Index(path, marker); i >= 0 {
			repo := strings.Trim(path[:i], "/")
			number := leadingInt(path[i+len(marker):])
			return repo, number
		}
	}
	return "", 0
}

// leadingInt reads the run of digits at the start of s (0 when none).
func leadingInt(s string) int {
	n, got := 0, false
	for i := 0; i < len(s) && s[i] >= '0' && s[i] <= '9'; i++ {
		n = n*10 + int(s[i]-'0')
		got = true
	}
	if !got {
		return 0
	}
	return n
}
