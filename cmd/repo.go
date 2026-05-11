package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/harisaginting/goon/internal/memory"
)

// runRepo manages the project→repo learned mappings stored in
// memory.json. Each successful confirm_repo gate writes one entry; if
// you re-route a project to a new path the cached entry would still
// win, so this subcommand surfaces a way to inspect / forget them.
//
//	goon repo               # show current learned map
//	goon repo list          # alias for above
//	goon repo forget <key>  # drop one project's learned mapping
//	goon repo clear         # wipe the entire learned map
//
// Env-explicit GOON_REPO_MAP entries always override learned ones —
// this subcommand only touches what the gate persisted.
func runRepo(_ context.Context, args []string, stdout, stderr io.Writer) error {
	mem, err := memory.New(os.Getenv("GOON_MEMORY_PATH"))
	if err != nil {
		return fmt.Errorf("memory: %w", err)
	}
	sub := "list"
	if len(args) > 0 {
		sub = args[0]
		args = args[1:]
	}
	switch sub {
	case "list", "show":
		return repoList(mem, stdout)
	case "forget", "rm":
		if len(args) == 0 {
			return fmt.Errorf("usage: goon repo forget <project>")
		}
		return repoForget(mem, args[0], stdout, stderr)
	case "clear":
		return repoClear(mem, stdout)
	default:
		return fmt.Errorf("unknown repo subcommand %q (try: list | forget <project> | clear)", sub)
	}
}

func repoList(mem *memory.Memory, stdout io.Writer) error {
	choices := mem.RepoChoices()
	if len(choices) == 0 {
		fmt.Fprintln(stdout, "no learned repo mappings yet.")
		fmt.Fprintln(stdout, "the daemon writes one entry every time you confirm a repo at the confirm_repo gate.")
		return nil
	}
	keys := make([]string, 0, len(choices))
	for k := range choices {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintf(stdout, "%d learned mapping(s):\n\n", len(choices))
	for _, k := range keys {
		fmt.Fprintf(stdout, "  %-20s → %s\n", k, choices[k])
	}
	fmt.Fprintln(stdout, "\ndrop one with: goon repo forget <project>")
	fmt.Fprintln(stdout, "GOON_REPO_MAP env vars take priority over these.")
	return nil
}

func repoForget(mem *memory.Memory, project string, stdout, stderr io.Writer) error {
	if mem.ForgetRepoChoice(project) {
		fmt.Fprintf(stdout, "✓ forgot mapping for %q. The next ticket from that project will re-ask the confirm_repo gate.\n", project)
		return nil
	}
	fmt.Fprintf(stderr, "no learned mapping for %q (run `goon repo list` to see what's stored).\n", project)
	return nil
}

func repoClear(mem *memory.Memory, stdout io.Writer) error {
	all := mem.RepoChoices()
	if len(all) == 0 {
		fmt.Fprintln(stdout, "nothing to clear.")
		return nil
	}
	for k := range all {
		mem.ForgetRepoChoice(k)
	}
	fmt.Fprintf(stdout, "✓ cleared %d learned mapping(s). Every project will re-ask on the next confirm_repo gate.\n", len(all))
	return nil
}
