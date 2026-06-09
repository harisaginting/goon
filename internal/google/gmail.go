package google

import (
	"context"
	"encoding/base64"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Mail is one Gmail message flattened for chat. Body is only populated by
// GetMessage (the full-fetch path); SearchMessages leaves it empty and
// fills Snippet instead, to keep list views cheap.
type Mail struct {
	ID      string
	From    string
	Subject string
	Date    time.Time
	Snippet string
	Body    string
}

type rawMsgList struct {
	Messages []struct {
		ID string `json:"id"`
	} `json:"messages"`
	ResultSizeEstimate int `json:"resultSizeEstimate"`
}

type rawPart struct {
	MimeType string `json:"mimeType"`
	Headers  []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"headers"`
	Body struct {
		Data string `json:"data"`
		Size int    `json:"size"`
	} `json:"body"`
	Parts []rawPart `json:"parts"`
}

type rawMsg struct {
	ID           string  `json:"id"`
	Snippet      string  `json:"snippet"`
	InternalDate string  `json:"internalDate"` // ms since epoch, as a string
	Payload      rawPart `json:"payload"`
}

const gmailBase = "https://gmail.googleapis.com/gmail/v1/users/me/messages"

// SearchMessages runs a Gmail search (same query syntax as the Gmail box:
// "from:finance newer_than:7d", "is:unread", "subject:invoice") and returns
// up to limit messages with From/Subject/Date/Snippet filled. Body is left
// empty — use GetMessage to read one in full.
func (c *Client) SearchMessages(ctx context.Context, query string, limit int) ([]Mail, error) {
	if limit <= 0 {
		limit = 8
	}
	if limit > 25 {
		limit = 25
	}
	v := url.Values{}
	if strings.TrimSpace(query) != "" {
		v.Set("q", query)
	}
	v.Set("maxResults", strconv.Itoa(limit))
	var list rawMsgList
	if err := c.getJSON(ctx, gmailBase+"?"+v.Encode(), &list); err != nil {
		return nil, err
	}
	out := make([]Mail, 0, len(list.Messages))
	for i, m := range list.Messages {
		if i >= limit {
			break
		}
		mv := url.Values{}
		mv.Set("format", "metadata")
		mv.Add("metadataHeaders", "From")
		mv.Add("metadataHeaders", "Subject")
		mv.Add("metadataHeaders", "Date")
		var raw rawMsg
		if err := c.getJSON(ctx, gmailBase+"/"+url.PathEscape(m.ID)+"?"+mv.Encode(), &raw); err != nil {
			return nil, err
		}
		out = append(out, mailFromRaw(raw, false))
	}
	return out, nil
}

// GetMessage fetches one message in full and decodes its text body.
func (c *Client) GetMessage(ctx context.Context, id string) (Mail, error) {
	v := url.Values{}
	v.Set("format", "full")
	var raw rawMsg
	if err := c.getJSON(ctx, gmailBase+"/"+url.PathEscape(id)+"?"+v.Encode(), &raw); err != nil {
		return Mail{}, err
	}
	return mailFromRaw(raw, true), nil
}

// mailFromRaw flattens a Gmail message resource. withBody decodes the MIME
// parts into plain text (full-fetch path); otherwise only Snippet is used.
func mailFromRaw(raw rawMsg, withBody bool) Mail {
	m := Mail{ID: raw.ID, Snippet: strings.TrimSpace(unescapeAmp(raw.Snippet))}
	m.From = headerValue(raw.Payload, "From")
	m.Subject = headerValue(raw.Payload, "Subject")
	if m.Subject == "" {
		m.Subject = "(no subject)"
	}
	if ms, err := strconv.ParseInt(raw.InternalDate, 10, 64); err == nil && ms > 0 {
		m.Date = time.UnixMilli(ms)
	}
	if withBody {
		m.Body = strings.TrimSpace(extractBody(raw.Payload))
	}
	return m
}

func headerValue(p rawPart, name string) string {
	for _, h := range p.Headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

// extractBody walks a (possibly multipart) payload, preferring text/plain.
// If only HTML is present it strips tags to a readable approximation.
func extractBody(p rawPart) string {
	if plain := findPart(p, "text/plain"); plain != "" {
		return plain
	}
	if html := findPart(p, "text/html"); html != "" {
		return stripHTML(html)
	}
	// Single-part message with body directly on the payload.
	if d := decodeB64(p.Body.Data); d != "" {
		if strings.Contains(strings.ToLower(p.MimeType), "html") {
			return stripHTML(d)
		}
		return d
	}
	return ""
}

func findPart(p rawPart, mime string) string {
	if strings.HasPrefix(strings.ToLower(p.MimeType), mime) {
		if d := decodeB64(p.Body.Data); d != "" {
			return d
		}
	}
	for _, sub := range p.Parts {
		if got := findPart(sub, mime); got != "" {
			return got
		}
	}
	return ""
}

// decodeB64 decodes Gmail's base64url payloads, tolerating missing padding.
func decodeB64(s string) string {
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "-", "+")
	s = strings.ReplaceAll(s, "_", "/")
	if m := len(s) % 4; m != 0 {
		s += strings.Repeat("=", 4-m)
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return ""
	}
	return string(b)
}

// stripHTML reduces an HTML body to readable text: drops <script>/<style>,
// removes tags, and collapses whitespace. Deliberately tiny (zero-dep).
func stripHTML(s string) string {
	s = dropBetween(s, "<script", "</script>")
	s = dropBetween(s, "<style", "</style>")
	var b strings.Builder
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
			b.WriteByte(' ')
		case !inTag:
			b.WriteRune(r)
		}
	}
	return collapseWS(unescapeAmp(b.String()))
}

func dropBetween(s, open, close string) string {
	for {
		lo := strings.Index(strings.ToLower(s), open)
		if lo < 0 {
			return s
		}
		hi := strings.Index(strings.ToLower(s[lo:]), close)
		if hi < 0 {
			return s[:lo]
		}
		s = s[:lo] + s[lo+hi+len(close):]
	}
}

func unescapeAmp(s string) string {
	r := strings.NewReplacer(
		"&amp;", "&", "&lt;", "<", "&gt;", ">",
		"&quot;", "\"", "&#39;", "'", "&nbsp;", " ",
	)
	return r.Replace(s)
}

func collapseWS(s string) string {
	var b strings.Builder
	var lastSpace, lastNL bool
	for _, r := range s {
		switch r {
		case ' ', '\t':
			if !lastSpace && !lastNL {
				b.WriteByte(' ')
			}
			lastSpace = true
		case '\n', '\r':
			if !lastNL {
				b.WriteByte('\n')
			}
			lastNL = true
			lastSpace = false
		default:
			b.WriteRune(r)
			lastSpace = false
			lastNL = false
		}
	}
	return strings.TrimSpace(b.String())
}
