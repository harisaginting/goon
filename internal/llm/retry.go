package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// httpTimeout is the per-request HTTP timeout shared by every LLM adapter.
// Large coding prompts on slow models/proxies routinely take longer than
// 30s to even return response headers (the cause of "context deadline
// exceeded while awaiting headers"), so the default is generous. Override
// with GOON_LLM_HTTP_TIMEOUT_SEC (seconds, 1–600).
func httpTimeout() time.Duration {
	if v := strings.TrimSpace(os.Getenv("GOON_LLM_HTTP_TIMEOUT_SEC")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 600 {
			return time.Duration(n) * time.Second
		}
	}
	return 120 * time.Second
}

// retryDefaults centralises the exponential-backoff parameters every LLM
// adapter uses. Keep them in one place so we can tune resilience uniformly.
const (
	retryBaseDelay = 500 * time.Millisecond
	retryMaxDelay  = 8 * time.Second
)

// retryableError wraps an underlying error to signal that doWithRetry should
// retry the request. Non-wrapped errors are treated as terminal.
type retryableError struct{ err error }

// Error implements the error interface.
func (e *retryableError) Error() string { return e.err.Error() }

// Unwrap exposes the underlying error to errors.Is / errors.As.
func (e *retryableError) Unwrap() error { return e.err }

// isRetryable reports whether err was tagged as retryable by the helper.
func isRetryable(err error) bool {
	var r *retryableError
	return errors.As(err, &r)
}

// shouldRetryStatus returns true for HTTP status codes worth retrying:
// 429 (rate limit) and the standard transient 5xx family.
func shouldRetryStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	}
	return false
}

// backoffFor returns the delay before retry attempt i (0-indexed), using
// exponential growth capped at retryMaxDelay plus a small jitter to avoid
// thundering-herd retries from many concurrent callers.
func backoffFor(attempt int) time.Duration {
	d := retryBaseDelay << attempt
	if d <= 0 || d > retryMaxDelay {
		d = retryMaxDelay
	}
	// Up to 25% jitter on top of the base delay.
	jitter := time.Duration(rand.Int63n(int64(d) / 4))
	return d + jitter
}

// doWithRetry executes the given request factory with bounded retries and
// exponential backoff. The req factory is called once per attempt so that
// callers can build a fresh *http.Request (and re-seek any body) each time.
//
// The helper retries on transport errors and on 429/500/502/503/504 responses
// up to maxAttempts times. Any other non-2xx response is surfaced verbatim so
// callers can interpret 4xx errors. Context cancellation is honoured
// immediately — no further sleeps or attempts after ctx.Err().
//
// On a retryable HTTP response the body is fully drained and closed before
// the next attempt; the returned response (on success or terminal failure)
// is left untouched for the caller to handle.
func doWithRetry(ctx context.Context, client *http.Client, req func() (*http.Request, error), maxAttempts int) (*http.Response, error) {
	if maxAttempts <= 0 {
		maxAttempts = 1
	}
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoffFor(attempt - 1)):
			}
		}
		r, err := req()
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		resp, err := client.Do(r)
		if err != nil {
			// Network / transport errors are retryable until ctx is cancelled.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			lastErr = &retryableError{err: err}
			continue
		}
		if shouldRetryStatus(resp.StatusCode) {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			lastErr = &retryableError{err: fmt.Errorf("http %d: %s", resp.StatusCode, string(body))}
			continue
		}
		return resp, nil
	}
	if lastErr == nil {
		lastErr = errors.New("retry exhausted with no error captured")
	}
	return nil, lastErr
}
