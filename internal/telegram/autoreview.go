// Package telegram — autoreview.go is the bot's background loop for the
// two proactive features:
//
//   - PR review: periodically draft an LLM review for every PR where the
//     current user is a requested reviewer, and push each draft to chat
//     with an inline "✅ Post as comment" button. Tapping it posts the
//     reviewed text to the PR — the user's one-tap approval.
//   - Notifications: periodically forward new review-request + mention
//     notifications, with an LLM digest when more than one is new.
//
// Both are OFF by default (GOON_AUTO_REVIEW / GOON_AUTO_NOTIFY) so an
// existing daemon doesn't suddenly start messaging chats after an
// upgrade. The loop runs inside the bot because the bot already holds
// the git host, the LLM, memory, the send helpers, and the authorized-
// chat list — everything the review engine needs to deliver.
package telegram

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/harisaginting/goon/internal/llm"
	"github.com/harisaginting/goon/internal/logx"
	"github.com/harisaginting/goon/internal/review"
)

// reviewFence delimits the model's review text inside a draft message so
// the approve-button callback can extract exactly what to post.
const reviewFence = "── review ──"

// autoReviewEnabled / autoNotifyEnabled read the env toggles. Both
// default OFF.
func autoReviewEnabled() bool { return envTrue("GOON_AUTO_REVIEW") }
func autoNotifyEnabled() bool { return envTrue("GOON_AUTO_NOTIFY") }

func envTrue(k string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(k))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// autoInterval is the delay between auto passes (GOON_AUTO_INTERVAL,
// default 15m, floor 1m).
func autoInterval() time.Duration {
	if v := strings.TrimSpace(os.Getenv("GOON_AUTO_INTERVAL")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= time.Minute {
			return d
		}
	}
	return 15 * time.Minute
}

// autoLoop periodically drafts reviews for review-requested PRs and
// forwards new notifications to every authorized chat. It is a no-op
// unless GOON_AUTO_REVIEW or GOON_AUTO_NOTIFY is set. Started in its own
// goroutine from Start.
func (b *Bot) autoLoop(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			logx.Error("telegram_bot.auto_panic", "panic", fmt.Sprintf("%v", r))
		}
	}()
	if !autoReviewEnabled() && !autoNotifyEnabled() {
		return
	}
	interval := autoInterval()
	logx.Info("telegram_bot.auto_loop_start",
		"review", autoReviewEnabled(), "notify", autoNotifyEnabled(),
		"interval", interval.String())
	// Small initial delay so the bot's startup output isn't interleaved
	// with a first pass.
	select {
	case <-ctx.Done():
		return
	case <-time.After(20 * time.Second):
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		b.autoPass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// autoPass runs one review + notification pass. A panic in a pass is
// recovered so a single bad cycle never kills the loop for good.
func (b *Bot) autoPass(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			logx.Error("telegram_bot.auto_pass_panic", "panic", fmt.Sprintf("%v", r))
		}
	}()
	if b.opts.Host == nil || b.opts.LLM == nil {
		return
	}
	runner := review.New(review.Options{
		Host:   b.opts.Host,
		LLM:    llm.NewForRoleOr(llm.RoleReview, b.opts.LLM),
		Memory: b.opts.Memory,
	})
	if autoReviewEnabled() {
		b.autoReviewPass(ctx, runner)
	}
	if autoNotifyEnabled() {
		b.autoNotifyPass(ctx, runner)
	}
}

func (b *Bot) autoReviewPass(ctx context.Context, runner *review.Runner) {
	passCtx, cancel := context.WithTimeout(ctx, 8*time.Minute)
	defer cancel()
	drafts, err := runner.PendingReviews(passCtx, false)
	if err != nil {
		logx.Warn("telegram_bot.auto_review", "error", err.Error())
		return
	}
	if len(drafts) == 0 {
		return
	}
	chats := b.opts.Memory.AuthorizedChats()
	if len(chats) == 0 {
		return
	}
	for _, d := range drafts {
		delivered := false
		for _, c := range chats {
			if err := b.sendReviewDraft(ctx, c.ChatID, d); err != nil {
				logx.Warn("telegram_bot.auto_review_send", "chat", c.ChatID, "error", err.Error())
				continue
			}
			delivered = true
		}
		// Mark only after a successful send so a Telegram hiccup is
		// retried on the next pass instead of silently swallowing a draft.
		if delivered {
			runner.MarkReviewed(d)
			logx.Info("telegram_bot.auto_review_sent", "repo", d.Repo, "pr", d.Number)
		}
	}
}

func (b *Bot) autoNotifyPass(ctx context.Context, runner *review.Runner) {
	passCtx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	batch, err := runner.Notifications(passCtx, false)
	if err != nil {
		logx.Warn("telegram_bot.auto_notify", "error", err.Error())
		return
	}
	if len(batch.Items) == 0 {
		return
	}
	text := "🔔 " + review.FormatNotifText(batch)
	if len(text) > 3900 {
		text = clampUTF8(text, 3900) + "\n…(truncated)"
	}
	chats := b.opts.Memory.AuthorizedChats()
	if len(chats) == 0 {
		return
	}
	delivered := false
	for _, c := range chats {
		if err := b.Send(ctx, c.ChatID, text); err != nil {
			logx.Warn("telegram_bot.auto_notify_send", "chat", c.ChatID, "error", err.Error())
			continue
		}
		delivered = true
	}
	if delivered {
		runner.MarkNotified(batch)
		logx.Info("telegram_bot.auto_notify_sent", "count", len(batch.Items))
	}
}

// sendReviewDraft delivers one review draft to a chat with an inline
// "✅ Post as comment" button. The draft is fenced so the button's
// callback can extract exactly what to post on the PR.
func (b *Bot) sendReviewDraft(ctx context.Context, chatID int64, d review.Draft) error {
	header := fmt.Sprintf("👀 Review draft · %s#%d", d.Repo, d.Number)
	if d.Title != "" {
		header += "\n" + d.Title
	}
	if d.URL != "" {
		header += "\n" + d.URL
	}
	body := d.Body
	// Keep the whole message inside one Telegram message (4096-char
	// limit) so the fence and the button stay attached to the text.
	const bodyCap = 3400
	if len(body) > bodyCap {
		body = clampUTF8(body, bodyCap) + "\n…(truncated)"
	}
	text := header + "\n\n" + reviewFence + "\n" + body + "\n" + reviewFence +
		"\n\nTap ✅ to post this review as a comment on the PR."

	cb := reviewCallbackData(d.Repo, d.Number)
	if cb == "" {
		// Repo slug too long to fit Telegram's 64-byte callback cap —
		// deliver without the button; the user can /comment manually.
		return b.Send(ctx, chatID, text+"\n\n(or post it with: /comment "+
			d.Repo+" "+strconv.Itoa(d.Number)+" <text>)")
	}
	rows := [][]InlineButton{{
		{Text: "✅ Post as comment", CallbackData: cb},
		{Text: "🗑 Dismiss", CallbackData: "rv:x"},
	}}
	return b.SendWithButtons(ctx, chatID, text, rows)
}

// reviewCallbackData builds the "rv:repo:number" payload, or "" when it
// would exceed Telegram's 64-byte callback_data limit.
func reviewCallbackData(repo string, number int) string {
	cb := "rv:" + repo + ":" + strconv.Itoa(number)
	if len(cb) > 64 {
		return ""
	}
	return cb
}

// callbackHandleReview claims the "rv:" inline-button callbacks (the
// review-draft approve / dismiss buttons). Returns true when it handled
// the tap. Mirrors callbackHandleRepos so interactive.go only needs a
// one-line hook. The caller has already verified the chat is authorized.
func (b *Bot) callbackHandleReview(ctx context.Context, q *CallbackQuery) bool {
	data := q.Data
	if !strings.HasPrefix(data, "rv:") {
		return false
	}
	chatID := int64(0)
	if q.Message != nil {
		chatID = q.Message.Chat.ID
	}
	if data == "rv:x" {
		b.AnswerCallback(ctx, q.ID, "dismissed")
		return true
	}
	// data == "rv:<repo>:<number>"
	parts := strings.SplitN(data, ":", 3)
	if len(parts) < 3 {
		b.AnswerCallback(ctx, q.ID, "bad payload")
		return true
	}
	repo := parts[1]
	number, err := strconv.Atoi(parts[2])
	if err != nil || number <= 0 {
		b.AnswerCallback(ctx, q.ID, "bad payload")
		return true
	}
	r, reason := b.prReviewer()
	if r == nil {
		b.AnswerCallback(ctx, q.ID, "review unavailable")
		if chatID != 0 {
			_ = b.Send(ctx, chatID, reason)
		}
		return true
	}
	body := ""
	if q.Message != nil {
		body = extractFenced(q.Message.Text)
	}
	if strings.TrimSpace(body) == "" {
		b.AnswerCallback(ctx, q.ID, "✗ could not read the draft")
		return true
	}
	postCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := r.CommentPR(postCtx, repo, number, body); err != nil {
		b.AnswerCallback(ctx, q.ID, "✗ "+oneLine(err.Error(), 180))
		if chatID != 0 {
			_ = b.Send(ctx, chatID,
				fmt.Sprintf("✗ posting review to %s#%d failed: %v", repo, number, err))
		}
		return true
	}
	b.AnswerCallback(ctx, q.ID, "✓ posted")
	logx.Info("telegram_bot.review_posted", "repo", repo, "pr", number)
	if chatID != 0 {
		_ = b.Send(ctx, chatID,
			fmt.Sprintf("✓ review posted as a comment on %s#%d", repo, number))
	}
	return true
}

// clampUTF8 truncates s to at most max bytes without splitting a rune —
// Telegram rejects messages that aren't valid UTF-8.
func clampUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// extractFenced returns the text between the two reviewFence lines.
func extractFenced(s string) string {
	first := strings.Index(s, reviewFence)
	if first < 0 {
		return ""
	}
	rest := strings.TrimPrefix(s[first+len(reviewFence):], "\n")
	last := strings.Index(rest, reviewFence)
	if last < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:last])
}
