package cmd

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/harisaginting/goon/internal/githost"
	"github.com/harisaginting/goon/internal/llm"
	"github.com/harisaginting/goon/internal/memory"
	"github.com/harisaginting/goon/internal/review"
	"github.com/harisaginting/goon/internal/tools"
)

// runReviewPRs implements `goon review-prs` — fetches every PR where the
// configured git host says the current user is a requested reviewer,
// drafts an LLM review for each, and prints the drafts (optionally also
// pushing them to Telegram).
//
//	goon review-prs [--watch] [--interval=15m] [--telegram] [--all]
//
// Dedup state is shared with the daemon's auto loop, so by default a PR
// whose diff hasn't changed since the last draft is skipped — which
// makes `--watch` safe to run as a cron-free standalone scheduler.
// --all ignores dedup for a one-off "show me everything" pass.
func runReviewPRs(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("review-prs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	watch := fs.Bool("watch", false, "keep running, repeating every --interval")
	interval := fs.Duration("interval", 15*time.Minute, "delay between passes in --watch mode")
	toTelegram := fs.Bool("telegram", false, "also send each draft to TELEGRAM_CHAT_ID")
	all := fs.Bool("all", false, "draft a review for every review-requested PR, ignoring dedup")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: goon review-prs [--watch] [--interval=15m] [--telegram] [--all]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	runner, err := newReviewRunner()
	if err != nil {
		return err
	}

	pass := func() error {
		passCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		drafts, err := runner.PendingReviews(passCtx, *all)
		if err != nil {
			return err
		}
		if len(drafts) == 0 {
			fmt.Fprintln(stdout, "no PRs awaiting your review.")
			return nil
		}
		for _, d := range drafts {
			fmt.Fprintln(stdout, strings.Repeat("─", 56))
			fmt.Fprintln(stdout, review.FormatDraftText(d))
			if *toTelegram {
				if err := sendTelegram(passCtx, review.FormatDraftText(d)); err != nil {
					fmt.Fprintf(stderr, "telegram: %v\n", err)
				}
			}
			runner.MarkReviewed(d)
		}
		fmt.Fprintln(stdout, strings.Repeat("─", 56))
		fmt.Fprintf(stdout, "%d review draft(s) ready.\n", len(drafts))
		return nil
	}

	if !*watch {
		return pass()
	}
	fmt.Fprintf(stdout, "watch mode — a pass every %s. Ctrl-C to stop.\n", *interval)
	for {
		if err := pass(); err != nil {
			fmt.Fprintf(stderr, "pass error: %v\n", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(*interval):
		}
	}
}

// runNotifications implements `goon notifications` — fetches the current
// user's review-request and mention notifications from the git host and
// prints them (optionally also pushing to Telegram). With more than one
// new item it includes an LLM-written digest.
//
//	goon notifications [--watch] [--interval=15m] [--telegram] [--all]
func runNotifications(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("notifications", flag.ContinueOnError)
	fs.SetOutput(stderr)
	watch := fs.Bool("watch", false, "keep running, repeating every --interval")
	interval := fs.Duration("interval", 15*time.Minute, "delay between passes in --watch mode")
	toTelegram := fs.Bool("telegram", false, "also send the summary to TELEGRAM_CHAT_ID")
	all := fs.Bool("all", false, "show every current notification, ignoring already-forwarded state")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: goon notifications [--watch] [--interval=15m] [--telegram] [--all]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	runner, err := newReviewRunner()
	if err != nil {
		return err
	}

	pass := func() error {
		passCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		batch, err := runner.Notifications(passCtx, *all)
		if err != nil {
			return err
		}
		if len(batch.Items) == 0 {
			fmt.Fprintln(stdout, "no new notifications.")
			return nil
		}
		text := review.FormatNotifText(batch)
		fmt.Fprintln(stdout, text)
		if *toTelegram {
			if err := sendTelegram(passCtx, "🔔 "+text); err != nil {
				fmt.Fprintf(stderr, "telegram: %v\n", err)
			}
		}
		runner.MarkNotified(batch)
		return nil
	}

	if !*watch {
		return pass()
	}
	fmt.Fprintf(stdout, "watch mode — a pass every %s. Ctrl-C to stop.\n", *interval)
	for {
		if err := pass(); err != nil {
			fmt.Fprintf(stderr, "pass error: %v\n", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(*interval):
		}
	}
}

// newReviewRunner builds a review.Runner from environment variables: the
// git host, the LLM provider, and the shared memory store (for dedup).
func newReviewRunner() (*review.Runner, error) {
	host, err := githost.NewFromEnv()
	if err != nil {
		return nil, fmt.Errorf("git host: %w", err)
	}
	prov, err := llm.NewFromEnv()
	if err != nil {
		return nil, onboardingError(err)
	}
	mem, err := memory.New(os.Getenv("GOON_MEMORY_PATH"))
	if err != nil {
		mem = memory.Disabled()
	}
	return review.New(review.Options{Host: host, LLM: prov, Memory: mem}), nil
}

// sendTelegram pushes text to TELEGRAM_CHAT_ID via the outbound Telegram
// tool, splitting on line boundaries to stay under Telegram's per-
// message size limit.
func sendTelegram(ctx context.Context, text string) error {
	tg := tools.NewTelegramFromEnv()
	const maxLen = 3800
	for len(text) > 0 {
		chunk := text
		if len(chunk) > maxLen {
			cut := strings.LastIndex(chunk[:maxLen], "\n")
			if cut <= 0 {
				cut = maxLen
			}
			chunk = chunk[:cut]
		}
		if _, err := tg.Run(ctx, map[string]string{"op": "send_message", "text": chunk}); err != nil {
			return err
		}
		text = strings.TrimPrefix(text[len(chunk):], "\n")
	}
	return nil
}
