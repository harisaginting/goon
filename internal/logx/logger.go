// Package logx is goon's structured logging facility.
//
// Everything goon does that touches the outside world or makes a non-trivial
// decision should land in here: every LLM prompt + response, every tool
// invocation with its args + result, every HTTP request to Jira / GitHub /
// Telegram / Confluence, every workflow phase transition, every daemon
// poll. The same stream is mirrored to stderr (so you see what's happening
// in real time) AND appended to a rotating file (so you have history).
//
// # Where the log goes
//
// By default: ~/.goon/logs/goon.log (override with GOON_LOG_FILE).
// Files rotate when they exceed 10 MB, keeping the 3 most recent rotations
// (goon.log, goon.log.1, goon.log.2, goon.log.3 — oldest is dropped).
//
// Set GOON_LOG_LEVEL=debug to capture full request/response bodies (truncated
// to 4 KB). Default level is "info".
//
// Set GOON_LOG_FORMAT=json to emit JSON Lines instead of the human-readable
// key=value text format. Useful when piping into jq, vector, loki, etc.
//
// # Usage from goon code
//
// Treat logx like the slog stdlib package:
//
//	logx.Info("workflow.phase", "phase", "triage", "ticket", t.Key)
//	logx.Debug("llm.request", "provider", "openai", "tokens", 1234)
//	logx.Error("api.fail", "endpoint", "/messages", "err", err)
//
// The default logger is initialized lazily on first use, so even tests get
// a working sink without explicit setup.
package logx

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Defaults — keep these synced with .env.example documentation.
const (
	DefaultMaxBytes = 10 * 1024 * 1024 // 10 MB before rotation
	DefaultKeep     = 3                // keep .1 .2 .3 rotations
)

// Default log file path resolution. Env vars win:
//   - GOON_LOG_FILE: absolute or ~-prefixed path
//   - else: ~/.goon/logs/goon.log
//   - if HOME isn't resolvable: ./.goon/logs/goon.log (so even containers work)
func defaultLogPath() string {
	if v := strings.TrimSpace(os.Getenv("GOON_LOG_FILE")); v != "" {
		return expandTilde(v)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".goon", "logs", "goon.log")
	}
	return filepath.Join(home, ".goon", "logs", "goon.log")
}

func expandTilde(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~"))
}

// parseLevel turns a string into an slog.Level. Unknown values fall back to info.
func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error", "err":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Config tunes a Logger at construction time. Zero-value defaults are used
// for anything left empty.
type Config struct {
	// Path is the on-disk log file. Empty → defaultLogPath().
	Path string
	// Level is the minimum severity captured. "" → info.
	Level string
	// Format is "text" (default, human-readable) or "json" (newline-delimited).
	Format string
	// MaxBytes is the rotation threshold. 0 → DefaultMaxBytes.
	MaxBytes int64
	// Keep is the number of rotations to retain. 0 → DefaultKeep.
	Keep int
	// AlsoStderr mirrors every record to stderr in addition to the file.
	// Default true. Set false when you want quiet operation.
	AlsoStderr *bool
}

// boolPtr is a tiny helper for AlsoStderr.
func boolPtr(b bool) *bool { return &b }

// Logger is goon's wrapper around slog.Logger. Use the package-level
// helpers (Info, Debug, …) for normal logging; construct one explicitly
// only when you need a sink with a different file or level.
type Logger struct {
	mu     sync.Mutex
	cfg    Config
	file   *rotatingFile
	slog   *slog.Logger
	closed bool
}

// New builds a Logger with the given config. Errors only when the log
// directory can't be created. After construction the logger is safe for
// concurrent use.
func New(cfg Config) (*Logger, error) {
	if cfg.Path == "" {
		cfg.Path = defaultLogPath()
	}
	if cfg.Level == "" {
		cfg.Level = strings.TrimSpace(os.Getenv("GOON_LOG_LEVEL"))
	}
	if cfg.Format == "" {
		cfg.Format = strings.TrimSpace(os.Getenv("GOON_LOG_FORMAT"))
		if cfg.Format == "" {
			cfg.Format = "text"
		}
	}
	if cfg.MaxBytes == 0 {
		cfg.MaxBytes = DefaultMaxBytes
	}
	if cfg.Keep == 0 {
		cfg.Keep = DefaultKeep
	}
	if cfg.AlsoStderr == nil {
		cfg.AlsoStderr = boolPtr(true)
	}

	if err := os.MkdirAll(filepath.Dir(cfg.Path), 0o755); err != nil {
		return nil, fmt.Errorf("logx: mkdir: %w", err)
	}
	rf, err := newRotatingFile(cfg.Path, cfg.MaxBytes, cfg.Keep)
	if err != nil {
		return nil, fmt.Errorf("logx: open: %w", err)
	}

	var sinks []io.Writer = []io.Writer{rf}
	if *cfg.AlsoStderr {
		sinks = append(sinks, os.Stderr)
	}
	w := io.MultiWriter(sinks...)

	opts := &slog.HandlerOptions{Level: parseLevel(cfg.Level)}
	var h slog.Handler
	switch strings.ToLower(cfg.Format) {
	case "json":
		h = slog.NewJSONHandler(w, opts)
	default:
		h = slog.NewTextHandler(w, opts)
	}
	return &Logger{cfg: cfg, file: rf, slog: slog.New(h)}, nil
}

// Close flushes and closes the underlying file. Safe to call multiple times.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.file == nil {
		return nil
	}
	l.closed = true
	return l.file.Close()
}

// Path returns the on-disk log file path.
func (l *Logger) Path() string { return l.cfg.Path }

// Level returns the minimum captured severity.
func (l *Logger) Level() string { return l.cfg.Level }

// Slog returns the underlying *slog.Logger so callers can build sub-loggers
// with per-component attrs via .With(...).
func (l *Logger) Slog() *slog.Logger { return l.slog }

// Convenience methods. These mirror slog's signature: alternating key/value
// args after the message.
func (l *Logger) Debug(msg string, args ...any) { l.slog.Debug(msg, args...) }
func (l *Logger) Info(msg string, args ...any)  { l.slog.Info(msg, args...) }
func (l *Logger) Warn(msg string, args ...any)  { l.slog.Warn(msg, args...) }
func (l *Logger) Error(msg string, args ...any) { l.slog.Error(msg, args...) }

// --- Package-level default ------------------------------------------------

var (
	defaultMu sync.Mutex
	defaultLg *Logger
)

// SetDefault installs lg as the package-level logger used by Info/Debug/etc.
// Pass nil to reset (the next call to a package-level helper will construct
// a fresh default from env vars).
func SetDefault(lg *Logger) {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	defaultLg = lg
}

// Default returns the package-level logger, constructing one lazily from
// env vars if none has been installed. The lazy constructor never fails —
// if file creation errors, it falls back to a stderr-only logger so callers
// are never blocked.
func Default() *Logger {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	if defaultLg != nil {
		return defaultLg
	}
	lg, err := New(Config{})
	if err != nil {
		// Fall back to a stderr-only sink so logging never blocks startup.
		opts := &slog.HandlerOptions{Level: parseLevel(os.Getenv("GOON_LOG_LEVEL"))}
		stderrOnly := &Logger{
			cfg:  Config{Path: "(stderr-only — file open failed: " + err.Error() + ")"},
			slog: slog.New(slog.NewTextHandler(os.Stderr, opts)),
		}
		defaultLg = stderrOnly
		return stderrOnly
	}
	defaultLg = lg
	return lg
}

// Package-level convenience — most callers want these.

// Debug logs at debug level via the package default.
func Debug(msg string, args ...any) { Default().Debug(msg, args...) }

// Info logs at info level via the package default.
func Info(msg string, args ...any) { Default().Info(msg, args...) }

// Warn logs at warn level via the package default.
func Warn(msg string, args ...any) { Default().Warn(msg, args...) }

// Error logs at error level via the package default.
func Error(msg string, args ...any) { Default().Error(msg, args...) }

// With returns a logger with the given attrs pre-bound to every message,
// useful for component tags: `logx.With("component", "agent")`.
func With(args ...any) *slog.Logger { return Default().slog.With(args...) }
