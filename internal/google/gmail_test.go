package google

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestDecodeB64_URLSafeNoPadding(t *testing.T) {
	// Gmail uses base64url, often without padding.
	raw := "Hello, world! >> ??"
	enc := base64.RawURLEncoding.EncodeToString([]byte(raw))
	if got := decodeB64(enc); got != raw {
		t.Fatalf("decodeB64 = %q, want %q", got, raw)
	}
	if got := decodeB64(""); got != "" {
		t.Fatalf("decodeB64(\"\") = %q, want empty", got)
	}
}

func TestStripHTML(t *testing.T) {
	html := `<html><head><style>.x{color:red}</style></head><body><p>Hi&nbsp;there</p><script>evil()</script><b>bye</b></body></html>`
	got := stripHTML(html)
	if strings.Contains(got, "color:red") || strings.Contains(got, "evil()") {
		t.Fatalf("stripHTML leaked script/style: %q", got)
	}
	if !strings.Contains(got, "Hi there") || !strings.Contains(got, "bye") {
		t.Fatalf("stripHTML dropped text: %q", got)
	}
}

func TestMailFromRaw_Headers(t *testing.T) {
	plain := base64.RawURLEncoding.EncodeToString([]byte("the body text"))
	raw := rawMsg{
		ID:           "abc",
		Snippet:      "a &amp; b",
		InternalDate: "1700000000000",
		Payload: rawPart{
			MimeType: "multipart/alternative",
			Headers: []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			}{
				{Name: "From", Value: "Finance <finance@co>"},
				{Name: "Subject", Value: "Invoice"},
			},
			Parts: []rawPart{
				{MimeType: "text/plain", Body: struct {
					Data string `json:"data"`
					Size int    `json:"size"`
				}{Data: plain}},
			},
		},
	}
	m := mailFromRaw(raw, true)
	if m.From != "Finance <finance@co>" || m.Subject != "Invoice" {
		t.Fatalf("headers wrong: from=%q subject=%q", m.From, m.Subject)
	}
	if m.Snippet != "a & b" {
		t.Fatalf("snippet unescape wrong: %q", m.Snippet)
	}
	if m.Body != "the body text" {
		t.Fatalf("body decode wrong: %q", m.Body)
	}
	if m.Date.IsZero() {
		t.Fatalf("date not parsed from internalDate")
	}
}
