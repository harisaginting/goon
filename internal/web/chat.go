package web

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/harisaginting/goon/internal/agentctx"
	"github.com/harisaginting/goon/internal/llm"
	"github.com/harisaginting/goon/internal/notes"
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
  the CLI's ` + "`" + `goon "<task>"` + "`" + ` for the agent runtime — that's outside chat scope.`

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

	// Snapshot existing history (without the new user turn) so the
	// agent-loop helper can append both turns atomically at the end.
	s.chatMu.Lock()
	historySnapshot := append([]llm.Message(nil), s.chatHistory...)
	s.chatMu.Unlock()

	// Run the LLM↔tool loop. The agent decides whether to query the
	// board live (search_jira tool) or answer from the GOON STATE
	// block alone. No more pre-fetch refresh — the agent pulls only
	// what it needs.
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	result, err := agentctx.ChatTurn(ctx, agentctx.ChatTurnOptions{
		LLM:          s.opts.LLM,
		Memory:       s.opts.Memory,
		Board:        s.opts.Board,
		SystemPrompt: chatSystemPrompt,
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

	// Return TWO bubbles back-to-back: the user's message echoed
	// (in case the form clear races the response render) + the
	// assistant reply. htmx's beforeend swap on the message column
	// appends both.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	writeChatBubble(w, "user", msg)
	writeChatBubble(w, "assistant", reply)
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
	io.WriteString(w, chatTranscriptEmpty)
}

// chatTranscriptEmpty is the "nothing here yet" pane rendered before
// the first message and after /api/chat/reset. Includes clickable
// example prompts that auto-fill the composer.
const chatTranscriptEmpty = `<div id="chat-transcript" class="flex flex-col gap-4 min-h-[280px] max-h-[60vh] overflow-y-auto scrollbar-thin pr-2 py-1">
	<div class="mx-auto max-w-md text-center py-6">
		<div class="mx-auto mb-3 h-12 w-12 rounded-2xl bg-gradient-to-br from-accent to-highlight text-white flex items-center justify-center text-base font-bold shadow-lift">GO</div>
		<div class="text-sm font-medium text-gray-700 dark:text-gray-300">Ask goon anything</div>
		<div class="mt-1 text-xs text-gray-500 dark:text-gray-500">
			Grounded on your live tickets, workflows, pending questions, and knowledge notes.
		</div>
		<div class="mt-5 grid grid-cols-1 sm:grid-cols-2 gap-2 text-left">
			<button type="button" onclick="goonChatFill(this.dataset.q)" data-q="what tickets are open?" class="rounded-md border border-gray-200 dark:border-surface-border bg-white dark:bg-surface px-3 py-2 text-xs text-left text-gray-700 dark:text-gray-300 hover:border-accent hover:text-accent transition">
				<div class="font-medium">→ what tickets are open?</div>
				<div class="mt-0.5 text-[11px] text-gray-500">list non-done tickets</div>
			</button>
			<button type="button" onclick="goonChatFill(this.dataset.q)" data-q="any pending approvals?" class="rounded-md border border-gray-200 dark:border-surface-border bg-white dark:bg-surface px-3 py-2 text-xs text-left text-gray-700 dark:text-gray-300 hover:border-accent hover:text-accent transition">
				<div class="font-medium">→ any pending approvals?</div>
				<div class="mt-0.5 text-[11px] text-gray-500">workflows waiting on you</div>
			</button>
			<button type="button" onclick="goonChatFill(this.dataset.q)" data-q="what do you know about this project?" class="rounded-md border border-gray-200 dark:border-surface-border bg-white dark:bg-surface px-3 py-2 text-xs text-left text-gray-700 dark:text-gray-300 hover:border-accent hover:text-accent transition">
				<div class="font-medium">→ what do you know about this project?</div>
				<div class="mt-0.5 text-[11px] text-gray-500">recall from SOUL.md + notes</div>
			</button>
			<button type="button" onclick="goonChatFill(this.dataset.q)" data-q="summarize the most recent workflow run" class="rounded-md border border-gray-200 dark:border-surface-border bg-white dark:bg-surface px-3 py-2 text-xs text-left text-gray-700 dark:text-gray-300 hover:border-accent hover:text-accent transition">
				<div class="font-medium">→ summarize recent runs</div>
				<div class="mt-0.5 text-[11px] text-gray-500">latest workflow state</div>
			</button>
		</div>
	</div>
</div>`

// writeChatBubble emits one styled message bubble. Roles map to
// distinct visual tones: user is right-aligned accent, assistant is
// left-aligned subtle, error is rose. Each bubble carries the
// `chat-bubble` class so the transcript auto-scrolls to the newest.
func writeChatBubble(w io.Writer, role, body string) {
	// Pre-render body with newlines preserved.
	safe := html.EscapeString(body)
	safe = strings.ReplaceAll(safe, "\n", "<br>")
	now := time.Now().Format("15:04")

	var wrap, bubble, avatar, label string
	switch role {
	case "user":
		wrap = "flex flex-row-reverse gap-2 chat-bubble animate-fade-in"
		bubble = `max-w-[85%] rounded-2xl rounded-tr-md bg-accent text-surface px-3.5 py-2 text-sm whitespace-pre-wrap shadow-sm`
		avatar = `<div class="shrink-0 h-7 w-7 rounded-full bg-accent/15 text-accent flex items-center justify-center text-xs font-semibold">YO</div>`
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
				<span class="text-[10px] text-gray-400 font-mono">%s</span>
			</div>
			<div class="%s">%s</div>
		</div>
	</div>`, wrap, avatar, metaAlign, html.EscapeString(label), now, bubble, safe)
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

// fragTabChat renders the chat panel — transcript + input form. The
// transcript pane is a single htmx swap target; each POST to
// /api/chat appends two bubbles (user echo + assistant reply).
func (s *Server) fragTabChat(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	llmAvailable := s.opts.LLM != nil
	fmt.Fprint(w, `<section>
		<div class="flex items-start justify-between mb-5 gap-4 flex-wrap">
			<div>
				<h2 class="text-xl font-semibold tracking-tight">Chat</h2>
				<p class="mt-0.5 text-sm text-gray-500 dark:text-gray-400 max-w-2xl">
					Ask about tickets, workflows, and your knowledge notes. Goon can also comment, transition, and update Jira tickets when you ask it to.
				</p>
			</div>
			<button type="button" hx-post="/api/chat/reset" hx-target="#chat-transcript" hx-swap="outerHTML"
				class="inline-flex items-center gap-1.5 rounded-md border border-gray-200 dark:border-surface-border px-3 py-1.5 text-xs text-gray-500 hover:border-rose-500/40 hover:text-rose-500 transition">
				<svg class="h-3.5 w-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 6h18"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6"/><path d="M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/></svg>
				reset
			</button>
		</div>`)
	if !llmAvailable {
		fmt.Fprint(w, `<div class="rounded-xl border border-amber-500/40 bg-amber-500/10 px-4 py-3 text-sm text-amber-700 dark:text-amber-400">
			No LLM provider configured. Set one on the Configuration tab and reload.
		</div></section>`)
		return
	}
	fmt.Fprint(w, `
		<div class="rounded-xl border border-gray-200 dark:border-surface-border bg-white dark:bg-surface-raised shadow-card overflow-hidden">
			<div class="px-4 py-4 sm:px-5 sm:py-5">
				`)
	fmt.Fprint(w, chatTranscriptEmpty)
	fmt.Fprint(w, `
			</div>
			<div class="border-t border-gray-200 dark:border-surface-border bg-gray-50/60 dark:bg-surface-sunken/60 px-3 py-3 sm:px-4">
				<form id="goon-chat-form" hx-post="/api/chat" hx-target="#chat-transcript" hx-swap="beforeend"
					hx-on::after-request="goonChatAfter()" class="flex items-end gap-2">
					<div class="flex-1 relative">
						<textarea id="goon-chat-input" name="message" autocomplete="off" required autofocus rows="1"
							placeholder="ask goon anything about your tickets, workflows, or knowledge…  (Shift+Enter for newline)"
							class="w-full resize-none rounded-lg border border-gray-300 dark:border-surface-border bg-white dark:bg-surface px-3 py-2 pr-10 text-sm focus:border-accent focus:ring-2 focus:ring-accent/30 focus:outline-none max-h-40"
							onkeydown="goonChatKey(event)"
							oninput="goonChatAutosize(this)"></textarea>
						<div class="absolute bottom-2 right-3 text-[10px] text-gray-400 pointer-events-none htmx-indicator">
							<svg class="h-4 w-4 animate-spin-slow text-accent" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="10" stroke-opacity="0.25"/><path d="M22 12a10 10 0 0 1-10 10"/></svg>
						</div>
					</div>
					<button type="submit"
						class="inline-flex items-center gap-1.5 rounded-lg bg-accent text-surface px-4 py-2 text-sm font-semibold hover:brightness-110 active:brightness-95 transition shadow-sm">
						<span>send</span>
						<svg class="h-4 w-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><line x1="22" y1="2" x2="11" y2="13"/><polygon points="22 2 15 22 11 13 2 9 22 2"/></svg>
					</button>
				</form>
			</div>
		</div>

		<script>
		(function() {
			window.goonChatFill = function(q) {
				const ta = document.getElementById('goon-chat-input');
				if (!ta) return;
				ta.value = q;
				ta.focus();
				goonChatAutosize(ta);
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
			window.goonChatAfter = function() {
				const ta = document.getElementById('goon-chat-input');
				if (ta) { ta.value = ''; ta.style.height = ''; ta.focus(); }
				const t = document.getElementById('chat-transcript');
				if (t) t.scrollTop = t.scrollHeight;
			};
			// Scroll on initial load if there's already content (resume cases).
			document.addEventListener('htmx:afterSwap', function(e) {
				const t = document.getElementById('chat-transcript');
				if (t && e.target && (e.target.id === 'chat-transcript' || t.contains(e.target))) {
					t.scrollTop = t.scrollHeight;
				}
			});
		})();
		</script>
	</section>`)
}

// fragTabMemory is the consolidated tab covering Knowledge + Skills.
// Two segmented buttons toggle between two pre-rendered bodies
// WITHOUT a network round-trip — pure CSS class flip via the
// goonSwitchStore helper at the bottom.
//
// The old Personal segment is gone — character / voice content was
// folded into SOUL.md (Knowledge tab). One always-loaded file is
// less confusing than two.
func (s *Server) fragTabMemory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<div class="flex items-start justify-between mb-5 gap-4 flex-wrap">
		<div>
			<h2 class="text-xl font-semibold tracking-tight">Memory</h2>
			<p class="mt-0.5 text-sm text-muted max-w-2xl">
				What goon remembers. <strong>Knowledge</strong> is the auto-loaded SOUL.md (character + project facts in one place), plus on-demand topic notes and HISTORY.md. <strong>Skills</strong> are specialist procedures activated on demand.
			</p>
		</div>
		<div class="inline-flex rounded-lg border border-surface-border bg-surface-sunken p-0.5 text-xs font-medium" role="tablist">
			<button type="button" data-store-switch="knowledge" onclick="goonSwitchStore('knowledge')"
				class="px-3 py-1.5 rounded-md transition bg-surface-raised text-accent shadow-card" aria-current="page">
				Knowledge
			</button>
			<button type="button" data-store-switch="skills" onclick="goonSwitchStore('skills')"
				class="px-3 py-1.5 rounded-md transition text-muted hover:text-accent">
				Skills
			</button>
		</div>
	</div>

	<div data-store="knowledge">`)
	s.renderKnowledgeBody(w)
	fmt.Fprint(w, `</div>
	<div data-store="skills" class="hidden">`)
	s.renderSkillsBody(w)
	fmt.Fprint(w, `</div>

	<script>
	(function() {
		// Defined once; subsequent loads just rebind handlers via inline onclick.
		if (window.goonSwitchStore) return;
		window.goonSwitchStore = function(target) {
			document.querySelectorAll('[data-store]').forEach(function(el) {
				el.classList.toggle('hidden', el.dataset.store !== target);
			});
			document.querySelectorAll('[data-store-switch]').forEach(function(btn) {
				var active = btn.dataset.storeSwitch === target;
				// Dark-only now: the active pill sits on surface-raised
				// with the brand-purple accent. Inactive rows fade to muted.
				btn.classList.toggle('bg-surface-raised', active);
				btn.classList.toggle('text-accent', active);
				btn.classList.toggle('shadow-card', active);
				btn.classList.toggle('text-muted', !active);
				if (active) btn.setAttribute('aria-current', 'page');
				else btn.removeAttribute('aria-current');
			});
		};
	})();
	</script>`)
}

// renderKnowledgeBody renders the original Knowledge UI inside the
// shared Memory tab shell. SOUL.md card + topic notes index.
// Caller (fragTabMemory) has already emitted the section opener and
// segmented header.
func (s *Server) renderKnowledgeBody(w http.ResponseWriter) {
	fmt.Fprint(w, `<details class="mb-4 rounded-xl border border-gray-200 dark:border-surface-border bg-white dark:bg-surface-raised">
			<summary class="px-4 py-3 cursor-pointer text-sm font-medium text-gray-700 dark:text-gray-300 flex items-center gap-2">
				<svg class="h-4 w-4 text-accent" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><line x1="12" y1="5" x2="12" y2="19"/><line x1="5" y1="12" x2="19" y2="12"/></svg>
				create or replace a note
			</summary>
			<form hx-post="/api/memory/write" hx-target="#memory-save-result" hx-swap="innerHTML" hx-on::after-request="if (event.detail.successful) this.reset()" class="px-4 pb-4 space-y-3">
				<input type="text" name="name" required placeholder="SOUL.md, kebab-case-name.md, …"
					class="w-full font-mono text-sm rounded-lg border border-gray-300 dark:border-surface-border bg-white dark:bg-surface px-3 py-2 focus:border-accent focus:ring-2 focus:ring-accent/30 focus:outline-none">
				<textarea name="body" rows="6" required placeholder="# Project context&#10;- API base URL is …&#10;- always run `+"make verify"+` before pushing"
					class="w-full font-mono text-sm rounded-lg border border-gray-300 dark:border-surface-border bg-white dark:bg-surface px-3 py-2 focus:border-accent focus:ring-2 focus:ring-accent/30 focus:outline-none"></textarea>
				<div class="flex items-center justify-between">
					<div id="memory-save-result"></div>
					<button type="submit" class="inline-flex items-center gap-1.5 rounded-lg bg-accent text-surface px-4 py-2 text-sm font-semibold hover:brightness-110 transition">save note</button>
				</div>
			</form>
		</details>`)

	soul := agentctx.Soul("")
	if strings.TrimSpace(soul) == "" {
		fmt.Fprint(w, `<div class="mb-6 rounded-xl border border-dashed border-accent/30 bg-surface-raised/60 p-6 text-center">
			<div class="mx-auto h-10 w-10 rounded-xl bg-accent-soft text-accent flex items-center justify-center mb-2">
				<svg class="h-5 w-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M9 11l3 3L22 4"/><path d="M21 12v7a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h11"/></svg>
			</div>
			<div class="text-sm font-medium text-white">SOUL.md is empty</div>
			<div class="mt-1 text-xs text-muted max-w-md mx-auto">
				Seed it with <code class="font-mono text-accent">goon memory init</code> — facts in here are visible to the agent on every run.
			</div>
		</div>`)
	} else {
		fmt.Fprint(w, `<div class="mb-6 relative overflow-hidden rounded-xl border border-accent/30 bg-gradient-to-br from-accent-soft to-transparent shadow-card">
			<div class="absolute left-0 top-0 bottom-0 w-1 bg-accent"></div>
			<div class="px-5 py-4">
				<div class="flex items-center gap-2 mb-3">
					<svg class="h-4 w-4 text-accent" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 2l3 6 6 .9-4.5 4.4 1 6.7L12 17l-5.5 3 1-6.7L3 8.9 9 8z"/></svg>
					<span class="text-[11px] font-semibold uppercase tracking-wider text-accent">SOUL.md</span>
					<span class="text-[11px] text-gray-500 font-mono">always-loaded</span>
				</div>
				<pre class="whitespace-pre-wrap text-sm font-mono text-gray-800 dark:text-gray-200 leading-relaxed">`)
		fmt.Fprint(w, html.EscapeString(strings.TrimSpace(soul)))
		fmt.Fprint(w, `</pre>
			</div>
		</div>`)
	}

	idx := agentctx.KnowledgeIndex("")
	if len(idx) == 0 {
		fmt.Fprint(w, `<div class="rounded-xl border border-dashed border-surface-border bg-surface-raised/40 p-6 text-center">
			<div class="mx-auto h-10 w-10 rounded-xl bg-surface-sunken text-muted flex items-center justify-center mb-2">
				<svg class="h-5 w-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M4 19.5A2.5 2.5 0 0 1 6.5 17H20"/><path d="M6.5 2H20v20H6.5A2.5 2.5 0 0 1 4 19.5v-15A2.5 2.5 0 0 1 6.5 2z"/></svg>
			</div>
			<div class="text-sm font-medium text-white">No topic notes yet</div>
			<div class="mt-1 text-xs text-muted max-w-md mx-auto">
				Workflows write here as they learn. You can also create them manually with
				<code class="font-mono text-xs text-accent">goon memory write &lt;name&gt; &lt;body&gt;</code>.
			</div>
		</div>`)
		return
	}
	fmt.Fprintf(w, `<div class="flex items-baseline justify-between mb-3">
		<h3 class="flex items-center gap-2 text-xs font-semibold uppercase tracking-wider text-gray-500">
			<svg class="h-3.5 w-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M4 19.5A2.5 2.5 0 0 1 6.5 17H20"/><path d="M6.5 2H20v20H6.5A2.5 2.5 0 0 1 4 19.5v-15A2.5 2.5 0 0 1 6.5 2z"/></svg>
			Topic notes
			<span class="rounded-full bg-gray-100 dark:bg-surface-sunken text-gray-600 dark:text-gray-300 px-2 py-0.5 text-[10px] font-mono normal-case tracking-normal">%d</span>
		</h3>
	</div>
	<div class="space-y-2">`, len(idx))
	for _, e := range idx {
		headline := html.EscapeString(e.Headline)
		if headline == "" {
			headline = "<span class='text-gray-400 italic'>(no headline)</span>"
		}
		nameEsc := html.EscapeString(e.Name)
		nameQ := urlQueryEscape(e.Name)
		fmt.Fprintf(w, `<details class="group rounded-xl border border-gray-200 dark:border-surface-border bg-white dark:bg-surface-raised hover:border-accent/40 transition open:border-accent/50 open:shadow-card">
			<summary class="flex items-center gap-3 px-4 py-3 cursor-pointer list-none">
				<svg class="h-4 w-4 text-gray-400 group-open:rotate-90 group-open:text-accent transition-transform shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><polyline points="9 18 15 12 9 6"/></svg>
				<div class="flex-1 min-w-0">
					<div class="font-mono text-sm text-gray-800 dark:text-gray-200 group-open:text-accent group-open:font-semibold truncate">%s</div>
					<div class="text-xs text-gray-500 truncate">%s</div>
				</div>
				<form hx-post="/api/memory/delete" hx-confirm="Delete %s?" hx-target="#memory-list-result" hx-swap="innerHTML" class="m-0">
					<input type="hidden" name="name" value="%s">
					<button type="submit" class="text-xs text-gray-400 hover:text-rose-500 px-2 py-1 transition" title="delete">✕</button>
				</form>
			</summary>
			<div hx-get="/api/knowledge/note?name=%s" hx-trigger="toggle from:closest details once" hx-swap="innerHTML"
				class="px-4 pb-4 -mt-1 text-sm">
				<div class="space-y-2"><div class="skel h-3 w-1/3"></div><div class="skel h-3 w-full"></div><div class="skel h-3 w-5/6"></div></div>
			</div>
		</details>`,
			nameEsc, headline, nameEsc, nameEsc, nameQ)
	}
	fmt.Fprint(w, `</div><div id="memory-list-result" class="mt-3"></div>`)
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
	fmt.Fprint(w, `<pre class="whitespace-pre-wrap text-sm font-mono text-gray-800 dark:text-gray-200 bg-gray-50 dark:bg-surface-sunken rounded-md p-3 border border-gray-200 dark:border-gray-800 overflow-x-auto">`)
	fmt.Fprint(w, html.EscapeString(body))
	fmt.Fprint(w, `</pre>`)
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
	fmt.Fprint(w, `<pre class="whitespace-pre-wrap text-sm font-mono text-gray-800 dark:text-gray-200 bg-gray-50 dark:bg-surface-sunken rounded-md p-3 border border-gray-200 dark:border-gray-800 overflow-x-auto">`)
	fmt.Fprint(w, html.EscapeString(body))
	fmt.Fprint(w, `</pre>`)
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

// renderSkillsBody renders the Skills index inside the shared Memory
// tab shell. Caller (fragTabMemory) has already emitted the section
// opener and segmented header.
func (s *Server) renderSkillsBody(w http.ResponseWriter) {
	fmt.Fprint(w, `<p class="mb-4 text-sm text-gray-500 dark:text-gray-400 max-w-2xl">
		Specialist procedures the agent can apply on demand — role definitions, how-tos, review checklists. Activated when you ask, not auto-loaded.
	</p>

	<details class="mb-4 rounded-xl border border-gray-200 dark:border-surface-border bg-white dark:bg-surface-raised">
		<summary class="px-4 py-3 cursor-pointer text-sm font-medium text-gray-700 dark:text-gray-300 flex items-center gap-2">
			<svg class="h-4 w-4 text-accent" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><line x1="12" y1="5" x2="12" y2="19"/><line x1="5" y1="12" x2="19" y2="12"/></svg>
			create new skill
		</summary>
		<form hx-post="/api/skills/write" hx-target="#skill-save-result" hx-swap="innerHTML" hx-on::after-request="if (event.detail.successful) this.reset()" class="px-4 pb-4 space-y-3">
			<input type="text" name="name" required placeholder="kebab-case-name.md (e.g. code-reviewer.md)"
				class="w-full font-mono text-sm rounded-lg border border-gray-300 dark:border-surface-border bg-white dark:bg-surface px-3 py-2 focus:border-accent focus:ring-2 focus:ring-accent/30 focus:outline-none">
			<textarea name="body" rows="6" required placeholder="# Code reviewer&#10;&#10;When asked to review code, focus on: ..."
				class="w-full font-mono text-sm rounded-lg border border-gray-300 dark:border-surface-border bg-white dark:bg-surface px-3 py-2 focus:border-accent focus:ring-2 focus:ring-accent/30 focus:outline-none"></textarea>
			<div class="flex items-center justify-between">
				<div id="skill-save-result"></div>
				<button type="submit" class="inline-flex items-center gap-1.5 rounded-lg bg-accent text-surface px-4 py-2 text-sm font-semibold hover:brightness-110 transition">save skill</button>
			</div>
		</form>
	</details>

	<div hx-get="/fragments/skills-list" hx-trigger="load, skillsChanged from:body" hx-swap="innerHTML">
		<div class="text-sm text-gray-500">Loading skills…</div>
	</div>`)
}

// fragSkillsList renders the skill index as a list with read /
// download / delete controls. Auto-refreshed when skillsChanged
// fires from anywhere (create, edit, delete).
func (s *Server) fragSkillsList(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	idx := agentctx.SkillsIndex("")
	if len(idx) == 0 {
		_, _ = io.WriteString(w, emptyState("No skills yet",
			"Create one with the form above, or via /skill write <name> <body> on Telegram."))
		return
	}
	fmt.Fprintf(w, `<div class="flex items-baseline justify-between mb-3">
		<h3 class="flex items-center gap-2 text-xs font-semibold uppercase tracking-wider text-gray-500">
			<svg class="h-3.5 w-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M14.7 6.3a1 1 0 0 0 0 1.4l1.6 1.6a1 1 0 0 0 1.4 0l3.77-3.77a6 6 0 0 1-7.94 7.94l-6.91 6.91a2.12 2.12 0 0 1-3-3l6.91-6.91a6 6 0 0 1 7.94-7.94l-3.76 3.76z"/></svg>
			Skills <span class="rounded-full bg-gray-100 dark:bg-surface-sunken text-gray-600 dark:text-gray-300 px-2 py-0.5 text-[10px] font-mono normal-case tracking-normal">%d</span>
		</h3>
	</div>
	<div class="space-y-2">`, len(idx))
	for _, e := range idx {
		headline := html.EscapeString(e.Headline)
		if headline == "" {
			headline = `<span class="text-gray-400 italic">(no headline)</span>`
		}
		nameEsc := html.EscapeString(e.Name)
		nameQ := urlQueryEscape(e.Name)
		fmt.Fprintf(w, `<details class="group rounded-xl border border-gray-200 dark:border-surface-border bg-white dark:bg-surface-raised hover:border-accent/40 open:border-accent/50 open:shadow-card transition">
			<summary class="flex items-center gap-3 px-4 py-3 cursor-pointer list-none">
				<svg class="h-4 w-4 text-gray-400 group-open:rotate-90 group-open:text-accent transition-transform shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><polyline points="9 18 15 12 9 6"/></svg>
				<div class="flex-1 min-w-0">
					<div class="font-mono text-sm text-gray-800 dark:text-gray-200 group-open:text-accent group-open:font-semibold truncate">%s</div>
					<div class="text-xs text-gray-500 truncate">%s</div>
				</div>
				<form hx-post="/api/skills/delete" hx-confirm="Delete %s?" hx-target="#skill-list-result" hx-swap="innerHTML" class="m-0">
					<input type="hidden" name="name" value="%s">
					<button type="submit" class="text-xs text-gray-400 hover:text-rose-500 px-2 py-1 transition" title="delete">✕</button>
				</form>
			</summary>
			<div hx-get="/api/skills/note?name=%s" hx-trigger="toggle from:closest details once" hx-swap="innerHTML" class="px-4 pb-4 -mt-1 text-sm">
				<div class="space-y-2"><div class="skel h-3 w-1/3"></div><div class="skel h-3 w-full"></div></div>
			</div>
		</details>`, nameEsc, headline, nameEsc, nameEsc, nameQ)
	}
	fmt.Fprint(w, `</div><div id="skill-list-result" class="mt-3"></div>`)
}
