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

# STRICT GROUNDING RULES — FOLLOW EXACTLY

1. The "GOON STATE" message that follows this one is the ONLY source
   of truth about tickets, workflows, pending questions, daemon
   status, and stored knowledge notes. You have NO other access to
   Jira, GitHub, the user's repos, or the network.

2. When the user asks for tickets / workflows / status:
   - List ONLY entries that appear in GOON STATE. Never invent.
   - Filter by exactly what the user specified. If they say
     "status != closed", include only tickets whose [status] is NOT
     done/closed/resolved/merged. If they say "assigned to me",
     filter on the assignee= field.
   - If nothing matches, say so plainly. Suggest the user click the
     "refresh tickets" button on the Tickets tab to pull fresh data,
     or widen the filter.
   - Quote keys / IDs / URLs verbatim. NEVER paraphrase IDs.

3. The GOON STATE block may say "tickets: 30 total, 30 most-recent
   shown" or "…N more not shown". If the user asks for "all tickets"
   and the state says some are not shown, answer with what you have
   AND warn them that some are not in your view — point them at the
   Tickets tab.

4. The durable knowledge block contains PINNED.md and a topic-note
   index. When the user asks about engineering specifics, look there
   first, name the relevant note, and tell them which one to open.

# OUT OF SCOPE — DO NOT

- Do not pretend to query Jira / GitHub / the repo. You see only the
  cached snapshot.
- Do not emit JSON tool calls.
- Do not give "I'm just an AI" non-answers — the state above is real.
- Do not include closed/done tickets when the user asked otherwise.

# STYLE

- Plain prose, tight. When listing more than 3 tickets, one per line:
  "KEY — title [status] assignee=NAME".
- When the user wants to ACT (edit code, ship work), tell them to use
  the CLI's ` + "`" + `goon "<task>"` + "`" + ` for the agent runtime.`

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

	// Append user turn, build history snapshot.
	s.chatMu.Lock()
	s.chatHistory = append(s.chatHistory, llm.Message{Role: llm.RoleUser, Content: msg})
	s.chatHistory = trimChatHistory(s.chatHistory)
	historySnapshot := append([]llm.Message(nil), s.chatHistory...)
	s.chatMu.Unlock()

	// Auto-refresh stale tickets before answering — same UX as the
	// Telegram chat. Silent if the network is down or no board is
	// configured.
	if s.opts.Board != nil {
		refreshCtx, refreshCancel := context.WithTimeout(r.Context(), 10*time.Second)
		_, _, _ = agentctx.MaybeRefreshStale(refreshCtx, s.opts.Memory, s.opts.Board, 2*time.Minute)
		refreshCancel()
	}

	// Build prompt with shared context block.
	stateBlock := agentctx.Build(s.opts.Memory, "")
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: chatSystemPrompt},
		{Role: llm.RoleSystem, Content: stateBlock},
	}
	msgs = append(msgs, historySnapshot...)

	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	out, err := s.opts.LLM.Generate(ctx, msgs, llm.Options{
		Temperature: 0.4,
		MaxTokens:   800,
	})
	if err != nil {
		// Drop the user turn we just optimistically appended so
		// retry doesn't leak orphaned context.
		s.chatMu.Lock()
		if n := len(s.chatHistory); n > 0 && s.chatHistory[n-1].Role == llm.RoleUser {
			s.chatHistory = s.chatHistory[:n-1]
		}
		s.chatMu.Unlock()
		writeChatBubble(w, "error", "llm error: "+err.Error())
		return
	}
	reply := strings.TrimSpace(out)
	if reply == "" {
		reply = "(no response from model)"
	}
	s.chatMu.Lock()
	s.chatHistory = append(s.chatHistory, llm.Message{Role: llm.RoleAssistant, Content: reply})
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
		<div class="mx-auto mb-3 h-12 w-12 rounded-2xl bg-gradient-to-br from-accent to-violet-500 text-white flex items-center justify-center text-base font-bold shadow-lift">GO</div>
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
				<div class="mt-0.5 text-[11px] text-gray-500">recall from PINNED.md + notes</div>
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
		avatar = `<div class="shrink-0 h-7 w-7 rounded-full bg-gradient-to-br from-accent to-violet-500 text-white flex items-center justify-center text-[10px] font-bold tracking-tight">GO</div>`
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
				<h2 class="text-xl font-semibold tracking-tight">Chat with goon</h2>
				<p class="mt-0.5 text-sm text-gray-500 dark:text-gray-400 max-w-2xl">
					Grounded on live tickets, workflows, pending questions, and your knowledge notes in
					<code class="font-mono text-xs">./storage/memory/</code>. Conversational only —
					for actions use the CLI or the Tickets/Workflows tabs.
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

// fragTabKnowledge renders the active markdown notes — PINNED.md body
// styled as a card on top, then a clickable list of topic notes. Each
// topic-note row expands inline via htmx when clicked.
func (s *Server) fragTabKnowledge(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<section>
		<div class="flex items-start justify-between mb-5 gap-4 flex-wrap">
			<div>
				<h2 class="text-xl font-semibold tracking-tight">Knowledge</h2>
				<p class="mt-0.5 text-sm text-gray-500 dark:text-gray-400 max-w-2xl">
					What goon remembers. <code class="font-mono text-xs">PINNED.md</code> is auto-loaded into every agent run and chat turn.
					Topic notes are written by the <code class="font-mono text-xs">update_memory</code> phase and read on demand.
					Edit locally with <code class="font-mono text-xs">goon memory edit &lt;name&gt;</code>.
				</p>
			</div>
		</div>`)

	pinned := agentctx.Pinned("")
	if strings.TrimSpace(pinned) == "" {
		fmt.Fprint(w, `<div class="mb-6 rounded-xl border border-dashed border-gray-300 dark:border-surface-border bg-gray-50/60 dark:bg-surface-sunken/40 p-6 text-center">
			<div class="mx-auto h-10 w-10 rounded-xl bg-gray-100 dark:bg-surface-raised text-gray-400 flex items-center justify-center mb-2">
				<svg class="h-5 w-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M9 11l3 3L22 4"/><path d="M21 12v7a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h11"/></svg>
			</div>
			<div class="text-sm font-medium text-gray-700 dark:text-gray-300">PINNED.md is empty</div>
			<div class="mt-1 text-xs text-gray-500 max-w-md mx-auto">
				Seed it with <code class="font-mono">goon memory init</code> — facts in here are visible to the agent on every run.
			</div>
		</div>`)
	} else {
		fmt.Fprint(w, `<div class="mb-6 relative overflow-hidden rounded-xl border border-accent/30 bg-gradient-to-br from-accent-soft to-transparent shadow-card">
			<div class="absolute left-0 top-0 bottom-0 w-1 bg-accent"></div>
			<div class="px-5 py-4">
				<div class="flex items-center gap-2 mb-3">
					<svg class="h-4 w-4 text-accent" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 2l3 6 6 .9-4.5 4.4 1 6.7L12 17l-5.5 3 1-6.7L3 8.9 9 8z"/></svg>
					<span class="text-[11px] font-semibold uppercase tracking-wider text-accent">PINNED.md</span>
					<span class="text-[11px] text-gray-500 font-mono">always-loaded</span>
				</div>
				<pre class="whitespace-pre-wrap text-sm font-mono text-gray-800 dark:text-gray-200 leading-relaxed">`)
		fmt.Fprint(w, html.EscapeString(strings.TrimSpace(pinned)))
		fmt.Fprint(w, `</pre>
			</div>
		</div>`)
	}

	idx := agentctx.KnowledgeIndex("")
	if len(idx) == 0 {
		fmt.Fprint(w, `<div class="rounded-xl border border-dashed border-gray-300 dark:border-surface-border bg-gray-50/60 dark:bg-surface-sunken/40 p-6 text-center">
			<div class="mx-auto h-10 w-10 rounded-xl bg-gray-100 dark:bg-surface-raised text-gray-400 flex items-center justify-center mb-2">
				<svg class="h-5 w-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M4 19.5A2.5 2.5 0 0 1 6.5 17H20"/><path d="M6.5 2H20v20H6.5A2.5 2.5 0 0 1 4 19.5v-15A2.5 2.5 0 0 1 6.5 2z"/></svg>
			</div>
			<div class="text-sm font-medium text-gray-700 dark:text-gray-300">No topic notes yet</div>
			<div class="mt-1 text-xs text-gray-500 max-w-md mx-auto">
				Workflows write here as they learn. You can also create them manually with
				<code class="font-mono text-xs">goon memory write &lt;name&gt; &lt;body&gt;</code>.
			</div>
		</div></section>`)
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
		fmt.Fprintf(w, `<details class="group rounded-xl border border-gray-200 dark:border-surface-border bg-white dark:bg-surface-raised hover:border-accent/40 transition open:border-accent/50 open:shadow-card">
			<summary class="flex items-center gap-3 px-4 py-3 cursor-pointer list-none">
				<svg class="h-4 w-4 text-gray-400 group-open:rotate-90 group-open:text-accent transition-transform shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><polyline points="9 18 15 12 9 6"/></svg>
				<div class="flex-1 min-w-0">
					<div class="font-mono text-sm text-gray-800 dark:text-gray-200 group-open:text-accent group-open:font-semibold truncate">%s</div>
					<div class="text-xs text-gray-500 truncate">%s</div>
				</div>
				<span class="hidden group-open:inline text-[10px] font-mono uppercase tracking-wider text-accent">open</span>
			</summary>
			<div hx-get="/api/knowledge/note?name=%s" hx-trigger="toggle from:closest details once" hx-swap="innerHTML"
				class="px-4 pb-4 -mt-1 text-sm">
				<div class="space-y-2"><div class="skel h-3 w-1/3"></div><div class="skel h-3 w-full"></div><div class="skel h-3 w-5/6"></div></div>
			</div>
		</details>`,
			html.EscapeString(e.Name), headline, urlQueryEscape(e.Name))
	}
	fmt.Fprint(w, `</div></section>`)
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
	fmt.Fprintf(w, `<div class="rounded-md bg-emerald-500/10 border border-emerald-500/30 px-3 py-2 text-sm text-emerald-700 dark:text-emerald-400">✓ pulled %d ticket(s) from the board</div>`, n)
}

// Keep notes import alive for tests/future references even if every
// caller goes through agentctx today.
var _ = notes.PinnedFilename
