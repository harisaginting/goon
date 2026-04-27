package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestTelegram_SendMessage_OK(t *testing.T) {
	var got url.Values
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		got = r.PostForm
		if !strings.HasSuffix(r.URL.Path, "/sendMessage") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer ts.Close()

	tg := &Telegram{
		Token:      "FAKE",
		DefaultTo:  "12345",
		HTTP:       ts.Client(),
		APIBaseURL: ts.URL,
	}
	res, err := tg.Run(context.Background(), map[string]string{
		"text": "hello goon",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res.Stdout, "sent") {
		t.Fatalf("expected sent confirmation, got %q", res.Stdout)
	}
	if got.Get("chat_id") != "12345" || got.Get("text") != "hello goon" {
		t.Fatalf("bad form: %v", got)
	}
}

func TestTelegram_RequiresChatID(t *testing.T) {
	tg := &Telegram{Token: "FAKE", APIBaseURL: "http://example", HTTP: http.DefaultClient}
	_, err := tg.Run(context.Background(), map[string]string{"text": "hi"})
	if err == nil || !strings.Contains(err.Error(), "chat_id") {
		t.Fatalf("expected chat_id error, got %v", err)
	}
}

func TestTelegram_RequiresToken(t *testing.T) {
	tg := &Telegram{}
	_, err := tg.Run(context.Background(), map[string]string{"text": "hi"})
	if err == nil || !strings.Contains(err.Error(), "TELEGRAM_BOT_TOKEN") {
		t.Fatalf("expected token error, got %v", err)
	}
}

func TestTelegram_APIError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false,"description":"chat not found"}`))
	}))
	defer ts.Close()
	tg := &Telegram{
		Token:      "FAKE",
		HTTP:       ts.Client(),
		APIBaseURL: ts.URL,
	}
	_, err := tg.Run(context.Background(), map[string]string{
		"text":    "hi",
		"chat_id": "0",
	})
	if err == nil || !strings.Contains(err.Error(), "chat not found") {
		t.Fatalf("expected api error, got %v", err)
	}
}
