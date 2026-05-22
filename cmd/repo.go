package cmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/harisaginting/goon/internal/memory"
	"github.com/harisaginting/goon/internal/notes"
	"github.com/harisaginting/goon/internal/repository"
)

// runRepo manages goon's two repo-related concepts:
//
//   1. REPOSITORY.md  — the user-maintained mapping of remote slug →
//      local checkout path. Source of truth for the confirm_repo
//      gate's candidate menu and for the triage LLM call. Lives at
//      ./storage/memory/REPOSITORY.md.
//   2. memory.RepoChoices — per-project learned mappings the gate
//      writes after the first confirm. Lives in memory.json.
//
// The "show / edit / scan / add" verbs target REPOSITORY.md.
// The "list / forget / clear" verbs target the learned mappings.
//
//	goon repo                  alias for `show`
//	goon repo show             print parsed REPOSITORY.md as a table
//	goon repo edit             open REPOSITORY.md in $EDITOR
//	goon repo scan             walk $GOON_WORKSPACE_DIR for .git folders
//	goon repo add <remote> <local> [notes...]  add one entry
//	goon repo list             show learned project→repo mappings (memory.json)
//	goon repo forget <project> drop one learned mapping
//	goon repo clear            wipe every learned mapping
func runRepo(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	mem, err := memory.New(os.Getenv("GOON_MEMORY_PATH"))
	if err != nil {
		return fmt.Errorf("memory: %w", err)
	}
	sub := "show"
	if len(args) > 0 {
		sub = args[0]
		args = args[1:]
	}
	switch sub {
	case "show":
		return repoShow(stdout)
	case "edit":
		return repoEdit(ctx, stdout, stderr, os.Stdin)
	case "scan":
		return repoScan(stdout, stderr)
	case "add":
		return repoAdd(args, stdout, stderr)
	case "list":
		return repoList(mem, stdout)
	case "forget", "rm":
		if len(args) == 0 {
			return fmt.Errorf("usage: goon repo forget <project>")
		}
		return repoForget(mem, args[0], stdout, stderr)
	case "clear":
		return repoClear(mem, stdout)
	case "help", "-h", "--help":
		printRepoHelp(stdout)
		return nil
	default:
		printRepoHelp(stderr)
		return fmt.Errorf("unknown repo subcommand %q", sub)
	}
}

// printRepoHelp lays out the full surface in one place — easier than
// scattering hints across every error path.
func printRepoHelp(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  goon repo                       show parsed REPOSITORY.md")
	fmt.Fprintln(w, "  goon repo show                  same as above")
	fmt.Fprintln(w, "  goon repo edit                  open REPOSITORY.md in $EDITOR")
	fmt.Fprintln(w, "  goon repo scan                  auto-discover repos under $GOON_WORKSPACE_DIR")
	fmt.Fprintln(w, "  goon repo add <remote> <local> [notes...]   add one entry")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "REPOSITORY.md lives at $GOON_MEMORY_DIR/REPOSITORY.md and is the source of truth")
	fmt.Fprintln(w, "for which remote repos exist and where their local checkouts are. Triage reads it")
	fmt.Fprintln(w, "so the LLM can name specific repos per ticket; the confirm_repo gate fires for")
	fmt.Fprintln(w, "EACH ticket separately — there's no per-project auto-skip anymore, since two")
	fmt.Fprintln(w, "tickets in the same project can need different repos (or sets of repos).")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "  goon repo list / forget / clear   deprecated (project→repo cache removed)")
}

// repoShow prints REPOSITORY.md parsed as a fixed-width table so the
// user can see what goon actually understands (vs the raw markdown).
func repoShow(stdout io.Writer) error {
	entries, err := repository.Read()
	if err != nil {
		return err
	}
	store, _ := notes.New("")
	if store != nil {
		fmt.Fprintf(stdout, "file: %s/%s\n\n", store.Path(), repository.Filename)
	}
	if len(entries) == 0 {
		fmt.Fprintln(stdout, "(no entries yet — try `goon repo scan` or `goon repo edit`)")
		return nil
	}
	// Compute column widths.
	maxName, maxRemote, maxLocal := len("Name"), len("Remote"), len("Local")
	for _, e := range entries {
		if n := len(e.Name()); n > maxName {
			maxName = n
		}
		if n := len(e.Remote); n > maxRemote {
			maxRemote = n
		}
		if n := len(e.Resolve()); n > maxLocal {
			maxLocal = n
		}
	}
	fmt.Fprintf(stdout, "  %-*s  %-*s  %-*s  %s\n",
		maxName, "Name", maxRemote, "Remote", maxLocal, "Local", "Notes")
	fmt.Fprintf(stdout, "  %s  %s  %s  %s\n",
		strings.Repeat("-", maxName), strings.Repeat("-", maxRemote),
		strings.Repeat("-", maxLocal), strings.Repeat("-", 5))
	for _, e := range entries {
		fmt.Fprintf(stdout, "  %-*s  %-*s  %-*s  %s\n",
			maxName, e.Name(), maxRemote, e.Remote, maxLocal, e.Resolve(), e.Notes)
	}
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "edit the file with: goon repo edit")
	return nil
}

// repoEdit opens REPOSITORY.md in $EDITOR. Mirrors `goon memory edit`.
func repoEdit(ctx context.Context, stdout, stderr io.Writer, stdin io.Reader) error {
	store, err := notes.New("")
	if err != nil {
		return err
	}
	full, err := store.Resolve(repository.Filename)
	if err != nil {
		return err
	}
	// Touch the file (with the default seed) if it doesn't exist so
	// the editor opens with template content instead of a blank
	// buffer.
	if _, statErr := os.Stat(full); os.IsNotExist(statErr) {
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		_, _ = repository.SeedDefault()
	}
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = os.Getenv("VISUAL")
	}
	if editor == "" {
		editor = "vi"
	}
	cmd := exec.CommandContext(ctx, editor, full)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// repoScan walks $GOON_WORKSPACE_DIR (one level deep) for entries
// that look like git checkouts (have a .git child) and offers to
// add the ones missing from REPOSITORY.md. Interactive y/N per repo.
//
// Non-interactive when stdin is not a terminal — accepts all found
// repos. We err on the side of accepting because the user can
// always edit REPOSITORY.md afterwards.
func repoScan(stdout, stderr io.Writer) error {
	workspace := strings.TrimSpace(os.Getenv("GOON_WORKSPACE_DIR"))
	if workspace == "" {
		return fmt.Errorf("scan: set GOON_WORKSPACE_DIR to the folder that holds your git repos and try again")
	}
	entries, _ := os.ReadDir(workspace)
	found := []repository.Entry{}
	for _, ent := range entries {
		if !ent.IsDir() || strings.HasPrefix(ent.Name(), ".") {
			continue
		}
		full := filepath.Join(workspace, ent.Name())
		if _, err := os.Stat(filepath.Join(full, ".git")); err != nil {
			continue
		}
		// Best-effort: discover the remote URL via `git remote`.
		remote := readGitRemote(full)
		if remote == "" {
			// Fall back to the directory basename so the entry still
			// has something useful in the Remote column.
			remote = ent.Name()
		}
		found = append(found, repository.Entry{
			Remote: remote,
			Local:  full,
			Notes:  "auto-scanned",
		})
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Remote < found[j].Remote })
	if len(found) == 0 {
		fmt.Fprintf(stdout, "no git checkouts found under %s\n", workspace)
		return nil
	}
	existing, _ := repository.Read()
	known := map[string]bool{}
	for _, e := range existing {
		known[strings.ToLower(e.Resolve())] = true
		known[strings.ToLower(e.Remote)] = true
	}
	var toAdd []repository.Entry
	for _, e := range found {
		if known[strings.ToLower(e.Local)] || known[strings.ToLower(e.Remote)] {
			continue
		}
		toAdd = append(toAdd, e)
	}
	if len(toAdd) == 0 {
		fmt.Fprintf(stdout, "found %d repo(s) under %s — all already in REPOSITORY.md.\n", len(found), workspace)
		return nil
	}
	fmt.Fprintf(stdout, "found %d new repo(s) under %s:\n\n", len(toAdd), workspace)
	for _, e := range toAdd {
		fmt.Fprintf(stdout, "  + %-40s  →  %s\n", e.Remote, e.Local)
	}
	fmt.Fprintln(stdout)
	fmt.Fprint(stdout, "Add all to REPOSITORY.md? [Y/n] ")
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	ans := strings.ToLower(strings.TrimSpace(line))
	if ans == "n" || ans == "no" {
		fmt.Fprintln(stdout, "aborted.")
		return nil
	}
	merged := append(existing, toAdd...)
	if err := repository.Write(merged); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "\n✓ added %d entries. Run `goon repo show` to see the table.\n", len(toAdd))
	return nil
}

// repoAdd is the one-shot non-interactive add. Notes is everything
// after the second positional arg (joined with spaces).
func repoAdd(args []string, stdout, stderr io.Writer) error {
	if len(args) < 2 {
		fmt.Fprintln(stderr, "usage: goon repo add <remote> <local-path> [notes...]")
		return fmt.Errorf("missing arguments")
	}
	entry := repository.Entry{
		Remote: args[0],
		Local:  args[1],
		Notes:  strings.Join(args[2:], " "),
	}
	if _, err := repository.Add(entry); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "✓ added %s → %s\n", entry.Remote, entry.Local)
	return nil
}

// readGitRemote returns the URL of the first remote (typically
// "origin") for the repo at dir, or "" on any failure. We don't shell
// out to `git` since that adds a process per repo; instead we read
// .git/config directly with a tiny INI-style scan.
func readGitRemote(dir string) string {
	cfgPath := filepath.Join(dir, ".git", "config")
	f, err := os.Open(cfgPath)
	if err != nil {
		// .git might be a file (worktree) rather than a dir — skip;
		// the bare directory name is good enough for those.
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	inRemote := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "[remote ") {
			inRemote = true
			continue
		}
		if strings.HasPrefix(line, "[") {
			inRemote = false
			continue
		}
		if !inRemote {
			continue
		}
		if strings.HasPrefix(line, "url") {
			eq := strings.IndexByte(line, '=')
			if eq < 0 {
				continue
			}
			url := strings.TrimSpace(line[eq+1:])
			return normalizeGitURL(url)
		}
	}
	return ""
}

// normalizeGitURL collapses SSH and HTTPS forms to a short host/owner/repo
// slug suitable for REPOSITORY.md and LLM prompts.
//
//	git@github.com:myorg/api.git    → github.com/myorg/api
//	https://github.com/myorg/api.git → github.com/myorg/api
func normalizeGitURL(url string) string {
	url = strings.TrimSpace(url)
	url = strings.TrimSuffix(url, ".git")
	// SSH: git@host:owner/repo
	if strings.HasPrefix(url, "git@") {
		rest := strings.TrimPrefix(url, "git@")
		if i := strings.IndexByte(rest, ':'); i >= 0 {
			return rest[:i] + "/" + rest[i+1:]
		}
	}
	// HTTPS / HTTP / git://
	for _, scheme := range []string{"https://", "http://", "git://", "ssh://git@"} {
		if strings.HasPrefix(url, scheme) {
			return strings.TrimPrefix(url, scheme)
		}
	}
	return url
}

// --- learned-mapping subcommands (memory.json) ------------------------------

// repoList / repoForget / repoClear remain wired up so old muscle
// memory + scripts don't crash, but they print a deprecation banner
// instead of pretending the per-project cache is still authoritative.
// The cache was the source of the "ENG-1 and ENG-2 forced to the
// same single repo" bug; the new model is per-ticket via triage +
// REPOSITORY.md and there's nothing to manage at the project level
// anymore.

const repoLegacyBanner = `the per-project repo cache is deprecated and no longer consulted by
the workflow. Each ticket now gets its own confirm_repo gate, with
suggestions drawn from REPOSITORY.md — that way ENG-1 can use repoA
while ENG-2 uses repoA+repoB without one forcing the other.

For the canonical remote→local table:
  goon repo show     # parsed view
  goon repo edit     # open REPOSITORY.md in $EDITOR
  goon repo scan     # auto-discover from $GOON_WORKSPACE_DIR
`

func repoList(mem *memory.Memory, stdout io.Writer) error {
	fmt.Fprintln(stdout, repoLegacyBanner)
	choices := mem.RepoChoices()
	if len(choices) > 0 {
		fmt.Fprintf(stdout, "\n(memory.json still holds %d legacy entr%s from before the rewrite — they're ignored at runtime.)\n",
			len(choices), pluralES(len(choices)))
	}
	return nil
}

func repoForget(mem *memory.Memory, project string, stdout, stderr io.Writer) error {
	fmt.Fprintln(stdout, repoLegacyBanner)
	if mem.ForgetRepoChoice(project) {
		fmt.Fprintf(stdout, "\n(✓ dropped a stale legacy entry for %q from memory.json)\n", project)
	}
	return nil
}

func repoClear(mem *memory.Memory, stdout io.Writer) error {
	fmt.Fprintln(stdout, repoLegacyBanner)
	all := mem.RepoChoices()
	if len(all) == 0 {
		return nil
	}
	for k := range all {
		mem.ForgetRepoChoice(k)
	}
	fmt.Fprintf(stdout, "\n(✓ wiped %d stale legacy entr%s from memory.json)\n",
		len(all), pluralES(len(all)))
	return nil
}

// pluralES returns the right "y → ies" suffix for "entry/entries".
// Keeps the deprecation message grammatical without dragging in a
// pluralization helper.
func pluralES(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
