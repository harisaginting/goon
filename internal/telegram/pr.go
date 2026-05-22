package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/harisaginting/goon/internal/githost"
	"github.com/harisaginting/goon/internal/llm"
	"github.com/harisaginting/goon/internal/logx"
)

// prReviewer extracts the PRReviewer interface from b.opts.Host. The second
// return is a user-facing reason string when review is unavailable, or ""
// on success. Callers send the reason verbatim to the chat so the user
// knows exactly which env var or feature is missing.
func (b *Bot) prReviewer() (githost.PRReviewer, string) {
	if b.opts.Host == nil {
		return nil, "PR review unavailable: no git host configured.\n" +
			"Set GOON_GIT_HOST to one of: github | gitlab | bitbucket\n" +
			"…and the matching auth env vars (e.g. BITBUCKET_TOKEN or BITBUCKET_USERNAME + BITBUCKET_APP_PASSWORD), then `goon stop && goon start`."
	}
	r, ok := b.opts.Host.(githost.PRReviewer)
	if !ok {
		return nil, fmt.Sprintf(
			"PR review not yet implemented for the %q git host adapter.\n"+
				"Currently supported: github, gitlab, bitbucket.",
			b.opts.Host.Name())
	}
	return r, ""
}

// cmdListPRs handles `/prs [repo]`. With no repo arg the host's
// ListPRs fallback chain takes over: GOON_REVIEW_REPOS → discovery
// (search API on GitHub, repos?role=member on Bitbucket).
func (b *Bot) cmdListPRs(ctx context.Context, chatID int64, args []string) {
	r, reason := b.prReviewer()
	if r == nil {
		_ = b.Send(ctx, chatID, reason)
		return
	}
	var repos []string
	if len(args) > 0 {
		repos = args
	}
	prs, err := r.ListPRs(ctx, repos)
	if err != nil {
		_ = b.Send(ctx, chatID, "✗ list PRs failed: "+err.Error())
		return
	}
	if len(prs) == 0 {
		_ = b.Send(ctx, chatID, "no open PRs found across every repo you can see. Narrow the search with `/prs <owner/repo>`.")
		return
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d open PR(s):\n\n", len(prs))
	for _, pr := range prs {
		fmt.Fprintf(&sb, "%s #%d  %s\n", pr.Repo, pr.Number, pr.Title)
		if pr.Author != "" {
			fmt.Fprintf(&sb, "  by @%s\n", pr.Author)
		}
		if pr.URL != "" {
			fmt.Fprintf(&sb, "  %s\n", pr.URL)
		}
		sb.WriteString("\n")
	}
	sb.WriteString("Next: /review <repo> <num> | /approve <repo> <num> | /decline <repo> <num> <reason> | /comment <repo> <num> <body>")
	b.SendChunked(ctx, chatID, sb.String())
}

// cmdReviewPR handles `/review <repo> <num>` — fetches the PR + diff and
// asks the model for a review summary, then surfaces the result with the
// next-step menu.
func (b *Bot) cmdReviewPR(ctx context.Context, chatID int64, args []string) {
	r, reason := b.prReviewer()
	if r == nil {
		_ = b.Send(ctx, chatID, reason)
		return
	}
	repo, number, err := parsePRRef(args)
	if err != nil {
		_ = b.Send(ctx, chatID, "usage: /review <owner/repo> <number>")
		return
	}
	if b.opts.LLM == nil {
		_ = b.Send(ctx, chatID, "/review unavailable: no LLM provider configured")
		return
	}

	_ = b.Send(ctx, chatID, fmt.Sprintf("→ fetching %s#%d…", repo, number))
	getCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pr, diff, err := r.GetPRDetails(getCtx, repo, number)
	if err != nil {
		_ = b.Send(ctx, chatID, "✗ fetch PR: "+err.Error())
		return
	}
	if strings.TrimSpace(diff) == "" {
		_ = b.Send(ctx, chatID, "(empty diff — nothing to review)")
		return
	}

	prompt := fmt.Sprintf(`Review this pull request and surface the highest-leverage feedback.
Format your reply as plain prose with these sections (skip a section if you have nothing to say):
SUMMARY — 1-2 sentences on what the PR does
RISKS — concrete bugs, regressions, security or correctness concerns (each one cited with a file/line if visible)
NITS — small style/readability/testability suggestions
RECOMMENDATION — one of: approve / request_changes / comment, plus a one-line reason

Keep it tight; an engineer will read this on a phone.

PR
title: %s
url: %s
author: %s
body:
%s

DIFF (unified):
%s`, pr.Title, pr.URL, pr.Author, snippet(pr.Body, 1000), trimDiff(diff, 18000))

	revCtx, cancel2 := context.WithTimeout(ctx, 90*time.Second)
	defer cancel2()
	out, err := b.opts.LLM.Generate(revCtx, []llm.Message{
		{Role: llm.RoleUser, Content: prompt},
	}, llm.Options{Temperature: 0.2, MaxTokens: 4096})
	if err != nil {
		_ = b.Send(ctx, chatID, "✗ llm review failed: "+err.Error())
		return
	}
	out = strings.TrimSpace(out)
	if out == "" {
		out = "(model produced no review)"
	}
	logx.Info("telegram_bot.review", "chat", chatID, "repo", repo, "pr", number, "bytes", len(out))

	header := fmt.Sprintf("Review of %s#%d — %s\n%s\n\n", repo, number, pr.Title, pr.URL)
	footer := fmt.Sprintf("\n\nNext:\n  /approve %s %d [body]\n  /decline %s %d <reason>\n  /comment %s %d <body>",
		repo, number, repo, number, repo, number)
	b.SendChunked(ctx, chatID, header+out+footer)
}

func (b *Bot) cmdApprovePR(ctx context.Context, chatID int64, args []string) {
	r, reason := b.prReviewer()
	if r == nil {
		_ = b.Send(ctx, chatID, reason)
		return
	}
	repo, number, body, err := parsePRRefAndBody(args)
	if err != nil {
		_ = b.Send(ctx, chatID, "usage: /approve <owner/repo> <number> [body]")
		return
	}
	if body == "" {
		body = "Approved via goon."
	}
	if err := r.ApprovePR(ctx, repo, number, body); err != nil {
		_ = b.Send(ctx, chatID, "✗ approve failed: "+err.Error())
		return
	}
	logx.Info("telegram_bot.approve", "chat", chatID, "repo", repo, "pr", number)
	_ = b.Send(ctx, chatID, fmt.Sprintf("✓ approved %s#%d", repo, number))
}

func (b *Bot) cmdDeclinePR(ctx context.Context, chatID int64, args []string) {
	r, reason := b.prReviewer()
	if r == nil {
		_ = b.Send(ctx, chatID, reason)
		return
	}
	repo, number, reason, err := parsePRRefAndBody(args)
	if err != nil || strings.TrimSpace(reason) == "" {
		_ = b.Send(ctx, chatID, "usage: /decline <owner/repo> <number> <reason>")
		return
	}
	if err := r.RequestChangesPR(ctx, repo, number, reason); err != nil {
		_ = b.Send(ctx, chatID, "✗ decline failed: "+err.Error())
		return
	}
	logx.Info("telegram_bot.decline", "chat", chatID, "repo", repo, "pr", number)
	_ = b.Send(ctx, chatID, fmt.Sprintf("✓ requested changes on %s#%d", repo, number))
}

func (b *Bot) cmdCommentPR(ctx context.Context, chatID int64, args []string) {
	r, reason := b.prReviewer()
	if r == nil {
		_ = b.Send(ctx, chatID, reason)
		return
	}
	repo, number, body, err := parsePRRefAndBody(args)
	if err != nil || strings.TrimSpace(body) == "" {
		_ = b.Send(ctx, chatID, "usage: /comment <owner/repo> <number> <body>")
		return
	}
	if err := r.CommentPR(ctx, repo, number, body); err != nil {
		_ = b.Send(ctx, chatID, "✗ comment failed: "+err.Error())
		return
	}
	logx.Info("telegram_bot.comment", "chat", chatID, "repo", repo, "pr", number)
	_ = b.Send(ctx, chatID, fmt.Sprintf("✓ commented on %s#%d", repo, number))
}

// parsePRRef expects ["owner/repo", "123"] and returns the components.
func parsePRRef(args []string) (string, int, error) {
	if len(args) < 2 {
		return "", 0, fmt.Errorf("not enough args")
	}
	repo := args[0]
	if !strings.Contains(repo, "/") {
		return "", 0, fmt.Errorf("repo must look like owner/name")
	}
	num, err := strconv.Atoi(args[1])
	if err != nil || num <= 0 {
		return "", 0, fmt.Errorf("bad number: %q", args[1])
	}
	return repo, num, nil
}

// parsePRRefAndBody is like parsePRRef but the trailing args (if any) are
// joined back into a body string.
func parsePRRefAndBody(args []string) (string, int, string, error) {
	repo, number, err := parsePRRef(args)
	if err != nil {
		return "", 0, "", err
	}
	body := ""
	if len(args) > 2 {
		body = strings.TrimSpace(strings.Join(args[2:], " "))
	}
	return repo, number, body, nil
}

// trimDiff caps a diff so the LLM prompt fits in the model's context. Cuts
// at a line boundary when possible.
func trimDiff(diff string, max int) string {
	if len(diff) <= max {
		return diff
	}
	cut := strings.LastIndex(diff[:max], "\n")
	if cut <= 0 {
		cut = max
	}
	return diff[:cut] + fmt.Sprintf("\n…(diff truncated; full size %d bytes)", len(diff))
}
