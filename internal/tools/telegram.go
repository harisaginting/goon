package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Telegram sends messages through a Telegram Bot via the Bot API.
//
// Configure with TELEGRAM_BOT_TOKEN and an optional default TELEGRAM_CHAT_ID
// (the per-call "chat_id" arg overrides it). Supported "op":
//
//   - "send_message" (default): op=send_message, text=<body>, chat_id=<id?>
//   - "get_me":                  health check, returns bot identity
type Telegram struct {
	Token      string
	DefaultTo  string
	HTTP       *http.Client
	APIBaseURL string
}

// NewTelegramFromEnv reads token + default chat from env.
func NewTelegramFromEnv() *Telegram {
	base := os.Getenv("TELEGRAM_API_BASE_URL")
	if base == "" {
		base = "https://api.telegram.org"
	}
	return &Telegram{
		Token:      os.Getenv("TELEGRAM_BOT_TOKEN"),
		DefaultTo:  os.Getenv("TELEGRAM_CHAT_ID"),
		HTTP:       &http.Client{Timeout: 15 * time.Second},
		APIBaseURL: base,
	}
}

func (*Telegram) Name() string { return "telegram" }
func (*Telegram) Description() string {
	return "send a message to a Telegram chat via Bot API"
}
func (*Telegram) Schema() map[string]string {
	return map[string]string{
		"op":      `"send_message" (default) or "get_me"`,
		"text":    "message body (send_message)",
		"chat_id": "override default chat id (send_message)",
		"parse":   `"" | "Markdown" | "HTML" (send_message)`,
	}
}

func (t *Telegram) Run(ctx context.Context, args map[string]string) (Result, error) {
	if t.Token == "" {
		return Result{ToolName: "telegram"}, errors.New("telegram: TELEGRAM_BOT_TOKEN is not set")
	}
	op := strings.ToLower(strings.TrimSpace(args["op"]))
	if op == "" {
		op = "send_message"
	}
	switch op {
	case "send_message":
		return t.sendMessage(ctx, args)
	case "get_me":
		return t.getMe(ctx)
	default:
		return Result{ToolName: "telegram"}, fmt.Errorf("telegram: unknown op %q", op)
	}
}

type telegramResp struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description"`
	Result      json.RawMessage `json:"result"`
}

func (t *Telegram) sendMessage(ctx context.Context, args map[string]string) (Result, error) {
	text := args["text"]
	if text == "" {
		return Result{ToolName: "telegram"}, errors.New(`telegram send_message: "text" is required`)
	}
	chatID := args["chat_id"]
	if chatID == "" {
		chatID = t.DefaultTo
	}
	if chatID == "" {
		return Result{ToolName: "telegram"}, errors.New(`telegram send_message: chat_id missing (set TELEGRAM_CHAT_ID or pass arg)`)
	}
	form := url.Values{}
	form.Set("chat_id", chatID)
	form.Set("text", text)
	if p := args["parse"]; p != "" {
		form.Set("parse_mode", p)
	}
	endpoint := fmt.Sprintf("%s/bot%s/sendMessage", t.APIBaseURL, t.Token)
	body, err := t.post(ctx, endpoint, form)
	if err != nil {
		return Result{ToolName: "telegram", Err: err}, err
	}
	var r telegramResp
	if err := json.Unmarshal(body, &r); err != nil {
		return Result{ToolName: "telegram", Err: err}, err
	}
	if !r.OK {
		return Result{ToolName: "telegram"}, fmt.Errorf("telegram: %s", r.Description)
	}
	return Result{ToolName: "telegram", Stdout: "sent\n"}, nil
}

func (t *Telegram) getMe(ctx context.Context) (Result, error) {
	endpoint := fmt.Sprintf("%s/bot%s/getMe", t.APIBaseURL, t.Token)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Result{ToolName: "telegram"}, err
	}
	resp, err := t.HTTP.Do(req)
	if err != nil {
		return Result{ToolName: "telegram", Err: err}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Result{ToolName: "telegram", Err: err}, err
	}
	if resp.StatusCode != 200 {
		return Result{ToolName: "telegram"}, fmt.Errorf("telegram http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	return Result{ToolName: "telegram", Stdout: string(raw)}, nil
}

func (t *Telegram) post(ctx context.Context, endpoint string, form url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		bytes.NewBufferString(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := t.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}
