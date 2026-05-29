package telegram

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestExtractFenced(t *testing.T) {
	msg := "header line\n\n" + reviewFence + "\nthe review body\nline two\n" + reviewFence + "\n\nfooter"
	if got := extractFenced(msg); got != "the review body\nline two" {
		t.Errorf("extractFenced = %q", got)
	}
	if extractFenced("no fence here") != "" {
		t.Error("expected empty string when no fence is present")
	}
	if extractFenced(reviewFence+"\nonly an opening fence") != "" {
		t.Error("expected empty string when the closing fence is missing")
	}
}

func TestClampUTF8(t *testing.T) {
	if got := clampUTF8("hello", 10); got != "hello" {
		t.Errorf("under cap: %q", got)
	}
	if got := clampUTF8("hello", 3); got != "hel" {
		t.Errorf("ascii cut: %q", got)
	}
	// "é" is two bytes — clamping at 1 byte must back off to 0 rather
	// than emit an invalid half-rune.
	if got := clampUTF8("é", 1); got != "" {
		t.Errorf("rune-unsafe cut: %q (len %d)", got, len(got))
	}
	// Every truncation length of a unicode string must stay valid UTF-8.
	u := "ababab🎯cdcd"
	for n := 1; n < len(u); n++ {
		if out := clampUTF8(u, n); !utf8.ValidString(out) {
			t.Errorf("clampUTF8(%q, %d) = %q is not valid UTF-8", u, n, out)
		}
	}
}

func TestReviewCallbackData(t *testing.T) {
	if cb := reviewCallbackData("o/r", 42); cb != "rv:o/r:42" {
		t.Errorf("callback data: %q", cb)
	}
	if long := reviewCallbackData(strings.Repeat("x", 80), 1); long != "" {
		t.Errorf("an over-long repo slug should yield an empty callback, got %q", long)
	}
}

func TestEnvTrue(t *testing.T) {
	for _, v := range []string{"1", "true", "yes", "on", "TRUE", "On"} {
		t.Setenv("GOON_TEST_FLAG", v)
		if !envTrue("GOON_TEST_FLAG") {
			t.Errorf("envTrue should be true for %q", v)
		}
	}
	for _, v := range []string{"", "0", "false", "no", "off"} {
		t.Setenv("GOON_TEST_FLAG", v)
		if envTrue("GOON_TEST_FLAG") {
			t.Errorf("envTrue should be false for %q", v)
		}
	}
}
