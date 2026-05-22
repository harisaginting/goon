// Package review drafts LLM pull-request reviews for PRs where the
// current user is a requested reviewer, and summarises the user's
// git-host notifications.
//
// It is host-agnostic: it depends only on the githost companion
// interfaces (ReviewRequester, PRReviewer, Notifier) and degrades
// gracefully when a host doesn't implement one.
//
// The Runner produces drafts and notification batches but does not
// deliver them — the caller (the `goon review-prs` / `goon
// notifications` CLI, or the Telegram bot's auto loop) decides where
// output goes. Dedup state lives in internal/memory so a PR is
// re-reviewed only when its diff changes, and a notification is
// forwarded only once. The caller calls MarkReviewed / MarkNotified
// after a successful delivery so a failed send is retried next pass.
package review

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/harisaginting/goon/internal/githost"
	"github.com/harisaginting/goon/internal/llm"
	"github.com/harisaginting/goon/internal/logx"
	"github.com/harisaginting/goon/internal/memory"
)

// maxDiffBytes caps how much of a diff is handed to the model so the
// prompt fits a typical context window.
const maxDiffBytes = 18000

// Options configures a Runner.
type Options struct {
	Host   githost.Host   // required
	LLM    llm.Provider   // required for drafting reviews + notification digests
	Memory *memory.Memory // optional; nil disables dedup
}

// Runner finds review-requested PRs and notifications for the current user.
type Runner struct {
	host githost.Host
	llm  llm.Provider
	mem  *memory.Memory
}

// New builds a Runner.
func New(o Options) *Runner {
	return &Runner{host: o.Host, llm: o.LLM, mem: o.Memory}
}

// Draft is one finished review awaiting the user's approval.
type Draft struct {
	Repo     string
	Number   int
	Title    string
	URL      string
	Author   string
	Body     string // the model-written review
	DiffHash string // fingerprint of the reviewed diff; used by MarkReviewed
}

// NotifBatch is the set of notifications goon has not forwarded yet.
type NotifBatch struct {
	Items   []githost.Notification
	Summary string // model-written digest; set only when len(Items) > 1
}

// HostName reports the configured host's name, or "none".
func (r *Runner) HostName() string {
	if r == nil || r.host == nil {
		return "none"
	}
	return r.host.Name()
}

// PendingReviews lists the PRs awaiting the current user's review and
// drafts a review for each one whose diff has changed since goon last
// looked. When ignoreDedup is true a fresh review is drafted for every
// PR regardless of dedup state.
//
// The returned drafts are NOT yet recorded as seen — the caller calls
// MarkReviewed after a successful delivery so a failed send is retried
// on the next pass.
func (r *Runner) PendingReviews(ctx context.Context, ignoreDedup bool) ([]Draft, error) {
	rr, ok := r.host.(githost.ReviewRequester)
	if !ok {
		return nil, fmt.Errorf("review: the %s git host cannot list review requests", r.HostName())
	}
	pr, ok := r.host.(githost.PRReviewer)
	if !ok {
		return nil, fmt.Errorf("review: the %s git host cannot read PR diffs", r.HostName())
	}
	if r.llm == nil {
		return nil, fmt.Errorf("review: no LLM provider configured")
	}
	prs, err := rr.ReviewRequestedPRs(ctx)
	if err != nil {
		return nil, err
	}
	out := []Draft{}
	for _, p := range prs {
		if p.Repo == "" || p.Number == 0 {
			continue
		}
		full, diff, err := pr.GetPRDetails(ctx, p.Repo, p.Number)
		if err != nil {
			logx.Warn("review.get_pr", "repo", p.Repo, "pr", p.Number, "error", err.Error())
			continue
		}
		if strings.TrimSpace(diff) == "" {
			continue
		}
		hash := fingerprint(diff)
		key := r.reviewKey(p.Repo, p.Number)
		if !ignoreDedup && r.mem != nil {
			if mark, ok := r.mem.ReviewMarkFor(key); ok && mark.DiffHash == hash {
				continue // unchanged since the last review
			}
		}
		title := firstNonEmpty(full.Title, p.Title)
		urlStr := firstNonEmpty(full.URL, p.URL)
		author := firstNonEmpty(full.Author, p.Author)
		body, err := r.draftReview(ctx, title, urlStr, author, full.Body, diff)
		if err != nil {
			logx.Warn("review.draft", "repo", p.Repo, "pr", p.Number, "error", err.Error())
			continue
		}
		out = append(out, Draft{
			Repo:     p.Repo,
			Number:   p.Number,
			Title:    title,
			URL:      urlStr,
			Author:   author,
			Body:     body,
			DiffHash: hash,
		})
	}
	return out, nil
}

// MarkReviewed records that a draft has been delivered, so the PR is not
// re-reviewed until its diff changes again.
func (r *Runner) MarkReviewed(d Draft) {
	if r.mem == nil {
		return
	}
	r.mem.RecordReview(r.reviewKey(d.Repo, d.Number), d.DiffHash)
}

func (r *Runner) reviewKey(repo string, number int) string {
	return fmt.Sprintf("%s:%s#%d", r.HostName(), repo, number)
}

// draftReview asks the model for a tight, phone-readable review.
func (r *Runner) draftReview(ctx context.Context, title, url, author, body, diff string) (string, error) {
	prompt := fmt.Sprintf(`Review this pull request and surface the highest-leverage feedback.
Format your reply as plain prose with these sections (skip a section if you have nothing to say):
SUMMARY — 1-2 sentences on what the PR does
RISKS — concrete bugs, regressions, security or correctness concerns (cite a file/line when visible)
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
%s`, title, url, author, snippet(body, 1000), trimDiff(diff, maxDiffBytes))

	out, err := r.llm.Generate(ctx, []llm.Message{
		{Role: llm.RoleUser, Content: prompt},
	}, llm.Options{Temperature: 0.2, MaxTokens: 4096})
	if err != nil {
		return "", err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", fmt.Errorf("model produced no review")
	}
	return out, nil
}

// Notifications lists the review-request + mention notifications goon
// has not forwarded yet. When more than one is new it also asks the
// model for a short digest. When ignoreDedup is true every current
// notification is returned regardless of forwarded state.
//
// Caller calls MarkNotified after a successful delivery.
func (r *Runner) Notifications(ctx context.Context, ignoreDedup bool) (NotifBatch, error) {
	n, ok := r.host.(githost.Notifier)
	if !ok {
		return NotifBatch{}, fmt.Errorf("review: the %s git host has no notification inbox API", r.HostName())
	}
	all, err := n.Notifications(ctx)
	if err != nil {
		return NotifBatch{}, err
	}
	fresh := []githost.Notification{}
	for _, it := range all {
		if it.Kind != "review_requested" && it.Kind != "mention" {
			continue
		}
		if !ignoreDedup && r.mem != nil && r.mem.NotificationSeen(r.notifKey(it)) {
			continue
		}
		fresh = append(fresh, it)
	}
	sort.SliceStable(fresh, func(i, j int) bool {
		return fresh[i].UpdatedAt.After(fresh[j].UpdatedAt)
	})
	batch := NotifBatch{Items: fresh}
	if len(fresh) > 1 && r.llm != nil {
		batch.Summary = r.summarize(ctx, fresh)
	}
	return batch, nil
}

// MarkNotified records every item in the batch as forwarded.
func (r *Runner) MarkNotified(batch NotifBatch) {
	if r.mem == nil {
		return
	}
	for _, it := range batch.Items {
		r.mem.MarkNotificationSeen(r.notifKey(it))
	}
}

func (r *Runner) notifKey(n githost.Notification) string {
	return r.HostName() + ":" + n.ID
}

// summarize asks the model for a short digest of a notification batch.
// A failure here is non-fatal — the caller still has the raw list.
func (r *Runner) summarize(ctx context.Context, items []githost.Notification) string {
	var sb strings.Builder
	for i, it := range items {
		fmt.Fprintf(&sb, "%d. [%s] %s — %s\n", i+1, it.Kind, it.Repo, it.Title)
	}
	prompt := fmt.Sprintf(`You are triaging a developer's git notifications.
Write a 2-3 sentence digest: how many items there are, what they are, and which one looks most urgent.
Plain prose, no markdown headers — it will be read on a phone.

NOTIFICATIONS (%d):
%s`, len(items), sb.String())
	out, err := r.llm.Generate(ctx, []llm.Message{
		{Role: llm.RoleUser, Content: prompt},
	}, llm.Options{Temperature: 0.3, MaxTokens: 512})
	if err != nil {
		logx.Warn("review.summarize", "error", err.Error())
		return ""
	}
	return strings.TrimSpace(out)
}

// FormatDraftText renders a draft as a plain-text block — used by the
// CLI's stdout output and by plain (no-button) Telegram delivery.
func FormatDraftText(d Draft) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Review draft · %s#%d\n", d.Repo, d.Number)
	if d.Title != "" {
		fmt.Fprintf(&sb, "%s\n", d.Title)
	}
	if d.URL != "" {
		fmt.Fprintf(&sb, "%s\n", d.URL)
	}
	sb.WriteString("\n")
	sb.WriteString(d.Body)
	return sb.String()
}

// FormatNotifText renders a notification batch as a plain-text block.
// Returns "" for an empty batch.
func FormatNotifText(batch NotifBatch) string {
	if len(batch.Items) == 0 {
		return ""
	}
	var sb strings.Builder
	if len(batch.Items) == 1 {
		sb.WriteString("1 new git notification:\n\n")
	} else {
		fmt.Fprintf(&sb, "%d new git notifications", len(batch.Items))
		if batch.Summary != "" {
			fmt.Fprintf(&sb, "\n\n%s", batch.Summary)
		}
		sb.WriteString("\n\n")
	}
	for i, it := range batch.Items {
		label := "mention"
		if it.Kind == "review_requested" {
			label = "review request"
		}
		fmt.Fprintf(&sb, "%d. [%s] %s\n   %s\n", i+1, label, it.Title, it.Repo)
		if it.URL != "" {
			fmt.Fprintf(&sb, "   %s\n", it.URL)
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

// fingerprint hashes a diff so an unchanged PR isn't re-reviewed.
func fingerprint(diff string) string {
	sum := sha256.Sum256([]byte(diff))
	return hex.EncodeToString(sum[:])[:16]
}

// firstNonEmpty returns the first argument that isn't blank.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// snippet truncates s to at most n bytes, rune-safely.
func snippet(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}

// trimDiff caps a diff at max bytes, cutting on a line boundary.
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
