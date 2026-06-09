package google

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/harisaginting/goon/internal/logx"
)

// Client is an authenticated handle to the Google REST APIs. It caches a
// short-lived access token and transparently refreshes it from the
// refresh token when it expires — callers never deal with tokens.
type Client struct {
	cfg Config
	hc  *http.Client

	mu          sync.Mutex
	accessToken string
	expiry      time.Time
}

// New constructs a Client from an explicit config (used by tests).
func New(cfg Config) *Client {
	return &Client{
		cfg: cfg,
		hc:  logx.InstrumentClient("google", &http.Client{Timeout: 30 * time.Second}),
	}
}

// NewFromEnv constructs a Client from the environment, or an error when
// goon hasn't been connected to Google yet.
func NewFromEnv() (*Client, error) {
	cfg := ConfigFromEnv()
	if !cfg.Filled() {
		return nil, errors.New("google: not connected — run `goon google auth` (or set GOOGLE_OAUTH_CLIENT_ID/SECRET/REFRESH_TOKEN)")
	}
	return New(cfg), nil
}

// Configured reports whether Google credentials are present (used to
// decide whether to advertise the google_* chat tools).
func Configured() bool { return ConfigFromEnv().Filled() }

// token returns a valid access token, refreshing when needed. A 30s skew
// guard avoids using a token that's about to expire mid-request.
func (c *Client) token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.accessToken != "" && time.Now().Before(c.expiry.Add(-30*time.Second)) {
		return c.accessToken, nil
	}
	tr, err := refreshToken(ctx, c.hc, c.cfg)
	if err != nil {
		return "", err
	}
	c.accessToken = tr.AccessToken
	c.expiry = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	return c.accessToken, nil
}

// getJSON performs an authenticated GET and decodes the JSON body into out.
func (c *Client) getJSON(ctx context.Context, rawURL string, out any) error {
	tok, err := c.token(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("google GET: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("google API http %d: %s", resp.StatusCode, snippet(body))
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("google API decode: %w", err)
		}
	}
	return nil
}

// postJSON performs an authenticated POST with a JSON body and decodes the
// response into out. Used by Cloud Logging's entries:list.
func (c *Client) postJSON(ctx context.Context, rawURL string, in, out any) error {
	tok, err := c.token(ctx)
	if err != nil {
		return err
	}
	buf, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("google POST: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("google API http %d: %s", resp.StatusCode, snippet(body))
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("google API decode: %w", err)
		}
	}
	return nil
}

// PostJSON is the exported form of postJSON, for sibling packages (e.g.
// internal/gcplog) that reuse this client's OAuth token against another
// Google REST endpoint.
func (c *Client) PostJSON(ctx context.Context, rawURL string, in, out any) error {
	return c.postJSON(ctx, rawURL, in, out)
}

func snippet(b []byte) string {
	s := string(b)
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
