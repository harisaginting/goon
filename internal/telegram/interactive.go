// Package telegram — interactive.go handles inline-keyboard taps and
// reply-to-prompt flows. The goal is "tap a ticket, choose an
// action, never type a key or remember a command".
//
// Callback-data wire format (kept short — Telegram caps at 64 bytes):
//
//	v:KEY                 → view ticket detail with action keyboard
//	m:KEY                 → show transition submenu for KEY
//	ms:KEY:status         → execute transition KEY → status
//	c:KEY                 → start "comment" flow (force-reply prompt)
//	e:KEY:field           → start "edit field" flow (force-reply prompt)
//
// For comment / edit, the bot sends a force-reply prompt whose text
// begins with a [tag] like "[c:KEY]" — when the user's reply hits
// handleUpdate's ReplyToMessage path, tryHandleReplyAction parses the
// tag and routes accordingly.
package telegram

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/harisaginting/goon/internal/boards"
)

// handleCallback dispatches one tapped inline-keyboard button.
func (b *Bot) handleCallback(ctx context.Context, q *CallbackQuery) {
	chatID := int64(0)
	if q.Message != nil {
		chatID = q.Message.Chat.ID
	}
	if chatID == 0 {
		b.AnswerCallback(ctx, q.ID, "")
		return
	}
	if !b.opts.Memory.IsChatAuthorized(chatID) {
		b.AnswerCallback(ctx, q.ID, "🔒 not authorized")
		return
	}
	parts := strings.SplitN(q.Data, ":", 3)
	if len(parts) == 0 {
		b.AnswerCallback(ctx, q.ID, "")
		return
	}
	// Repo-picker callbacks live in repos.go — give it first crack
	// at the data so it can claim its own prefixes (rt:, rsa, rsc).
	if b.callbackHandleRepos(ctx, q) {
		return
	}
	switch parts[0] {
	case "v":
		if len(parts) < 2 {
			b.AnswerCallback(ctx, q.ID, "bad payload")
			return
		}
		b.AnswerCallback(ctx, q.ID, "")
		b.actionView(ctx, chatID, parts[1])
	case "m":
		if len(parts) < 2 {
			b.AnswerCallback(ctx, q.ID, "bad payload")
			return
		}
		b.AnswerCallback(ctx, q.ID, "")
		b.actionMoveMenu(ctx, chatID, parts[1])
	case "ms":
		if len(parts) < 3 {
			b.AnswerCallback(ctx, q.ID, "bad payload")
			return
		}
		b.actionMoveExec(ctx, q.ID, chatID, parts[1], parts[2])
	case "c":
		if len(parts) < 2 {
			b.AnswerCallback(ctx, q.ID, "bad payload")
			return
		}
		b.AnswerCallback(ctx, q.ID, "")
		b.actionCommentPrompt(ctx, chatID, parts[1])
	case "e":
		if len(parts) < 3 {
			b.AnswerCallback(ctx, q.ID, "bad payload")
			return
		}
		b.AnswerCallback(ctx, q.ID, "")
		b.actionEditPrompt(ctx, chatID, parts[1], parts[2])
	default:
		b.AnswerCallback(ctx, q.ID, "")
	}
}

// actionView fetches one ticket and re-renders the detail with the
// per-ticket action keyboard underneath.
func (b *Bot) actionView(ctx context.Context, chatID int64, key string) {
	if b.opts.Board == nil {
		_ = b.Send(ctx, chatID, "✗ no board configured")
		return
	}
	getCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	t, err := b.opts.Board.Get(getCtx, key)
	if err != nil {
		_ = b.Send(ctx, chatID, "✗ "+err.Error())
		return
	}
	body := formatTicketDetail(t)
	kb := ticketActionKeyboard(key)
	if err := b.SendWithButtons(ctx, chatID, body, kb); err != nil {
		_ = b.Send(ctx, chatID, body)
	}
}

// actionMoveMenu replaces the action panel with a status submenu so
// the user can transition the ticket without typing.
func (b *Bot) actionMoveMenu(ctx context.Context, chatID int64, key string) {
	rows := [][]InlineButton{
		{
			{Text: "▶ in progress", CallbackData: "ms:" + key + ":in_progress"},
			{Text: "✓ done", CallbackData: "ms:" + key + ":done"},
		},
		{
			{Text: "👀 in review", CallbackData: "ms:" + key + ":in_review"},
			{Text: "⛔ blocked", CallbackData: "ms:" + key + ":blocked"},
		},
		{
			{Text: "↻ open", CallbackData: "ms:" + key + ":open"},
			{Text: "← back", CallbackData: "v:" + key},
		},
	}
	_ = b.SendWithButtons(ctx, chatID, "Move "+key+" to:", rows)
}

// actionMoveExec runs the transition. Surfaces success/failure via
// the callback toast (small popup over the chat) so the user gets
// immediate feedback without a new message.
func (b *Bot) actionMoveExec(ctx context.Context, callbackID string, chatID int64, key, statusRaw string) {
	if b.opts.Board == nil {
		b.AnswerCallback(ctx, callbackID, "✗ no board configured")
		return
	}
	target := boards.MapStatus(statusRaw)
	if target == boards.StatusUnknown {
		b.AnswerCallback(ctx, callbackID, "unknown status")
		return
	}
	tCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := b.opts.Board.Transition(tCtx, key, target); err != nil {
		b.AnswerCallback(ctx, callbackID, "✗ "+oneLine(err.Error(), 180))
		_ = b.Send(ctx, chatID, "✗ transition "+key+" → "+string(target)+" failed: "+err.Error())
		return
	}
	b.AnswerCallback(ctx, callbackID, "✓ "+key+" → "+string(target))
	_ = b.Send(ctx, chatID, fmt.Sprintf("✓ %s → %s", key, target))
}

// actionCommentPrompt sends a force-reply asking the user for the
// comment body. The tag at the start lets tryHandleReplyAction route
// the eventual reply back here.
func (b *Bot) actionCommentPrompt(ctx context.Context, chatID int64, key string) {
	_ = b.SendForceReply(ctx, chatID,
		fmt.Sprintf("[c:%s]\n💬 Reply with your comment for %s.", key, key))
}

// actionEditPrompt asks for a new title/desc/labels value via
// force-reply. field is one of title|desc|labels.
func (b *Bot) actionEditPrompt(ctx context.Context, chatID int64, key, field string) {
	hint := "the new value"
	switch field {
	case "title":
		hint = "the new title"
	case "desc":
		hint = "the new description"
	case "labels":
		hint = "labels as a,b,c (- to clear)"
	}
	_ = b.SendForceReply(ctx, chatID,
		fmt.Sprintf("[e:%s:%s]\n✏ Reply with %s for %s.", key, field, hint, key))
}

// tryHandleReplyAction inspects the inbound message's quoted prompt
// for our [c:KEY] / [e:KEY:field] tag. If matched, executes the
// underlying API call and returns true so the caller skips normal
// command dispatch. Returns false for messages that aren't replies
// to our prompts.
func (b *Bot) tryHandleReplyAction(ctx context.Context, chatID int64, msg *Message) bool {
	if msg.ReplyToMessage == nil {
		return false
	}
	prompt := strings.TrimSpace(msg.ReplyToMessage.Text)
	if !strings.HasPrefix(prompt, "[") {
		return false
	}
	end := strings.IndexByte(prompt, ']')
	if end <= 1 {
		return false
	}
	tag := prompt[1:end]
	parts := strings.SplitN(tag, ":", 3)
	if len(parts) < 2 {
		return false
	}
	body := strings.TrimSpace(msg.Text)
	if body == "" {
		_ = b.Send(ctx, chatID, "✗ empty reply — try again")
		return true
	}
	switch parts[0] {
	case "c":
		key := parts[1]
		if b.opts.Board == nil {
			_ = b.Send(ctx, chatID, "✗ no board configured")
			return true
		}
		cCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := b.opts.Board.Comment(cCtx, key, body); err != nil {
			_ = b.Send(ctx, chatID, "✗ comment failed: "+err.Error())
			return true
		}
		_ = b.Send(ctx, chatID, "✓ commented on "+key+" — "+oneLine(body, 80))
		return true
	case "e":
		if len(parts) < 3 {
			return false
		}
		key := parts[1]
		field := parts[2]
		updater, ok := b.opts.Board.(boards.Updater)
		if !ok {
			_ = b.Send(ctx, chatID, "✗ board doesn't support edits")
			return true
		}
		patch := boards.TicketPatch{}
		switch field {
		case "title":
			t := body
			patch.Title = &t
		case "desc":
			d := body
			patch.Description = &d
		case "labels":
			if body == "-" || body == "" {
				patch.Labels = []string{}
			} else {
				out := []string{}
				for _, p := range strings.Split(body, ",") {
					p = strings.TrimSpace(p)
					if p != "" {
						out = append(out, p)
					}
				}
				patch.Labels = out
			}
		default:
			_ = b.Send(ctx, chatID, "✗ unknown field: "+field)
			return true
		}
		uCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := updater.Update(uCtx, key, patch); err != nil {
			_ = b.Send(ctx, chatID, "✗ edit failed: "+err.Error())
			return true
		}
		_ = b.Send(ctx, chatID, "✓ updated "+key+" · "+field)
		return true
	}
	return false
}

// formatTicketDetail renders a single ticket as the body of a detail
// message. Same shape as /jira get but shared so View-via-button
// matches.
func formatTicketDetail(t boards.Ticket) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s — %s\n", t.Key, t.Title)
	fmt.Fprintf(&sb, "[%s]", t.Status)
	if t.Assignee != "" {
		fmt.Fprintf(&sb, " · assignee: %s", t.Assignee)
	}
	if t.Project != "" {
		fmt.Fprintf(&sb, " · project: %s", t.Project)
	}
	sb.WriteString("\n")
	if len(t.Labels) > 0 {
		fmt.Fprintf(&sb, "labels: %s\n", strings.Join(t.Labels, ", "))
	}
	if t.URL != "" {
		fmt.Fprintf(&sb, "%s\n", t.URL)
	}
	if strings.TrimSpace(t.Description) != "" {
		sb.WriteString("\n")
		sb.WriteString(oneLine(t.Description, 800))
	}
	return sb.String()
}

// ticketActionKeyboard is the 2x2 grid of per-ticket actions surfaced
// under any single-ticket detail view.
func ticketActionKeyboard(key string) [][]InlineButton {
	return [][]InlineButton{
		{
			{Text: "💬 Comment", CallbackData: "c:" + key},
			{Text: "➡ Move", CallbackData: "m:" + key},
		},
		{
			{Text: "✏ Edit title", CallbackData: "e:" + key + ":title"},
			{Text: "✏ Edit labels", CallbackData: "e:" + key + ":labels"},
		},
	}
}
