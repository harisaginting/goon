// Package google is a zero-dependency client for the Google APIs goon
// uses (Calendar, Tasks, Gmail, Cloud Logging). It hand-rolls OAuth2 and
// the REST calls with net/http + encoding/json — no third-party SDKs, in
// keeping with goon's stdlib-only rule. One OAuth refresh token (multiple
// read-only scopes) backs every service.
package google

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

const (
	authEndpoint  = "https://accounts.google.com/o/oauth2/v2/auth"
	tokenEndpoint = "https://oauth2.googleapis.com/token"
)

// ReadonlyScopes are the OAuth scopes goon requests in the read-only
// phase: calendar, tasks, gmail, and Cloud Logging. Adding write scopes
// later is a deliberate, separate opt-in.
var ReadonlyScopes = []string{
	"https://www.googleapis.com/auth/calendar.readonly",
	"https://www.googleapis.com/auth/tasks.readonly",
	"https://www.googleapis.com/auth/gmail.readonly",
	"https://www.googleapis.com/auth/logging.read",
}

// PersonalScopes are the minimal read-only scopes for personal Gmail +
// Calendar access. Used by the web UI "Connect Google" button — narrower
// than ReadonlyScopes (no Tasks or Cloud Logging, both of which require
// extra Google Cloud setup). Upgrade to ReadonlyScopes when log search
// is needed.
var PersonalScopes = []string{
	"https://www.googleapis.com/auth/gmail.readonly",
	"https://www.googleapis.com/auth/calendar.readonly",
}

// Config holds the OAuth client credentials + the long-lived refresh
// token (obtained once via `goon google auth`).
type Config struct {
	ClientID     string
	ClientSecret string
	RefreshToken string
}

// ConfigFromEnv reads the OAuth config from the environment.
func ConfigFromEnv() Config {
	return Config{
		ClientID:     strings.TrimSpace(os.Getenv("GOOGLE_OAUTH_CLIENT_ID")),
		ClientSecret: strings.TrimSpace(os.Getenv("GOOGLE_OAUTH_CLIENT_SECRET")),
		RefreshToken: strings.TrimSpace(os.Getenv("GOOGLE_OAUTH_REFRESH_TOKEN")),
	}
}

// Filled reports whether all three credentials are present.
func (c Config) Filled() bool {
	return c.ClientID != "" && c.ClientSecret != "" && c.RefreshToken != ""
}

// HasClient reports whether just the OAuth client (id+secret) is set —
// enough to start the `goon google auth` consent flow.
func (c Config) HasClient() bool {
	return c.ClientID != "" && c.ClientSecret != ""
}

// AuthCodeURL builds the consent URL the user opens to grant access.
// access_type=offline + prompt=consent guarantees a refresh token.
func AuthCodeURL(clientID, redirectURI string, scopes []string) string {
	v := url.Values{}
	v.Set("client_id", clientID)
	v.Set("redirect_uri", redirectURI)
	v.Set("response_type", "code")
	v.Set("scope", strings.Join(scopes, " "))
	v.Set("access_type", "offline")
	v.Set("prompt", "consent")
	v.Set("include_granted_scopes", "true")
	return authEndpoint + "?" + v.Encode()
}

// tokenResponse is the shared shape of both token endpoints.
type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int    `json:"expires_in"`
	TokenType        string `json:"token_type"`
	Scope            string `json:"scope"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// ExchangeCode swaps an authorization code (from the consent redirect)
// for tokens, returning (refreshToken, accessToken). `goon google auth`
// persists the refresh token.
func ExchangeCode(ctx context.Context, hc *http.Client, cfg Config, code, redirectURI string) (refresh, access string, err error) {
	form := url.Values{}
	form.Set("code", code)
	form.Set("client_id", cfg.ClientID)
	form.Set("client_secret", cfg.ClientSecret)
	form.Set("redirect_uri", redirectURI)
	form.Set("grant_type", "authorization_code")
	tr, e := postToken(ctx, hc, form)
	if e != nil {
		return "", "", e
	}
	return tr.RefreshToken, tr.AccessToken, nil
}

// refreshToken swaps the refresh token for a fresh access token.
func refreshToken(ctx context.Context, hc *http.Client, cfg Config) (tokenResponse, error) {
	form := url.Values{}
	form.Set("client_id", cfg.ClientID)
	form.Set("client_secret", cfg.ClientSecret)
	form.Set("refresh_token", cfg.RefreshToken)
	form.Set("grant_type", "refresh_token")
	return postToken(ctx, hc, form)
}

func postToken(ctx context.Context, hc *http.Client, form url.Values) (tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req)
	if err != nil {
		return tokenResponse{}, fmt.Errorf("google token request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return tokenResponse{}, fmt.Errorf("google token decode (http %d): %w", resp.StatusCode, err)
	}
	if tr.Error != "" {
		return tokenResponse{}, fmt.Errorf("google token error: %s — %s", tr.Error, tr.ErrorDescription)
	}
	if resp.StatusCode != http.StatusOK || tr.AccessToken == "" {
		return tokenResponse{}, fmt.Errorf("google token: unexpected response (http %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return tr, nil
}
