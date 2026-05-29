package agentctx

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestClampForChat(t *testing.T) {
	if got := clampForChat("short", 100); got != "short" {
		t.Errorf("under cap should be unchanged, got %q", got)
	}
	got := clampForChat(strings.Repeat("x", 200), 50)
	if !strings.Contains(got, "truncated for chat") {
		t.Errorf("expected truncation notice, got %q", got)
	}
	// Rune-safe: truncating a 2-byte-per-rune string at any length must
	// still yield valid UTF-8.
	u := strings.Repeat("é", 100)
	for n := 1; n < 80; n++ {
		if out := clampForChat(u, n); !utf8.ValidString(out) {
			t.Errorf("clampForChat at %d produced invalid UTF-8", n)
		}
	}
}

func TestExtActionsValid(t *testing.T) {
	for _, a := range []string{"confluence_search", "confluence_get", "web_search", "web_fetch"} {
		if !validActions[a] {
			t.Errorf("action %q missing from validActions", a)
		}
	}
}

// TestExtTools_RejectEmptyArgs checks the argument-validation paths,
// which return before any network call — so the test is hermetic.
func TestExtTools_RejectEmptyArgs(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		fn   func() (string, string)
	}{
		{"confluence_search", func() (string, string) {
			return execConfluenceSearch(ctx, ToolCall{Action: "confluence_search"})
		}},
		{"confluence_get", func() (string, string) {
			return execConfluenceGet(ctx, ToolCall{Action: "confluence_get"})
		}},
		{"web_search", func() (string, string) {
			return execWebSearch(ctx, ToolCall{Action: "web_search"})
		}},
		{"web_fetch", func() (string, string) {
			return execWebFetch(ctx, ToolCall{Action: "web_fetch"})
		}},
	}
	for _, c := range cases {
		out, summary := c.fn()
		if !strings.Contains(out, "TOOL ERROR") {
			t.Errorf("%s with empty args: expected TOOL ERROR, got %q", c.name, out)
		}
		if !strings.Contains(summary, "rejected") {
			t.Errorf("%s summary: expected 'rejected', got %q", c.name, summary)
		}
	}
}
