// Package envstore is the shared writer for goon's user-config .env
// file at ~/.config/goon/.env (override via $XDG_CONFIG_HOME). Both
// the web UI and the Telegram bot persist user-edited env settings
// through here so they don't drift apart.
package envstore

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Path returns the resolved config file path. Empty on environments
// without HOME and no XDG override (very rare). Callers should treat
// "" as a hard error.
func Path() string {
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		return filepath.Join(xdg, "goon", ".env")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "goon", ".env")
}

// Set writes (or replaces) KEY=VALUE in the config file. Creates the
// parent directory if missing. Atomic via tmp + rename. Preserves
// comments and blank lines.
func Set(key, value string) error {
	path := Path()
	if path == "" {
		return fmt.Errorf("envstore: cannot resolve config path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	lines := []string{}
	if data, err := os.ReadFile(path); err == nil {
		lines = strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	}
	out := make([]string, 0, len(lines)+1)
	found := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			out = append(out, line)
			continue
		}
		eq := strings.IndexByte(trimmed, '=')
		if eq > 0 && strings.TrimSpace(trimmed[:eq]) == key {
			out = append(out, fmt.Sprintf("%s=%s", key, value))
			found = true
			continue
		}
		out = append(out, line)
	}
	if !found {
		out = append(out, fmt.Sprintf("%s=%s", key, value))
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(out, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Unset removes KEY=... from the config file. No-op if file or key
// is missing.
func Unset(key string) error {
	path := Path()
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			out = append(out, line)
			continue
		}
		eq := strings.IndexByte(trimmed, '=')
		if eq > 0 && strings.TrimSpace(trimmed[:eq]) == key {
			continue
		}
		out = append(out, line)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(out, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
