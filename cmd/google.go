package cmd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/harisaginting/goon/internal/envstore"
	"github.com/harisaginting/goon/internal/google"
)

// runGoogle handles `goon google <sub>`. Today the only sub is `auth`,
// which runs a one-time OAuth consent flow and saves the resulting
// refresh token to config.json so the chat agent can read your Google
// Workspace (Calendar, Tasks, Gmail) + Cloud Logging — read-only.
func runGoogle(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "auth", "":
		return runGoogleAuth(ctx, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown google subcommand %q (try: goon google auth)\n", sub)
		return fmt.Errorf("unknown subcommand %q", sub)
	}
}

// runGoogleAuth performs the OAuth authorization-code flow: it serves a
// localhost callback, opens (or prints) the consent URL, captures the
// code, exchanges it for a refresh token, and persists it.
func runGoogleAuth(ctx context.Context, stdout, stderr io.Writer) error {
	cfg := google.ConfigFromEnv()
	if !cfg.HasClient() {
		fmt.Fprintln(stderr, "Google OAuth client not set. First create an OAuth client (Desktop app) in Google Cloud, then:")
		fmt.Fprintln(stderr, "  goon config set GOOGLE_OAUTH_CLIENT_ID <id>")
		fmt.Fprintln(stderr, "  goon config set GOOGLE_OAUTH_CLIENT_SECRET <secret>")
		fmt.Fprintln(stderr, "  goon config set GOOGLE_CLOUD_PROJECT <project-id>   # for log search")
		return fmt.Errorf("GOOGLE_OAUTH_CLIENT_ID / GOOGLE_OAUTH_CLIENT_SECRET not set")
	}

	const addr = "127.0.0.1:8765"
	redirectURI := "http://" + addr + "/callback"
	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		if e := r.URL.Query().Get("error"); e != "" {
			http.Error(w, "authorization failed: "+e, http.StatusBadRequest)
			errCh <- fmt.Errorf("consent denied: %s", e)
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "no code in callback", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><body style="font-family:sans-serif;padding:40px"><h2>✓ goon is connected to Google.</h2><p>You can close this tab and return to your terminal.</p></body></html>`)
		codeCh <- code
	})
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	authURL := google.AuthCodeURL(cfg.ClientID, redirectURI, google.ReadonlyScopes)
	fmt.Fprintln(stdout, "Opening your browser to grant goon read-only access to Google Workspace + Cloud Logging…")
	fmt.Fprintln(stdout, "If it doesn't open, visit this URL:")
	fmt.Fprintln(stdout, "  "+authURL)
	openBrowser(authURL)

	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		return err
	case <-time.After(3 * time.Minute):
		return fmt.Errorf("timed out waiting for consent (3 min)")
	case <-ctx.Done():
		return ctx.Err()
	}

	tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	refresh, _, err := google.ExchangeCode(tctx, &http.Client{Timeout: 30 * time.Second}, cfg, code, redirectURI)
	if err != nil {
		return fmt.Errorf("exchange code: %w", err)
	}
	if strings.TrimSpace(refresh) == "" {
		return fmt.Errorf("Google did not return a refresh token — re-run; ensure the consent screen is in 'Testing'/published and you approved offline access")
	}
	if err := envstore.Set("GOOGLE_OAUTH_REFRESH_TOKEN", refresh); err != nil {
		return fmt.Errorf("save refresh token: %w", err)
	}
	fmt.Fprintln(stdout, "✓ Connected. goon can now read your Calendar, Tasks, Gmail, and Cloud Logging.")
	fmt.Fprintln(stdout, "  Try it in Chat: \"what meetings do I have today?\"")
	return nil
}

// openBrowser best-effort opens a URL in the default browser. Failure is
// fine — the URL is also printed for manual opening.
func openBrowser(url string) {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd, args = "open", []string{url}
	case "windows":
		cmd, args = "rundll32", []string{"url.dll,FileProtocolHandler", url}
	default:
		cmd, args = "xdg-open", []string{url}
	}
	_ = exec.Command(cmd, args...).Start()
}
