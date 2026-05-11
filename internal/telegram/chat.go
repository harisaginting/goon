package telegram

import (
	"context"
	"strings"
	"time"

	"github.com/harisaginting/goon/internal/agentctx"
	"github.com/harisaginting/goon/internal/llm"
)

// chatRefreshStale controls when handleChat auto-pulls a fresh board
// snapshot before answering. Tuned to half the default poll interval
// so the user's typical question hits live data, not 5-minute-old
// cache, without slamming the board API on every chat turn.
const chatRefreshStale = 2 * time.Minute

// chatSystemPrompt is the persona for plain-text Telegram chat. It is
// deliberately short and steers the model away from hallucinating tool
// calls (the chat path doesn't execute tools — that's what /run is for).
//
// A second, dynamic system message — built by buildContextBlock — is
// prepended on every turn. That one carries the LIVE state (tickets
// goon has seen, workflows in flight, pending questions, daemon
// running/paused) so the LLM can actually answer "list tickets" or
// "what's pending" without making things up.
const chatSystemPrompt = `You are GOON, an autonomous engineering co-pilot reachable over Telegram.

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
     filter on the assignee= field (or skip the line if absent).
   - If you cannot find any matching entries, say so plainly:
     "no tickets match in the current snapshot — try /refresh to
     pull from the board live, or widen your filter."
   - Quote keys / IDs / URLs verbatim from state. NEVER paraphrase
     a ticket ID or invent a number.
   - When you list more than 3 tickets, render one per line in the
     format "KEY — title [status]" so the user can scan them.

3. The GOON STATE block includes a "tickets" section with a per-line
   schema like:
     KEY [status] assignee=NAME project=KEY labels=A,B Title here
   Use the bracketed status to filter; never claim a ticket's status
   that contradicts what's printed.

4. The GOON STATE block may say "tickets: 12 total, 12 most-recent
   shown" or "…N more not shown — suggest user run /tickets". If the
   user is asking for "all tickets" and the state says some are not
   shown, ANSWER with what you have AND warn them that some are not
   in your view — point them at /tickets or /refresh.

5. The DURABLE KNOWLEDGE block contains PINNED.md and a topic-note
   index. When the user asks about engineering specifics, look there
   first, name the relevant note, and tell them /memory read <name>
   for the full body.

# OUT OF SCOPE — DO NOT

- Do not pretend to query Jira / GitHub / the repo. You see only the
  cached snapshot.
- Do not emit JSON tool calls.
- Do not give "I'm just an AI" non-answers — the state above is real
  and you must use it.
- Do not include closed/done tickets when the user asked "status !=
  closed" or similar.

# STYLE

- Plain prose. Tight: one or two short paragraphs unless a list is
  needed.
- When the user wants to ACT (edit code, ship work), tell them to use
  /run <task> for the agent runtime.

# COMMANDS THE USER HAS:

  /tickets [filter]    list every ticket goon has seen
  /ticket <key>        full detail for one ticket
  /workflows [n]       recent workflow runs
  /queue               pending approval questions
  /answer <id> <text>  answer a pending question
  /status              daemon snapshot
  /refresh             force-pull a fresh snapshot from the board
  /prs [repo]          open PRs on the configured git host
  /review <repo> <num> AI-review a PR
  /knowledge           show PINNED.md + topic-note index
  /memory read <name>  read one topic note
  /run <task>          run a one-shot agent task`

// maxChatHistory is the number of (user, assistant) turn pairs we retain
// per chat for context. Older turns are evicted FIFO. Memory is per-bot
// process — restarting goon clears it.
const maxChatHistory = 6

// handleChat treats a plain-text message as a free-form conversation with
// the model. The bot maintains a short rolling history per chat ID so
// follow-up questions have context.
//
// Every turn injects a fresh GOON STATE system message built from
// memory.json. This is the difference between "I'm an AI and don't know
// your project" (the previous behaviour) and "you have 4 open tickets:
// ENG-1, ENG-2, ..." (the desired behaviour). The state is rebuilt per
// turn so the LLM always sees current daemon + queue + workflow state.
func (b *Bot) handleChat(ctx context.Context, chatID int64, text string) {
	if b.opts.LLM == nil {
		_ = b.Send(ctx, chatID, "(chat unavailable: no LLM provider configured)")
		return
	}
	history := b.appendUserTurn(chatID, text)

	// Auto-refresh stale tickets before answering. Without this, the
	// chat answers from a snapshot up to GOON_POLL_SECONDS old; a
	// user asking "list my open tickets" right after creating one
	// in Jira got an answer that didn't include it. We refresh
	// silently — if the network is down, we just use cached data.
	if b.opts.Board != nil {
		refreshCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, _, _ = agentctx.MaybeRefreshStale(refreshCtx, b.opts.Memory, b.opts.Board, chatRefreshStale)
		cancel()
	}

	// agentctx.Build returns the same state + knowledge block the
	// web UI's chat panel uses. One source of truth.
	stateBlock := agentctx.Build(b.opts.Memory, "")
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: chatSystemPrompt},
		{Role: llm.RoleSystem, Content: stateBlock},
	}
	msgs = append(msgs, history...)

	out, err := b.opts.LLM.Generate(ctx, msgs, llm.Options{
		Temperature: 0.4,
		MaxTokens:   800,
	})
	if err != nil {
		_ = b.Send(ctx, chatID, "✗ llm error: "+err.Error())
		return
	}
	out = strings.TrimSpace(out)
	if out == "" {
		out = "(no response from model)"
	}
	b.appendAssistantTurn(chatID, out)
	b.SendChunked(ctx, chatID, out)
}

// appendUserTurn records the new user message and returns the rolling
// history (always containing the just-added turn at the tail).
func (b *Bot) appendUserTurn(chatID int64, text string) []llm.Message {
	b.chatHistMu.Lock()
	defer b.chatHistMu.Unlock()
	hist := b.chatHist[chatID]
	hist = append(hist, llm.Message{Role: llm.RoleUser, Content: text})
	hist = trimHistory(hist)
	b.chatHist[chatID] = hist
	out := make([]llm.Message, len(hist))
	copy(out, hist)
	return out
}

// appendAssistantTurn records the model's reply for future context.
func (b *Bot) appendAssistantTurn(chatID int64, text string) {
	b.chatHistMu.Lock()
	defer b.chatHistMu.Unlock()
	hist := b.chatHist[chatID]
	hist = append(hist, llm.Message{Role: llm.RoleAssistant, Content: text})
	hist = trimHistory(hist)
	b.chatHist[chatID] = hist
}

// trimHistory caps a chat to the last 2*maxChatHistory entries (a "turn"
// is one user + one assistant message, so 12 messages = 6 turns).
func trimHistory(hist []llm.Message) []llm.Message {
	cap := 2 * maxChatHistory
	if len(hist) <= cap {
		return hist
	}
	return hist[len(hist)-cap:]
}
