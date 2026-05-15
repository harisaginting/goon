// Package telegram — repos.go is the /repos picker for managing
// GOON_REVIEW_REPOS interactively. Lists every repo the configured
// git host can see and offers a one-tap "include / exclude" toggle
// per repo. Persists the selection straight to ~/.config/goon/.env
// via internal/envstore, so the daemon's next /prs call uses it
// without any restart.
package telegram

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/harisaginting/goon/internal/envstore"
	"github.com/harisaginting/goon/internal/githost"
)

const reposEnvKey = "GOON_REVIEW_REPOS"

// cmdRepos renders the picker as a series of inline-keyboard messages.
// Each tap toggles inclusion; the keyboard re-renders with a ✓ for
// included repos.
func (b *Bot) cmdRepos(ctx context.Context, chatID int64, args []string) {
	if b.opts.Host == nil {
		_ = b.Send(ctx, chatID, "✗ no git host configured (set GOON_GIT_HOST=github|bitbucket and the matching token)")
		return
	}
	lister, ok := b.opts.Host.(githost.RepoLister)
	if !ok {
		_ = b.Send(ctx, chatID, "✗ this git host doesn't support repo discovery yet")
		return
	}

	// Optional ?filter=substring (passed as first arg) to narrow huge lists.
	filter := strings.ToLower(strings.TrimSpace(strings.Join(args, " ")))

	lsCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	repos, err := lister.ListRepos(lsCtx)
	if err != nil {
		_ = b.Send(ctx, chatID, "✗ list repos failed: "+err.Error())
		return
	}
	if filter != "" {
		filtered := repos[:0]
		for _, r := range repos {
			if strings.Contains(strings.ToLower(r.Slug), filter) {
				filtered = append(filtered, r)
			}
		}
		repos = filtered
	}
	if len(repos) == 0 {
		_ = b.Send(ctx, chatID, "no repos visible to the token. Check the token's scopes.")
		return
	}

	included := currentReviewRepos()
	header := fmt.Sprintf("📂 %d repo(s) visible — tap to toggle. Currently following %d.\n", len(repos), len(included))
	_ = b.Send(ctx, chatID, header)

	// Telegram caps messages + keyboards. 50 buttons per message is
	// well within limits and reads cleanly.
	const pageSize = 50
	for start := 0; start < len(repos); start += pageSize {
		end := start + pageSize
		if end > len(repos) {
			end = len(repos)
		}
		rows := reposToButtons(repos[start:end], included)
		title := fmt.Sprintf("Repos %d–%d", start+1, end)
		_ = b.SendWithButtons(ctx, chatID, title, rows)
	}

	// Footer with bulk actions.
	bulk := [][]InlineButton{
		{
			{Text: "✓ select all", CallbackData: "rsa"},
			{Text: "✗ clear", CallbackData: "rsc"},
		},
	}
	_ = b.SendWithButtons(ctx, chatID, "Bulk actions:", bulk)
}

// reposToButtons builds a 1-column keyboard. Each button has the
// repo slug with a ✓ when included, ○ when not.
func reposToButtons(repos []githost.Repo, included map[string]bool) [][]InlineButton {
	out := make([][]InlineButton, 0, len(repos))
	for _, r := range repos {
		mark := "○"
		if included[r.Slug] {
			mark = "✓"
		}
		// Trim the label so it fits within Telegram's button width.
		label := r.Slug
		if len(label) > 50 {
			label = "…" + label[len(label)-49:]
		}
		out = append(out, []InlineButton{
			{Text: mark + " " + label, CallbackData: "rt:" + r.Slug},
		})
	}
	return out
}

// callbackHandleRepos dispatches the inline-keyboard taps:
//
//	rt:<slug>   → toggle inclusion of <slug>
//	rsa         → select all (writes every visible repo)
//	rsc         → clear (empties GOON_REVIEW_REPOS)
//
// Wired into interactive.go's handleCallback router.
func (b *Bot) callbackHandleRepos(ctx context.Context, q *CallbackQuery) bool {
	if q == nil || q.Data == "" {
		return false
	}
	chatID := int64(0)
	if q.Message != nil {
		chatID = q.Message.Chat.ID
	}
	switch {
	case strings.HasPrefix(q.Data, "rt:"):
		slug := githost.NormalizeRepoSlug(q.Data[len("rt:"):])
		if slug == "" {
			b.AnswerCallback(ctx, q.ID, "")
			return true
		}
		current := currentReviewRepos()
		if current[slug] {
			delete(current, slug)
		} else {
			current[slug] = true
		}
		if err := saveReviewRepos(current); err != nil {
			b.AnswerCallback(ctx, q.ID, "✗ "+oneLine(err.Error(), 100))
			return true
		}
		b.AnswerCallback(ctx, q.ID, fmt.Sprintf("✓ %d repo(s) selected", len(current)))
		return true
	case q.Data == "rsa":
		// Pull the list again, write every slug.
		if b.opts.Host == nil {
			b.AnswerCallback(ctx, q.ID, "✗ no git host")
			return true
		}
		lister, ok := b.opts.Host.(githost.RepoLister)
		if !ok {
			b.AnswerCallback(ctx, q.ID, "✗ host doesn't support listing")
			return true
		}
		lsCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		repos, err := lister.ListRepos(lsCtx)
		if err != nil {
			b.AnswerCallback(ctx, q.ID, "✗ "+oneLine(err.Error(), 100))
			return true
		}
		all := make(map[string]bool, len(repos))
		for _, r := range repos {
			all[r.Slug] = true
		}
		if err := saveReviewRepos(all); err != nil {
			b.AnswerCallback(ctx, q.ID, "✗ "+oneLine(err.Error(), 100))
			return true
		}
		b.AnswerCallback(ctx, q.ID, "✓ all selected")
		_ = b.Send(ctx, chatID, fmt.Sprintf("✓ now following %d repo(s)", len(all)))
		return true
	case q.Data == "rsc":
		if err := envstore.Unset(reposEnvKey); err != nil {
			b.AnswerCallback(ctx, q.ID, "✗ "+oneLine(err.Error(), 100))
			return true
		}
		_ = os.Unsetenv(reposEnvKey)
		b.AnswerCallback(ctx, q.ID, "✓ cleared")
		_ = b.Send(ctx, chatID, "✓ cleared — /prs will fall back to auto-discovery")
		return true
	}
	return false
}

// currentReviewRepos parses GOON_REVIEW_REPOS into a set keyed by
// normalized slug. Empty when the env var is unset.
func currentReviewRepos() map[string]bool {
	out := map[string]bool{}
	for _, s := range strings.Split(os.Getenv(reposEnvKey), ",") {
		s2 := githost.NormalizeRepoSlug(s)
		if s2 != "" {
			out[s2] = true
		}
	}
	return out
}

// saveReviewRepos persists the set to env + .env file. Stable order
// (alphabetical) so successive saves produce stable diffs in source
// control if the user happens to commit their .env (please don't).
func saveReviewRepos(set map[string]bool) error {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	value := strings.Join(keys, ",")
	if value == "" {
		_ = os.Unsetenv(reposEnvKey)
		return envstore.Unset(reposEnvKey)
	}
	if err := envstore.Set(reposEnvKey, value); err != nil {
		return err
	}
	return os.Setenv(reposEnvKey, value)
}
