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

	"github.com/harisaginting/goon/internal/executor"
	"github.com/harisaginting/goon/internal/githost"
	"github.com/harisaginting/goon/internal/llm"
	"github.com/harisaginting/goon/internal/logx"
	"github.com/harisaginting/goon/internal/memory"
	"github.com/harisaginting/goon/internal/tools"
)

// Options configures a Bot. Memory and Token are required; everything else
// is optional and turns specific commands on or off.
type Options struct {
	Token      string // TELEGRAM_BOT_TOKEN
	Secret     string // GOON_TELEGRAM_SECRET
	APIBaseURL string // override for tests; defaults to https://api.telegram.org

	Memory   *memory.Memory   // required: persistent state for auth + Q&A
	LLM      llm.Provider     // optional: enables /run + plain-text chat
	Tools    *tools.Registry  // optional: enables /run
	Executor *executor.Executor // optional: enables /run
	Host     githost.Host     // optional: enables /prs /review /approve /decline /comment

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
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message"`
	// EditedMessage etc intentionally ignored: we only handle fresh sends.
}

// Message is the inbound text message we dispatch on.
type Message struct {
	MessageID int64  `json:"message_id"`
	Text      string `json:"text"`
	Chat      Chat   `json:"chat"`
	From      User   `json:"from"`
	Date      int64  `json:"date"`
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
	v.Set("allowed_updates", `["message"]`)
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
	form := url.Values{}
	form.Set("chat_id", fmt.Sprintf("%d", chatID))
	form.Set("text", text)
	form.Set("disable_web_page_preview", "true")
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
