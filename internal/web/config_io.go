package web

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// configFilePath mirrors cmd/config.go: ~/.config/goon/.env, override via
// $XDG_CONFIG_HOME. Kept local to web/ so we don't import cmd/.
func configFilePath() string {
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		return filepath.Join(xdg, "goon", ".env")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "goon", ".env")
}

// setConfigKey writes (or replaces) KEY=VALUE in the config file.
func setConfigKey(key, value string) error {
	path := configFilePath()
	if path == "" {
		return fmt.Errorf("cannot resolve config path")
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

// unsetConfigKey removes KEY=... from the config file. No-op if missing.
func unsetConfigKey(key string) error {
	path := configFilePath()
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
