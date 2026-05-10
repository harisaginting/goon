package telegram

import (
	"context"
	"strings"

	"github.com/harisaginting/goon/internal/llm"
)

// chatSystemPrompt is the persona for plain-text Telegram chat. It is
// deliberately short and steers the model away from hallucinating tool
// calls (the chat path doesn't execute tools — that's what /run is for).
const chatSystemPrompt = `You are GOON, a calm engineering co-pilot reachable over Telegram.
The user is chatting with you informally — they may want quick advice,
explanations, or someone to think out loud with. You can reference what
you know about goon's pipeline, ticket workflow, and project conventions
when relevant.

Constraints:
- This is a chat surface, not an agent run. Do NOT emit JSON tool calls
  or pretend to execute commands. Reply in plain prose.
- Keep replies tight: one or two short paragraphs is usually enough.
- If the user wants you to actually do something (run code, edit files,
  inspect the repo), tell them to use /run <task> so the agent runtime
  can pick it up.`

// maxChatHistory is the number of (user, assistant) turn pairs we retain
// per chat for context. Older turns are evicted FIFO. Memory is per-bot
// process — restarting goon clears it.
const maxChatHistory = 6

// handleChat treats a plain-text message as a free-form conversation with
// the model. The bot maintains a short rolling history per chat ID so
// follow-up questions have context.
func (b *Bot) handleChat(ctx context.Context, chatID int64, text string) {
	if b.opts.LLM == nil {
		_ = b.Send(ctx, chatID, "(chat unavailable: no LLM provider configured)")
		return
	}
	history := b.appendUserTurn(chatID, text)

	msgs := []llm.Message{{Role: llm.RoleSystem, Content: chatSystemPrompt}}
	msgs = append(msgs, history...)

	out, err := b.opts.LLM.Generate(ctx, msgs, llm.Options{
		Temperature: 0.5,
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
