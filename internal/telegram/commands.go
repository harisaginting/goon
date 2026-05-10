package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/harisaginting/goon/internal/agent"
	"github.com/harisaginting/goon/internal/logx"
)

// menuCommands is the list published to Telegram's setMyCommands API so
// they appear in the in-app command menu (the ☰ button next to the input
// bar). Order here is the order the user sees. Descriptions are capped
// at 256 chars by Telegram; we keep them short so they fit on a phone.
var menuCommands = []struct {
	Name        string `json:"command"`
	Description string `json:"description"`
}{
	{"help", "Show command reference"},
	{"auth", "Authenticate: /auth <secret>"},
	{"status", "Daemon status snapshot"},
	{"queue", "Pending questions waiting for a reply"},
	{"answer", "Answer a question: /answer <id> <text>"},
	{"workflows", "Recent workflow runs"},
	{"logs", "Last N log lines: /logs [n]"},
	{"memory", "Notes: /memory list|read|search"},
	{"prs", "List open pull requests"},
	{"review", "AI-review a PR: /review <repo> <num>"},
	{"approve", "Approve a PR: /approve <repo> <num> [body]"},
	{"decline", "Request changes: /decline <repo> <num> <reason>"},
	{"comment", "Comment on a PR: /comment <repo> <num> <body>"},
	{"tickets", "List tickets goon has seen + status"},
	{"ticket", "Show a ticket: /ticket <id-or-key>"},
	{"run", "Run a one-shot agent task: /run <task>"},
	{"whoami", "Show your chat record"},
	{"logout", "Revoke auth and forget this chat"},
}

// registerCommands publishes menuCommands to Telegram via setMyCommands.
// Idempotent — safe to call on every Start. Telegram clients refresh the
// menu within ~minutes; users can force-refresh by closing and reopening
// the chat.
func (b *Bot) registerCommands(ctx context.Context) error {
	payload, err := json.Marshal(map[string]any{"commands": menuCommands})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		b.endpoint("setMyCommands"), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("telegram setMyCommands http %d: %s", resp.StatusCode, snippet(string(raw), 200))
	}
	var r apiResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return err
	}
	if !r.OK {
		return fmt.Errorf("telegram setMyCommands: %s", r.Description)
	}
	logx.Info("telegram_bot.commands_registered", "count", len(menuCommands))
	if b.opts.Stdout != nil {
		fmt.Fprintf(b.opts.Stdout, "→ telegram bot: %d commands registered in menu\n", len(menuCommands))
	}
	return nil
}

// helpText is what `/help` returns. Kept short so it fits in one Telegram
// message and reads well on a phone screen.
const helpText = `goon bot — commands

monitoring:
  /status          daemon status snapshot
  /logs [n]        last n log lines (default 30)
  /workflows [n]   recent workflow runs (default 5)
  /tickets         every ticket goon has seen + current status
  /ticket <id>     full detail for one ticket (plan, approvals, PR)
  /memory list           list note names
  /memory read <name>    print one note
  /memory search <q>     grep across notes

questions / approvals:
  /queue           pending questions waiting for input
  /answer <id> <a> answer a pending question

PR review (if a git host is configured):
  /prs [repo]      list open PRs
  /review <repo> <num>  ask the model to review the PR
  /approve <repo> <num> [body]
  /decline <repo> <num> <reason>
  /comment <repo> <num> <body>

agent / chat:
  /run <task>      run a one-shot agent task
  any plain text   chat with the model (in-process history per chat)

session:
  /whoami          show your chat record
  /logout          revoke your auth and forget this chat
  /help            this message

Anything else with a leading slash is forwarded to the goon CLI:
  /train list  →  goon train --list
`

// disallowedCLI is the deny-list for the full-CLI-parity passthrough.
// Commands here either don't make sense over a remote bot (start/stop the
// daemon we're running inside) or have unbounded blast radius.
var disallowedCLI = map[string]bool{
	"start":     true,
	"stop":      true,
	"uninstall": true,
	"update":    true,
}

// builtins is the set of commands implemented in-process. Anything not in
// here AND not in disallowedCLI falls through to the goon CLI passthrough.
var builtins = map[string]bool{
	"help":      true,
	"status":    true,
	"logs":      true,
	"workflows": true,
	"tickets":   true,
	"ticket":    true,
	"memory":    true,
	"queue":     true,
	"answer":    true,
	"prs":       true,
	"review":    true,
	"approve":   true,
	"decline":   true,
	"comment":   true,
	"run":       true,
	"whoami":    true,
	"logout":    true,
}

// handleCommand parses a slash-prefixed message and routes it.
func (b *Bot) handleCommand(ctx context.Context, chatID int64, from User, text string) {
	parts := strings.Fields(text)
	cmd := strings.TrimPrefix(parts[0], "/")
	cmd = strings.SplitN(cmd, "@", 2)[0] // strip /cmd@botname suffix that Telegram adds in groups
	cmd = strings.ToLower(cmd)
	args := parts[1:]

	if disallowedCLI[cmd] {
		_ = b.Send(ctx, chatID, "✗ command `"+cmd+"` is not allowed via Telegram")
		return
	}

	switch cmd {
	case "help":
		_ = b.Send(ctx, chatID, helpText)
	case "status":
		b.cmdStatus(ctx, chatID)
	case "logs":
		b.cmdLogs(ctx, chatID, args)
	case "workflows":
		b.cmdWorkflows(ctx, chatID, args)
	case "tickets":
		b.cmdListTickets(ctx, chatID, args)
	case "ticket":
		b.cmdTicketDetail(ctx, chatID, args)
	case "memory":
		b.cmdMemory(ctx, chatID, args)
	case "queue":
		b.cmdQueue(ctx, chatID)
	case "answer":
		b.cmdAnswer(ctx, chatID, args)
	case "prs":
		b.cmdListPRs(ctx, chatID, args)
	case "review":
		b.cmdReviewPR(ctx, chatID, args)
	case "approve":
		b.cmdApprovePR(ctx, chatID, args)
	case "decline":
		b.cmdDeclinePR(ctx, chatID, args)
	case "comment":
		b.cmdCommentPR(ctx, chatID, args)
	case "run":
		b.cmdRun(ctx, chatID, args)
	case "whoami":
		b.cmdWhoami(ctx, chatID, from)
	case "logout":
		b.cmdLogout(ctx, chatID)
	default:
		// Full CLI parity — shell out to the goon binary.
		b.cmdPassthrough(ctx, chatID, cmd, args)
	}
}

// --- monitoring ------------------------------------------------------------

func (b *Bot) cmdStatus(ctx context.Context, chatID int64) {
	st := b.opts.Memory.GetStatus()
	var sb strings.Builder
	fmt.Fprintf(&sb, "running:        %v\n", st.Running)
	if st.PID > 0 {
		fmt.Fprintf(&sb, "pid:            %d\n", st.PID)
	}
	if !st.StartedAt.IsZero() {
		fmt.Fprintf(&sb, "started:        %s\n", st.StartedAt.Format(time.RFC3339))
	}
	if !st.LastPoll.IsZero() {
		fmt.Fprintf(&sb, "last poll:      %s\n", st.LastPoll.Format(time.RFC3339))
	}
	if st.LastTicket != "" {
		fmt.Fprintf(&sb, "last ticket:    %s\n", st.LastTicket)
	}
	if st.ActiveWorkflow != "" {
		fmt.Fprintf(&sb, "active wf:      %s\n", st.ActiveWorkflow)
	}
	if st.BoardName != "" {
		fmt.Fprintf(&sb, "board:          %s\n", st.BoardName)
	}
	if st.HostName != "" {
		fmt.Fprintf(&sb, "git host:       %s\n", st.HostName)
	}
	if sb.Len() == 0 {
		sb.WriteString("(no status recorded yet — daemon idle)")
	}
	_ = b.Send(ctx, chatID, sb.String())
}

func (b *Bot) cmdLogs(ctx context.Context, chatID int64, args []string) {
	n := 30
	if len(args) > 0 {
		fmt.Sscanf(args[0], "%d", &n)
	}
	if n < 1 {
		n = 30
	}
	if n > 200 {
		n = 200
	}
	// Easiest path: shell out to `goon logs --tail N`. The CLI already
	// knows how to find the log file via storage.Path().
	out, err := b.runGoonCLI(ctx, "logs", "--tail", fmt.Sprintf("%d", n))
	if err != nil {
		_ = b.Send(ctx, chatID, "logs error: "+err.Error())
		return
	}
	if strings.TrimSpace(out) == "" {
		_ = b.Send(ctx, chatID, "(log empty)")
		return
	}
	b.SendChunked(ctx, chatID, out)
}

func (b *Bot) cmdWorkflows(ctx context.Context, chatID int64, args []string) {
	n := 5
	if len(args) > 0 {
		fmt.Sscanf(args[0], "%d", &n)
	}
	if n < 1 {
		n = 5
	}
	if n > 50 {
		n = 50
	}
	wfs := b.opts.Memory.ListWorkflows(n)
	if len(wfs) == 0 {
		_ = b.Send(ctx, chatID, "no workflows yet")
		return
	}
	var sb strings.Builder
	for _, w := range wfs {
		stage := w.Stage
		if stage == "" {
			stage = "—"
		}
		fmt.Fprintf(&sb, "%s  %s  state=%s  stage=%s\n",
			w.TicketKey, w.ID, w.State, stage)
		if w.PRURL != "" {
			fmt.Fprintf(&sb, "    pr: %s\n", w.PRURL)
		}
		if w.Error != "" {
			fmt.Fprintf(&sb, "    error: %s\n", snippet(w.Error, 200))
		}
	}
	b.SendChunked(ctx, chatID, sb.String())
}

func (b *Bot) cmdQueue(ctx context.Context, chatID int64) {
	pending := b.opts.Memory.PendingQuestions()
	if len(pending) == 0 {
		_ = b.Send(ctx, chatID, "no pending questions ✓")
		return
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d pending question(s):\n\n", len(pending))
	for _, q := range pending {
		fmt.Fprintf(&sb, "[%s]", q.ID)
		if q.TicketID != "" {
			fmt.Fprintf(&sb, " (%s)", q.TicketID)
		}
		fmt.Fprintf(&sb, "\n%s\n\n", q.Question)
	}
	sb.WriteString("Reply with: /answer <id> <text>")
	b.SendChunked(ctx, chatID, sb.String())
}

func (b *Bot) cmdAnswer(ctx context.Context, chatID int64, args []string) {
	if len(args) < 2 {
		_ = b.Send(ctx, chatID, "usage: /answer <question-id> <text>")
		return
	}
	qid := args[0]
	ans := strings.Join(args[1:], " ")
	if !b.opts.Memory.AnswerQuestion(qid, ans) {
		_ = b.Send(ctx, chatID, "✗ "+qid+" not found or already answered")
		return
	}
	logx.Info("telegram_bot.answer", "chat", chatID, "qid", qid)
	_ = b.Send(ctx, chatID, "✓ answered "+qid)
}

func (b *Bot) cmdMemory(ctx context.Context, chatID int64, args []string) {
	if len(args) == 0 {
		_ = b.Send(ctx, chatID, "usage: /memory list | /memory read <name> | /memory search <query>")
		return
	}
	switch args[0] {
	case "list":
		out, err := b.runGoonCLI(ctx, "memory", "list")
		if err != nil {
			_ = b.Send(ctx, chatID, "memory list error: "+err.Error())
			return
		}
		b.SendChunked(ctx, chatID, out)
	case "read":
		if len(args) < 2 {
			_ = b.Send(ctx, chatID, "usage: /memory read <name>")
			return
		}
		out, err := b.runGoonCLI(ctx, "memory", "read", args[1])
		if err != nil {
			_ = b.Send(ctx, chatID, "memory read error: "+err.Error())
			return
		}
		b.SendChunked(ctx, chatID, out)
	case "search":
		if len(args) < 2 {
			_ = b.Send(ctx, chatID, "usage: /memory search <query>")
			return
		}
		query := strings.Join(args[1:], " ")
		out, err := b.runGoonCLI(ctx, "memory", "search", query)
		if err != nil {
			_ = b.Send(ctx, chatID, "memory search error: "+err.Error())
			return
		}
		b.SendChunked(ctx, chatID, out)
	default:
		_ = b.Send(ctx, chatID, "unknown subcommand: "+args[0])
	}
}

// --- session ---------------------------------------------------------------

func (b *Bot) cmdWhoami(ctx context.Context, chatID int64, from User) {
	chats := b.opts.Memory.AuthorizedChats()
	for _, c := range chats {
		if c.ChatID == chatID {
			msg := fmt.Sprintf("chat:        %d\nusername:    @%s\ndisplay:     %s\nauthorized:  %s",
				c.ChatID, c.Username, c.DisplayName, c.AuthorizedAt.Format(time.RFC3339))
			_ = b.Send(ctx, chatID, msg)
			return
		}
	}
	_ = b.Send(ctx, chatID, fmt.Sprintf("you are %s @%s (chat=%d)", from.DisplayName(), from.Username, chatID))
}

func (b *Bot) cmdLogout(ctx context.Context, chatID int64) {
	if b.opts.Memory.RevokeChat(chatID) {
		// Drop chat history too.
		b.chatHistMu.Lock()
		delete(b.chatHist, chatID)
		b.chatHistMu.Unlock()
		_ = b.Send(ctx, chatID, "✓ logged out")
	} else {
		_ = b.Send(ctx, chatID, "(no auth record found)")
	}
}

// --- /run agent task -------------------------------------------------------

func (b *Bot) cmdRun(ctx context.Context, chatID int64, args []string) {
	if len(args) == 0 {
		_ = b.Send(ctx, chatID, "usage: /run <task>")
		return
	}
	if b.opts.LLM == nil || b.opts.Tools == nil || b.opts.Executor == nil {
		_ = b.Send(ctx, chatID, "/run unavailable: agent runtime not configured (missing LLM/Tools/Executor)")
		return
	}
	task := strings.Join(args, " ")
	_ = b.Send(ctx, chatID, "→ running agent task…")

	var out bytes.Buffer
	a := agent.New(agent.Options{
		LLM:      b.opts.LLM,
		Tools:    b.opts.Tools,
		Executor: b.opts.Executor,
		Memory:   b.opts.Memory,
		Stdout:   &out,
		Stderr:   &out,
		Debug:    b.opts.Debug,
	})
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	err := a.Run(runCtx, task)
	body := strings.TrimSpace(out.String())
	if err != nil {
		body += "\n\n[error] " + err.Error()
	}
	if body == "" {
		body = "(agent produced no output)"
	}
	b.SendChunked(ctx, chatID, body)
}

// --- full CLI passthrough --------------------------------------------------

// cmdPassthrough is the catch-all for `/<subcmd>` that has no in-process
// handler. It shells out to the goon binary with the same args. Output is
// captured (combined stdout+stderr) and sent back chunked.
func (b *Bot) cmdPassthrough(ctx context.Context, chatID int64, cmd string, args []string) {
	if b.opts.GoonExe == "" {
		_ = b.Send(ctx, chatID, "✗ cannot passthrough: goon executable path unknown")
		return
	}
	out, err := b.runGoonCLI(ctx, append([]string{cmd}, args...)...)
	if err != nil {
		out = strings.TrimSpace(out)
		body := "✗ " + cmd + " failed: " + err.Error()
		if out != "" {
			body += "\n\n" + out
		}
		b.SendChunked(ctx, chatID, body)
		return
	}
	if strings.TrimSpace(out) == "" {
		_ = b.Send(ctx, chatID, "✓ "+cmd+" (no output)")
		return
	}
	b.SendChunked(ctx, chatID, out)
}

// runGoonCLI shells out to the goon binary and returns the combined stdout
// + stderr. Stdin is /dev/null so commands that prompt fail fast instead of
// hanging the long-poll goroutine.
func (b *Bot) runGoonCLI(ctx context.Context, args ...string) (string, error) {
	if b.opts.GoonExe == "" {
		return "", fmt.Errorf("goon executable not located")
	}
	runCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	c := exec.CommandContext(runCtx, b.opts.GoonExe, args...)
	c.Env = os.Environ()
	c.Stdin = bytes.NewReader(nil)
	out, err := c.CombinedOutput()
	return string(out), err
}
