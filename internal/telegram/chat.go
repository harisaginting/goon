package telegram

import (
	"context"
	"strings"

	"github.com/harisaginting/goon/internal/agentctx"
	"github.com/harisaginting/goon/internal/llm"
)

// The chat now runs through agentctx.ChatTurn, an LLM↔tool loop that
// lets the model invoke a live jira_search call when it needs data
// the GOON STATE block doesn't already contain. No more "refresh the
// cache first" — the agent decides what (if anything) to query.

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

# HOW TO ANSWER

You have THREE kinds of moves available each turn:

1. Answer in prose — use the GOON STATE block when it already
   answers the question (daemon status, recently-cached tickets,
   workflows in flight, pending approvals, knowledge notes).

2. Call a READ tool — jira_search, when the user asks about tickets
   and the cached state is insufficient. Don't make the user say
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
- Do NOT recommend the user run /refresh; you have the tools now.
- For project facts / how-it-works questions, check the knowledge
  notes in GOON STATE first and name the relevant note.
- When the user wants to ACT on CODE (edit, ship), tell them to use
  /run <task> for the agent runtime — that's outside chat scope.

# COMMANDS THE USER HAS:

  /tickets [filter]    list every ticket goon has seen (cached)
  /ticket <key>        full detail for one ticket
  /workflows [n]       recent workflow runs
  /queue               pending approval questions
  /answer <id> <text>  answer a pending question
  /status              daemon snapshot
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

	// Snapshot existing history WITHOUT the new user turn — ChatTurn
	// appends both turns atomically at the end. The bot's per-chat
	// trim still runs after.
	b.chatHistMu.Lock()
	historySnapshot := append([]llm.Message(nil), b.chatHist[chatID]...)
	b.chatHistMu.Unlock()

	result, err := agentctx.ChatTurn(ctx, agentctx.ChatTurnOptions{
		LLM:          b.opts.LLM,
		Memory:       b.opts.Memory,
		Board:        b.opts.Board,
		SystemPrompt: chatSystemPrompt,
		History:      historySnapshot,
		UserMessage:  text,
	})
	if err != nil {
		_ = b.Send(ctx, chatID, "✗ llm error: "+err.Error())
		return
	}
	reply := strings.TrimSpace(result.Reply)
	if reply == "" {
		reply = "(no response from model)"
	}

	// Commit both turns to history together. trimHistory keeps the
	// per-chat ring bounded.
	b.chatHistMu.Lock()
	hist := append(b.chatHist[chatID], result.NewTurns...)
	b.chatHist[chatID] = trimHistory(hist)
	b.chatHistMu.Unlock()

	b.SendChunked(ctx, chatID, reply)
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
