package web

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/harisaginting/goon/internal/agentctx"
	"github.com/harisaginting/goon/internal/chats"
	"github.com/harisaginting/goon/internal/google"
	"github.com/harisaginting/goon/internal/llm"
	"github.com/harisaginting/goon/internal/notes"
	"github.com/harisaginting/goon/internal/repository"
	"github.com/harisaginting/goon/internal/tools"
	"github.com/harisaginting/goon/internal/usage"
)

// chatSystemPrompt mirrors the Telegram bot's chat persona. Both
// surfaces ground the LLM in the same goon-context block via
// agentctx.Build; only the wrapper "channel" differs.
const chatSystemPrompt = `You are GOON, an autonomous engineering co-pilot reachable from the web dashboard.

# HOW TO ANSWER

You have THREE kinds of moves available each turn:

1. Answer in prose — use the GOON STATE block when it already
   answers the question (daemon status, recently-cached tickets,
   workflows in flight, pending approvals, knowledge notes).

2. Call a READ tool — jira_search, when the user asks about tickets
   and the cached state is insufficient. Don't make the user click
   "refresh"; just query.

3. Call a WRITE tool — jira_comment, jira_transition, jira_update —
   when the user explicitly asks you to act on a ticket:
     - "comment on ENG-123 that the build is green" → jira_comment
     - "move ENG-123 to in progress" → jira_transition
     - "change ENG-123 title to ..." or "update description to ..." →
       jira_update

When you call ANY tool, your ENTIRE response that turn is the single
JSON line specified in the TOOLS block. The server runs the action
and re-prompts you with the result; you confirm in prose on the
next turn.

# RULES

- Quote ticket KEYs, URLs, and IDs verbatim. Never invent.
- When listing more than 3 tickets, one per line:
  "KEY — title [status] assignee=NAME"
- If a search returns nothing, say so plainly and suggest the user
  widen their filter; don't pretend.
- Only call WRITE tools when the user is explicit. "what does ENG-1
  say" → read; "comment on ENG-1 that ..." → write.
- After a successful action, confirm with a short prose line like
  "✓ commented on ENG-123 — '<first words of the comment>'".
- Do NOT tell the user to click "refresh"; you have the tools now.
- For project facts / how-it-works questions, check the knowledge
  notes in GOON STATE first and name the relevant note.
- When the user wants to ACT on CODE (edit, ship), tell them to use
  the CLI's ` + "`" + `goon "<task>"` + "`" + ` for the agent runtime — that's outside chat scope.
- You CAN read and act on pull requests directly via the pr_* tools
  (reviewers, status, comment, approve, request changes). When the user
  pastes a PR URL, pass it straight through as the "pr" argument.
- You can also search the Confluence wiki (confluence_search,
  confluence_get) and the web (web_search, web_fetch). Check the TOOLS
  block for which ones are wired this session.`

const maxWebChatHistory = 6 // turn pairs

// handleChat accepts a plain-text message via POST and returns the
// model's reply as an htmx-friendly HTML fragment. The bot's
// rolling-history pattern is mirrored here: one server-side session
// per process (the dashboard is intended for one operator), context
// block rebuilt each call.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	msg := strings.TrimSpace(r.FormValue("message"))
	if msg == "" {
		http.Error(w, "message required", http.StatusBadRequest)
		return
	}
	if s.opts.LLM == nil {
		writeChatBubble(w, "assistant",
			"chat unavailable — configure an LLM provider on the Configuration tab and reload.")
		return
	}

	// Per-thread persistence. The form may carry a thread_id (continuing
	// an existing thread) or omit it (starting a fresh thread). The repo
	// form value scopes the conversation to a specific repo — surfaces as
	// a system-prompt prefix the LLM sees on every turn AND is stored on
	// the thread so reopening preserves the scope.
	threadID := strings.TrimSpace(r.FormValue("thread_id"))
	repoCtx := strings.TrimSpace(r.FormValue("repo"))
	var existingThread chats.Thread
	if threadID != "" {
		if t, err := chats.Read(threadID); err == nil {
			existingThread = t
			// If the form didn't carry a repo context but the thread
			// has one, inherit it — keeps the scope sticky across
			// page reloads where the form fields might be empty.
			if repoCtx == "" {
				repoCtx = t.RepoContext
			}
		}
	}
	if threadID == "" {
		threadID = chats.NewID()
	}

	// Build the system prompt. When the user picked a repo context,
	// prepend a small "REPO CONTEXT" block so the agent grounds its
	// answers in that repo without the user having to repeat the slug
	// on every message.
	systemPrompt := chatSystemPrompt
	if repoCtx != "" {
		systemPrompt = "REPO CONTEXT (the user is talking about this repo unless they say otherwise): " +
			repoCtx + "\n\n" + chatSystemPrompt
	}

	// Snapshot existing history (without the new user turn) so the
	// agent-loop helper can append both turns atomically at the end.
	// The thread's persisted history takes precedence when present —
	// it's the canonical record across page reloads. Falls back to
	// the in-process rolling history when no thread is loaded.
	var historySnapshot []llm.Message
	if len(existingThread.Messages) > 0 {
		historySnapshot = append([]llm.Message(nil), existingThread.Messages...)
	} else {
		s.chatMu.Lock()
		historySnapshot = append([]llm.Message(nil), s.chatHistory...)
		s.chatMu.Unlock()
	}

	// Run the LLM↔tool loop. The agent decides whether to query the
	// board live (search_jira tool) or answer from the GOON STATE
	// block alone. No more pre-fetch refresh — the agent pulls only
	// what it needs.
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	ctx = usage.WithLabel(ctx, "chat")
	result, err := agentctx.ChatTurn(ctx, agentctx.ChatTurnOptions{
		LLM:          s.opts.LLM,
		Memory:       s.opts.Memory,
		Board:        s.opts.Board,
		Host:         s.opts.Host,
		SystemPrompt: systemPrompt,
		History:      historySnapshot,
		UserMessage:  msg,
	})
	if err != nil {
		writeChatBubble(w, "error", "llm error: "+err.Error())
		return
	}
	reply := result.Reply
	if strings.TrimSpace(reply) == "" {
		reply = "(no response from model)"
	}
	// Commit both turns to history together so retries don't orphan
	// the user turn (which the previous implementation could do).
	s.chatMu.Lock()
	s.chatHistory = append(s.chatHistory, result.NewTurns...)
	s.chatHistory = trimChatHistory(s.chatHistory)
	s.chatMu.Unlock()

	// Persist the thread atomically. Persisted history is the FULL
	// conversation (no trim) — chats.Thread is the durable record;
	// the in-process s.chatHistory is just the working window for
	// turns when no thread id is in play.
	persisted := append([]llm.Message(nil), historySnapshot...)
	persisted = append(persisted, result.NewTurns...)
	thread := chats.Thread{
		ID:          threadID,
		Title:       existingThread.Title,
		RepoContext: repoCtx,
		Created:     existingThread.Created,
		Messages:    persisted,
	}
	if err := chats.Write(thread); err != nil {
		// Non-fatal — the in-process history still works. Log via
		// stderr so the operator can spot disk issues.
		fmt.Fprintf(s.opts.Stderr, "chat: persist thread %s: %v\n", threadID, err)
	} else {
		// Tell the client side which thread id won, so the form can
		// continue posting against the right one. HX-Trigger fires
		// the threadsChanged event so the sidebar list re-renders
		// with the new title.
		w.Header().Set("HX-Trigger", fmt.Sprintf(`{"threadsChanged":{},"chatThreadID":"%s"}`,
			jsEscape(threadID)))
		s.events.Publish("threadsChanged")
	}

	// Return ONLY the assistant bubble. The user bubble is injected
	// client-side in goonChatBeforeRequest before the request fires,
	// so it appears instantly without waiting for the LLM.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	writeChatBubble(w, "assistant", reply)
}

// jsEscape is a tiny JSON-string escape so we can interpolate the
// thread id safely into the HX-Trigger header without pulling in
// encoding/json just for one value. Only handles the characters
// that break a JSON string token — backslash and double quote.
func jsEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`)
}

// handleChatReset clears the rolling history. Lets the user start a
// fresh conversation without restarting the daemon.
func (s *Server) handleChatReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	s.chatMu.Lock()
	s.chatHistory = nil
	s.chatMu.Unlock()
	// Return the empty conversation log so htmx outerHTML-swaps the
	// transcript pane back to its zero state.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, chatTranscriptEmpty())
}

// handleChatThreads renders the left-rail thread list — past
// conversations the user can reopen. Pure HTML fragment (no JSON)
// so htmx swaps it directly into the sidebar.
func (s *Server) handleChatThreads(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	threads, err := chats.List()
	if err != nil {
		fmt.Fprintf(w, `<div class="text-xs text-rose-500">failed to list threads: %s</div>`, html.EscapeString(err.Error()))
		return
	}
	fmt.Fprint(w, `<div class="space-y-1">`)
	if len(threads) == 0 {
		fmt.Fprint(w, `<div class="text-[11px] text-muted/70 px-2 py-1 italic">No saved chats yet. Send a message to start one.</div>`)
	}
	for _, t := range threads {
		ago := humanizeAgo(time.Since(t.Updated))
		repoBadge := ""
		if t.RepoContext != "" {
			repoBadge = fmt.Sprintf(`<span class="block text-[10px] text-accent/70 truncate">@ %s</span>`, html.EscapeString(t.RepoContext))
		}
		fmt.Fprintf(w, `<div class="group flex items-center gap-1 rounded-md hover:bg-surface-raised transition px-1">
			<button type="button" onclick="goonChatLoadThread('%s')"
				title="%s"
				class="flex-1 min-w-0 text-left py-1.5 px-1.5">
				<div class="text-xs text-ink truncate">%s</div>
				%s
				<div class="text-[10px] text-muted/70">%s · %d msg%s</div>
			</button>
			<form hx-post="/api/chat/thread/delete" hx-target="#chat-threads" hx-swap="innerHTML" class="opacity-0 group-hover:opacity-100 transition">
				<input type="hidden" name="id" value="%s">
				<button type="submit" onclick="return confirm('Delete this chat?')" class="text-xs text-muted hover:text-rose-500 px-1.5 py-1" title="delete">✕</button>
			</form>
		</div>`,
			html.EscapeString(t.ID),
			html.EscapeString(t.Title),
			html.EscapeString(t.Title),
			repoBadge,
			html.EscapeString(ago), t.MessageN, pluralS(t.MessageN),
			html.EscapeString(t.ID),
		)
	}
	fmt.Fprint(w, `</div>`)
}

// handleChatThread returns one full thread as JSON so the client can
// hydrate the transcript pane (and the active thread_id form field)
// without a full page reload.
func (s *Server) handleChatThread(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	t, err := chats.Read(id)
	if err != nil {
		http.Error(w, "load failed: "+err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(t); err != nil {
		fmt.Fprintf(s.opts.Stderr, "chat: encode thread %s: %v\n", id, err)
	}
}

// handleChatThreadDelete removes a saved thread + refreshes the
// sidebar list. Idempotent: re-deleting a missing thread is a no-op
// so the UI never gets stuck on a stale row.
func (s *Server) handleChatThreadDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id := strings.TrimSpace(r.FormValue("id"))
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	if err := chats.Delete(id); err != nil {
		http.Error(w, "delete failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Re-render the sidebar list with the row gone.
	s.handleChatThreads(w, r)
}

// handleChatSaveAsNote distils a saved thread into a markdown file
// under the notes store so the LLM sees it in every future run.
// The user supplies a kebab-case filename via the `name` form field;
// empty falls back to a slug of the thread title.
func (s *Server) handleChatSaveAsNote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id := strings.TrimSpace(r.FormValue("id"))
	name := strings.TrimSpace(r.FormValue("name"))
	if id == "" {
		fragErr(w, "thread id required")
		return
	}
	path, err := chats.SaveAsNote(id, name)
	if err != nil {
		fragErr(w, "save as note failed: "+err.Error())
		return
	}
	w.Header().Set("HX-Trigger", "memoryChanged")
	s.events.Publish("memoryChanged")
	fragOK(w, "saved to "+path+" — visible to goon in every future run")
}

// chatTranscriptEmpty is the "nothing here yet" pane rendered before the
// first message and after /api/chat/reset. It's a function (not a const)
// so it can surface Google Workspace prompt chips once goon is connected —
// otherwise those tools are invisible and users never discover them.
// #chat-transcript is the htmx beforeend target AND the scroll container —
// no max-height cap here; height comes from the flex parent in fragTabChat.
func chatTranscriptEmpty() string {
	subtitle := "Tickets · PRs · Confluence · web · Obsidian"
	if google.Configured() {
		subtitle = "Tickets · PRs · Calendar · Mail · Logs · web"
	}
	chips := chatChip("what tickets are open?", "open tickets", "non-done tickets") +
		chatChip("any pending approvals?", "pending approvals", "workflows waiting on you") +
		chatChip("review my open PRs awaiting my review", "review open PRs", "draft a PR review") +
		chatChip("what do you know about this project?", "project knowledge", "what goon knows")
	if google.Configured() {
		chips += chatChip("what meetings do I have today?", "today's schedule", "Google Calendar") +
			chatChip("what are my tasks today?", "my tasks", "Google Tasks + Jira") +
			chatChip("check my email from the last week", "recent email", "search Gmail")
		if strings.TrimSpace(os.Getenv("GOOGLE_CLOUD_PROJECT")) != "" {
			chips += chatChip("any errors in the logs in the last hour?", "recent errors", "Cloud Logging")
		}
	}
	return `<div id="chat-transcript" class="h-full overflow-y-auto scrollbar-thin px-4 py-4 space-y-4">
	<div class="mx-auto max-w-sm text-center py-10">
		<div class="mx-auto mb-4 h-12 w-12 rounded-2xl bg-gradient-to-br from-accent to-highlight text-white flex items-center justify-center text-base font-bold shadow-lift">GO</div>
		<div class="text-sm font-semibold text-ink">Ask goon anything</div>
		<div class="mt-1 text-xs text-muted">` + subtitle + `</div>
		<div class="mt-5 grid grid-cols-2 gap-2 text-left">` + chips + `</div>
	</div>
</div>`
}

// chatChip renders one starter-prompt button for the empty chat state.
func chatChip(query, title, sub string) string {
	return `<button type="button" onclick="goonChatFill(this.dataset.q)" data-q="` + html.EscapeString(query) + `" class="rounded-lg border border-surface-border bg-surface px-3 py-2.5 text-xs text-ink hover:border-accent hover:text-accent transition text-left">
				<div class="font-medium">→ ` + html.EscapeString(title) + `</div>
				<div class="mt-0.5 text-[11px] text-muted">` + html.EscapeString(sub) + `</div>
			</button>`
}

// writeChatBubble emits one styled message bubble. Roles map to
// distinct visual tones: user is right-aligned accent, assistant is
// left-aligned subtle, error is rose. Each bubble carries the
// `chat-bubble` class so the transcript auto-scrolls to the newest.
func writeChatBubble(w io.Writer, role, body string) {
	// Pre-render body with newlines preserved.
	safe := html.EscapeString(body)
	safe = strings.ReplaceAll(safe, "\n", "<br>")
	now := time.Now().Format("15:04")
	tsUnix := time.Now().Unix()

	var wrap, bubble, avatar, label string
	switch role {
	case "user":
		wrap = "flex flex-row-reverse gap-2 chat-bubble animate-fade-in"
		bubble = `max-w-[85%] rounded-2xl rounded-tr-md bg-accent text-surface px-3.5 py-2 text-sm whitespace-pre-wrap shadow-sm`
		avatar = `<div class="shrink-0 h-7 w-7 rounded-full bg-accent/15 text-accent flex items-center justify-center text-xs font-semibold">H</div>`
		label = "you"
	case "assistant":
		wrap = "flex flex-row gap-2 chat-bubble animate-fade-in"
		bubble = `max-w-[85%] rounded-2xl rounded-tl-md border border-gray-200 dark:border-surface-border bg-white dark:bg-surface-raised text-gray-800 dark:text-gray-200 px-3.5 py-2 text-sm shadow-sm`
		avatar = `<div class="shrink-0 h-7 w-7 rounded-full bg-gradient-to-br from-accent to-highlight text-white flex items-center justify-center text-[10px] font-bold tracking-tight">GO</div>`
		label = "goon"
	case "error":
		wrap = "flex flex-row gap-2 chat-bubble animate-fade-in"
		bubble = `max-w-[85%] rounded-2xl rounded-tl-md border border-rose-500/40 bg-rose-500/10 text-rose-700 dark:text-rose-400 px-3.5 py-2 text-sm shadow-sm`
		avatar = `<div class="shrink-0 h-7 w-7 rounded-full bg-rose-500/15 text-rose-500 flex items-center justify-center text-xs">!</div>`
		label = "error"
	default:
		wrap = "flex flex-row gap-2 chat-bubble animate-fade-in"
		bubble = `max-w-[85%] rounded-2xl px-3.5 py-2 text-sm`
		avatar = ``
		label = role
	}
	// Align the meta column (label + time) with the bubble side via flex order.
	metaAlign := "items-start"
	if role == "user" {
		metaAlign = "items-end"
	}
	fmt.Fprintf(w, `<div class="%s">
		%s
		<div class="min-w-0 flex flex-col %s">
			<div class="flex items-baseline gap-2 mb-0.5 px-0.5">
				<span class="text-[11px] font-medium text-gray-600 dark:text-gray-400">%s</span>
				<span class="msg-time text-[10px] text-muted font-mono" data-ts="%d">%s</span>
			</div>
			<div class="%s">%s</div>
		</div>
	</div>`, wrap, avatar, metaAlign, html.EscapeString(label), tsUnix, now, bubble, safe)
}

// trimChatHistory keeps the last maxWebChatHistory*2 messages.
func trimChatHistory(hist []llm.Message) []llm.Message {
	cap := 2 * maxWebChatHistory
	if len(hist) <= cap {
		return hist
	}
	return hist[len(hist)-cap:]
}

// --- Tab composers ---------------------------------------------------------

// fragTabChat renders the full-height chat shell.
//
// Layout (flex column fills viewport below header):
//   ┌─ sidebar (history) ─┬─ chat panel (flex col) ─────────────────┐
//   │  thread list        │  topbar (repo picker + new)             │
//   │  (scroll)           ├─────────────────────────────────────────┤
//   │                     │  #chat-transcript (flex-1, scrolls)     │
//   │                     ├─────────────────────────────────────────┤
//   │                     │  composer (shrink-0, always visible)    │
//   └─────────────────────┴─────────────────────────────────────────┘
//
// The fix for "chat is so damn bad":
//   - chatTranscriptEmpty no longer has max-h-[60vh] — height comes
//     from the flex parent.
//   - #chat-transcript has h-full overflow-y-auto so it fills and
//     scrolls internally rather than fighting a nested scroll container.
//   - Overall container uses calc height to fill the viewport exactly.
func (s *Server) fragTabChat(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	llmAvailable := s.opts.LLM != nil
	repoEntries, _ := repositoryEntriesForPicker()

	if !llmAvailable {
		fmt.Fprint(w, `<div class="rounded-xl border border-amber-500/40 bg-amber-500/10 px-4 py-4 text-sm text-amber-700 dark:text-amber-400">
			No LLM provider configured — set one on the <strong>Setup</strong> tab and reload.
		</div>`)
		return
	}

	// Outer flex row — fills viewport below header (3.5rem) + main top padding (1.5rem).
	// -mt-6 cancels the main section's py-6 top so we reach from header to bottom
	// without the transcript getting clipped. -mx-4 lg:-mx-8 bleeds to edge.
	fmt.Fprint(w, `<div class="-mx-4 lg:-mx-8 -mt-6 flex h-[calc(100vh-3.5rem)] overflow-hidden">`)

	// ── Left sidebar: chat history (toggleable drawer) ─────────────────
	fmt.Fprint(w, `<aside id="chat-history-drawer" class="flex flex-col w-56 shrink-0 border-r border-surface-border bg-surface-sunken/60 overflow-hidden lg:flex hidden">
		<div class="px-3 py-3 border-b border-surface-border flex items-center justify-between shrink-0">
			<span class="text-[11px] uppercase tracking-wider text-muted font-semibold">History</span>
			<button type="button" onclick="goonChatNewThread()"
				class="text-[11px] text-accent hover:brightness-125 font-medium transition">+ new</button>
		</div>
		<div id="chat-threads" class="flex-1 overflow-y-auto scrollbar-thin px-1.5 py-2"
			hx-get="/api/chat/threads" hx-trigger="load, threadsChanged from:body" hx-swap="innerHTML">
			<div class="text-xs text-muted px-2 py-2">Loading…</div>
		</div>
	</aside>`)

	// ── Right: chat panel (flex col, fills remaining width) ────────────
	fmt.Fprint(w, `<div class="flex-1 min-w-0 flex flex-col overflow-hidden">`)

	// Topbar — compact bar with history toggle, repo picker, new chat
	fmt.Fprint(w, `<div class="shrink-0 flex items-center gap-2 px-4 py-2.5 border-b border-surface-border bg-surface-raised/80 backdrop-blur-sm">
		<button id="chat-history-toggle" type="button"
			class="p-1.5 rounded-md text-muted hover:text-accent hover:bg-accent-soft transition"
			onclick="(function(){var d=document.getElementById('chat-history-drawer');d.classList.toggle('hidden');d.classList.toggle('flex');})()"
			title="Toggle history sidebar">☰</button>
		<span class="text-sm font-semibold text-ink shrink-0">goon</span>
		<div class="flex-1 min-w-0">`)
	if len(repoEntries) > 0 {
		fmt.Fprint(w, `<select id="chat-repo-context" name="repo" title="Conversation scope"
				class="w-full max-w-[260px] rounded-md border border-surface-border bg-surface text-xs text-muted px-2 py-1 focus:border-accent focus:outline-none truncate">
				<option value="">— any repo —</option>`)
		for _, e := range repoEntries {
			fmt.Fprintf(w, `<option value="%s">%s</option>`, html.EscapeString(e), html.EscapeString(e))
		}
		fmt.Fprint(w, `</select>`)
	} else {
		fmt.Fprint(w, `<input type="hidden" id="chat-repo-context" value="">`)
	}
	fmt.Fprint(w, `</div>
		<button type="button" onclick="goonChatNewThread()"
			class="shrink-0 inline-flex items-center gap-1 rounded-md bg-accent text-surface px-2.5 py-1.5 text-xs font-semibold hover:brightness-110 transition shadow-sm">
			<svg class="h-3 w-3" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><line x1="12" y1="5" x2="12" y2="19"/><line x1="5" y1="12" x2="19" y2="12"/></svg>
			new
		</button>
	</div>`)

	// Transcript — flex-1 so it fills remaining height; #chat-transcript
	// has h-full so overflow-y-auto works without a hard px cap.
	fmt.Fprint(w, `<div class="flex-1 overflow-hidden">`)
	fmt.Fprint(w, chatTranscriptEmpty())
	fmt.Fprint(w, `</div>`)

	// Composer — shrink-0 keeps it pinned to the bottom at all times.
	fmt.Fprint(w, `<div class="shrink-0 border-t border-surface-border bg-surface-raised/80 px-4 py-3">
		<form id="goon-chat-form" hx-post="/api/chat" hx-target="#chat-transcript" hx-swap="beforeend"
			hx-on::before-request="goonChatBeforeRequest(event)"
			hx-on::after-request="goonChatAfter(event)" class="flex items-end gap-2">
			<input type="hidden" id="goon-chat-thread-id" name="thread_id" value="">
			<input type="hidden" id="goon-chat-repo-mirror" name="repo" value="">
			<div class="flex-1 relative">
				<textarea id="goon-chat-input" name="message" autocomplete="off" required autofocus rows="1"
					placeholder="ask goon anything… (Shift+Enter for newline)"
					class="w-full resize-none rounded-xl border border-surface-border bg-surface px-4 py-2.5 pr-10 text-sm text-ink placeholder:text-muted focus:border-accent focus:ring-2 focus:ring-accent/30 focus:outline-none max-h-40 scrollbar-thin"
					onkeydown="goonChatKey(event)"
					oninput="goonChatAutosize(this)"></textarea>
				<div class="absolute bottom-2.5 right-3 pointer-events-none htmx-indicator">
					<svg class="h-4 w-4 animate-spin-slow text-accent" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="10" stroke-opacity="0.25"/><path d="M22 12a10 10 0 0 1-10 10"/></svg>
				</div>
			</div>
			<button type="submit"
				class="shrink-0 inline-flex items-center justify-center rounded-xl bg-accent text-surface w-10 h-10 hover:brightness-110 active:brightness-95 transition shadow-sm">
				<svg class="h-4 w-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><line x1="22" y1="2" x2="11" y2="13"/><polygon points="22 2 15 22 11 13 2 9 22 2"/></svg>
			</button>
		</form>
		<div class="mt-2 flex items-center justify-between gap-2 text-[11px] text-muted/70">
			<span>Shift+Enter for newline</span>
			<div id="chat-thread-actions" class="hidden flex items-center gap-2">
				<form hx-post="/api/chat/save-as-note" hx-target="#chat-action-result" hx-swap="innerHTML" class="flex items-center gap-1.5">
					<input type="hidden" id="goon-chat-thread-id-mirror" name="id" value="">
					<input type="text" name="name" placeholder="note name (optional)"
						class="rounded-md border border-surface-border bg-surface px-2 py-1 text-[11px] w-36 focus:border-accent focus:outline-none">
					<button type="submit"
						class="text-emerald-700 dark:text-emerald-400 hover:text-emerald-300 transition whitespace-nowrap text-[11px]">
						✨ save as note
					</button>
				</form>
				<span id="chat-action-result" class="text-muted"></span>
			</div>
		</div>
	</div>`)

	fmt.Fprint(w, `</div>`) // close chat panel
	fmt.Fprint(w, `</div>`) // close outer flex row

	// ── JS plumbing ────────────────────────────────────────────────────
	fmt.Fprint(w, `<script>
	(function() {
		window.goonChatFill = function(q) {
			const ta = document.getElementById('goon-chat-input');
			if (!ta) return;
			ta.value = q; ta.focus(); goonChatAutosize(ta);
		};
		window.goonChatAutosize = function(el) {
			el.style.height = 'auto';
			el.style.height = Math.min(el.scrollHeight, 160) + 'px';
		};
		window.goonChatKey = function(ev) {
			if (ev.key === 'Enter' && !ev.shiftKey) {
				ev.preventDefault();
				const form = document.getElementById('goon-chat-form');
				if (form) htmx.trigger(form, 'submit');
			}
		};
		// Mirror repo picker → hidden field so POST always carries latest scope.
		(function syncRepoPicker() {
			const picker = document.getElementById('chat-repo-context');
			const mirror = document.getElementById('goon-chat-repo-mirror');
			if (picker && mirror) {
				const sync = function(){ mirror.value = picker.value; };
				picker.addEventListener('change', sync);
				sync();
			}
		})();
		// Before the POST fires: inject user bubble + thinking indicator
		// immediately so the UI responds in zero milliseconds.
		window.goonChatBeforeRequest = function() {
			const ta = document.getElementById('goon-chat-input');
			const msg = ta ? ta.value.trim() : '';
			if (!msg) return;
			const t = document.getElementById('chat-transcript');
			if (!t) return;
			// Remove empty-state prompt grid on first message.
			const empty = document.getElementById('chat-empty-state');
			if (empty) empty.remove();
			// User bubble.
			const ub = document.createElement('div');
			ub.className = 'flex flex-row-reverse gap-2 animate-fade-in';
			const esc = function(s){ return s.replace(/[<>&"]/g, function(c){return({'<':'&lt;','>':'&gt;','&':'&amp;','"':'&quot;'})[c];}); };
			ub.innerHTML =
				'<div class="shrink-0 h-7 w-7 rounded-full bg-accent/15 text-accent flex items-center justify-center text-xs font-semibold">H</div>' +
				'<div class="min-w-0 flex flex-col items-end">' +
				'<div class="max-w-[85%] rounded-2xl rounded-tr-md bg-accent text-surface px-3.5 py-2 text-sm whitespace-pre-wrap shadow-sm">' + esc(msg) + '</div>' +
				'</div>';
			t.appendChild(ub);
			// Thinking indicator.
			const think = document.createElement('div');
			think.id = 'goon-thinking';
			think.className = 'flex flex-row gap-2 animate-fade-in';
			think.innerHTML =
				'<div class="shrink-0 h-7 w-7 rounded-full bg-gradient-to-br from-accent to-highlight text-white flex items-center justify-center text-[10px] font-bold">GO</div>' +
				'<div class="max-w-[85%] rounded-2xl rounded-tl-md border border-surface-border bg-surface-raised px-4 py-3 shadow-sm">' +
				'<div class="flex gap-1 items-center">' +
				'<span class="h-1.5 w-1.5 rounded-full bg-muted" style="animation:bounce 1s ease-in-out infinite;animation-delay:0ms"></span>' +
				'<span class="h-1.5 w-1.5 rounded-full bg-muted" style="animation:bounce 1s ease-in-out infinite;animation-delay:160ms"></span>' +
				'<span class="h-1.5 w-1.5 rounded-full bg-muted" style="animation:bounce 1s ease-in-out infinite;animation-delay:320ms"></span>' +
				'</div></div>';
			t.appendChild(think);
			// Scroll + clear textarea.
			requestAnimationFrame(function(){ t.scrollTop = t.scrollHeight; });
			if (ta) { ta.value = ''; ta.style.height = ''; ta.focus(); }
		};
		// After each POST: remove thinking indicator, scroll, sync thread id.
		window.goonChatAfter = function(ev) {
			const think = document.getElementById('goon-thinking');
			if (think) think.remove();
			const t = document.getElementById('chat-transcript');
			if (t) requestAnimationFrame(function(){ t.scrollTop = t.scrollHeight; });
			try {
				const trig = ev && ev.detail && ev.detail.xhr && ev.detail.xhr.getResponseHeader('HX-Trigger');
				if (trig) {
					const j = JSON.parse(trig);
					if (j && j.chatThreadID) {
						['goon-chat-thread-id','goon-chat-thread-id-mirror'].forEach(function(id){
							const el = document.getElementById(id);
							if (el) el.value = j.chatThreadID;
						});
						const acts = document.getElementById('chat-thread-actions');
						if (acts) acts.classList.remove('hidden');
					}
				}
			} catch(e) {}
		};
		// Start a fresh conversation: clear ids, replace transcript with empty state,
		// clear composer. Uses outerHTML swap to inject the empty pane in-place.
		window.goonChatNewThread = function() {
			['goon-chat-thread-id','goon-chat-thread-id-mirror'].forEach(function(id){
				const el = document.getElementById(id);
				if (el) el.value = '';
			});
			const acts = document.getElementById('chat-thread-actions');
			if (acts) acts.classList.add('hidden');
			const ta = document.getElementById('goon-chat-input');
			if (ta) { ta.value = ''; ta.style.height = ''; ta.focus(); }
			// Swap the transcript pane via the reset endpoint.
			htmx.ajax('POST', '/api/chat/reset', {target: '#chat-transcript', swap: 'outerHTML'});
		};
		// Load an existing thread from the sidebar.
		window.goonChatLoadThread = function(id) {
			fetch('/api/chat/thread?id=' + encodeURIComponent(id))
				.then(function(r){ if (!r.ok) throw new Error('load failed'); return r.json(); })
				.then(function(t) {
					['goon-chat-thread-id','goon-chat-thread-id-mirror'].forEach(function(fid){
						const el = document.getElementById(fid);
						if (el) el.value = t.id;
					});
					const picker = document.getElementById('chat-repo-context');
					if (picker && t.repo_context) {
						picker.value = t.repo_context;
						const m = document.getElementById('goon-chat-repo-mirror');
						if (m) m.value = t.repo_context;
					}
					const acts = document.getElementById('chat-thread-actions');
					if (acts) acts.classList.remove('hidden');
					const pane = document.getElementById('chat-transcript');
					if (!pane) return;
					pane.innerHTML = '';
					(t.messages || []).forEach(function(m) {
						const wrap = document.createElement('div');
						wrap.className = 'flex ' + (m.role === 'user' ? 'flex-row-reverse' : 'flex-row') + ' gap-2 animate-fade-in';
						const esc = function(s){ return (s||'').replace(/[<>&]/g, function(c){return ({'<':'&lt;','>':'&gt;','&':'&amp;'})[c];}); };
						const avatarCls = m.role === 'user'
							? 'shrink-0 h-7 w-7 rounded-full bg-accent/15 text-accent flex items-center justify-center text-xs font-semibold'
							: 'shrink-0 h-7 w-7 rounded-full bg-gradient-to-br from-accent to-highlight text-white flex items-center justify-center text-[10px] font-bold';
						const bubbleCls = m.role === 'user'
							? 'max-w-[85%] rounded-2xl rounded-tr-md bg-accent text-surface px-3.5 py-2 text-sm whitespace-pre-wrap shadow-sm'
							: 'max-w-[85%] rounded-2xl rounded-tl-md border border-surface-border bg-surface text-gray-200 px-3.5 py-2 text-sm whitespace-pre-wrap shadow-sm';
						const avatarLabel = m.role === 'user' ? 'H' : 'GO';
						wrap.innerHTML =
							'<div class="' + avatarCls + '">' + avatarLabel + '</div>' +
							'<div class="min-w-0 flex flex-col ' + (m.role==='user'?'items-end':'items-start') + '">' +
							'<div class="' + bubbleCls + '">' + esc(m.content) + '</div>' +
							'</div>';
						pane.appendChild(wrap);
					});
					requestAnimationFrame(function(){ pane.scrollTop = pane.scrollHeight; });
				})
				.catch(function(err){ console.error('Could not load thread:', err); });
		};
		// Auto-scroll when htmx appends new bubbles.
		document.addEventListener('htmx:afterSwap', function(e) {
			const t = document.getElementById('chat-transcript');
			if (t && e.target && (e.target.id === 'chat-transcript' || t.contains(e.target))) {
				requestAnimationFrame(function(){ t.scrollTop = t.scrollHeight; });
			}
		});
		// Relative timestamps — update all [data-ts] spans every 30s.
		window.goonUpdateTimestamps = function() {
			var now = Math.floor(Date.now() / 1000);
			document.querySelectorAll('.msg-time[data-ts]').forEach(function(el) {
				var ts = parseInt(el.dataset.ts, 10);
				if (isNaN(ts)) return;
				var diff = now - ts;
				var label;
				if (diff < 60) label = 'just now';
				else if (diff < 3600) label = Math.floor(diff/60) + 'm ago';
				else if (diff < 86400) label = Math.floor(diff/3600) + 'h ago';
				else label = Math.floor(diff/86400) + 'd ago';
				el.textContent = label;
			});
		};
		goonUpdateTimestamps();
		setInterval(goonUpdateTimestamps, 30000);
	})();
	</script>`)
}

// repositoryEntriesForPicker returns the slugs from REPOSITORY.md for
// the chat repo-context picker. Returns nil cleanly when the file is
// missing — the picker collapses to "— no specific repo —" in that
// case so the rest of the page still renders.
func repositoryEntriesForPicker() ([]string, error) {
	entries, err := repository.Read()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		s := strings.TrimSpace(e.Remote)
		if s == "" {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

// fragTabMemory renders the Memory tab as a two-column layout:
// a sidebar nav on the left (Soul / Notes / Skills) and the active
// panel on the right. This replaces the old horizontal tab switcher.
func (s *Server) fragTabMemory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	noteIdx := agentctx.KnowledgeIndex("")
	skillIdx := agentctx.SkillsIndex("")

	// ── Two-column layout ────────────────────────────────────────────────
	fmt.Fprint(w, `<div class="flex gap-6 min-h-[420px]">`)

	// ── Left sidebar nav ─────────────────────────────────────────────────
	fmt.Fprint(w, `<nav class="w-40 shrink-0 flex flex-col">
		<div class="mb-3 px-2 text-[10px] uppercase tracking-wider font-semibold text-muted/60">Memory</div>`)

	type navItem struct{ id, svgPath, label string }
	items := []navItem{
		{"soul",
			`<path d="M12 2l3 6 6 .9-4.5 4.4 1 6.7L12 17l-5.5 3 1-6.7L3 8.9 9 8z"/>`,
			"Soul"},
		{"notes",
			`<path d="M4 19.5A2.5 2.5 0 0 1 6.5 17H20"/><path d="M6.5 2H20v20H6.5A2.5 2.5 0 0 1 4 19.5v-15A2.5 2.5 0 0 1 6.5 2z"/>`,
			"Notes"},
		{"skills",
			`<path d="M13 2L3 14h9l-1 8 10-12h-9l1-8z"/>`,
			"Skills"},
	}
	for i, it := range items {
		activeClass := ""
		if i == 0 {
			activeClass = "bg-accent/10 text-accent font-semibold"
		}
		badge := ""
		if it.id == "notes" && len(noteIdx) > 0 {
			badge = fmt.Sprintf(`<span class="ml-auto text-[10px] font-mono bg-surface-sunken px-1.5 py-0.5 rounded-full shrink-0">%d</span>`, len(noteIdx))
		} else if it.id == "skills" && len(skillIdx) > 0 {
			badge = fmt.Sprintf(`<span class="ml-auto text-[10px] font-mono bg-surface-sunken px-1.5 py-0.5 rounded-full shrink-0">%d</span>`, len(skillIdx))
		}
		fmt.Fprintf(w, `<button type="button" data-mem-nav="%s" onclick="goonMemoryNav('%s')"
			class="w-full text-left px-3 py-2 mb-0.5 rounded-lg text-sm flex items-center gap-2.5 transition text-muted hover:text-ink hover:bg-surface-raised/60 %s">
			<svg class="h-3.5 w-3.5 shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">%s</svg>
			<span>%s</span>%s
		</button>`, it.id, it.id, activeClass, it.svgPath, it.label, badge)
	}

	// Separator + context-aware new buttons
	fmt.Fprint(w, `<div class="mt-auto pt-4 border-t border-surface-border space-y-0.5">
		<button id="mem-new-note-btn" type="button" onclick="goonMemoryNewNote()"
			class="w-full text-left text-xs px-3 py-1.5 rounded-md text-muted hover:text-accent hover:bg-accent/5 transition flex items-center gap-1.5">
			<svg class="h-3 w-3" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><line x1="12" y1="5" x2="12" y2="19"/><line x1="5" y1="12" x2="19" y2="12"/></svg>
			new note
		</button>
		<button id="mem-new-skill-btn" type="button" onclick="goonMemoryNewSkill()"
			class="hidden w-full text-left text-xs px-3 py-1.5 rounded-md text-muted hover:text-accent hover:bg-accent/5 transition flex items-center gap-1.5">
			<svg class="h-3 w-3" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><line x1="12" y1="5" x2="12" y2="19"/><line x1="5" y1="12" x2="19" y2="12"/></svg>
			new skill
		</button>
	</div>
	</nav>`)

	// ── Right content area ───────────────────────────────────────────────
	fmt.Fprint(w, `<div class="flex-1 min-w-0">`)

	// Inline create form — hidden until triggered
	fmt.Fprint(w, `<div id="memory-new-form" class="hidden mb-5 rounded-xl border border-accent/40 bg-surface-raised shadow-card overflow-hidden">
		<div class="px-4 py-3 border-b border-surface-border flex items-center justify-between">
			<span id="memory-new-form-label" class="text-xs font-semibold text-accent uppercase tracking-wider">New note</span>
			<button type="button" onclick="document.getElementById('memory-new-form').classList.add('hidden')"
				class="text-muted hover:text-ink transition text-sm">✕</button>
		</div>
		<form id="memory-new-form-inner" hx-post="/api/memory/write" hx-target="#memory-save-result" hx-swap="innerHTML"
			hx-on::after-request="if(event.detail.successful){this.reset();document.getElementById('memory-new-form').classList.add('hidden');}"
			class="px-4 py-4 space-y-3">
			<input type="text" name="name" required placeholder="api-notes.md, research.md, …"
				class="w-full font-mono text-sm rounded-lg border border-surface-border bg-surface px-3 py-2 text-ink placeholder:text-muted focus:border-accent focus:ring-1 focus:ring-accent/40 focus:outline-none">
			<textarea name="body" rows="5" required placeholder="# Notes&#10;- …"
				class="w-full font-mono text-sm rounded-lg border border-surface-border bg-surface px-3 py-2 text-ink placeholder:text-muted focus:border-accent focus:ring-1 focus:ring-accent/40 focus:outline-none resize-none"></textarea>
			<div class="flex items-center justify-between gap-3">
				<div id="memory-save-result" class="text-xs"></div>
				<button type="submit"
					class="inline-flex items-center gap-1.5 rounded-lg bg-accent text-surface px-4 py-2 text-xs font-semibold hover:brightness-110 transition">save</button>
			</div>
		</form>
	</div>`)

	// Soul panel — default visible
	fmt.Fprint(w, `<div data-mem-panel="soul">`)
	s.renderSoulPanel(w)
	fmt.Fprint(w, `</div>`)

	// Notes panel — hidden by default
	fmt.Fprint(w, `<div data-mem-panel="notes" class="hidden">`)
	s.renderNotesPanel(w, noteIdx)
	fmt.Fprint(w, `</div>`)

	// Skills panel — hidden by default
	fmt.Fprint(w, `<div data-mem-panel="skills" class="hidden">`)
	s.renderSkillsBody(w)
	fmt.Fprint(w, `</div>`)

	fmt.Fprint(w, `</div>`) // end content
	fmt.Fprint(w, `</div>`) // end flex

	// ── JS ───────────────────────────────────────────────────────────────
	fmt.Fprint(w, `<script>
	(function() {
		// Sidebar nav switch: show panel, highlight item, toggle new buttons
		window.goonMemoryNav = function(target) {
			document.querySelectorAll('[data-mem-panel]').forEach(function(el) {
				el.classList.toggle('hidden', el.dataset.memPanel !== target);
			});
			document.querySelectorAll('[data-mem-nav]').forEach(function(btn) {
				var active = btn.dataset.memNav === target;
				btn.classList.toggle('bg-accent/10', active);
				btn.classList.toggle('text-accent', active);
				btn.classList.toggle('font-semibold', active);
				btn.classList.toggle('text-muted', !active);
				if (active) btn.setAttribute('aria-current', 'page');
				else btn.removeAttribute('aria-current');
			});
			var noteBtn  = document.getElementById('mem-new-note-btn');
			var skillBtn = document.getElementById('mem-new-skill-btn');
			if (noteBtn)  noteBtn.classList.toggle('hidden',  target === 'skills' || target === 'soul');
			if (skillBtn) skillBtn.classList.toggle('hidden', target !== 'skills');
			// Reset form endpoint to match panel
			var form = document.getElementById('memory-new-form-inner');
			var lbl  = document.getElementById('memory-new-form-label');
			if (form && lbl) {
				if (target === 'skills') {
					form.setAttribute('hx-post', '/api/skills/write');
					lbl.textContent = 'New skill';
				} else {
					form.setAttribute('hx-post', '/api/memory/write');
					lbl.textContent = 'New note';
				}
				htmx.process(form);
			}
		};
		window.goonMemoryNewNote = function() {
			var f = document.getElementById('memory-new-form');
			if (f) {
				f.classList.remove('hidden');
				var inp = f.querySelector('input[name=name]');
				if (inp) setTimeout(function(){ inp.focus(); }, 30);
			}
		};
		window.goonMemoryNewSkill = function() {
			var form = document.getElementById('memory-new-form-inner');
			var lbl  = document.getElementById('memory-new-form-label');
			if (form) { form.setAttribute('hx-post', '/api/skills/write'); htmx.process(form); }
			if (lbl)  { lbl.textContent = 'New skill'; }
			window.goonMemoryNewNote();
		};
		window.goonSoulToggleEdit = function() {
			var pw  = document.getElementById('soul-preview-wrap');
			var ed  = document.getElementById('soul-editor');
			var btn = document.getElementById('soul-edit-btn');
			if (!ed) return;
			var editing = !ed.classList.contains('hidden');
			if (editing) {
				ed.classList.add('hidden');
				if (pw)  pw.classList.remove('hidden');
				if (btn) btn.textContent = 'edit';
			} else {
				ed.classList.remove('hidden');
				if (pw)  pw.classList.add('hidden');
				if (btn) btn.textContent = 'cancel';
				var ta = ed.querySelector('textarea');
				if (ta) setTimeout(function(){ ta.focus(); ta.setSelectionRange(0,0); }, 20);
			}
		};
		window.goonSoulToggleFull = function() {
			var el  = document.getElementById('soul-full-content');
			var btn = document.getElementById('soul-show-more');
			if (!el) return;
			var hidden = el.classList.toggle('hidden');
			if (btn) btn.textContent = hidden ? btn.textContent.replace('↑','↓') : btn.textContent.replace('↓','↑');
		};
		window.goonToggleNote = function(slug, nameQ) {
			var body    = document.getElementById('note-body-' + slug);
			var chevron = document.getElementById('note-chevron-' + slug);
			if (!body) return;
			var isOpen = !body.classList.contains('hidden');
			if (isOpen) {
				body.classList.add('hidden');
				if (chevron) chevron.style.transform = '';
			} else {
				body.classList.remove('hidden');
				if (chevron) chevron.style.transform = 'rotate(90deg)';
				if (!body.dataset.loaded) {
					body.dataset.loaded = '1';
					body.innerHTML = '<div class="px-4 py-3 space-y-1.5 animate-pulse"><div class="h-3 bg-surface-sunken rounded w-1/3 mb-2"></div><div class="h-3 bg-surface-sunken rounded w-full"></div></div>';
					htmx.ajax('GET', '/api/knowledge/note?name=' + nameQ, {target: '#note-body-' + slug, swap: 'innerHTML'});
				}
			}
		};
		window.goonFilterNotes = function(q) {
			var lq = q.trim().toLowerCase();
			document.querySelectorAll('[data-note-name]').forEach(function(row) {
				var nm = (row.dataset.noteName      || '').toLowerCase();
				var hl = (row.dataset.noteHeadline  || '').toLowerCase();
				row.style.display = (!lq || nm.includes(lq) || hl.includes(lq)) ? '' : 'none';
			});
		};
	})();
	</script>`)
}

// renderSoulPanel renders the SOUL.md card with inline edit and show-more.
func (s *Server) renderSoulPanel(w http.ResponseWriter) {
	soul := stripSoulMigrationBanner(agentctx.Soul(""))
	soulTrimmed := strings.TrimSpace(soul)
	if soulTrimmed == "" {
		fmt.Fprint(w, `<div class="mb-5 rounded-xl border border-dashed border-accent/30 bg-surface-raised/60 p-6 text-center">
			<div class="mx-auto h-10 w-10 rounded-xl bg-accent-soft text-accent flex items-center justify-center mb-2">
				<svg class="h-5 w-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M9 11l3 3L22 4"/><path d="M21 12v7a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h11"/></svg>
			</div>
			<div class="text-sm font-medium text-ink">SOUL.md is empty</div>
			<div class="mt-1 text-xs text-muted max-w-md mx-auto">
				Seed it with <code class="font-mono text-accent">goon memory init</code> — facts in here are visible to the agent on every run.
			</div>
		</div>`)
	} else {
		lines := strings.Split(soulTrimmed, "\n")
		const previewN = 20
		previewText := strings.Join(lines, "\n")
		var restText string
		if len(lines) > previewN {
			previewText = strings.Join(lines[:previewN], "\n")
			restText = strings.Join(lines[previewN:], "\n")
		}
		fmt.Fprint(w, `<div class="mb-5 rounded-xl border border-accent/25 bg-surface-raised overflow-hidden shadow-card">
			<div class="flex items-center justify-between px-4 py-2.5 border-b border-surface-border">
				<div class="flex items-center gap-2">
					<svg class="h-4 w-4 text-accent" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 2l3 6 6 .9-4.5 4.4 1 6.7L12 17l-5.5 3 1-6.7L3 8.9 9 8z"/></svg>
					<span class="text-xs font-semibold text-accent uppercase tracking-wider">SOUL.md</span>
					<span class="text-[10px] text-muted font-mono bg-surface-sunken px-1.5 py-0.5 rounded-md">always-loaded</span>
				</div>
				<button id="soul-edit-btn" type="button" onclick="goonSoulToggleEdit()"
					class="text-xs text-muted hover:text-accent transition font-medium">edit</button>
			</div>
			<div id="soul-preview-wrap" class="px-4 py-3">
				<pre class="text-xs font-mono text-ink whitespace-pre-wrap leading-relaxed">`)
		fmt.Fprint(w, html.EscapeString(previewText))
		fmt.Fprint(w, `</pre>`)
		if restText != "" {
			fmt.Fprint(w, `<div id="soul-full-content" class="hidden"><pre class="text-xs font-mono text-ink whitespace-pre-wrap leading-relaxed">`)
			fmt.Fprint(w, html.EscapeString(restText))
			fmt.Fprintf(w, `</pre></div>
			<button id="soul-show-more" type="button" onclick="goonSoulToggleFull()"
				class="mt-2 text-[10px] text-accent hover:brightness-125 transition">show all (%d lines) ↓</button>`, len(lines))
		}
		fmt.Fprint(w, `</div>
			<div id="soul-editor" class="hidden border-t border-surface-border px-4 py-3">
				<form hx-post="/api/memory/write" hx-target="#soul-save-result" hx-swap="innerHTML"
					hx-on::after-request="if(event.detail.successful){goonSoulToggleEdit();}"
					class="space-y-2">
					<input type="hidden" name="name" value="SOUL.md">
					<textarea name="body" rows="14"
						class="w-full font-mono text-xs rounded-lg border border-surface-border bg-surface px-3 py-2 text-gray-200 focus:border-accent focus:ring-1 focus:ring-accent/40 focus:outline-none resize-none leading-relaxed">`)
		fmt.Fprint(w, html.EscapeString(soulTrimmed))
		fmt.Fprint(w, `</textarea>
					<div class="flex items-center justify-between gap-3">
						<div id="soul-save-result" class="text-xs text-emerald-700 dark:text-emerald-400"></div>
						<div class="flex items-center gap-2">
							<button type="button" onclick="goonSoulToggleEdit()" class="text-xs text-muted hover:text-ink transition">cancel</button>
							<button type="submit" class="inline-flex items-center gap-1 rounded-md bg-accent text-surface px-3 py-1.5 text-xs font-semibold hover:brightness-110 transition">save</button>
						</div>
					</div>
				</form>
			</div>
		</div>`)
	}
}

// renderNotesPanel renders the topic-notes list with search filter and
// the Obsidian vault card when configured. idx is the pre-fetched
// KnowledgeIndex slice.
func (s *Server) renderNotesPanel(w http.ResponseWriter, idx []agentctx.IndexEntry) {
	if len(idx) == 0 {
		fmt.Fprint(w, `<div class="rounded-xl border border-dashed border-surface-border bg-surface-raised/40 p-6 text-center">
			<div class="mx-auto h-10 w-10 rounded-xl bg-surface-sunken text-muted flex items-center justify-center mb-2">
				<svg class="h-5 w-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M4 19.5A2.5 2.5 0 0 1 6.5 17H20"/><path d="M6.5 2H20v20H6.5A2.5 2.5 0 0 1 4 19.5v-15A2.5 2.5 0 0 1 6.5 2z"/></svg>
			</div>
			<div class="text-sm font-medium text-ink">No topic notes yet</div>
			<div class="mt-1 text-xs text-muted max-w-md mx-auto">
				Workflows write here as they learn. You can also create them with the + new note button above.
			</div>
		</div>`)
	} else {
		fmt.Fprintf(w, `<div class="flex items-center gap-3 mb-3">
			<label class="relative flex-1 max-w-xs">
				<svg class="absolute left-2.5 top-1/2 -translate-y-1/2 h-3 w-3 text-muted pointer-events-none" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="11" cy="11" r="8"/><line x1="21" y1="21" x2="16.65" y2="16.65"/></svg>
				<input type="search" placeholder="filter notes…" oninput="goonFilterNotes(this.value)"
					class="w-full rounded-md border border-surface-border bg-surface text-xs pl-7 pr-3 py-1.5 text-ink placeholder:text-muted focus:border-accent focus:outline-none">
			</label>
			<span class="text-[10px] font-mono text-muted bg-surface-sunken px-2 py-0.5 rounded-full shrink-0">%d notes</span>
		</div>
		<div id="notes-list" class="space-y-1.5">`, len(idx))

		for _, e := range idx {
			slug := noteSlug(e.Name)
			nameEsc := html.EscapeString(e.Name)
			nameQ := url.QueryEscape(e.Name)
			headlineEsc := html.EscapeString(e.Headline)
			if headlineEsc == "" {
				headlineEsc = `<span class="italic text-muted/60">(no headline)</span>`
			}
			fmt.Fprintf(w,
				`<div data-note-name="%s" data-note-headline="%s"
					class="group rounded-lg border border-surface-border bg-surface-raised hover:border-accent/40 transition overflow-hidden">
					<div class="flex items-center gap-3 px-4 py-2.5 cursor-pointer select-none"
						onclick="goonToggleNote('%s','%s')">
						<svg id="note-chevron-%s" class="h-3.5 w-3.5 text-muted shrink-0 transition-transform duration-150"
							viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
							<polyline points="9 18 15 12 9 6"/>
						</svg>
						<div class="flex-1 min-w-0">
							<div class="text-xs font-mono text-gray-200 truncate">%s</div>
							<div class="text-[11px] text-muted truncate">%s</div>
						</div>
						<div class="opacity-0 group-hover:opacity-100 flex items-center gap-1 shrink-0 ml-2 transition-opacity"
							onclick="event.stopPropagation()">
							<form hx-post="/api/memory/delete" hx-target="#memory-list-result" hx-swap="innerHTML" class="m-0">
								<input type="hidden" name="name" value="%s">
								<button type="submit" onclick="return confirm('Delete this note?')" class="text-[10px] text-muted hover:text-rose-500 px-1.5 py-1 rounded transition" title="delete">✕</button>
							</form>
						</div>
					</div>
					<div id="note-body-%s" class="hidden border-t border-surface-border"></div>
				</div>`,
				html.EscapeString(e.Name), html.EscapeString(e.Headline),
				jsEscape(slug), nameQ,
				slug,
				nameEsc, headlineEsc,
				nameEsc,
				slug,
			)
		}
		fmt.Fprint(w, `</div>
		<div id="memory-list-result" class="mt-3"></div>`)
	}

	// ── Obsidian vault card (only when GOON_OBSIDIAN_VAULT is set) ──────────
	if tools.ObsidianConfigured() {
		vaultPath := strings.TrimSpace(os.Getenv("GOON_OBSIDIAN_VAULT"))
		hasRepo := strings.TrimSpace(os.Getenv("GOON_OBSIDIAN_REPO")) != ""
		repoLabel := ""
		if hasRepo {
			repoLabel = ` <span class="text-[10px] text-muted font-mono">git-backed</span>`
		}
		fmt.Fprintf(w, `
	<div class="mt-6 rounded-xl border border-surface-border bg-surface-raised">
		<div class="flex items-center justify-between px-4 py-3 border-b border-surface-border">
			<div class="flex items-center gap-2">
				<svg class="h-4 w-4 text-accent" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M2 3h6a4 4 0 0 1 4 4v14a3 3 0 0 0-3-3H2z"/><path d="M22 3h-6a4 4 0 0 0-4 4v14a3 3 0 0 1 3-3h7z"/></svg>
				<span class="text-xs font-semibold uppercase tracking-wider text-muted">Obsidian Vault</span>
				%s
			</div>
			<span class="font-mono text-[10px] text-gray-500 truncate max-w-[200px]" title="%s">%s</span>
		</div>
		<div class="px-4 py-3 flex items-center justify-between gap-4 flex-wrap">
			<p class="text-xs text-muted">
				Agent tools: <code class="text-accent">obsidian_list</code> · <code class="text-accent">obsidian_read</code> · <code class="text-accent">obsidian_search</code>
			</p>
			<div class="flex items-center gap-2">
				<button
					hx-post="/api/obsidian/sync"
					hx-target="#obsidian-sync-result"
					hx-swap="innerHTML"
					hx-indicator="#obsidian-sync-spinner"
					class="inline-flex items-center gap-1.5 rounded-lg border border-surface-border bg-surface px-3 py-1.5 text-xs font-medium hover:border-accent/50 hover:text-accent transition">
					<svg id="obsidian-sync-spinner" class="htmx-indicator h-3 w-3 animate-spin" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><path d="M21 12a9 9 0 1 1-6.219-8.56"/></svg>
					<svg class="h-3.5 w-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="1 4 1 10 7 10"/><path d="M3.51 15a9 9 0 1 0 .49-3.5"/></svg>
					sync vault
				</button>
				<button
					hx-post="/api/obsidian/push"
					hx-target="#obsidian-sync-result"
					hx-swap="innerHTML"
					class="inline-flex items-center gap-1.5 rounded-lg border border-surface-border bg-surface px-3 py-1.5 text-xs font-medium hover:border-accent/50 hover:text-accent transition">
					<svg class="h-3.5 w-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><line x1="12" y1="19" x2="12" y2="5"/><polyline points="5 12 12 5 19 12"/></svg>
					push
				</button>
			</div>
		</div>
		<div id="obsidian-sync-result" class="px-4 text-xs font-mono text-muted empty:hidden"></div>
		<div id="obsidian-notes-list"
			hx-get="/fragments/obsidian-notes"
			hx-trigger="load, obsidianSynced from:body"
			hx-swap="innerHTML"
			class="border-t border-surface-border px-4 py-3">
			<span class="text-xs text-muted">loading notes…</span>
		</div>
	</div>`, repoLabel, html.EscapeString(vaultPath), html.EscapeString(vaultPath))
	}
}

// stripSoulMigrationBanner removes the one-time "<!-- migrated from
// personal.md on YYYY-MM-DD ... -->" leading HTML comment that the
// MergePersonalIntoSoul migration prepends to SOUL.md. The comment
// is fine on disk (it's a real audit trail), but in the Memory tab
// we render SOUL.md inside an <pre> with html.EscapeString, so the
// raw comment shows up verbatim to the user — looks like junk in
// an otherwise clean knowledge view. We strip it from the displayed
// copy only.
//
// Also strips any other leading run of HTML comments + blank lines
// so future one-time migration banners get the same treatment.
func stripSoulMigrationBanner(s string) string {
	for {
		trimmed := strings.TrimLeft(s, " \t\r\n")
		if !strings.HasPrefix(trimmed, "<!--") {
			return trimmed
		}
		end := strings.Index(trimmed, "-->")
		if end < 0 {
			return trimmed // malformed; leave as-is
		}
		s = trimmed[end+3:]
	}
}

// urlQueryEscape is a tiny wrapper so we don't import net/url at the
// call site for one use.
func urlQueryEscape(s string) string {
	return strings.NewReplacer(
		" ", "%20",
		"#", "%23",
		"&", "%26",
		"+", "%2B",
		"?", "%3F",
	).Replace(s)
}

// noteSlug turns a note name into a safe HTML element id suffix by
// replacing non-alphanumeric runs with a single hyphen.
func noteSlug(name string) string {
	var b strings.Builder
	prev := false
	for _, c := range name {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
			prev = false
		} else if !prev {
			b.WriteByte('-')
			prev = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// handleKnowledgeNote returns the full body of one note as a styled
// fragment, called lazily from the details-expand handler.
func (s *Server) handleKnowledgeNote(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	body, err := agentctx.ReadNote("", name)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err != nil {
		fmt.Fprintf(w, `<div class="text-xs text-rose-500">%s</div>`, html.EscapeString(err.Error()))
		return
	}
	body = strings.TrimSpace(body)
	if body == "" {
		fmt.Fprint(w, `<div class="text-xs text-gray-500">(empty)</div>`)
		return
	}
	fmt.Fprint(w, `<div class="px-4 py-3"><pre class="text-xs font-mono text-ink whitespace-pre-wrap leading-relaxed bg-surface-sunken rounded-md p-3 border border-surface-border overflow-x-auto">`)
	fmt.Fprint(w, html.EscapeString(body))
	fmt.Fprint(w, `</pre></div>`)
}

// handleRefresh forces a fresh board.List and updates memory so the
// next ticket render / chat turn sees the latest snapshot. Returns
// an htmx-friendly fragment plus statusChanged so the header pill
// + ticket table refresh.
func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.opts.Board == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<div class="rounded-md bg-amber-500/10 border border-amber-500/30 px-3 py-2 text-sm text-amber-700 dark:text-amber-400">No board configured. Set GOON_BOARD in the Configuration tab.</div>`)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	n, err := agentctx.RefreshTickets(ctx, s.opts.Memory, s.opts.Board)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err != nil {
		fmt.Fprintf(w, `<div class="rounded-md bg-rose-500/10 border border-rose-500/30 px-3 py-2 text-sm text-rose-700 dark:text-rose-400">refresh failed: %s</div>`, html.EscapeString(err.Error()))
		return
	}
	w.Header().Set("HX-Trigger", "ticketsChanged, statusChanged")
	s.events.Publish("ticketsChanged")
	s.events.Publish("statusChanged")
	fmt.Fprintf(w, `<div class="rounded-md bg-emerald-500/10 border border-emerald-500/30 px-3 py-2 text-sm text-emerald-700 dark:text-emerald-400">✓ pulled %d ticket(s) from the board</div>`, n)
}

// Keep notes import alive for tests/future references even if every
// caller goes through agentctx today.
var _ = notes.SoulFilename

// --- Memory + Skills CRUD --------------------------------------------------
//
// Two parallel stores, two parallel handlers. We keep them in this
// file (chat.go already owns the Knowledge tab) instead of a new
// file because the surface is small.

// handleMemoryWrite creates or replaces a memory note. Body comes
// from a form (`name`, `body`). Returns a small confirmation +
// fires HX-Trigger so the Knowledge tab refreshes its list.
func (s *Server) handleMemoryWrite(w http.ResponseWriter, r *http.Request) {
	s.handleStoreWrite(w, r, "memory", agentctx.WriteNote)
}

// handleMemoryDelete removes a memory note. Triggered by per-note
// delete buttons in the Knowledge tab.
func (s *Server) handleMemoryDelete(w http.ResponseWriter, r *http.Request) {
	s.handleStoreDelete(w, r, "memory", agentctx.DeleteNote)
}

// handleSkillWrite — same as memory but for the skills store.
func (s *Server) handleSkillWrite(w http.ResponseWriter, r *http.Request) {
	s.handleStoreWrite(w, r, "skills", agentctx.WriteSkill)
}

// handleSkillDelete — same as memory but for the skills store.
func (s *Server) handleSkillDelete(w http.ResponseWriter, r *http.Request) {
	s.handleStoreDelete(w, r, "skills", agentctx.DeleteSkill)
}

// handleSkillNote returns one skill's body — analogue of
// handleKnowledgeNote for the skills store.
func (s *Server) handleSkillNote(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	body, err := agentctx.ReadSkill("", name)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err != nil {
		fmt.Fprintf(w, `<div class="text-xs text-rose-500">%s</div>`, html.EscapeString(err.Error()))
		return
	}
	body = strings.TrimSpace(body)
	if body == "" {
		fmt.Fprint(w, `<div class="text-xs text-gray-500">(empty)</div>`)
		return
	}
	fmt.Fprint(w, `<div class="px-4 py-3"><pre class="text-xs font-mono text-ink whitespace-pre-wrap leading-relaxed bg-surface-sunken rounded-md p-3 border border-surface-border overflow-x-auto">`)
	fmt.Fprint(w, html.EscapeString(body))
	fmt.Fprint(w, `</pre></div>`)
}

// handleStoreWrite is the shared implementation for the memory and
// skill write endpoints. kind controls the HX-Trigger event and the
// confirmation label; writeFn delegates the actual save to whichever
// agentctx helper matches.
func (s *Server) handleStoreWrite(w http.ResponseWriter, r *http.Request, kind string, writeFn func(string, string, string) (string, error)) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	body := r.FormValue("body")
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	if _, err := writeFn("", name, body); err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<div class="rounded-md bg-rose-500/10 border border-rose-500/30 px-3 py-2 text-sm text-rose-700 dark:text-rose-400">%s save failed: %s</div>`,
			html.EscapeString(kind), html.EscapeString(err.Error()))
		return
	}
	w.Header().Set("HX-Trigger", kind+"Changed")
	s.events.Publish(kind + "Changed")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<div class="rounded-md bg-emerald-500/10 border border-emerald-500/30 px-3 py-2 text-sm text-emerald-700 dark:text-emerald-400">✓ saved %s · <span class="font-mono">%s</span></div>`,
		html.EscapeString(kind), html.EscapeString(name))
}

// handleStoreDelete is the shared implementation for the memory and
// skill delete endpoints.
func (s *Server) handleStoreDelete(w http.ResponseWriter, r *http.Request, kind string, deleteFn func(string, string) error) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	if err := deleteFn("", name); err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<div class="rounded-md bg-rose-500/10 border border-rose-500/30 px-3 py-2 text-sm text-rose-700 dark:text-rose-400">delete failed: %s</div>`,
			html.EscapeString(err.Error()))
		return
	}
	w.Header().Set("HX-Trigger", kind+"Changed")
	s.events.Publish(kind + "Changed")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<div class="rounded-md bg-gray-500/10 border border-gray-500/30 px-3 py-2 text-sm text-gray-700 dark:text-gray-400">✓ deleted %s · <span class="font-mono">%s</span></div>`,
		html.EscapeString(kind), html.EscapeString(name))
}

// renderSkillsBody renders the Skills tab content. The create form
// lives in the fragTabMemory header; this just triggers the lazy list.
func (s *Server) renderSkillsBody(w http.ResponseWriter) {
	fmt.Fprint(w, `<div hx-get="/fragments/skills-list" hx-trigger="load, skillsChanged from:body" hx-swap="innerHTML">
		<div class="space-y-2 animate-pulse">
			<div class="h-4 bg-surface-raised rounded w-3/4"></div>
			<div class="h-4 bg-surface-raised rounded w-1/2"></div>
			<div class="h-4 bg-surface-raised rounded w-2/3"></div>
		</div>
	</div>`)
}

// fragSkillsList renders the skill index as card rows with lazy-load
// content expansion. Auto-refreshed on skillsChanged.
func (s *Server) fragSkillsList(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	idx := agentctx.SkillsIndex("")
	if len(idx) == 0 {
		_, _ = io.WriteString(w, emptyState("No skills yet",
			"Use the + new skill button above, or goon skill write &lt;name&gt; via Telegram."))
		return
	}
	fmt.Fprintf(w, `<div class="flex items-center gap-3 mb-3">
		<span class="text-[11px] font-semibold uppercase tracking-wider text-muted">Skills</span>
		<span class="text-[10px] font-mono text-muted bg-surface-sunken px-2 py-0.5 rounded-full">%d</span>
	</div>
	<div id="skills-list" class="space-y-1.5">`, len(idx))

	for _, e := range idx {
		slug := noteSlug(e.Name)
		displayName := strings.TrimSuffix(e.Name, ".md")
		nameEsc := html.EscapeString(displayName)
		rawNameEsc := html.EscapeString(e.Name)
		nameQ := url.QueryEscape(e.Name)
		headlineEsc := html.EscapeString(e.Headline)
		if headlineEsc == "" {
			headlineEsc = `<span class="italic text-muted/60">(no headline)</span>`
		}
		fmt.Fprintf(w,
			`<div data-note-name="%s" data-note-headline="%s"
				class="group rounded-lg border border-surface-border bg-surface-raised hover:border-accent/40 transition overflow-hidden">
				<div class="flex items-center gap-3 px-4 py-2.5 cursor-pointer select-none"
					onclick="goonToggleSkill('%s','%s')">
					<svg id="skill-chevron-%s" class="h-3.5 w-3.5 text-muted shrink-0 transition-transform duration-150"
						viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
						<polyline points="9 18 15 12 9 6"/>
					</svg>
					<div class="flex-1 min-w-0">
						<div class="text-xs font-mono text-gray-200 truncate">%s</div>
						<div class="text-[11px] text-muted truncate">%s</div>
					</div>
					<div class="opacity-0 group-hover:opacity-100 flex items-center gap-1 shrink-0 ml-2 transition-opacity"
						onclick="event.stopPropagation()">
						<form hx-post="/api/skills/delete" hx-target="#skill-list-result" hx-swap="innerHTML" class="m-0">
							<input type="hidden" name="name" value="%s">
							<button type="submit" onclick="return confirm('Delete this skill?')" class="text-[10px] text-muted hover:text-rose-500 px-1.5 py-1 rounded transition" title="delete">✕</button>
						</form>
					</div>
				</div>
				<div id="skill-body-%s" class="hidden border-t border-surface-border"></div>
			</div>`,
			rawNameEsc, html.EscapeString(e.Headline),
			jsEscape(slug), nameQ,
			slug,
			nameEsc, headlineEsc,
			rawNameEsc,
			slug,
		)
	}
	fmt.Fprint(w, `</div>
	<div id="skill-list-result" class="mt-3"></div>
	<script>
	(function(){
		if (window.goonToggleSkill) return;
		window.goonToggleSkill = function(slug, nameQ) {
			var body = document.getElementById('skill-body-' + slug);
			var chevron = document.getElementById('skill-chevron-' + slug);
			if (!body) return;
			var isOpen = !body.classList.contains('hidden');
			if (isOpen) {
				body.classList.add('hidden');
				if (chevron) chevron.style.transform = '';
			} else {
				body.classList.remove('hidden');
				if (chevron) chevron.style.transform = 'rotate(90deg)';
				if (!body.dataset.loaded) {
					body.dataset.loaded = '1';
					body.innerHTML = '<div class="px-4 py-3 space-y-1.5"><div class="skel h-3 w-1/3"></div><div class="skel h-3 w-full"></div></div>';
					htmx.ajax('GET', '/api/skills/note?name=' + nameQ, {target: '#skill-body-' + slug, swap: 'innerHTML'});
				}
			}
		};
	})();
	</script>`)
}

// handleObsidianSync runs git pull + store reload and returns a one-line
// status fragment that the web UI swaps into #obsidian-sync-result.
// Fires HX-Trigger: obsidianSynced so the notes list auto-reloads.
func (s *Server) handleObsidianSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("HX-Trigger", "obsidianSynced")
	msg := tools.ObsidianSync()
	fmt.Fprintf(w, `<p class="pb-3">%s</p>`, html.EscapeString(msg))
}

// fragObsidianNotes renders the note list grouped by top-level folder.
// Each note has a ▸ button that expands to show the raw markdown via
// /api/obsidian/read?note=<path>.
func (s *Server) fragObsidianNotes(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	notes, err := tools.ObsidianList("")
	if err != nil {
		fmt.Fprintf(w, `<p class="text-xs text-red-400">%s</p>`, html.EscapeString(err.Error()))
		return
	}
	if notes == "" {
		fmt.Fprint(w, `<p class="text-xs text-muted">No notes found — run a sync first.</p>`)
		return
	}

	// Group by top-level folder (or "—" for root notes).
	type entry struct{ name, display string }
	groups := map[string][]entry{}
	var order []string
	seen := map[string]bool{}

	for _, line := range strings.Split(strings.TrimSpace(notes), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "/", 2)
		var folder, display string
		if len(parts) == 2 {
			folder = parts[0]
			display = parts[1]
			if strings.HasSuffix(display, ".md") {
				display = display[:len(display)-3]
			}
		} else {
			folder = "—"
			display = line
			if strings.HasSuffix(display, ".md") {
				display = display[:len(display)-3]
			}
		}
		if !seen[folder] {
			seen[folder] = true
			order = append(order, folder)
		}
		groups[folder] = append(groups[folder], entry{name: line, display: display})
	}

	var b strings.Builder
	b.WriteString(`<div class="space-y-2 text-xs">`)
	for _, folder := range order {
		entries := groups[folder]
		b.WriteString(`<details class="group">`)
		fmt.Fprintf(&b,
			`<summary class="flex items-center gap-1.5 cursor-pointer list-none text-muted hover:text-accent font-medium py-0.5 select-none">
				<svg class="h-3 w-3 transition-transform group-open:rotate-90" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><polyline points="9 18 15 12 9 6"/></svg>
				<span>%s</span>
				<span class="ml-auto text-[10px] text-gray-500">%d</span>
			</summary>
			<ul class="mt-1 ml-4 space-y-0.5 border-l border-surface-border pl-3">`,
			html.EscapeString(folder), len(entries))
		for _, e := range entries {
			noteQ := url.QueryEscape(e.name)
			fmt.Fprintf(&b,
				`<li class="py-0.5">
					<button
						hx-get="/api/obsidian/read?note=%s"
						hx-target="#obsidian-note-reader"
						hx-swap="innerHTML"
						class="w-full text-left truncate text-ink hover:text-accent transition cursor-pointer">
						%s
					</button>
				</li>`,
				noteQ, html.EscapeString(e.display))
		}
		b.WriteString(`</ul></details>`)
	}
	b.WriteString(`</div>`)
	b.WriteString(`<div id="obsidian-note-reader" class="mt-3 empty:hidden"></div>`)
	fmt.Fprint(w, b.String())
}

// handleObsidianRead returns one Obsidian note in an editable panel swapped
// into #obsidian-note-reader. The panel has a Save button (POST to
// /api/obsidian/write) and a Push button (POST to /api/obsidian/push).
func (s *Server) handleObsidianRead(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	note := strings.TrimSpace(r.URL.Query().Get("note"))
	if note == "" {
		fmt.Fprint(w, `<p class="text-xs text-red-400">note parameter missing</p>`)
		return
	}
	body, err := tools.ObsidianRead(note)
	if err != nil {
		fmt.Fprintf(w, `<p class="text-xs text-red-400">%s</p>`, html.EscapeString(err.Error()))
		return
	}
	noteQ := url.QueryEscape(note)
	fmt.Fprintf(w,
		`<div class="rounded-lg border border-surface-border bg-surface p-3 mt-3" id="obsidian-editor">
			<div class="flex items-center justify-between mb-2 gap-2">
				<span class="text-[10px] font-mono text-muted truncate flex-1">%s</span>
				<button onclick="document.getElementById('obsidian-editor').remove()"
					class="text-[10px] text-gray-500 hover:text-accent shrink-0">✕</button>
			</div>
			<form hx-post="/api/obsidian/write?note=%s"
				hx-target="#obsidian-save-result"
				hx-swap="innerHTML"
				hx-encoding="multipart/form-data">
				<textarea name="body"
					class="w-full text-[11px] text-gray-200 bg-transparent border border-surface-border rounded p-2 font-mono resize-y min-h-[16rem] focus:outline-none focus:border-accent/60"
					spellcheck="false">%s</textarea>
				<div class="flex items-center gap-2 mt-2">
					<button type="submit"
						class="text-[11px] px-3 py-1 rounded bg-accent/20 hover:bg-accent/30 text-accent border border-accent/30 transition">
						save
					</button>
					<span id="obsidian-save-result" class="text-[10px] text-gray-500 ml-1"></span>
				</div>
			</form>
		</div>`,
		html.EscapeString(note), noteQ, html.EscapeString(body))
}

// handleObsidianWrite saves the posted body to the vault note named in the
// query param. Returns a short status fragment for #obsidian-save-result.
func (s *Server) handleObsidianWrite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	note := strings.TrimSpace(r.URL.Query().Get("note"))
	if note == "" {
		fmt.Fprint(w, `<span class="text-red-400">note param missing</span>`)
		return
	}
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		r.ParseForm() //nolint:errcheck
	}
	body := r.FormValue("body")
	if err := tools.ObsidianWrite(note, body); err != nil {
		fmt.Fprintf(w, `<span class="text-red-400">%s</span>`, html.EscapeString(err.Error()))
		return
	}
	fmt.Fprint(w, `<span class="text-green-400">saved ✓</span>`)
}

// handleObsidianPush commits and pushes pending vault changes.
// Returns a short status fragment for #obsidian-save-result.
func (s *Server) handleObsidianPush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// First save if the form body was forwarded (it won't be from the push
	// button alone — that's fine, push just commits whatever's on disk).
	msg := tools.ObsidianPush()
	safe := html.EscapeString(msg)
	if strings.HasPrefix(msg, "git") && strings.Contains(msg, "failed") {
		fmt.Fprintf(w, `<span class="text-red-400">%s</span>`, safe)
		return
	}
	fmt.Fprintf(w, `<span class="text-green-400">%s</span>`, safe)
}
