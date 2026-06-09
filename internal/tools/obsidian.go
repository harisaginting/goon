package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/harisaginting/goon/internal/logx"
	"github.com/harisaginting/goon/internal/notes"
)

// obsidianTool is shared state for every obsidian_* tool. All tools point at
// the same vault directory and share one lazy-initialised notes.Store.
//
// Config env vars:
//
//	GOON_OBSIDIAN_VAULT — absolute or relative path to the vault root.
//	                      Supports leading ~/ expansion.
//	GOON_OBSIDIAN_REPO  — optional git remote URL. On first use (and on
//	                       obsidian_sync calls) goon runs "git pull" so a
//	                       git-backed vault (e.g. Obsidian Git plugin) stays
//	                       current without restarting the process.
type obsidianTool struct {
	mu      sync.Mutex
	store   *notes.Store // nil until first successful init
	vault   string       // resolved vault path (empty = not configured)
	initted bool         // true after first init attempt
}

// globalObsidian is the singleton shared by all obsidian_* tools.
var globalObsidian = &obsidianTool{}

// obsidianInit opens the vault on first call. Subsequent calls are no-ops
// unless force=true (used by ObsidianSync to reload after a git pull).
func (o *obsidianTool) obsidianInit(force bool) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.initted && !force {
		return
	}

	vaultDir := strings.TrimSpace(os.Getenv("GOON_OBSIDIAN_VAULT"))
	if vaultDir == "" {
		return // not configured; don't cache so we retry if env var is set later
	}

	// Expand ~/ (stdlib doesn't do it).
	if strings.HasPrefix(vaultDir, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			vaultDir = filepath.Join(home, vaultDir[2:])
		}
	}
	o.vault = vaultDir

	s, err := notes.New(vaultDir)
	if err != nil {
		logx.Warn("obsidian.store_init_failed", "vault", vaultDir, "error", err.Error())
	} else {
		o.store = s
		logx.Info("obsidian.store_ready", "vault", vaultDir)
	}
	o.initted = true
}

// syncAndReload runs git pull (or clone) in the vault directory, then reloads
// the notes.Store so the in-process view reflects the updated files.
// Returns a human-readable status string for tool output.
func (o *obsidianTool) syncAndReload() string {
	vaultDir := strings.TrimSpace(os.Getenv("GOON_OBSIDIAN_VAULT"))
	if vaultDir == "" {
		return "GOON_OBSIDIAN_VAULT is not set — nothing to sync"
	}
	if strings.HasPrefix(vaultDir, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			vaultDir = filepath.Join(home, vaultDir[2:])
		}
	}

	repo := strings.TrimSpace(os.Getenv("GOON_OBSIDIAN_REPO"))
	var msg string

	info, statErr := os.Stat(vaultDir)
	switch {
	case repo == "":
		// No git repo configured — vault is local-only, just reload the store.
		msg = fmt.Sprintf("no GOON_OBSIDIAN_REPO set; reloaded store from %s (no git pull)", vaultDir)

	case os.IsNotExist(statErr):
		// Directory doesn't exist — clone.
		logx.Info("obsidian.git_clone", "repo", repo, "target", vaultDir)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		//nolint:gosec // vault path is user-supplied via trusted env var
		cmd := exec.CommandContext(ctx, "git", "clone", "--depth=1", repo, vaultDir)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Sprintf("git clone failed: %v\n%s", err, out)
		}
		msg = fmt.Sprintf("cloned %s → %s", repo, vaultDir)

	case statErr != nil || !info.IsDir():
		return fmt.Sprintf("vault path is not a directory: %s", vaultDir)

	default:
		// Directory exists — pull.
		logx.Info("obsidian.git_pull", "vault", vaultDir)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		//nolint:gosec // vaultDir is the user's own config
		cmd := exec.CommandContext(ctx, "git", "-C", vaultDir, "pull", "--ff-only")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Sprintf("git pull failed: %v\n%s", err, out)
		}
		msg = fmt.Sprintf("git pull OK\n%s", strings.TrimSpace(string(out)))
	}

	// Reload the notes.Store so searches/reads see the updated files.
	o.obsidianInit(true /* force */)

	// Append note count so the user can confirm the vault loaded correctly.
	o.mu.Lock()
	st := o.store
	o.mu.Unlock()
	if st != nil {
		if names, err := st.List(); err == nil {
			msg += fmt.Sprintf("\n%d notes loaded", len(names))
		}
	}
	return msg
}

func (o *obsidianTool) require() (*notes.Store, error) {
	o.obsidianInit(false)
	if o.vault == "" {
		return nil, fmt.Errorf(
			"obsidian vault not configured — set GOON_OBSIDIAN_VAULT to your vault path " +
				"(and optionally GOON_OBSIDIAN_REPO if it is git-backed)")
	}
	if o.store == nil {
		return nil, fmt.Errorf(
			"obsidian vault could not be opened at %s — check the path exists and is readable",
			o.vault)
	}
	return o.store, nil
}

// ─── tool implementations (unexported — registered via RegisterObsidianTools)

type obsidianListTool struct{}

func (*obsidianListTool) Name() string { return "obsidian_list" }
func (*obsidianListTool) Description() string {
	return "list markdown notes in the Obsidian vault. Optional folder arg filters to a subdirectory (e.g. \"Projects\" or \"Areas/Work\")."
}
func (*obsidianListTool) Schema() map[string]string {
	return map[string]string{
		"folder": "(optional) subfolder to list; omit for all notes",
	}
}

func (*obsidianListTool) Run(_ context.Context, args map[string]string) (Result, error) {
	store, err := globalObsidian.require()
	if err != nil {
		return Result{ToolName: "obsidian_list", Err: err}, err
	}
	names, err := store.List()
	if err != nil {
		return Result{ToolName: "obsidian_list", Err: err}, err
	}
	prefix := filepath.ToSlash(strings.TrimSpace(args["folder"]))
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	var b strings.Builder
	count := 0
	for _, n := range names {
		if prefix != "" && !strings.HasPrefix(n, prefix) {
			continue
		}
		b.WriteString(n)
		b.WriteByte('\n')
		count++
	}
	if count == 0 {
		if prefix != "" {
			return Result{ToolName: "obsidian_list", Stdout: fmt.Sprintf("(no notes found under %q)", prefix)}, nil
		}
		return Result{ToolName: "obsidian_list", Stdout: "(vault is empty)"}, nil
	}
	return Result{ToolName: "obsidian_list", Stdout: b.String()}, nil
}

type obsidianReadTool struct{}

func (*obsidianReadTool) Name() string { return "obsidian_read" }
func (*obsidianReadTool) Description() string {
	return "read the full contents of an Obsidian note by its vault-relative path " +
		"(e.g. \"Projects/goon.md\" or \"Daily/2025-01-15\"). Returns the raw markdown."
}
func (*obsidianReadTool) Schema() map[string]string {
	return map[string]string{
		"note": "vault-relative path to the note (.md auto-appended if omitted)",
	}
}

func (*obsidianReadTool) Run(_ context.Context, args map[string]string) (Result, error) {
	store, err := globalObsidian.require()
	if err != nil {
		return Result{ToolName: "obsidian_read", Err: err}, err
	}
	name := strings.TrimSpace(args["note"])
	if name == "" {
		e := fmt.Errorf("obsidian_read: \"note\" arg is required")
		return Result{ToolName: "obsidian_read", Err: e}, e
	}
	body, err := store.Read(name)
	if err != nil {
		if os.IsNotExist(err) {
			return Result{ToolName: "obsidian_read", Stdout: fmt.Sprintf("(note not found: %s)", name)}, nil
		}
		return Result{ToolName: "obsidian_read", Err: err}, err
	}
	const maxBytes = 64 * 1024
	if len(body) > maxBytes {
		body = body[:maxBytes] + fmt.Sprintf(
			"\n…(truncated; full note is %d bytes — use obsidian_search to find specific sections)", len(body))
	}
	return Result{ToolName: "obsidian_read", Stdout: body}, nil
}

type obsidianSearchTool struct{}

func (*obsidianSearchTool) Name() string { return "obsidian_search" }
func (*obsidianSearchTool) Description() string {
	return "case-insensitive substring search across all Obsidian vault notes. " +
		"Returns matching note:line: text lines. Use this to find relevant notes before reading them in full."
}
func (*obsidianSearchTool) Schema() map[string]string {
	return map[string]string{
		"query": "substring or keyword to search for across all notes",
		"limit": "(optional) max number of hits to return; default 30",
	}
}

func (*obsidianSearchTool) Run(_ context.Context, args map[string]string) (Result, error) {
	store, err := globalObsidian.require()
	if err != nil {
		return Result{ToolName: "obsidian_search", Err: err}, err
	}
	query := strings.TrimSpace(args["query"])
	if query == "" {
		e := fmt.Errorf("obsidian_search: \"query\" arg is required")
		return Result{ToolName: "obsidian_search", Err: e}, e
	}
	limit := 30
	if l := strings.TrimSpace(args["limit"]); l != "" {
		if _, err := fmt.Sscanf(l, "%d", &limit); err != nil || limit <= 0 {
			limit = 30
		}
	}
	hits, err := store.Search(query, limit)
	if err != nil {
		return Result{ToolName: "obsidian_search", Err: err}, err
	}
	if len(hits) == 0 {
		return Result{ToolName: "obsidian_search",
			Stdout: fmt.Sprintf("(no matches for %q in Obsidian vault)", query)}, nil
	}
	var b strings.Builder
	for _, h := range hits {
		fmt.Fprintf(&b, "%s:%d: %s\n", h.Name, h.Line, h.Text)
	}
	return Result{ToolName: "obsidian_search", Stdout: b.String()}, nil
}

type obsidianSyncTool struct{}

func (*obsidianSyncTool) Name() string { return "obsidian_sync" }
func (*obsidianSyncTool) Description() string {
	return "pull the latest notes from the Obsidian vault git repo (GOON_OBSIDIAN_REPO) " +
		"and reload the vault. Call this after pushing new notes so goon sees them " +
		"without restarting the process."
}
func (*obsidianSyncTool) Schema() map[string]string { return map[string]string{} }

func (*obsidianSyncTool) Run(_ context.Context, _ map[string]string) (Result, error) {
	msg := globalObsidian.syncAndReload()
	return Result{ToolName: "obsidian_sync", Stdout: msg}, nil
}

// ─── package-level helpers (used by Telegram and web UI handlers) ─────────────

// ObsidianConfigured reports whether GOON_OBSIDIAN_VAULT is set.
func ObsidianConfigured() bool {
	return strings.TrimSpace(os.Getenv("GOON_OBSIDIAN_VAULT")) != ""
}

// ObsidianSync pulls the latest changes from the vault git repo and reloads
// the in-process store. Returns a human-readable status string.
func ObsidianSync() string {
	return globalObsidian.syncAndReload()
}

// ObsidianList returns a newline-separated list of vault-relative note paths,
// optionally filtered to a subfolder prefix. Returns ("", nil) when empty.
func ObsidianList(folder string) (string, error) {
	store, err := globalObsidian.require()
	if err != nil {
		return "", err
	}
	names, err := store.List()
	if err != nil {
		return "", err
	}
	prefix := filepath.ToSlash(strings.TrimSpace(folder))
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	var b strings.Builder
	for _, n := range names {
		if prefix != "" && !strings.HasPrefix(n, prefix) {
			continue
		}
		b.WriteString(n)
		b.WriteByte('\n')
	}
	return b.String(), nil
}

// ObsidianRead returns the full markdown body of a vault note by its
// vault-relative path (.md auto-appended if omitted). Returns os.ErrNotExist
// when the note is absent so callers can distinguish missing vs error.
func ObsidianRead(note string) (string, error) {
	store, err := globalObsidian.require()
	if err != nil {
		return "", err
	}
	return store.Read(note)
}

// ObsidianSearch runs a case-insensitive substring search across the vault.
// Returns newline-separated "note:line: text" hits, or ("", nil) when empty.
func ObsidianSearch(query string, limit int) (string, error) {
	store, err := globalObsidian.require()
	if err != nil {
		return "", err
	}
	if limit <= 0 {
		limit = 30
	}
	hits, err := store.Search(query, limit)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, h := range hits {
		fmt.Fprintf(&b, "%s:%d: %s\n", h.Name, h.Line, h.Text)
	}
	return b.String(), nil
}

// ObsidianWrite writes (creates or replaces) a note in the vault by its
// vault-relative path. Returns an error if the vault is not configured or
// the path is unsafe.
func ObsidianWrite(note, body string) error {
	store, err := globalObsidian.require()
	if err != nil {
		return err
	}
	return store.Write(note, body)
}

// ObsidianPush runs "git add -A && git commit -m 'goon: update notes' && git push"
// inside the vault directory. Returns a human-readable status string.
// No-ops and returns an informational message when GOON_OBSIDIAN_VAULT is unset
// or when the vault is not a git repo.
func ObsidianPush() string {
	vaultDir := strings.TrimSpace(os.Getenv("GOON_OBSIDIAN_VAULT"))
	if vaultDir == "" {
		return "GOON_OBSIDIAN_VAULT is not set — nothing to push"
	}
	if strings.HasPrefix(vaultDir, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			vaultDir = filepath.Join(home, vaultDir[2:])
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	run := func(args ...string) (string, error) {
		//nolint:gosec // vaultDir is user-supplied trusted env var
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.Dir = vaultDir
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	// Stage all changes.
	if out, err := run("git", "add", "-A"); err != nil {
		return fmt.Sprintf("git add failed: %v\n%s", err, out)
	}

	// Commit — tolerate "nothing to commit".
	commitOut, commitErr := run("git", "commit", "-m", "goon: update notes")
	if commitErr != nil {
		if strings.Contains(commitOut, "nothing to commit") {
			return "nothing to commit — vault is already up to date"
		}
		return fmt.Sprintf("git commit failed: %v\n%s", commitErr, commitOut)
	}

	// Push.
	if out, err := run("git", "push"); err != nil {
		return fmt.Sprintf("git push failed: %v\n%s", err, out)
	}
	return fmt.Sprintf("pushed OK\n%s", commitOut)
}

// ─── tool implementations (write + push) ─────────────────────────────────────

type obsidianWriteTool struct{}

func (*obsidianWriteTool) Name() string { return "obsidian_write" }
func (*obsidianWriteTool) Description() string {
	return "create or replace an Obsidian vault note by its vault-relative path " +
		"(e.g. \"Projects/goon.md\"). Provide the full markdown body in the \"body\" arg."
}
func (*obsidianWriteTool) Schema() map[string]string {
	return map[string]string{
		"note": "vault-relative path to the note (.md auto-appended if omitted)",
		"body": "full markdown content to write",
	}
}
func (*obsidianWriteTool) Run(_ context.Context, args map[string]string) (Result, error) {
	name := strings.TrimSpace(args["note"])
	if name == "" {
		e := fmt.Errorf("obsidian_write: \"note\" arg is required")
		return Result{ToolName: "obsidian_write", Err: e}, e
	}
	body := args["body"]
	if err := ObsidianWrite(name, body); err != nil {
		return Result{ToolName: "obsidian_write", Err: err}, err
	}
	return Result{ToolName: "obsidian_write", Stdout: fmt.Sprintf("wrote %s (%d bytes)", name, len(body))}, nil
}

type obsidianPushTool struct{}

func (*obsidianPushTool) Name() string { return "obsidian_push" }
func (*obsidianPushTool) Description() string {
	return "commit and push all pending changes in the Obsidian vault git repo " +
		"(GOON_OBSIDIAN_REPO). Runs git add -A && git commit && git push. " +
		"Call after obsidian_write to persist changes to the remote."
}
func (*obsidianPushTool) Schema() map[string]string { return map[string]string{} }
func (*obsidianPushTool) Run(_ context.Context, _ map[string]string) (Result, error) {
	msg := ObsidianPush()
	return Result{ToolName: "obsidian_push", Stdout: msg}, nil
}

// ─── registration ─────────────────────────────────────────────────────────────

// RegisterObsidianTools attaches all obsidian_* tools to a registry.
func RegisterObsidianTools(r *Registry) {
	r.Register(&obsidianListTool{})
	r.Register(&obsidianReadTool{})
	r.Register(&obsidianSearchTool{})
	r.Register(&obsidianSyncTool{})
	r.Register(&obsidianWriteTool{})
	r.Register(&obsidianPushTool{})
}
