// Package telegram is goon's inbound Telegram bot.
//
// It long-polls the Telegram Bot API (via getUpdates) and dispatches
// authenticated messages to per-command handlers — the outbound notify
// path lives in internal/tools/telegram.go and is unchanged.
//
// Authentication is a single shared secret loaded from
// GOON_TELEGRAM_SECRET. A user proves they know it by sending
// `/auth <secret>` once; the bot then records their chat ID via
// memory.AuthorizeChat and accepts every later message from that chat
// until they /logout. The secret never leaves the process.
//
// Lifecycle: the daemon starts the bot in a goroutine when both
// TELEGRAM_BOT_TOKEN and GOON_TELEGRAM_SECRET are set in the env. The
// bot shuts down when ctx is cancelled (e.g. `goon stop`).
package telegram

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/harisaginting/goon/internal/executor"
	"github.com/harisaginting/goon/internal/boards"
	"github.com/harisaginting/goon/internal/githost"
	"github.com/harisaginting/goon/internal/llm"
	"github.com/harisaginting/goon/internal/logx"
	"github.com/harisaginting/goon/internal/memory"
	"github.com/harisaginting/goon/internal/tools"
)

// Waker is satisfied by *daemon.Daemon's Wake() method. We accept it
// as an interface here instead of importing daemon directly so the
// telegram package doesn't pull in workflow + tools + executor.
type Waker interface {
	Wake()
}

// Options configures a Bot. Memory and Token are required; everything else
// is optional and turns specific commands on or off.
type Options struct {
	Token      string // TELEGRAM_BOT_TOKEN
	Secret     string // GOON_TELEGRAM_SECRET
	APIBaseURL string // override for tests; defaults to https://api.telegram.org

	Memory   *memory.Memory     // required: persistent state for auth + Q&A
	LLM      llm.Provider       // optional: enables /run + plain-text chat
	Tools    *tools.Registry    // optional: enables /run
	Executor *executor.Executor // optional: enables /run
	Host     githost.Host       // optional: enables /prs /review /approve /decline /comment
	Board    boards.Board       // optional: enables /refresh + chat auto-refresh of ticket cache

	// Daemon, when non-nil and implementing Wake(), is signalled by
	// /answer so a paused workflow resumes within a second of the
	// reply instead of waiting up to PollInterval for the next tick.
	// Declared as an interface (not *daemon.Daemon) to avoid an
	// import cycle: daemon imports telegram already.
	Daemon Waker

	// GoonExe is the absolute path to the goon binary used for full CLI
	// parity (`/<subcmd>` shells out to `goon <subcmd>`). Defaults to
	// os.Executable() at New() time.
	GoonExe string

	Stdout io.Writer
	Stderr io.Writer
	Debug  bool

	// PollTimeout is the long-poll timeout (seconds) sent to getUpdates.
	// 0 → 30s. Lower for tests.
	PollTimeout int
}

// Bot is the long-polling Telegram client. Safe to call Start once per
// instance. Multiple processes pointed at the same bot token will get a 409
// from Telegram — only one bot reader at a time.
type Bot struct {
	opts Options
	http *http.Client

	chatHistMu sync.Mutex
	chatHist   map[int64][]llm.Message // best-effort in-process chat history per chat id
}

// New constructs a Bot. Token + Secret + Memory are required; missing fields
// return an error so the daemon can decide whether to start the loop.
func New(o Options) (*Bot, error) {
	if strings.TrimSpace(o.Token) == "" {
		return nil, errors.New("telegram bot: TELEGRAM_BOT_TOKEN is empty")
	}
	if strings.TrimSpace(o.Secret) == "" {
		return nil, errors.New("telegram bot: GOON_TELEGRAM_SECRET is empty")
	}
	if o.Memory == nil {
		return nil, errors.New("telegram bot: Memory is required")
	}
	if o.APIBaseURL == "" {
		o.APIBaseURL = "https://api.telegram.org"
	}
	o.APIBaseURL = strings.TrimRight(o.APIBaseURL, "/")
	if o.PollTimeout <= 0 {
		o.PollTimeout = 30
	}
	if o.GoonExe == "" {
		if exe, err := os.Executable(); err == nil {
			o.GoonExe = exe
		}
	}
	return &Bot{
		opts: o,
		http: logx.InstrumentClient("telegram-bot",
			&http.Client{Timeout: time.Duration(o.PollTimeout+10) * time.Second}),
		chatHist: map[int64][]llm.Message{},
	}, nil
}

// Start blocks the caller in the long-poll loop until ctx is cancelled.
// Errors during a single poll cycle are logged and retried with backoff,
// so transient network failures don't stop the bot.
func (b *Bot) Start(ctx context.Context) error {
	logx.Info("telegram_bot.start", "api", b.opts.APIBaseURL, "poll_timeout_s", b.opts.PollTimeout)
	if me, err := b.getMe(ctx); err == nil {
		fmt.Fprintf(b.opts.Stdout, "→ telegram bot ready: @%s\n", me)
	} else {
		fmt.Fprintf(b.opts.Stderr, "telegram bot: getMe failed: %v\n", err)
	}
	// Publish the command list so Telegram's menu (☰ in the input bar)
	// shows /help, /status, /prs, etc. without the user having to know
	// them in advance. Best-effort: failures are logged but don't block
	// the long-poll loop from starting.
	if err := b.registerCommands(ctx); err != nil {
		logx.Warn("telegram_bot.register_commands_failed", "error", err.Error())
	}
	var offset int64
	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			logx.Info("telegram_bot.stop")
			return ctx.Err()
		default:
		}
		updates, err := b.getUpdates(ctx, offset)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			logx.Warn("telegram_bot.poll_error", "error", err.Error(), "backoff", backoff.String())
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			if backoff < 60*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			b.handleUpdate(ctx, u)
		}
	}
}

// --- Telegram API types ----------------------------------------------------

type apiResponse struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description"`
	Result      json.RawMessage `json:"result"`
}

// Update mirrors the slice of fields we consume from the Bot API.
type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
	// EditedMessage etc intentionally ignored: we only handle fresh sends.
}

// CallbackQuery fires when the user taps an inline-keyboard button.
// We use them as the "select a ticket → see actions" affordance so
// users don't have to type ticket keys.
type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message,omitempty"`
	Data    string   `json:"data,omitempty"`
}

// Message is the inbound text message we dispatch on.
type Message struct {
	MessageID      int64    `json:"message_id"`
	Text           string   `json:"text"`
	Chat           Chat     `json:"chat"`
	From           User     `json:"from"`
	Date           int64    `json:"date"`
	ReplyToMessage *Message `json:"reply_to_message,omitempty"`
}

// Chat identifies the conversation.
type Chat struct {
	ID    int64  `json:"id"`
	Type  string `json:"type"` // "private" | "group" | "supergroup" | "channel"
	Title string `json:"title,omitempty"`
}

// User is the sender.
type User struct {
	ID        int64  `json:"id"`
	Username  string `json:"username,omitempty"`
	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
	IsBot     bool   `json:"is_bot,omitempty"`
}

// DisplayName returns "First Last" or username when name fields are empty.
func (u User) DisplayName() string {
	name := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if name != "" {
		return name
	}
	return u.Username
}

// --- API call helpers ------------------------------------------------------

func (b *Bot) endpoint(method string) string {
	return fmt.Sprintf("%s/bot%s/%s", b.opts.APIBaseURL, b.opts.Token, method)
}

// getMe pings Telegram for the bot identity. Returns the bot's username on
// success — useful as a one-shot "config OK" check at startup.
func (b *Bot) getMe(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.endpoint("getMe"), nil)
	if err != nil {
		return "", err
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var r apiResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", err
	}
	if !r.OK {
		return "", fmt.Errorf("telegram getMe: %s", r.Description)
	}
	var me struct {
		Username string `json:"username"`
	}
	_ = json.Unmarshal(r.Result, &me)
	return me.Username, nil
}

// getUpdates long-polls Telegram for new updates starting at offset. It
// honors b.opts.PollTimeout for the long-poll wait and ctx for cancellation.
func (b *Bot) getUpdates(ctx context.Context, offset int64) ([]Update, error) {
	v := url.Values{}
	if offset > 0 {
		v.Set("offset", fmt.Sprintf("%d", offset))
	}
	v.Set("timeout", fmt.Sprintf("%d", b.opts.PollTimeout))
	v.Set("allowed_updates", `["message","callback_query"]`)
	endpoint := b.endpoint("getUpdates") + "?" + v.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusConflict {
		// Another process is polling this token. Don't tight-loop.
		return nil, fmt.Errorf("telegram 409: another goon process is polling this bot token")
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("telegram http %d: %s", resp.StatusCode, snippet(string(raw), 200))
	}
	var r apiResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	if !r.OK {
		return nil, fmt.Errorf("telegram getUpdates: %s", r.Description)
	}
	var ups []Update
	if err := json.Unmarshal(r.Result, &ups); err != nil {
		return nil, err
	}
	return ups, nil
}

// Send delivers text to chatID as a single Telegram message. Long content
// should use SendChunked which splits at line boundaries.
func (b *Bot) Send(ctx context.Context, chatID int64, text string) error {
	return b.send(ctx, chatID, text, "")
}

// SendWithButtons posts a message with an attached inline keyboard.
// The keyboard is a list of rows; each row is a list of buttons. Each
// button is {text, callbackData} — callbackData is what Telegram
// echoes back when the user taps. Use SendForceReply for the
// "user-types-text-as-the-answer" pattern.
//
// Telegram caps callback_data at 64 bytes; callers should keep
// payloads short (e.g. "v:KEY", "m:KEY:in_progress").
type InlineButton struct {
	Text         string
	CallbackData string
	URL          string // mutually exclusive with CallbackData
}

func (b *Bot) SendWithButtons(ctx context.Context, chatID int64, text string, rows [][]InlineButton) error {
	if len(rows) == 0 {
		return b.send(ctx, chatID, text, "")
	}
	// Build the reply_markup JSON manually so we don't import a
	// telegram SDK. Shape: {"inline_keyboard":[[{...},{...}], ...]}
	type btnWire struct {
		Text         string `json:"text"`
		CallbackData string `json:"callback_data,omitempty"`
		URL          string `json:"url,omitempty"`
	}
	type kbWire struct {
		InlineKeyboard [][]btnWire `json:"inline_keyboard"`
	}
	wire := kbWire{InlineKeyboard: make([][]btnWire, 0, len(rows))}
	for _, row := range rows {
		wRow := make([]btnWire, 0, len(row))
		for _, bt := range row {
			wRow = append(wRow, btnWire{Text: bt.Text, CallbackData: bt.CallbackData, URL: bt.URL})
		}
		wire.InlineKeyboard = append(wire.InlineKeyboard, wRow)
	}
	buf, err := json.Marshal(wire)
	if err != nil {
		return err
	}
	return b.send(ctx, chatID, text, string(buf))
}

// SendForceReply posts a message and asks Telegram to surface the
// system "reply" UI on the user's keyboard, with the original
// message quoted. This is how we collect multi-line input (comment
// bodies, descriptions) without needing per-chat state machines —
// the inbound reply carries ReplyToMessage pointing at our prompt,
// which has the ticket key embedded in plain text for us to parse.
func (b *Bot) SendForceReply(ctx context.Context, chatID int64, text string) error {
	markup := `{"force_reply":true,"input_field_placeholder":"type your reply here…"}`
	return b.send(ctx, chatID, text, markup)
}

// send is the shared sendMessage implementation. replyMarkup is the
// JSON-encoded reply_markup value (inline keyboard, force reply,
// etc.) — empty string means "no markup".
func (b *Bot) send(ctx context.Context, chatID int64, text, replyMarkup string) error {
	form := url.Values{}
	form.Set("chat_id", fmt.Sprintf("%d", chatID))
	form.Set("text", text)
	form.Set("disable_web_page_preview", "true")
	if replyMarkup != "" {
		form.Set("reply_markup", replyMarkup)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		b.endpoint("sendMessage"), bytes.NewBufferString(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("telegram send http %d: %s", resp.StatusCode, snippet(string(raw), 200))
	}
	var r apiResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return err
	}
	if !r.OK {
		return fmt.Errorf("telegram send: %s", r.Description)
	}
	return nil
}

// AnswerCallback dismisses the loading spinner on a tapped inline
// button. Toast text is shown briefly above the chat. Best-effort —
// errors here are logged but not surfaced.
func (b *Bot) AnswerCallback(ctx context.Context, callbackID, toast string) {
	form := url.Values{}
	form.Set("callback_query_id", callbackID)
	if toast != "" {
		form.Set("text", toast)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		b.endpoint("answerCallbackQuery"), bytes.NewBufferString(form.Encode()))
	if err != nil {
		logx.Warn("telegram_bot.answer_callback_build", "error", err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := b.http.Do(req)
	if err != nil {
		logx.Warn("telegram_bot.answer_callback_send", "error", err.Error())
		return
	}
	resp.Body.Close()
}

// SendChunked splits a long body into messages under Telegram's 4096-char
// per-message limit, breaking on line boundaries when possible. Errors on
// individual chunks are logged but don't stop the rest from being delivered.
func (b *Bot) SendChunked(ctx context.Context, chatID int64, text string) {
	const maxLen = 4000 // leave headroom for safety
	if len(text) <= maxLen {
		if err := b.Send(ctx, chatID, text); err != nil {
			logx.Warn("telegram_bot.send_error", "chat", chatID, "error", err.Error())
		}
		return
	}
	for len(text) > 0 {
		chunk := text
		if len(chunk) > maxLen {
			cut := strings.LastIndex(chunk[:maxLen], "\n")
			if cut <= 0 {
				cut = maxLen
			}
			// Rune-aware: byte-cut at maxLen can land mid-UTF-8
			// (CJK / emoji in Jira titles, accented chars). Walk
			// back to the nearest rune start so Telegram's UTF-8
			// validator accepts every chunk.
			for cut > 0 && !utf8.RuneStart(chunk[cut]) {
				cut--
			}
			if cut <= 0 {
				cut = maxLen // truly degenerate input — give up
			}
			chunk = chunk[:cut]
		}
		if err := b.Send(ctx, chatID, chunk); err != nil {
			logx.Warn("telegram_bot.send_error", "chat", chatID, "error", err.Error())
		}
		text = strings.TrimPrefix(text[len(chunk):], "\n")
	}
}

// --- update dispatch -------------------------------------------------------

// handleUpdate dispatches one Update through the auth + command pipeline.
// All errors are surfaced back to the user as Telegram messages so they're
// never lost.
func (b *Bot) handleUpdate(ctx context.Context, u Update) {
	// Callback queries come from inline-keyboard button taps. They
	// have no Message of their own (well, they reference the original
	// message but the text is empty), so handle them first.
	if u.CallbackQuery != nil {
		b.handleCallback(ctx, u.CallbackQuery)
		return
	}
	msg := u.Message
	if msg == nil || strings.TrimSpace(msg.Text) == "" {
		return
	}
	chatID := msg.Chat.ID
	text := strings.TrimSpace(msg.Text)

	logx.Info("telegram_bot.message",
		"chat", chatID, "user", msg.From.Username,
		"len", len(text))

	// /auth is the only command we accept from un-authorized chats.
	if strings.HasPrefix(text, "/auth") {
		b.handleAuth(ctx, chatID, msg.From, text)
		return
	}
	if !b.opts.Memory.IsChatAuthorized(chatID) {
		_ = b.Send(ctx, chatID, "🔒 you're not authorized.\nsend `/auth <secret>` first.")
		return
	}
	b.opts.Memory.TouchChat(chatID)

	// "Reply to this message" flows: when the inbound message has a
	// ReplyToMessage that we previously sent as a force-reply prompt
	// (with an embedded ticket key tag), route into the action handler
	// instead of the generic command/chat path.
	if msg.ReplyToMessage != nil && b.tryHandleReplyAction(ctx, chatID, msg) {
		return
	}

	if strings.HasPrefix(text, "/") {
		b.handleCommand(ctx, chatID, msg.From, text)
		return
	}
	b.handleChat(ctx, chatID, text)
}

// handleAuth checks the supplied secret with constant-time comparison and,
// on a match, stores the chat ID via Memory.AuthorizeChat.
func (b *Bot) handleAuth(ctx context.Context, chatID int64, from User, text string) {
	parts := strings.Fields(text)
	if len(parts) < 2 {
		_ = b.Send(ctx, chatID, "usage: /auth <secret>")
		return
	}
	supplied := strings.Join(parts[1:], " ")
	got := []byte(supplied)
	want := []byte(b.opts.Secret)
	// pad to equal length so constant-time compare doesn't reveal lengths
	if len(got) != len(want) {
		_ = b.Send(ctx, chatID, "✗ wrong secret")
		logx.Warn("telegram_bot.auth_fail", "chat", chatID, "reason", "length")
		return
	}
	if subtle.ConstantTimeCompare(got, want) != 1 {
		_ = b.Send(ctx, chatID, "✗ wrong secret")
		logx.Warn("telegram_bot.auth_fail", "chat", chatID, "reason", "mismatch")
		return
	}
	b.opts.Memory.AuthorizeChat(chatID, from.Username, from.DisplayName())
	logx.Info("telegram_bot.auth_ok", "chat", chatID, "user", from.Username)
	_ = b.Send(ctx, chatID,
		"✓ authenticated.\nTry /help, /status, or just send a message to chat with the model.")
}

// snippet truncates s for log-friendly output.
func snippet(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
