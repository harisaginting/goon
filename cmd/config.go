package cmd

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/harisaginting/goon/internal/envstore"
)

// configKey describes one setting goon understands.
type configKey struct {
	Name      string
	Default   string
	Sensitive bool
	Group     string
}

// knownConfigKeys is the canonical list shown by `goon config show`.
var knownConfigKeys = []configKey{
	{Name: "GOON_LLM_PROVIDER", Default: "openai", Group: "agent"},
	// Per-role model routing. Each is "provider:model" | "provider" |
	// "model"; empty inherits GOON_LLM_PROVIDER. Lets you run e.g. a strong
	// model for code and a cheap one for chat when several are configured.
	{Name: "GOON_LLM_CHAT", Group: "agent"},
	{Name: "GOON_LLM_PLAN", Group: "agent"},
	{Name: "GOON_LLM_CODE", Group: "agent"},
	{Name: "GOON_LLM_REVIEW", Group: "agent"},
	{Name: "GOON_MAX_STEPS", Default: "5", Group: "agent"},
	{Name: "GOON_STORAGE_DIR", Default: "./storage", Group: "agent"},
	{Name: "GOON_MEMORY_PATH", Default: "$GOON_STORAGE_DIR/memory.json", Group: "agent"},
	{Name: "GOON_MEMORY_DIR", Default: "$GOON_STORAGE_DIR/memory", Group: "agent"},
	{Name: "GOON_UPSTREAM", Default: "https://github.com/harisaginting/goon", Group: "agent"},
	{Name: "GOON_WORKSPACE_DIR", Group: "agent"},

	{Name: "GOON_BOARD", Default: "", Group: "daemon"},
	{Name: "GOON_GIT_HOST", Default: "", Group: "daemon"},
	{Name: "GOON_POLL_SECONDS", Default: "300", Group: "daemon"},
	{Name: "GOON_VERIFY_RUNS", Default: "3", Group: "daemon"},
	{Name: "GOON_PID_FILE", Default: "$GOON_STORAGE_DIR/goon.pid", Group: "daemon"},
	{Name: "GOON_LOG_FILE", Default: "$GOON_STORAGE_DIR/logs/goon.log", Group: "daemon"},
	{Name: "GOON_WORKFLOW_FILE", Default: "./workflow.json", Group: "daemon"},
	// GOON_TICKET_STATUSES controls which canonical goon statuses the daemon
	// picks up from the board on each poll. Comma-separated list of:
	// open, in_progress, in_review, blocked, done.
	{Name: "GOON_TICKET_STATUSES", Default: "open,in_progress", Group: "daemon"},
	// GOON_DAEMON_AUTO_START controls whether the daemon's poll loop starts
	// active (true, default) or paused (false). Set to "false" if you want to
	// open the web UI and review configuration before the daemon begins polling.
	{Name: "GOON_DAEMON_AUTO_START", Default: "true", Group: "daemon"},
	// GOON_AUTO_LEARN toggles goon's self-learning (post-run distillation +
	// the daily standby reflection). On unless set to off/false/0/no.
	{Name: "GOON_AUTO_LEARN", Default: "true", Group: "daemon"},
	// GOON_LEARN_INTERVAL_HOURS throttles how often standby self-learning runs
	// while idle. Positive integer hours; default 24 (once per idle day).
	{Name: "GOON_LEARN_INTERVAL_HOURS", Default: "24", Group: "daemon"},
	// GOON_AUTO_CONFIRM_REPO skips the confirm_repo gate when triage
	// produced a single repo that resolves to a REPOSITORY.md entry.
	// Off by default. Narrower than GOON_AUTO_APPROVE (which skips all gates).
	{Name: "GOON_AUTO_CONFIRM_REPO", Default: "", Group: "daemon"},
	// GOON_AUTO_APPROVE_PLAN auto-accepts the plan (keeps the confirm_repo
	// gate) so the only human actions are: set repo + review the PR.
	{Name: "GOON_AUTO_APPROVE_PLAN", Default: "", Group: "daemon"},
	// GOON_LLM_HTTP_TIMEOUT_SEC bounds each LLM HTTP request. Default 120s;
	// raise for slow proxies/models that take >30s to return headers.
	{Name: "GOON_LLM_HTTP_TIMEOUT_SEC", Default: "120", Group: "daemon"},

	// Google Workspace (read-only) — Calendar, Tasks, Gmail + Cloud Logging.
	// Run `goon google auth` to obtain the refresh token after setting the
	// OAuth client id/secret.
	{Name: "GOOGLE_OAUTH_CLIENT_ID", Group: "google"},
	{Name: "GOOGLE_OAUTH_CLIENT_SECRET", Sensitive: true, Group: "google"},
	{Name: "GOOGLE_OAUTH_REFRESH_TOKEN", Sensitive: true, Group: "google"},
	{Name: "GOOGLE_CLOUD_PROJECT", Group: "google"},

	{Name: "OPENAI_API_KEY", Sensitive: true, Group: "openai"},
	{Name: "OPENAI_MODEL", Default: "gpt-4o-mini", Group: "openai"},
	{Name: "OPENAI_BASE_URL", Default: "https://api.openai.com/v1", Group: "openai"},

	{Name: "ANTHROPIC_API_KEY", Sensitive: true, Group: "anthropic"},
	{Name: "ANTHROPIC_MODEL", Default: "claude-sonnet-4-5", Group: "anthropic"},
	{Name: "ANTHROPIC_BASE_URL", Default: "https://api.anthropic.com/v1", Group: "anthropic"},

	{Name: "OLLAMA_BASE_URL", Default: "http://localhost:11434", Group: "ollama"},
	{Name: "OLLAMA_MODEL", Default: "llama3", Group: "ollama"},

	{Name: "GEMINI_API_KEY", Sensitive: true, Group: "gemini"},
	{Name: "GEMINI_MODEL", Default: "gemini-2.5-flash", Group: "gemini"},
	{Name: "GEMINI_BASE_URL", Default: "https://generativelanguage.googleapis.com/v1beta", Group: "gemini"},

	// Shared Atlassian credentials. Used as fallback by both Jira and
	// Confluence so a typical Cloud user only fills these three.
	{Name: "ATLASSIAN_BASE_URL", Group: "atlassian"},
	{Name: "ATLASSIAN_EMAIL", Group: "atlassian"},
	{Name: "ATLASSIAN_API_TOKEN", Sensitive: true, Group: "atlassian"},

	{Name: "JIRA_BASE_URL", Group: "jira"},
	{Name: "JIRA_EMAIL", Group: "jira"},
	{Name: "JIRA_API_TOKEN", Sensitive: true, Group: "jira"},
	{Name: "JIRA_JQL", Default: `assignee=currentUser() AND statusCategory!=Done`, Group: "jira"},

	{Name: "GITHUB_TOKEN", Sensitive: true, Group: "github"},
	{Name: "GITHUB_REPOS", Group: "github"},
	{Name: "GITHUB_LABEL", Group: "github"},
	{Name: "GITHUB_ASSIGNEE", Default: "@me", Group: "github"},
	{Name: "GITHUB_API_URL", Default: "https://api.github.com", Group: "github"},
	// GITHUB_STATE filters GitHub Issues by state. Accepted values: open,
	// closed, all. Defaults to "open".
	{Name: "GITHUB_STATE", Default: "open", Group: "github"},

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

	// Obsidian vault integration.
	{Name: "GOON_OBSIDIAN_VAULT", Group: "obsidian"},
	{Name: "GOON_OBSIDIAN_REPO", Group: "obsidian"},
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
	fmt.Fprintln(w, "  goon config get <KEY>        # print one value from config.json")
	fmt.Fprintln(w, "  goon config set <KEY> <VAL>  # write to ./config.json")
	fmt.Fprintln(w, "  goon config set KEY=VAL      # KEY=VAL form also accepted")
	fmt.Fprintln(w, "  goon config unset <KEY>      # remove from config.json")
	fmt.Fprintln(w, "  goon config path             # print path to config.json")
	fmt.Fprintln(w, "  goon config edit             # open config.json in $EDITOR")
}

// configShow prints all known keys with their values and source.
func configShow(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("config show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	reveal := fs.Bool("reveal", false, "print secret values verbatim")
	if err := fs.Parse(args); err != nil {
		return err
	}

	fileVals, _ := envstore.Load()

	groups := map[string][]configKey{}
	order := []string{}
	for _, k := range knownConfigKeys {
		if _, ok := groups[k.Group]; !ok {
			order = append(order, k.Group)
		}
		groups[k.Group] = append(groups[k.Group], k)
	}

	fmt.Fprintf(stdout, "config file: %s\n\n", configFilePath())

	for _, g := range order {
		fmt.Fprintf(stdout, "[%s]\n", g)
		for _, k := range groups[g] {
			value, source := resolveValue(k, fileVals)
			display := value
			if k.Sensitive && !*reveal {
				display = mask(value)
			}
			if display == "" {
				display = "(unset)"
			}
			fmt.Fprintf(stdout, "  %-22s = %s    [%s]\n", k.Name, display, source)
		}
		fmt.Fprintln(stdout)
	}
	return nil
}

func configGet(args []string, stdout, _ io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: goon config get <KEY>")
	}
	key := strings.ToUpper(strings.TrimSpace(args[0]))
	m, _ := envstore.Load()
	val := m[key]
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
		return fmt.Errorf("usage: goon config set <KEY> <VALUE>  (or KEY=VALUE)")
	}
	key = strings.ToUpper(key)
	if key == "" {
		return fmt.Errorf("config: empty key")
	}
	if err := envstore.Set(key, value); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "set %s in %s\n", key, configFilePath())
	return nil
}

func configUnset(args []string, stdout, _ io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: goon config unset <KEY>")
	}
	key := strings.ToUpper(strings.TrimSpace(args[0]))
	if err := envstore.Unset(key); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "unset %s from %s\n", key, configFilePath())
	return nil
}

func configEdit(ctx context.Context, stdout, stderr io.Writer) error {
	path := configFilePath()
	// Seed an empty JSON object if the file doesn't exist yet.
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := envstore.Set("_placeholder", ""); err == nil {
			_ = envstore.Unset("_placeholder")
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

// configFilePath returns the path to ./config.json.
func configFilePath() string { return envstore.Path() }

// resolveValue returns (value, source) for a known key.
// Source is one of "config-file", "default", or "unset".
func resolveValue(k configKey, fileVals map[string]string) (string, string) {
	if v, ok := fileVals[k.Name]; ok && v != "" {
		return v, "config-file"
	}
	if k.Default != "" {
		return k.Default, "default"
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
