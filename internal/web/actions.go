// Package web — actions.go houses the direct CRUD endpoints for
// Jira and the configured git host. These work without any LLM
// provider configured — they go straight to the underlying board /
// host API. The chat agent (chat.go) sits on top of these for the
// LLM-mediated flow, but the user can also drive every action by
// clicking buttons.
package web

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/harisaginting/goon/internal/boards"
	"github.com/harisaginting/goon/internal/githost"
	"github.com/harisaginting/goon/internal/memory"
	"github.com/harisaginting/goon/internal/repository"
	"github.com/harisaginting/goon/internal/review"
	"github.com/harisaginting/goon/internal/safety"
	"github.com/harisaginting/goon/internal/util"
)

// --- Jira (board) actions --------------------------------------------------

// handleTicketComment posts a comment on a Jira ticket. Form: key,
// body. Returns a small confirmation fragment.
func (s *Server) handleTicketComment(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.opts.Board == nil {
		boardMissing(w, "comment")
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	key := strings.TrimSpace(r.FormValue("key"))
	body := strings.TrimSpace(r.FormValue("body"))
	if key == "" || body == "" {
		fragErr(w, "key and body required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := s.opts.Board.Comment(ctx, key, body); err != nil {
		fragErr(w, "comment failed: "+err.Error())
		return
	}
	w.Header().Set("HX-Trigger", "ticketsChanged")
	s.events.Publish("ticketsChanged")
	fragOK(w, "commented on "+key)
}

// handleTicketTransition changes a ticket's status. Form: key, status.
//
// When the board supports the real workflow names (boards.TransitionResolver),
// the status field is treated as a board-defined name (e.g. "Ready to Test")
// and routed via TransitionByName. Otherwise it falls back to the canonical
// enum mapping. The enum path is the legacy bug we hit on Jira workflows with
// custom statuses — "Ready to Test" was matched as substring "ready" → Open →
// silently routed to Backlog. Resolver-first kills that whole class of bug.
func (s *Server) handleTicketTransition(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.opts.Board == nil {
		boardMissing(w, "transition")
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	key := strings.TrimSpace(r.FormValue("key"))
	statusRaw := strings.TrimSpace(r.FormValue("status"))
	if key == "" || statusRaw == "" {
		fragErr(w, "key and status required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if resolver, ok := s.opts.Board.(boards.TransitionResolver); ok {
		applied, err := resolver.TransitionByName(ctx, key, statusRaw)
		if err != nil {
			fragErr(w, "transition failed: "+err.Error())
			return
		}
		w.Header().Set("HX-Trigger", "ticketsChanged")
		s.events.Publish("ticketsChanged")
		fragOK(w, fmt.Sprintf("%s → %s", key, applied))
		return
	}
	// Legacy enum fallback for boards that don't resolve real names
	// (e.g. GitHub Issues, mock). Honors the same canonical 5 statuses.
	target := boards.MapStatus(strings.ToLower(statusRaw))
	if target == boards.StatusUnknown {
		fragErr(w, "unknown status: "+statusRaw+" (use open|in_progress|in_review|blocked|done)")
		return
	}
	if err := s.opts.Board.Transition(ctx, key, target); err != nil {
		fragErr(w, "transition failed: "+err.Error())
		return
	}
	w.Header().Set("HX-Trigger", "ticketsChanged")
	s.events.Publish("ticketsChanged")
	fragOK(w, fmt.Sprintf("%s → %s", key, target))
}

// handleTicketTransitions returns the real workflow status names available
// for a ticket, as <option> tags ready for hx-swap into a <select>. When
// the board doesn't implement TransitionResolver, falls back to the
// canonical 5-status enum so the dropdown is never empty.
func (s *Server) handleTicketTransitions(w http.ResponseWriter, r *http.Request) {
	if s.opts.Board == nil {
		boardMissing(w, "transitions")
		return
	}
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if key == "" {
		fragErr(w, "key required")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	if resolver, ok := s.opts.Board.(boards.TransitionResolver); ok {
		names, err := resolver.ListTransitions(ctx, key)
		if err == nil && len(names) > 0 {
			for _, n := range names {
				fmt.Fprintf(w, `<option value="%s">%s</option>`,
					html.EscapeString(n), html.EscapeString(n))
			}
			return
		}
		// Fall through to the canonical enum if the resolver fails or
		// returns no transitions — better than an empty dropdown.
	}
	for _, opt := range []struct{ v, label string }{
		{"open", "open"},
		{"in_progress", "in progress"},
		{"in_review", "in review"},
		{"blocked", "blocked"},
		{"done", "done"},
	} {
		fmt.Fprintf(w, `<option value="%s">%s</option>`, opt.v, opt.label)
	}
}

// handleTicketIgnore opts a ticket out of the daemon workflow. The
// daemon's nextTicket() filter respects the ignore set, so the next
// poll won't pick this key. Fires ticketsChanged so the row visually
// flips to the muted/ignored state immediately.
func (s *Server) handleTicketIgnore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		fragErr(w, "invalid form: "+err.Error())
		return
	}
	key := strings.TrimSpace(r.FormValue("key"))
	if key == "" {
		fragErr(w, "key required")
		return
	}
	s.opts.Memory.IgnoreTicket(key)
	w.Header().Set("HX-Trigger", "ticketsChanged")
	s.events.Publish("ticketsChanged")
	fragOK(w, key+" ignored — daemon will skip it")
}

// handleTicketUnignore claims a previously-ignored ticket back into
// the daemon workflow. The next poll cycle is free to pick it.
func (s *Server) handleTicketUnignore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		fragErr(w, "invalid form: "+err.Error())
		return
	}
	key := strings.TrimSpace(r.FormValue("key"))
	if key == "" {
		fragErr(w, "key required")
		return
	}
	s.opts.Memory.UnignoreTicket(key)
	w.Header().Set("HX-Trigger", "ticketsChanged")
	s.events.Publish("ticketsChanged")
	fragOK(w, key+" claimed back — daemon will consider it on the next poll")
}

// handleTicketEdit updates one of title / description / labels.
// Form: key, field, value.
func (s *Server) handleTicketEdit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.opts.Board == nil {
		boardMissing(w, "edit")
		return
	}
	updater, ok := s.opts.Board.(boards.Updater)
	if !ok {
		fragErr(w, "the configured board does not support edits")
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	key := strings.TrimSpace(r.FormValue("key"))
	field := strings.ToLower(strings.TrimSpace(r.FormValue("field")))
	value := r.FormValue("value")
	if key == "" || field == "" {
		fragErr(w, "key and field required")
		return
	}
	patch := boards.TicketPatch{}
	switch field {
	case "title", "summary":
		t := value
		patch.Title = &t
	case "desc", "description":
		d := value
		patch.Description = &d
	case "labels":
		raw := strings.TrimSpace(value)
		if raw == "" || raw == "-" {
			patch.Labels = []string{}
		} else {
			parts := strings.Split(raw, ",")
			out := make([]string, 0, len(parts))
			for _, p := range parts {
				p = strings.TrimSpace(p)
				if p != "" {
					out = append(out, p)
				}
			}
			patch.Labels = out
		}
	default:
		fragErr(w, "unknown field: "+field+" (use title|desc|labels)")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := updater.Update(ctx, key, patch); err != nil {
		fragErr(w, "edit failed: "+err.Error())
		return
	}
	w.Header().Set("HX-Trigger", "ticketsChanged")
	s.events.Publish("ticketsChanged")
	fragOK(w, "updated "+key+" · "+field)
}

// --- Git host (PR) actions -------------------------------------------------

// handlePRList returns a rendered HTML list of open PRs from the
// configured host. The fallback chain (GOON_REVIEW_REPOS → discover-
// all) lives in the host adapters, so an empty Options.Host renders
// the unconfigured hint while a populated one always shows PRs even
// without configuration.
func (s *Server) handlePRList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if s.opts.Host == nil {
		w.Write([]byte(`<div class="rounded-md bg-amber-500/10 border border-amber-500/30 px-3 py-2 text-sm text-amber-700 dark:text-amber-400">
			No git host configured. Set GOON_GIT_HOST=github (or gitlab|bitbucket) and the matching token.
		</div>`))
		return
	}
	reviewer, ok := s.opts.Host.(githost.PRReviewer)
	if !ok {
		fmt.Fprintf(w, `<div class="rounded-md bg-amber-500/10 border border-amber-500/30 px-3 py-2 text-sm text-amber-700 dark:text-amber-400">
			PR management is not supported on %s yet.
		</div>`, html.EscapeString(s.opts.Host.Name()))
		return
	}

	// Optional ?repo= filter pinches the list to one repo. Used by the
	// per-row "see related PRs" affordance and the new Repositories tab
	// detail panel. When set, the "manage which repos goon follows"
	// expander is suppressed — that's a tab-level global control, it
	// doesn't belong inside one specific repo's view.
	var repos []string
	embedded := false
	if rp := strings.TrimSpace(r.URL.Query().Get("repo")); rp != "" {
		repos = []string{rp}
		embedded = true
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	prs, err := reviewer.ListPRs(ctx, repos)
	if err != nil {
		fmt.Fprintf(w, `<div class="rounded-md bg-rose-500/10 border border-rose-500/30 px-3 py-2 text-sm text-rose-700 dark:text-rose-400">list failed: %s</div>`, html.EscapeString(err.Error()))
		return
	}
	if len(prs) == 0 {
		w.Write([]byte(`<div class="rounded-xl border border-dashed border-gray-300 dark:border-surface-border bg-gray-50/60 dark:bg-surface-sunken/40 p-6 text-center">
			<div class="text-sm font-medium text-gray-700 dark:text-gray-300">No open PRs</div>
			<div class="mt-1 text-xs text-gray-500">Nothing to review right now in the repos goon is following.</div>
		</div>`))
		if !embedded {
			w.Write([]byte(`<details class="mt-3 rounded-lg border border-gray-200 dark:border-surface-border bg-gray-50/60 dark:bg-surface-sunken/40">
				<summary class="px-4 py-2 cursor-pointer text-xs text-gray-600 dark:text-gray-400 hover:text-accent transition">
					⚙ manage which repos goon follows
				</summary>
				<div class="px-4 pb-4 pt-2"
					hx-get="/fragments/repos-picker" hx-trigger="toggle from:closest details once" hx-swap="innerHTML">
					<div class="text-xs text-gray-500">Loading repo list…</div>
				</div>
			</details>`))
		}
		return
	}
	// Inline picker affordance — collapsed by default so it doesn't
	// dominate the PR list. Clicking the summary fetches the full
	// repo list with checkboxes. Only renders at the top-level (i.e.
	// the global view); the per-repo detail panel gets a clean PR
	// list without this tab-level control.
	if !embedded {
		fmt.Fprint(w, `<details class="mb-3 rounded-lg border border-gray-200 dark:border-surface-border bg-gray-50/60 dark:bg-surface-sunken/40">
			<summary class="px-4 py-2 cursor-pointer text-xs text-gray-600 dark:text-gray-400 hover:text-accent transition">
				⚙ manage which repos goon follows
			</summary>
			<div class="px-4 pb-4 pt-2"
				hx-get="/fragments/repos-picker" hx-trigger="toggle from:closest details once" hx-swap="innerHTML">
				<div class="text-xs text-gray-500">Loading repo list…</div>
			</div>
		</details>`)
	}
	fmt.Fprintf(w, `<div class="space-y-2">`)
	for _, pr := range prs {
		actionID := fmt.Sprintf("pr-actions-%s-%d", strings.ReplaceAll(pr.Repo, "/", "-"), pr.Number)
		title := pr.Title
		if title == "" {
			title = "(untitled)"
		}
		author := pr.Author
		if author == "" {
			author = "—"
		}
		fmt.Fprintf(w, `<div class="rounded-xl border border-gray-200 dark:border-surface-border bg-white dark:bg-surface-raised p-4">
			<div class="flex items-start justify-between gap-3 mb-2">
				<div class="min-w-0 flex-1">
					<a href="%s" target="_blank" rel="noopener" class="font-mono text-xs text-accent hover:underline">%s #%d</a>
					<div class="mt-0.5 text-sm text-gray-800 dark:text-gray-200 truncate" title="%s">%s</div>
					<div class="mt-1 text-[11px] text-gray-500">by @%s</div>
				</div>
			</div>
			<!-- Top row: ONE-CLICK approve + TWO TOGGLES that disclose
			     comment/request-changes forms below. The chevron flips
			     when open so the user can tell at a glance whether
			     they're looking at a closed toggle or an open form. -->
			<div class="flex flex-wrap gap-2">
				<button type="button" onclick="goonPRRowToggle(this,'%s')"
					class="text-xs rounded-md border border-gray-300 dark:border-surface-border px-2 py-1 hover:border-accent hover:text-accent transition inline-flex items-center gap-1"
					data-disclose="closed">
					<span data-disclose-icon>▸</span> write comment
				</button>
				<form hx-post="/api/pr/approve" hx-target="#%s-result" hx-swap="innerHTML" class="m-0 inline-flex">
					<input type="hidden" name="repo" value="%s">
					<input type="hidden" name="number" value="%d">
					<button type="submit" class="text-xs rounded-md bg-emerald-500/15 border border-emerald-500/40 text-emerald-700 dark:text-emerald-400 px-2 py-1 font-medium hover:bg-emerald-500/25 transition">✓ approve</button>
				</form>
				<button type="button" onclick="goonPRRowToggle(this,'%s-rc')"
					class="text-xs rounded-md border border-rose-500/40 text-rose-700 dark:text-rose-400 px-2 py-1 hover:bg-rose-500/10 transition inline-flex items-center gap-1"
					data-disclose="closed">
					<span data-disclose-icon>▸</span> block (request changes)
				</button>
				<span class="text-xs text-gray-400 ml-auto self-center" id="%s-result"></span>
			</div>
			<form hx-post="/api/pr/comment" hx-target="#%s-result" hx-swap="innerHTML" hx-on::after-request="if(event.detail.successful){this.reset();document.getElementById('%s').classList.add('hidden');}"
				class="mt-2 hidden" id="%s">
				<input type="hidden" name="repo" value="%s">
				<input type="hidden" name="number" value="%d">
				<textarea name="body" id="%s-body" rows="3" required placeholder="leave a comment, or click ✨ Draft with AI to have goon read the diff and write one for you…"
					class="w-full font-mono text-xs rounded-md border border-gray-300 dark:border-surface-border bg-white dark:bg-surface px-2 py-1 focus:border-accent focus:ring-1 focus:ring-accent/30 focus:outline-none"></textarea>
				<div class="flex items-center gap-2 mt-1">
					<button type="button"
						hx-get="/api/pr/draft-review?repo=%s&number=%d"
						hx-target="#%s-body"
						hx-swap="innerHTML"
						hx-indicator="#%s-spin"
						class="inline-flex items-center gap-1 text-xs rounded-md border border-accent/40 text-accent px-2.5 py-1 hover:bg-accent/10 transition">✨ Draft with AI</button>
					<span id="%s-spin" class="htmx-indicator text-xs text-gray-500">drafting…</span>
					<div class="flex-1"></div>
					<button type="submit" class="text-xs rounded-md bg-accent text-surface px-3 py-1 font-semibold hover:brightness-110 transition">send →</button>
				</div>
			</form>
			<form hx-post="/api/pr/request-changes" hx-target="#%s-result" hx-swap="innerHTML" hx-on::after-request="if(event.detail.successful){this.reset();document.getElementById('%s-rc').classList.add('hidden');}"
				class="mt-2 hidden" id="%s-rc">
				<input type="hidden" name="repo" value="%s">
				<input type="hidden" name="number" value="%d">
				<textarea name="body" rows="2" required placeholder="why changes are needed…"
					class="w-full font-mono text-xs rounded-md border border-rose-300 dark:border-rose-700 bg-white dark:bg-surface px-2 py-1 focus:border-rose-500 focus:ring-1 focus:ring-rose-500/30 focus:outline-none"></textarea>
				<div class="flex justify-end mt-1"><button type="submit" class="text-xs rounded-md bg-rose-500 text-white px-3 py-1 font-semibold hover:bg-rose-600 transition">send →</button></div>
			</form>
		</div>`,
			html.EscapeString(pr.URL),
			html.EscapeString(pr.Repo), pr.Number,
			html.EscapeString(title), html.EscapeString(title),
			html.EscapeString(author),
			actionID,
			actionID,
			html.EscapeString(pr.Repo), pr.Number,
			actionID,
			actionID,
			actionID, actionID, actionID,
			html.EscapeString(pr.Repo), pr.Number,
			actionID,
			html.EscapeString(pr.Repo), pr.Number,
			actionID, actionID, actionID,
			actionID, actionID, actionID,
			html.EscapeString(pr.Repo), pr.Number,
		)
	}
	fmt.Fprint(w, `</div>`)
}

// handlePRComment posts a top-level comment on a PR.
func (s *Server) handlePRComment(w http.ResponseWriter, r *http.Request) {
	s.prAction(w, r, "comment", func(rev githost.PRReviewer, ctx context.Context, repo string, num int, body string) error {
		return rev.CommentPR(ctx, repo, num, body)
	})
}

// handlePRApprove posts an approval review. Body is optional.
func (s *Server) handlePRApprove(w http.ResponseWriter, r *http.Request) {
	s.prAction(w, r, "approve", func(rev githost.PRReviewer, ctx context.Context, repo string, num int, body string) error {
		return rev.ApprovePR(ctx, repo, num, body)
	})
}

// handlePRDraftReview fetches the PR's diff, asks the LLM for a tight
// review using the same prompt the chat / auto-review loop uses, and
// returns the draft as HTML-escaped text suitable for hx-swap=innerHTML
// directly into the comment textarea — the user can then edit and
// submit the existing /api/pr/comment form to post it. Errors land in
// the textarea too (prefixed "✗"), so the user always sees what
// happened without a separate error channel.
func (s *Server) handlePRDraftReview(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	writeMsg := func(msg string) {
		fmt.Fprint(w, html.EscapeString(msg))
	}
	if s.opts.Host == nil {
		writeMsg("✗ no git host configured — set GOON_GIT_HOST + token first")
		return
	}
	if s.opts.LLM == nil {
		writeMsg("✗ no LLM provider configured — drafting needs one (set OPENAI_API_KEY / ANTHROPIC_API_KEY / GEMINI_API_KEY / etc.)")
		return
	}
	rev, ok := s.opts.Host.(githost.PRReviewer)
	if !ok {
		writeMsg("✗ host " + s.opts.Host.Name() + " does not support PR review")
		return
	}
	repo := strings.TrimSpace(r.URL.Query().Get("repo"))
	numStr := strings.TrimSpace(r.URL.Query().Get("number"))
	if repo == "" || numStr == "" {
		writeMsg("✗ repo and number required")
		return
	}
	num, err := strconv.Atoi(numStr)
	if err != nil || num <= 0 {
		writeMsg("✗ invalid PR number")
		return
	}
	// Generous timeout — fetch the diff AND run an LLM call. Matches
	// the chat pr_review budget so users have a consistent ceiling.
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	pr, diff, err := rev.GetPRDetails(ctx, repo, num)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			writeMsg("✗ timed out fetching the diff after 3 minutes — diff likely very large; try again or post a focused comment manually")
			return
		}
		writeMsg("✗ fetch failed: " + err.Error())
		return
	}
	if strings.TrimSpace(diff) == "" {
		writeMsg("✗ empty diff — nothing to review")
		return
	}
	draft, err := review.DraftReview(ctx, s.opts.LLM, pr, diff)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			writeMsg("✗ model timed out drafting the review — try again or focus the request")
			return
		}
		writeMsg("✗ draft failed: " + err.Error())
		return
	}
	// Plain text into the textarea (innerHTML preserves newlines in
	// <textarea>). html.EscapeString keeps any stray angle-brackets
	// from the LLM from being treated as markup.
	writeMsg(draft)
}

// handlePRRequestChanges submits a request-changes review with the
// supplied body explaining what needs changing.
func (s *Server) handlePRRequestChanges(w http.ResponseWriter, r *http.Request) {
	s.prAction(w, r, "request changes", func(rev githost.PRReviewer, ctx context.Context, repo string, num int, body string) error {
		return rev.RequestChangesPR(ctx, repo, num, body)
	})
}

// prAction is the shared body-parsing + auth glue for the three PR
// mutation endpoints. fn is the per-action API call.
func (s *Server) prAction(w http.ResponseWriter, r *http.Request, label string,
	fn func(githost.PRReviewer, context.Context, string, int, string) error) {

	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.opts.Host == nil {
		hostMissing(w, label)
		return
	}
	rev, ok := s.opts.Host.(githost.PRReviewer)
	if !ok {
		fragErr(w, label+" not supported on "+s.opts.Host.Name())
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	repo := strings.TrimSpace(r.FormValue("repo"))
	numStr := strings.TrimSpace(r.FormValue("number"))
	body := r.FormValue("body")
	if repo == "" || numStr == "" {
		fragErr(w, "repo and number required")
		return
	}
	num, err := strconv.Atoi(numStr)
	if err != nil {
		fragErr(w, "invalid PR number")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := fn(rev, ctx, repo, num, body); err != nil {
		fragErr(w, label+" failed: "+err.Error())
		return
	}
	w.Header().Set("HX-Trigger", "prsChanged")
	s.events.Publish("prsChanged")
	fragOK(w, label+" recorded")
}

// --- Repositories tab (repo-centric view) --------------------------------
//
// Frame: REPOSITORY.md is the source of truth for "which repos does
// goon work on." We list every entry as a row, plus any repos the git
// host returned PRs for that aren't yet tracked (so the user has an
// "Add to REPOSITORY.md" affordance for stragglers). Each row shows
// a count badge of open PRs, the local-checkout status (✓ cloned vs
// ⚠ not cloned), and an expand affordance that lazy-loads the per-
// repo detail panel.
//
// Why this shape: the old "Pull requests" tab was a flat firehose
// across all repos, which buried the user's natural mental model
// ("I work in N repos; show me each one and what's in it"). The
// repo-centric frame also gives map-to-local + clone-here a natural
// home — both are per-repo operations, but had no surface before.

// repoSummary aggregates everything the Repositories list needs to
// render a single row: REPOSITORY.md metadata, open-PR bucket, and
// local-path status.
type repoSummary struct {
	Remote      string // canonical slug ("owner/repo") or full URL
	DisplayName string // short label for the row header
	Local       string // resolved local path; empty when not mapped
	Notes       string // free-text column from REPOSITORY.md
	OpenPRs     int    // count of open PRs returned by the git host
	Cloned      bool   // true when Local exists on disk
	Tracked     bool   // true when present in REPOSITORY.md
}

// handleRepositoryList is the top-level Repositories fragment. Returns
// HTML — one row per repo (REPOSITORY.md ∪ any repo the host returned
// a PR for), sorted alphabetically. Wraps the rows in a typeahead
// filter so a 100-repo org isn't a scroll-of-shame.
func (s *Server) handleRepositoryList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// Side A: REPOSITORY.md — the user-curated list.
	tracked, _ := repository.Read()

	// Side B: PRs from the host, bucketed by repo. Empty when no host
	// is configured (the page still renders, just without counts and
	// without the "Detected (not yet tracked)" bottom section).
	prsByRepo := map[string]int{}
	hostHasPRReviewer := false
	if s.opts.Host != nil {
		if reviewer, ok := s.opts.Host.(githost.PRReviewer); ok {
			hostHasPRReviewer = true
			ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			defer cancel()
			if prs, err := reviewer.ListPRs(ctx, nil); err == nil {
				for _, pr := range prs {
					if pr.Repo == "" {
						continue
					}
					prsByRepo[strings.ToLower(pr.Repo)]++
				}
			}
		}
	}

	// Merge: every REPOSITORY.md entry becomes a row (tracked=true);
	// any PR-bucket key NOT in REPOSITORY.md becomes a "detected"
	// row at the bottom (tracked=false).
	rows := make([]repoSummary, 0, len(tracked)+len(prsByRepo))
	seen := map[string]bool{}
	for _, e := range tracked {
		key := strings.ToLower(e.Remote)
		seen[key] = true
		local := e.Resolve()
		cloned := false
		if local != "" {
			if st, err := os.Stat(local); err == nil && st.IsDir() {
				cloned = true
			}
		}
		rows = append(rows, repoSummary{
			Remote:      e.Remote,
			DisplayName: e.Name(),
			Local:       local,
			Notes:       e.Notes,
			OpenPRs:     prsByRepo[key],
			Cloned:      cloned,
			Tracked:     true,
		})
	}
	// Detected stragglers (host returned PRs but not in REPOSITORY.md).
	for k, n := range prsByRepo {
		if seen[k] {
			continue
		}
		rows = append(rows, repoSummary{
			Remote:      k,
			DisplayName: shortRepoName(k),
			OpenPRs:     n,
			Tracked:     false,
		})
	}
	// Sort: tracked first (alpha), then detected (alpha).
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Tracked != rows[j].Tracked {
			return rows[i].Tracked
		}
		return strings.ToLower(rows[i].DisplayName) < strings.ToLower(rows[j].DisplayName)
	})

	if len(rows) == 0 {
		// Genuine empty state — no REPOSITORY.md and no host PRs.
		w.Write([]byte(`<div class="rounded-xl border border-dashed border-accent/30 bg-surface-raised/40 p-8 text-center">
			<div class="text-sm font-semibold text-white">No repositories yet</div>
			<div class="mt-1 text-xs text-muted max-w-md mx-auto">
				Add one via <code class="font-mono text-accent">goon repo add &lt;remote&gt; &lt;local&gt;</code> or set <code class="font-mono text-accent">GOON_GIT_HOST</code> on the Setup tab so goon can list your repos.
			</div>
		</div>`))
		return
	}

	// Tab-level "manage which repos goon follows" control. Lives at
	// the top of the Repositories page (NOT inside per-repo detail
	// panels) because the GOON_REVIEW_REPOS scope is a global filter
	// — same reason the search bar lives above the rows, not inside
	// one of them. Collapsed by default so the row list dominates.
	if hostHasPRReviewer {
		fmt.Fprint(w, `<details class="mb-3 rounded-lg border border-surface-border bg-surface-raised/40">
			<summary class="px-4 py-2 cursor-pointer text-xs text-muted hover:text-accent transition flex items-center gap-2">
				<span>⚙</span>
				<span>manage which repos goon follows</span>
				<span class="text-[10px] text-muted/70">(global · sets GOON_REVIEW_REPOS)</span>
			</summary>
			<div class="px-4 pb-4 pt-2"
				hx-get="/fragments/repos-picker" hx-trigger="toggle from:closest details once" hx-swap="innerHTML">
				<div class="text-xs text-muted">Loading repo list…</div>
			</div>
		</details>`)
	}

	// Stats strip — total tracked / total open PRs / how many are
	// still uncloned. Lets the user calibrate at a glance.
	var totalPRs, totalCloned, totalTracked int
	for _, r := range rows {
		totalPRs += r.OpenPRs
		if r.Tracked {
			totalTracked++
		}
		if r.Cloned {
			totalCloned++
		}
	}
	fmt.Fprintf(w, `<div class="flex flex-wrap items-center gap-3 mb-4 text-xs text-muted">
		<span><span class="text-white font-semibold">%d</span> tracked</span>
		<span>·</span>
		<span><span class="text-accent font-semibold">%d</span> open PR%s</span>
		<span>·</span>
		<span><span class="text-emerald-400 font-semibold">%d</span> cloned locally</span>
	</div>`, totalTracked, totalPRs, pluralS(totalPRs), totalCloned)

	// Typeahead filter — same pattern as the question-card picker.
	if len(rows) > 8 {
		fmt.Fprint(w, `<input type="text" placeholder="filter repos by name…" autocomplete="off"
			oninput="goonRepoFilter(this.value)"
			class="w-full mb-3 rounded-lg border border-surface-border bg-surface px-3 py-1.5 text-sm focus:border-accent focus:outline-none">`)
	}

	fmt.Fprint(w, `<div class="space-y-2" id="repo-list">`)
	prevTracked := true
	for _, row := range rows {
		// Divider between Tracked and Detected sections.
		if prevTracked && !row.Tracked {
			fmt.Fprint(w, `<div class="pt-3 pb-1 text-[11px] uppercase tracking-wider text-muted/70">Detected · not tracked in REPOSITORY.md</div>`)
			prevTracked = false
		}
		renderRepoRow(w, row, hostHasPRReviewer)
	}
	fmt.Fprint(w, `</div>`)

	// Filter JS — re-use the same hidden-toggle pattern as
	// renderRepoPickButtons.
	fmt.Fprint(w, `<script>
		window.goonRepoFilter = window.goonRepoFilter || function(q) {
			var qq = (q||'').toLowerCase().trim();
			document.querySelectorAll('#repo-list [data-repo-row]').forEach(function(el){
				var name = (el.getAttribute('data-repo-name')||'').toLowerCase();
				el.style.display = (!qq || name.indexOf(qq) >= 0) ? '' : 'none';
			});
		};
	</script>`)
}

// shortRepoName returns the basename of a repo slug ("owner/repo" →
// "repo") for use as a display label. Falls back to the input when
// there's no slash.
func shortRepoName(s string) string {
	s = strings.Trim(s, "/")
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// renderRepoRow renders one collapsible card for the Repositories
// list. The card header shows the slug + count bubble + local
// status; clicking expands the per-repo detail panel via htmx.
func renderRepoRow(w http.ResponseWriter, row repoSummary, hostHasPRReviewer bool) {
	prsBadge := `<span class="text-[11px] text-muted">no open PRs</span>`
	if row.OpenPRs > 0 {
		prsBadge = fmt.Sprintf(`<span class="inline-flex items-center gap-1 rounded-full bg-accent/15 text-accent border border-accent/30 px-2 py-0.5 text-[11px] font-semibold">%d open PR%s</span>`,
			row.OpenPRs, pluralS(row.OpenPRs))
	} else if !hostHasPRReviewer {
		// No host means we never asked — don't lie with "no open PRs".
		prsBadge = `<span class="text-[11px] text-muted/60">PRs: (no git host)</span>`
	}
	localBadge := ""
	switch {
	case !row.Tracked:
		localBadge = `<span class="text-[11px] text-muted/70">untracked</span>`
	case row.Local == "":
		localBadge = `<span class="inline-flex items-center gap-1 rounded-full bg-amber-500/15 text-amber-700 dark:text-amber-400 border border-amber-500/40 px-2 py-0.5 text-[11px]">⚠ no local path</span>`
	case row.Cloned:
		localBadge = fmt.Sprintf(`<span class="inline-flex items-center gap-1 rounded-full bg-emerald-500/15 text-emerald-700 dark:text-emerald-400 border border-emerald-500/40 px-2 py-0.5 text-[11px]" title="%s">✓ cloned</span>`, html.EscapeString(row.Local))
	default:
		localBadge = `<span class="inline-flex items-center gap-1 rounded-full bg-amber-500/15 text-amber-700 dark:text-amber-400 border border-amber-500/40 px-2 py-0.5 text-[11px]" title="path mapped but not cloned to disk yet">⚠ path mapped · not cloned</span>`
	}
	notes := ""
	if row.Notes != "" {
		notes = fmt.Sprintf(`<div class="text-[11px] text-muted mt-0.5 max-w-md truncate">%s</div>`, html.EscapeString(row.Notes))
	}
	// data-repo-name powers the typeahead filter.
	slugEnc := strings.ReplaceAll(html.EscapeString(row.Remote), "/", "%2F")
	fmt.Fprintf(w, `<details data-repo-row data-repo-name="%s" class="group rounded-xl border border-surface-border bg-surface-raised hover:border-accent/40 transition open:border-accent/60 open:shadow-card">
		<summary class="flex items-start gap-3 px-4 py-3 cursor-pointer list-none">
			<svg class="h-4 w-4 mt-0.5 text-muted group-open:rotate-90 group-open:text-accent transition-transform shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><polyline points="9 18 15 12 9 6"/></svg>
			<div class="min-w-0 flex-1">
				<div class="flex items-center gap-2 flex-wrap">
					<span class="font-mono text-sm text-white group-open:text-accent group-open:font-semibold">%s</span>
					%s
					%s
				</div>
				%s
			</div>
		</summary>
		<div class="border-t border-surface-border px-4 py-3"
			hx-get="/fragments/repo?slug=%s"
			hx-trigger="toggle from:closest details once"
			hx-swap="innerHTML">
			<div class="text-xs text-muted">Loading…</div>
		</div>
	</details>`,
		html.EscapeString(strings.ToLower(row.Remote)),
		html.EscapeString(row.Remote),
		prsBadge, localBadge,
		notes,
		slugEnc,
	)
}

// handleRepoDetail renders the per-repo expand panel: map-to-local
// form, clone button (when uncloned), and the open-PR list for this
// repo with the same comment/approve/block actions as before.
func (s *Server) handleRepoDetail(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	slug := strings.TrimSpace(r.URL.Query().Get("slug"))
	if slug == "" {
		fragErr(w, "slug required")
		return
	}

	// Re-read REPOSITORY.md every time (cheap; ensures we reflect
	// edits made elsewhere — CLI, file editor, etc.)
	entries, _ := repository.Read()
	var existing repository.Entry
	for _, e := range entries {
		if strings.EqualFold(e.Remote, slug) {
			existing = e
			break
		}
	}
	localPath := existing.Resolve()
	cloned := false
	if localPath != "" {
		if st, err := os.Stat(localPath); err == nil && st.IsDir() {
			cloned = true
		}
	}
	// Default suggested local path: $GOON_WORKSPACE_DIR/<basename> if
	// the env is set; ./<basename> otherwise. The user can edit before
	// saving or cloning.
	defaultLocal := existing.Local
	if defaultLocal == "" {
		base := shortRepoName(slug)
		if ws := strings.TrimSpace(os.Getenv("GOON_WORKSPACE_DIR")); ws != "" {
			defaultLocal = filepath.Join(ws, base)
		} else {
			defaultLocal = "./" + base
		}
	}

	// --- Map-to-local form ---------------------------------------------------
	fmt.Fprint(w, `<div class="space-y-3">`)
	fmt.Fprintf(w, `<div class="rounded-lg border border-surface-border bg-surface p-3">
		<div class="text-[11px] uppercase tracking-wider text-muted mb-2">Local workspace mapping</div>
		<form hx-post="/api/repo/map" hx-target="#repo-detail-result-%s" hx-swap="innerHTML"
			class="flex flex-col sm:flex-row gap-2 items-stretch">
			<input type="hidden" name="remote" value="%s">
			<input type="text" name="local" value="%s" placeholder="/absolute/path/to/local/checkout"
				class="flex-1 font-mono text-xs rounded-md border border-surface-border bg-surface text-white px-3 py-1.5 focus:border-accent focus:ring-1 focus:ring-accent/30 focus:outline-none">
			<button type="submit" class="rounded-md bg-accent text-surface px-3 py-1.5 text-xs font-semibold hover:brightness-110 transition">save mapping</button>
		</form>
		<div class="mt-1 text-[11px] text-muted">Saves to REPOSITORY.md so goon knows which folder to operate in. Tilde and <code>$HOME</code> are expanded.</div>
	</div>`,
		html.EscapeString(slug),
		html.EscapeString(slug),
		html.EscapeString(defaultLocal),
	)

	// --- Clone button (only when not cloned) --------------------------------
	if !cloned {
		// Need a clone URL. Prefer the entry's Remote when it looks
		// like a full URL; otherwise compose from the slug (assumes
		// the host the user already configured).
		cloneURL := slug
		if !strings.Contains(cloneURL, "://") && !strings.HasPrefix(cloneURL, "git@") {
			// Best-effort: probe the host adapter to derive a URL.
			// Falls back to https://github.com/<slug>.git so the user
			// can paste and edit if they're on a different host.
			cloneURL = guessCloneURL(slug, s.opts.Host)
		}
		fmt.Fprintf(w, `<div class="rounded-lg border border-amber-500/40 bg-amber-500/5 p-3">
			<div class="text-[11px] uppercase tracking-wider text-amber-700 dark:text-amber-400 mb-2">Clone to local workspace</div>
			<form hx-post="/api/repo/clone" hx-target="#repo-detail-result-%s" hx-swap="innerHTML"
				hx-indicator="#repo-clone-spin-%s"
				class="space-y-2">
				<input type="hidden" name="remote" value="%s">
				<div class="flex flex-col sm:flex-row gap-2">
					<input type="text" name="url" value="%s" placeholder="https://… or git@…"
						class="flex-1 font-mono text-xs rounded-md border border-surface-border bg-surface text-white px-3 py-1.5 focus:border-accent focus:ring-1 focus:ring-accent/30 focus:outline-none">
					<input type="text" name="target" value="%s" placeholder="local target dir"
						class="flex-1 font-mono text-xs rounded-md border border-surface-border bg-surface text-white px-3 py-1.5 focus:border-accent focus:ring-1 focus:ring-accent/30 focus:outline-none">
				</div>
				<div class="flex items-center gap-2">
					<button type="submit" class="rounded-md bg-amber-500 text-surface px-3 py-1.5 text-xs font-semibold hover:brightness-110 transition">git clone</button>
					<span id="repo-clone-spin-%s" class="htmx-indicator text-[11px] text-muted">cloning…</span>
					<span class="flex-1"></span>
					<span class="text-[11px] text-muted">Uses your existing SSH / HTTPS auth. goon won't ask for credentials.</span>
				</div>
			</form>
		</div>`,
			html.EscapeString(slug),
			html.EscapeString(slug),
			html.EscapeString(slug),
			html.EscapeString(cloneURL),
			html.EscapeString(defaultLocal),
			html.EscapeString(slug),
		)
	}

	// --- Open PRs for this repo ---------------------------------------------
	// Re-use the existing /fragments/prs handler with ?repo=<slug> so
	// the markup and the comment/approve/block actions stay 100%
	// in-sync between the legacy flat view and this new per-repo view.
	if s.opts.Host != nil {
		if _, ok := s.opts.Host.(githost.PRReviewer); ok {
			fmt.Fprintf(w, `<div class="rounded-lg border border-surface-border bg-surface p-3">
				<div class="text-[11px] uppercase tracking-wider text-muted mb-2">Open pull requests</div>
				<div hx-get="/fragments/prs?repo=%s" hx-trigger="load, prsChanged from:body" hx-swap="innerHTML">
					<div class="text-xs text-muted">Loading PRs…</div>
				</div>
			</div>`, html.EscapeString(slug))
		}
	}

	// Result slot for map / clone POST responses.
	fmt.Fprintf(w, `<div id="repo-detail-result-%s" class="text-xs"></div>`, html.EscapeString(slug))
	fmt.Fprint(w, `</div>`)
}

// guessCloneURL turns a slug into a plausible clone URL. Prefers the
// host adapter when it can answer (RepoLister), falls back to a
// GitHub-style HTTPS URL — wrong for GitLab/Bitbucket but obvious
// enough that the user notices and corrects it before submitting.
func guessCloneURL(slug string, host githost.Host) string {
	if host != nil {
		if lister, ok := host.(githost.RepoLister); ok {
			if repos, err := lister.ListRepos(context.Background()); err == nil {
				for _, r := range repos {
					if strings.EqualFold(r.Slug, slug) && r.URL != "" {
						return r.URL
					}
				}
			}
		}
		switch strings.ToLower(host.Name()) {
		case "gitlab":
			return "https://gitlab.com/" + slug + ".git"
		case "bitbucket":
			return "https://bitbucket.org/" + slug + ".git"
		}
	}
	return "https://github.com/" + slug + ".git"
}

// handleRepoMap upserts a (remote, local) mapping into REPOSITORY.md.
// On success fires repositoriesChanged so the list re-renders with
// the new mapping immediately.
func (s *Server) handleRepoMap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		fragErr(w, "invalid form: "+err.Error())
		return
	}
	remote := strings.TrimSpace(r.FormValue("remote"))
	local := strings.TrimSpace(r.FormValue("local"))
	if remote == "" {
		fragErr(w, "remote required")
		return
	}
	// Local "" is allowed — it clears the mapping (remote-only entry).
	entry := repository.Entry{Remote: remote, Local: local}
	if _, err := repository.Add(entry); err != nil {
		fragErr(w, "save failed: "+err.Error())
		return
	}
	w.Header().Set("HX-Trigger", "repositoriesChanged")
	s.events.Publish("repositoriesChanged")
	if local == "" {
		fragOK(w, "mapping cleared for "+remote)
	} else {
		fragOK(w, "mapped "+remote+" → "+local)
	}
}

// handleRepoClone shells out `git clone <url> <target>` via the
// safety validator. Refuses to clone over a non-empty target so an
// accidental click can't nuke existing work. Auth is whatever the
// user already has on their machine — goon doesn't touch
// credentials.
func (s *Server) handleRepoClone(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		fragErr(w, "invalid form: "+err.Error())
		return
	}
	remote := strings.TrimSpace(r.FormValue("remote"))
	url := strings.TrimSpace(r.FormValue("url"))
	target := strings.TrimSpace(r.FormValue("target"))
	if url == "" || target == "" {
		fragErr(w, "url and target required")
		return
	}
	// Expand ~ in the target path so the user can type ~/work/foo.
	if strings.HasPrefix(target, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			target = filepath.Join(home, strings.TrimPrefix(target, "~"))
		}
	}
	// Refuse to clone over a non-empty target. Empty dir is OK —
	// `git clone` will populate it. Missing dir is also OK — git
	// creates it.
	if st, err := os.Stat(target); err == nil {
		if !st.IsDir() {
			fragErr(w, "target exists and is not a directory: "+target)
			return
		}
		entries, _ := os.ReadDir(target)
		if len(entries) > 0 {
			fragErr(w, "target is not empty: "+target+" — refusing to overwrite")
			return
		}
	}
	// Compose + safety-validate the command. The validator's blocklist
	// covers rm-of-root, mkfs, fork bombs, etc.; a plain `git clone`
	// matches none of those, but the call still goes through so we
	// stay consistent with every other command surface.
	cmdStr := fmt.Sprintf("git clone %s %s", shellQuote(url), shellQuote(target))
	if err := safety.Default().Validate(cmdStr); err != nil {
		fragErr(w, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	cmd := safety.ShellCommand(ctx, cmdStr)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Surface git's own error message verbatim — it's almost
		// always more useful than wrapping it ("Permission denied
		// (publickey)", "Repository not found", etc.)
		fragErr(w, fmt.Sprintf("clone failed: %s\n\n%s",
			err.Error(), strings.TrimSpace(string(out))))
		return
	}
	// On success: auto-upsert the mapping so the row instantly shows
	// "✓ cloned" + the new local path. Same atomic write path as
	// /api/repo/map.
	if remote != "" {
		_, _ = repository.Add(repository.Entry{Remote: remote, Local: target})
	}
	w.Header().Set("HX-Trigger", "repositoriesChanged")
	s.events.Publish("repositoriesChanged")
	fragOK(w, "cloned to "+target)
}

// shellQuote single-quotes a shell argument so spaces / metacharacters
// don't get re-parsed by `sh -c`. Embedded single-quotes are escaped
// the POSIX way ('"'"').
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// --- Repo picker (review-list management) ---------------------------------

// handleReposPicker renders an HTML fragment listing every repo the
// configured git host can see, with a checkbox per repo. Checkboxes
// reflect the current GOON_REVIEW_REPOS slice. Submitting the form
// (handlePickReposSave) updates that env var on disk + in-process.
//
// This is the no-typing path the user asked for: pull every repo,
// tick the ones goon should pay attention to, save.
func (s *Server) handleReposPicker(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if s.opts.Host == nil {
		w.Write([]byte(`<div class="rounded-md bg-amber-500/10 border border-amber-500/30 px-3 py-2 text-sm text-amber-700 dark:text-amber-400">
			No git host configured. Set GOON_GIT_HOST=github (or bitbucket) and the matching token first.
		</div>`))
		return
	}
	lister, ok := s.opts.Host.(githost.RepoLister)
	if !ok {
		fmt.Fprintf(w, `<div class="rounded-md bg-amber-500/10 border border-amber-500/30 px-3 py-2 text-sm text-amber-700 dark:text-amber-400">
			Repo discovery is not supported on %s yet.
		</div>`, html.EscapeString(s.opts.Host.Name()))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	repos, err := lister.ListRepos(ctx)
	if err != nil {
		fmt.Fprintf(w, `<div class="rounded-md bg-rose-500/10 border border-rose-500/30 px-3 py-2 text-sm text-rose-700 dark:text-rose-400">list failed: %s</div>`, html.EscapeString(err.Error()))
		return
	}
	current := map[string]bool{}
	for _, slug := range strings.Split(strings.TrimSpace(envEcho("GOON_REVIEW_REPOS")), ",") {
		slug = strings.TrimSpace(slug)
		if slug != "" {
			current[githost.NormalizeRepoSlug(slug)] = true
		}
	}
	fmt.Fprintf(w, `<form hx-post="/api/repos/save" hx-target="#repos-save-result" hx-swap="innerHTML" class="space-y-3">
		<div class="text-xs text-gray-500">
			%d repo(s) visible to your token. Tick the ones goon should follow — the daemon will list PRs across these in <code class="font-mono">/prs</code> and the dashboard's Pull-requests section. Leave all unchecked to fall back to the auto-discovery default.
		</div>
		<div class="rounded-lg border border-gray-200 dark:border-surface-border bg-white dark:bg-surface-raised divide-y divide-gray-100 dark:divide-surface-border/60 max-h-[420px] overflow-y-auto scrollbar-thin">`, len(repos))
	for _, repo := range repos {
		checked := ""
		if current[repo.Slug] {
			checked = " checked"
		}
		priv := ""
		if repo.Private {
			priv = ` <span class="text-[10px] uppercase tracking-wider text-gray-400">private</span>`
		}
		desc := ""
		if d := strings.TrimSpace(repo.Description); d != "" {
			desc = fmt.Sprintf(`<div class="text-[11px] text-gray-500 truncate" title="%s">%s</div>`,
				html.EscapeString(d), html.EscapeString(util.Truncate(d, 140)))
		}
		fmt.Fprintf(w, `<label class="flex items-center gap-3 px-4 py-2.5 hover:bg-gray-50 dark:hover:bg-surface-sunken/40 cursor-pointer transition">
			<input type="checkbox" name="slugs" value="%s"%s class="h-4 w-4 accent-accent shrink-0">
			<div class="min-w-0 flex-1">
				<div class="font-mono text-sm flex items-center gap-2">%s%s</div>
				%s
			</div>
		</label>`, html.EscapeString(repo.Slug), checked,
			html.EscapeString(repo.Slug), priv, desc)
	}
	fmt.Fprint(w, `</div>
		<div class="flex items-center gap-2 flex-wrap">
			<button type="button" onclick="this.closest('form').querySelectorAll('input[name=slugs]').forEach(x=>x.checked=true)" class="text-xs rounded-md border border-gray-300 dark:border-surface-border px-2.5 py-1 hover:border-accent hover:text-accent transition">select all</button>
			<button type="button" onclick="this.closest('form').querySelectorAll('input[name=slugs]').forEach(x=>x.checked=false)" class="text-xs rounded-md border border-gray-300 dark:border-surface-border px-2.5 py-1 hover:border-accent hover:text-accent transition">clear</button>
			<div class="flex-1"></div>
			<div id="repos-save-result" class="text-xs"></div>
			<button type="submit" class="text-xs rounded-md bg-accent text-surface px-3 py-1.5 font-semibold hover:brightness-110 transition">save selection</button>
		</div>
	</form>`)
}

// handleReposSave persists the picked repos into GOON_REVIEW_REPOS
// (~/.config/goon/.env + process env) so /prs and the dashboard's
// Pull-requests section see them immediately. No restart needed.
func (s *Server) handleReposSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	picked := r.Form["slugs"]
	clean := make([]string, 0, len(picked))
	for _, slug := range picked {
		s2 := githost.NormalizeRepoSlug(slug)
		if s2 != "" {
			clean = append(clean, s2)
		}
	}
	value := strings.Join(clean, ",")
	if value == "" {
		_ = unsetConfigKey("GOON_REVIEW_REPOS")
		_ = os.Unsetenv("GOON_REVIEW_REPOS")
	} else {
		if err := setConfigKey("GOON_REVIEW_REPOS", value); err != nil {
			fragErr(w, "save failed: "+err.Error())
			return
		}
		_ = os.Setenv("GOON_REVIEW_REPOS", value)
	}
	w.Header().Set("HX-Trigger", "configChanged, prsChanged")
	s.events.Publish("configChanged")
	s.events.Publish("prsChanged")
	if len(clean) == 0 {
		fragOK(w, "cleared — fallback to auto-discovery")
		return
	}
	fragOK(w, fmt.Sprintf("saved %d repo(s)", len(clean)))
}

// --- Workflow plan editor --------------------------------------------------

// handlePlanSave replaces a workflow's Plan with user-edited steps
// and approves the approve_plan gate in one shot. Form fields:
//
//	wf_id     — the workflow id
//	q_id      — the pending question id (the approve_plan gate)
//	step      — repeated field, one per step title (empty entries
//	            are dropped, order preserved)
//
// The "approval value" recorded against the workflow is "yes:edited"
// so audit trails can distinguish user-edited plans from plain "yes".
// phaseApprovePlan only checks isYes(), so the edited path proceeds
// straight to execute with the new plan.
func (s *Server) handlePlanSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	wfID := strings.TrimSpace(r.FormValue("wf_id"))
	qID := strings.TrimSpace(r.FormValue("q_id"))
	if wfID == "" || qID == "" {
		fragErr(w, "wf_id and q_id required")
		return
	}
	wf, ok := s.opts.Memory.GetWorkflow(wfID)
	if !ok {
		fragErr(w, "workflow not found: "+wfID)
		return
	}
	raw := r.Form["step"]
	steps := make([]memory.PlanStep, 0, len(raw))
	for i, title := range raw {
		title = strings.TrimSpace(title)
		if title == "" {
			continue
		}
		steps = append(steps, memory.PlanStep{
			Index: i + 1,
			Title: title,
			// Done preserved if the index matches an existing step
			// that was already marked done — covers the case where
			// the agent partially executed a plan before the user
			// stepped in to edit the rest.
			Done: i < len(wf.Plan) && wf.Plan[i].Done && wf.Plan[i].Title == title,
		})
	}
	if len(steps) == 0 {
		fragErr(w, "plan needs at least one step")
		return
	}
	wf.Plan = steps
	s.opts.Memory.UpsertWorkflow(wf)
	if !s.opts.Memory.AnswerQuestion(qID, "yes:edited") {
		fragErr(w, "question "+qID+" not found or already answered")
		return
	}
	if waker, ok := s.opts.Daemon.(Waker); ok {
		waker.Wake()
	}
	w.Header().Set("HX-Trigger", "questionsChanged, workflowsChanged, workflowDetailRefresh")
	s.events.Publish("questionsChanged")
	s.events.Publish("workflowsChanged")
	s.events.Publish("workflowDetailRefresh")
	fragOK(w, fmt.Sprintf("plan updated (%d step%s) — daemon resuming", len(steps), pluralS(len(steps))))
}

// pluralS returns "s" unless n == 1. Local helper so we don't depend
// on the plural() func in server.go from inside this file.
func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// --- tiny response helpers -------------------------------------------------

func fragOK(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<span class="inline-flex items-center gap-1 text-emerald-700 dark:text-emerald-400">✓ %s</span>`, html.EscapeString(msg))
}

func fragErr(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<span class="inline-flex items-center gap-1 text-rose-700 dark:text-rose-400">✗ %s</span>`, html.EscapeString(msg))
}

func boardMissing(w http.ResponseWriter, action string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<span class="text-amber-700 dark:text-amber-400">cannot %s — no board configured</span>`, html.EscapeString(action))
}

func hostMissing(w http.ResponseWriter, action string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<span class="text-amber-700 dark:text-amber-400">cannot %s — no git host configured</span>`, html.EscapeString(action))
}
