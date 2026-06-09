// Package envstore is the shared reader/writer for goon's config file at
// ./config.json (relative to the working directory, same level as workflow.json).
//
// Both the web UI and the Telegram bot persist user-edited settings through
// here so they don't drift apart.
//
// At boot, call LoadIntoEnv() once to inject all stored keys into the process
// environment so the rest of the codebase can continue using os.Getenv as
// usual. Keys already present in the environment (e.g. set by a test) are
// never overwritten. Empty-string values in config.json are skipped — they
// are treated as "not configured" and won't shadow anything.
package envstore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Path returns the absolute path to ./config.json.
// Set GOON_CONFIG_FILE to redirect (used in tests).
func Path() string {
	if v := strings.TrimSpace(os.Getenv("GOON_CONFIG_FILE")); v != "" {
		return v
	}
	if abs, err := filepath.Abs("config.json"); err == nil {
		return abs
	}
	return "config.json"
}

// Load reads config.json and returns all non-empty key/value pairs.
// Returns an empty map if the file is missing.
// Returns (emptyMap, err) — check err to distinguish "missing" from "bad JSON".
func Load() (map[string]string, error) {
	out := map[string]string{}
	data, err := os.ReadFile(Path())
	if err != nil {
		// File missing is normal on first run — not an error worth surfacing.
		return out, nil
	}
	raw := map[string]string{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return out, fmt.Errorf("config.json: invalid JSON: %w", err)
	}
	for k, v := range raw {
		// Skip blank values and internal comment keys (e.g. "_comment").
		if v != "" && !strings.HasPrefix(k, "_") {
			out[k] = v
		}
	}
	return out, nil
}

// LoadIntoEnv reads config.json and calls os.Setenv for every non-empty key
// that is not already set in the process environment. Returns an error if the
// file exists but contains invalid JSON (so callers can surface it early).
func LoadIntoEnv() error {
	m, err := Load()
	for k, v := range m {
		if _, exists := os.LookupEnv(k); !exists {
			_ = os.Setenv(k, v)
		}
	}
	return err
}

// Set writes (or replaces) key=value in config.json. Creates the file if
// missing. Atomic via tmp+rename.
func Set(key, value string) error {
	path := Path()
	if path == "" {
		return fmt.Errorf("envstore: cannot resolve config path")
	}
	m, _ := Load()
	m[key] = value
	return writeJSON(path, m)
}

// Unset removes key from config.json. No-op if file or key absent.
func Unset(key string) error {
	path := Path()
	if path == "" {
		return nil
	}
	m, _ := Load()
	if _, ok := m[key]; !ok {
		return nil
	}
	delete(m, key)
	return writeJSON(path, m)
}

func writeJSON(path string, m map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
