package cmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// configKey describes one environment variable goon understands.
type configKey struct {
	Name      string
	Default   string
	Sensitive bool
	Group     string
}

// knownConfigKeys is the canonical list shown by `goon config show`.
var knownConfigKeys = []configKey{
	{Name: "GOON_LLM_PROVIDER", Default: "openai", Group: "agent"},
	{Name: "GOON_MAX_STEPS", Default: "5", Group: "agent"},
	{Name: "GOON_MEMORY_PATH", Default: "~/.goon/memory.json", Group: "agent"},
	{Name: "GOON_UPSTREAM", Default: "https://github.com/harisaginting/goon", Group: "agent"},

	{Name: "GOON_BOARD", Default: "", Group: "daemon"},
	{Name: "GOON_GIT_HOST", Default: "", Group: "daemon"},
	{Name: "GOON_POLL_SECONDS", Default: "300", Group: "daemon"},
	{Name: "GOON_VERIFY_RUNS", Default: "3", Group: "daemon"},
	{Name: "GOON_REPO_MAP", Default: "", Group: "daemon"},
	{Name: "GOON_PID_FILE", Default: "~/.goon/goon.pid", Group: "daemon"},

	{Name: "OPENAI_API_KEY", Sensitive: true, Group: "openai"},
	{Name: "OPENAI_MODEL", Default: "gpt-4o-mini", Group: "openai"},
	{Name: "OPENAI_BASE_URL", Default: "https://api.openai.com/v1", Group: "openai"},

	{Name: "ANTHROPIC_API_KEY", Sensitive: true, Group: "anthropic"},
	{Name: "ANTHROPIC_MODEL", Default: "claude-sonnet-4-5", Group: "anthropic"},
	{Name: "ANTHROPIC_BASE_URL", Default: "https://api.anthropic.com/v1", Group: "anthropic"},

	{Name: "OLLAMA_BASE_URL", Default: "http://localhost:11434", Group: "ollama"},
	{Name: "OLLAMA_MODEL", Default: "llama3", Group: "ollama"},

	{Name: "JIRA_BASE_URL", Group: "jira"},
	{Name: "JIRA_EMAIL", Group: "jira"},
	{Name: "JIRA_API_TOKEN", Sensitive: true, Group: "jira"},
	{Name: "JIRA_JQL", Default: `assignee=currentUser() AND statusCategory!=Done`, Group: "jira"},

	{Name: "GITHUB_TOKEN", Sensitive: true, Group: "github"},
	{Name: "GITHUB_REPOS", Group: "github"},
	{Name: "GITHUB_LABEL", Group: "github"},
	{Name: "GITHUB_ASSIGNEE", Default: "@me", Group: "github"},
	{Name: "GITHUB_API_URL", Default: "https://api.github.com", Group: "github"},

	{Name: "GITLAB_TOKEN", Sensitive: true, Group: "gitlab"},
	{Name: "GITLAB_API_URL", Default: "https://gitlab.com/api/v4", Group: "gitlab"},

	{Name: "BITBUCKET_TOKEN", Sensitive: true, Group: "bitbucket"},
	{Name: "BITBUCKET_USERNAME", Group: "bitbucket"},
	{Name: "BITBUCKET_APP_PASSWORD", Sensitive: true, Group: "bitbucket"},
	{Name: "BITBUCKET_API_URL", Default: "https://api.bitbucket.org/2.0", Group: "bitbucket"},

	{Name: "CONFLUENCE_BASE_URL", Group: "confluence"},
	{Name: "CONFLUENCE_EMAIL", Group: "confluence"},
	{Name: "CONFLUENCE_API_TOKEN", Sensitive: true, Group: "confluence"},

	{Name: "TELEGRAM_BOT_TOKEN", Sensitive: true, Group: "telegram"},
	{Name: "TELEGRAM_CHAT_ID", Group: "telegram"},
	{Name: "TELEGRAM_API_BASE_URL", Default: "https://api.telegram.org", Group: "telegram"},
}

// runConfig dispatches the `goon config <action>` subsubcommand.
func runConfig(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	action := "show"
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		action = args[0]
		rest = args[1:]
	}

	switch action {
	case "show":
		return configShow(rest, stdout, stderr)
	case "get":
		return configGet(rest, stdout, stderr)
	case "set":
		return configSet(rest, stdout, stderr)
	case "unset":
		return configUnset(rest, stdout, stderr)
	case "path":
		fmt.Fprintln(stdout, configFilePath())
		return nil
	case "edit":
		return configEdit(ctx, stdout, stderr)
	case "help", "-h", "--help":
		printConfigHelp(stdout)
		return nil
	default:
		printConfigHelp(stderr)
		return fmt.Errorf("unknown config action %q", action)
	}
}

func printConfigHelp(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  goon config                  # show all config (secrets masked)")
	fmt.Fprintln(w, "  goon config show [--reveal]  # show all config (--reveal prints secrets)")
	fmt.Fprintln(w, "  goon config get <KEY>        # print one value")
	fmt.Fprintln(w, "  goon config set <KEY> <VAL>  # write to ~/.config/goon/.env")
	fmt.Fprintln(w, "  goon config set KEY=VAL      # KEY=VAL form also accepted")
	fmt.Fprintln(w, "  goon config unset <KEY>      # remove from config file")
	fmt.Fprintln(w, "  goon config path             # print path to config file")
	fmt.Fprintln(w, "  goon config edit             # open config file in $EDITOR")
}

// configShow prints all known + unknown GOON_*-style env keys.
func configShow(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("config show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	reveal := fs.Bool("reveal", false, "print secret values verbatim")
	if err := fs.Parse(args); err != nil {
		return err
	}

	fileVals := readConfigFile(configFilePath())

	groups := map[string][]configKey{}
	order := []string{}
	for _, k := range knownConfigKeys {
		if _, ok := groups[k.Group]; !ok {
			order = append(order, k.Group)
		}
		groups[k.Group] = append(groups[k.Group], k)
	}

	fmt.Fprintf(stdout, "config file: %s\n", configFilePath())
	fmt.Fprintf(stdout, "(env vars in shell take precedence over config file)\n\n")

	for _, g := range order {
		fmt.Fprintf(stdout, "[%s]\n", g)
		for _, k := range groups[g] {
			value, source := resolveValue(k, fileVals)
			display := value
			if k.Sensitive && !*reveal {
				display = mask(value)
			}
			if value == "" {
				display = "(unset)"
				if k.Default != "" {
					display = fmt.Sprintf("(default: %s)", k.Default)
				}
			}
			fmt.Fprintf(stdout, "  %-22s = %s    [%s]\n", k.Name, display, source)
		}
		fmt.Fprintln(stdout)
	}
	return nil
}

func configGet(args []string, stdout, _ io.Writer) error {
	if len(args) != 1 {
		return errors.New("usage: goon config get <KEY>")
	}
	key := strings.ToUpper(strings.TrimSpace(args[0]))
	val := os.Getenv(key)
	if val == "" {
		val = readConfigFile(configFilePath())[key]
	}
	fmt.Fprintln(stdout, val)
	return nil
}

func configSet(args []string, stdout, _ io.Writer) error {
	var key, value string
	switch {
	case len(args) == 1 && strings.Contains(args[0], "="):
		eq := strings.IndexByte(args[0], '=')
		key = strings.TrimSpace(args[0][:eq])
		value = strings.TrimSpace(args[0][eq+1:])
	case len(args) == 2:
		key = strings.TrimSpace(args[0])
		value = strings.TrimSpace(args[1])
	default:
		return errors.New("usage: goon config set <KEY> <VALUE>  (or KEY=VALUE)")
	}
	key = strings.ToUpper(key)
	if key == "" {
		return errors.New("config: empty key")
	}
	path := configFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := writeConfigKey(path, key, value); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "set %s in %s\n", key, path)
	return nil
}

func configUnset(args []string, stdout, _ io.Writer) error {
	if len(args) != 1 {
		return errors.New("usage: goon config unset <KEY>")
	}
	key := strings.ToUpper(strings.TrimSpace(args[0]))
	path := configFilePath()
	if err := removeConfigKey(path, key); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "unset %s from %s\n", key, path)
	return nil
}

func configEdit(ctx context.Context, stdout, stderr io.Writer) error {
	path := configFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if !exists(path) {
		if err := os.WriteFile(path, []byte("# goon config\n"), 0o600); err != nil {
			return err
		}
	}
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = os.Getenv("VISUAL")
	}
	if editor == "" {
		editor = "vi"
	}
	cmd := exec.CommandContext(ctx, editor, path)
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// configFilePath returns ~/.config/goon/.env (or $XDG_CONFIG_HOME/goon/.env).
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

// resolveValue returns (value, source) for a known key. Source is one of
// "shell", "config-file", "default", or "unset".
func resolveValue(k configKey, fileVals map[string]string) (string, string) {
	if v := os.Getenv(k.Name); v != "" {
		// Differentiate between shell-set and config-file-set when the file
		// contains the same value.
		if fv, ok := fileVals[k.Name]; ok && fv == v {
			return v, "config-file"
		}
		return v, "shell"
	}
	if v, ok := fileVals[k.Name]; ok && v != "" {
		return v, "config-file"
	}
	if k.Default != "" {
		return "", "default"
	}
	return "", "unset"
}

func mask(v string) string {
	if v == "" {
		return ""
	}
	if len(v) <= 6 {
		return "***"
	}
	return v[:2] + "…" + v[len(v)-3:]
}

// readConfigFile parses a .env-style file and returns its key/value pairs.
// Returns an empty map if the file is missing.
func readConfigFile(path string) map[string]string {
	out := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		k := strings.TrimSpace(line[:eq])
		v := strings.Trim(strings.TrimSpace(line[eq+1:]), `"'`)
		out[k] = v
	}
	return out
}

// writeConfigKey replaces or appends key=value in path, atomically.
func writeConfigKey(path, key, value string) error {
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
	return atomicWrite(path, strings.Join(out, "\n")+"\n", 0o600)
}

// removeConfigKey deletes key from the .env file. No-op if file or key absent.
func removeConfigKey(path, key string) error {
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
			continue // drop
		}
		out = append(out, line)
	}
	return atomicWrite(path, strings.Join(out, "\n")+"\n", 0o600)
}

func atomicWrite(path, content string, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// sortedKnownKeys is a small helper used by tests; keeps the ordering
// deterministic when iterating the config table.
func sortedKnownKeys() []string {
	out := make([]string, 0, len(knownConfigKeys))
	for _, k := range knownConfigKeys {
		out = append(out, k.Name)
	}
	sort.Strings(out)
	return out
}
