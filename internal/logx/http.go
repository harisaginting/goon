package logx

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"time"
)

// InstrumentClient is the one-line entry point for callers: pass in your
// existing *http.Client (or nil for a defaulted one) plus a component tag
// for the logs, get back a wrapped client that logs every request via the
// package default logger.
//
// Recommended usage when constructing API clients:
//
//	HTTP: logx.InstrumentClient("openai", &http.Client{Timeout: 20 * time.Second})
//
// Passing nil yields a client with default settings:
//
//	HTTP: logx.InstrumentClient("openai", nil)
func InstrumentClient(component string, c *http.Client) *http.Client {
	t := &LoggingTransport{Component: component}
	return t.WrapClient(c)
}

// LoggingTransport wraps any http.RoundTripper to log every request and
// response. Suitable for swapping into http.Client.Transport so callers
// don't need to know logging exists.
//
// At INFO level: method, URL, status, latency, bytes-out, bytes-in.
// At DEBUG level: above + truncated request body + truncated response body.
//
// The wrapper never modifies the request; it only observes. Bodies are
// captured by reading-and-replacing the io.ReadCloser, so callers see the
// same bytes they'd see without the wrapper.
type LoggingTransport struct {
	// Inner is the wrapped RoundTripper. nil → http.DefaultTransport.
	Inner http.RoundTripper
	// Component tags every log line with a category (e.g. "openai", "jira").
	// Optional but very useful for grep/filter.
	Component string
	// MaxBodyBytes caps how much of each body is captured for debug logging.
	// 0 → 4096.
	MaxBodyBytes int
	// Logger overrides the package default (mostly for tests).
	Logger *Logger
}

// WrapClient returns a copy of c with t spliced into Transport. If c is nil
// it returns &http.Client{Transport: t}. Pass-through-safe.
func (t *LoggingTransport) WrapClient(c *http.Client) *http.Client {
	if c == nil {
		return &http.Client{Transport: t}
	}
	out := *c
	if c.Transport != nil {
		t.Inner = c.Transport
	}
	out.Transport = t
	return &out
}

// RoundTrip implements http.RoundTripper.
func (t *LoggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	logger := t.Logger
	if logger == nil {
		logger = Default()
	}
	component := t.Component
	if component == "" {
		component = "http"
	}
	maxBody := t.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = 4096
	}

	// Capture the outgoing body if present, so we can both log it AND let
	// the underlying transport consume it.
	var reqBody []byte
	if req.Body != nil {
		buf, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err == nil {
			reqBody = buf
			req.Body = io.NopCloser(bytes.NewReader(buf))
		}
	}

	start := time.Now()
	inner := t.Inner
	if inner == nil {
		inner = http.DefaultTransport
	}
	resp, err := inner.RoundTrip(req)
	elapsed := time.Since(start)

	// Build the common attrs.
	attrs := []any{
		"component", component,
		"method", req.Method,
		"url", redactedURL(req.URL.String()),
		"req_bytes", len(reqBody),
		"latency_ms", elapsed.Milliseconds(),
	}
	if err != nil {
		logger.Error("http.error", append(attrs, "err", err.Error())...)
		return nil, err
	}
	attrs = append(attrs, "status", resp.StatusCode)

	// Capture the response body too, again so callers see it unchanged.
	var respBody []byte
	if resp.Body != nil {
		buf, rerr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if rerr == nil {
			respBody = buf
			resp.Body = io.NopCloser(bytes.NewReader(buf))
		}
	}
	attrs = append(attrs, "resp_bytes", len(respBody))

	// At debug level, attach truncated bodies. (Skipped at info level so
	// the production log doesn't fill up with massive LLM payloads.)
	debugAttrs := append(append([]any{}, attrs...),
		"req_body", truncate(reqBody, maxBody),
		"resp_body", truncate(respBody, maxBody),
	)
	logger.Debug("http", debugAttrs...)

	switch {
	case resp.StatusCode >= 500:
		logger.Error("http", attrs...)
	case resp.StatusCode >= 400:
		logger.Warn("http", attrs...)
	default:
		logger.Info("http", attrs...)
	}
	return resp, nil
}

// truncate returns up to max bytes of b as a string. If truncated, the
// suffix "…(N more)" is appended so the reader knows there was more.
func truncate(b []byte, max int) string {
	if len(b) == 0 {
		return ""
	}
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "…(+" + itoa(len(b)-max) + " bytes)"
}

// itoa avoids a strconv import for the one place we need it.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// redactedURL strips obvious secrets from the URL we log. Catches the most
// common case: tokens embedded in the path (e.g. Telegram /bot<token>/sendMessage)
// and basic-auth in userinfo.
func redactedURL(u string) string {
	// Telegram-style /bot<token>/...
	if i := strings.Index(u, "/bot"); i >= 0 {
		end := strings.Index(u[i+4:], "/")
		if end > 0 {
			return u[:i+4] + "***" + u[i+4+end:]
		}
	}
	// userinfo (https://user:pass@host/path)
	if at := strings.Index(u, "@"); at > 0 {
		// Find the // before the userinfo.
		if proto := strings.Index(u, "://"); proto >= 0 && at > proto+3 {
			return u[:proto+3] + "***@" + u[at+1:]
		}
	}
	return u
}
