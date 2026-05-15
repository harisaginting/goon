// Package web — actions.go houses the direct CRUD endpoints for
// Jira and the configured git host. These work without any LLM
// provider configured — they go straight to the underlying board /
// host API. The chat agent (chat.go) sits on top of these for the
// LLM-mediated flow, but the user can also drive every action by
// clicking buttons.
package web

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/harisaginting/goon/internal/boards"
	"github.com/harisaginting/goon/internal/githost"
	"github.com/harisaginting/goon/internal/memory"
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
	statusRaw := strings.ToLower(strings.TrimSpace(r.FormValue("status")))
	if key == "" || statusRaw == "" {
		fragErr(w, "key and status required")
		return
	}
	target := boards.MapStatus(statusRaw)
	if target == boards.StatusUnknown {
		fragErr(w, "unknown status: "+statusRaw+" (use open|in_progress|in_review|blocked|done)")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := s.opts.Board.Transition(ctx, key, target); err != nil {
		fragErr(w, "transition failed: "+err.Error())
		return
	}
	w.Header().Set("HX-Trigger", "ticketsChanged")
	s.events.Publish("ticketsChanged")
	fragOK(w, fmt.Sprintf("%s → %s", key, target))
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
	// per-row "see related PRs" affordance later.
	var repos []string
	if rp := strings.TrimSpace(r.URL.Query().Get("repo")); rp != "" {
		repos = []string{rp}
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
		</div>
		<details class="mt-3 rounded-lg border border-gray-200 dark:border-surface-border bg-gray-50/60 dark:bg-surface-sunken/40">
			<summary class="px-4 py-2 cursor-pointer text-xs text-gray-600 dark:text-gray-400 hover:text-accent transition">
				⚙ manage which repos goon follows
			</summary>
			<div class="px-4 pb-4 pt-2"
				hx-get="/fragments/repos-picker" hx-trigger="toggle from:closest details once" hx-swap="innerHTML">
				<div class="text-xs text-gray-500">Loading repo list…</div>
			</div>
		</details>`))
		return
	}
	// Inline picker affordance — collapsed by default so it doesn't
	// dominate the PR list. Clicking the summary fetches the full
	// repo list with checkboxes.
	fmt.Fprint(w, `<details class="mb-3 rounded-lg border border-gray-200 dark:border-surface-border bg-gray-50/60 dark:bg-surface-sunken/40">
		<summary class="px-4 py-2 cursor-pointer text-xs text-gray-600 dark:text-gray-400 hover:text-accent transition">
			⚙ manage which repos goon follows
		</summary>
		<div class="px-4 pb-4 pt-2"
			hx-get="/fragments/repos-picker" hx-trigger="toggle from:closest details once" hx-swap="innerHTML">
			<div class="text-xs text-gray-500">Loading repo list…</div>
		</div>
	</details>`)
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
			<div class="flex flex-wrap gap-2">
				<button type="button" onclick="document.getElementById('%s').classList.toggle('hidden')"
					class="text-xs rounded-md border border-gray-300 dark:border-surface-border px-2 py-1 hover:border-accent hover:text-accent transition">comment</button>
				<form hx-post="/api/pr/approve" hx-target="#%s-result" hx-swap="innerHTML" class="m-0 inline-flex">
					<input type="hidden" name="repo" value="%s">
					<input type="hidden" name="number" value="%d">
					<button type="submit" class="text-xs rounded-md border border-emerald-500/40 text-emerald-700 dark:text-emerald-400 px-2 py-1 hover:bg-emerald-500/10 transition">approve</button>
				</form>
				<button type="button" onclick="var el=document.getElementById('%s-rc'); el.classList.toggle('hidden'); if(!el.classList.contains('hidden')) el.querySelector('input,textarea')?.focus()"
					class="text-xs rounded-md border border-rose-500/40 text-rose-700 dark:text-rose-400 px-2 py-1 hover:bg-rose-500/10 transition">request changes</button>
				<span class="text-xs text-gray-400 ml-auto self-center" id="%s-result"></span>
			</div>
			<form hx-post="/api/pr/comment" hx-target="#%s-result" hx-swap="innerHTML" hx-on::after-request="if(event.detail.successful){this.reset();document.getElementById('%s').classList.add('hidden');}"
				class="mt-2 hidden" id="%s">
				<input type="hidden" name="repo" value="%s">
				<input type="hidden" name="number" value="%d">
				<textarea name="body" rows="2" required placeholder="leave a comment…"
					class="w-full font-mono text-xs rounded-md border border-gray-300 dark:border-surface-border bg-white dark:bg-surface px-2 py-1 focus:border-accent focus:ring-1 focus:ring-accent/30 focus:outline-none"></textarea>
				<div class="flex justify-end mt-1"><button type="submit" class="text-xs rounded-md bg-accent text-surface px-3 py-1 hover:brightness-110 transition">post comment</button></div>
			</form>
			<form hx-post="/api/pr/request-changes" hx-target="#%s-result" hx-swap="innerHTML" hx-on::after-request="if(event.detail.successful){this.reset();document.getElementById('%s-rc').classList.add('hidden');}"
				class="mt-2 hidden" id="%s-rc">
				<input type="hidden" name="repo" value="%s">
				<input type="hidden" name="number" value="%d">
				<textarea name="body" rows="2" required placeholder="why changes are needed…"
					class="w-full font-mono text-xs rounded-md border border-rose-300 dark:border-rose-700 bg-white dark:bg-surface px-2 py-1 focus:border-rose-500 focus:ring-1 focus:ring-rose-500/30 focus:outline-none"></textarea>
				<div class="flex justify-end mt-1"><button type="submit" class="text-xs rounded-md bg-rose-500 text-white px-3 py-1 hover:bg-rose-600 transition">request changes</button></div>
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
