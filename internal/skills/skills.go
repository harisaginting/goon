// Package skills is goon's "specialist" markdown store — a sibling of
// internal/notes (which holds knowledge/memory). The split is
// intentional and user-facing:
//
//   - memory  → facts, observations, project context (SOUL.md is
//     auto-injected into every LLM run; HISTORY.md holds the running
//     log of past tasks)
//   - skills  → specialist procedures, role definitions, how-tos
//     (NOT auto-injected; activated on-demand by the user or agent)
//
// On disk:
//
//	./storage/skills/<name>.md
//
// The same path-safety guarantees as notes — absolute paths, ".."
// segments, and symlink escapes are all rejected at Resolve(). We
// achieve this by delegating to *notes.Store under the hood: the
// store doesn't care what the "type" of the markdown is, only the
// root directory differs.
//
// Storage location, in order of precedence:
//
//	1. explicit `dir` argument to New("...")
//	2. $GOON_SKILLS_DIR
//	3. <storage.Root()>/skills
package skills

import (
	"os"
	"strings"

	"github.com/harisaginting/goon/internal/notes"
	"github.com/harisaginting/goon/internal/storage"
)

// New opens (and creates) a skills store. Falls back to
// $GOON_SKILLS_DIR, then <storage.Root()>/skills (i.e. ./storage/skills
// by default) when dir is empty.
//
// Returns *notes.Store so callers can use the full notes API
// (List/Read/Write/Append/Delete/Search) uniformly — the only
// difference from a memory store is the on-disk location and the
// fact that we don't call .Soul() on the result. Skills are
// activated on-demand, not auto-loaded.
func New(dir string) (*notes.Store, error) {
	resolved := strings.TrimSpace(dir)
	if resolved == "" {
		resolved = strings.TrimSpace(os.Getenv("GOON_SKILLS_DIR"))
	}
	if resolved == "" {
		resolved = storage.Path("skills")
	}
	return notes.New(resolved)
}

// SeedDefaults writes a small starter set of skills if the store is
// empty. Idempotent — never overwrites an existing file. Designed to
// be called at boot so first-run users get a tangible example of
// what a skill looks like.
//
// The three seeds cover the most common asks: review code, write a
// good commit message, and walk through a bug. They're short on
// purpose: skills should fit on one screen.
func SeedDefaults() error {
	store, err := New("")
	if err != nil {
		return err
	}
	existing, err := store.List()
	if err != nil {
		return err
	}
	// If the user already has any skills, don't add to them. Their
	// store, their content.
	if len(existing) > 0 {
		return nil
	}
	for name, body := range defaultSkills {
		// .Write overwrites unconditionally — guard each file just
		// in case a user created one with the same name between the
		// list check and now.
		p, _ := store.Resolve(name)
		if _, err := os.Stat(p); err == nil {
			continue
		}
		if err := store.Write(name, body); err != nil {
			return err
		}
	}
	return nil
}

// defaultSkills is the seed set written on first run. Markdown,
// kebab-case filenames, one role per file. Users edit / delete /
// extend freely.
var defaultSkills = map[string]string{
	"code-reviewer.md": `# code-reviewer

When the user asks you to review code or a PR, act as a senior
engineer doing a thorough but kind review.

## Cover the obvious failure modes first

- Does the change compile / pass the existing tests?
- Are new tests added for new behaviour?
- Any obvious nil/null/zero-value bugs in branches?

## Then design

- Is the change in the right layer? Reaching across boundaries?
- Are public APIs minimal and stable?
- Will this scale (data size, request volume, concurrent edits)?

## Then code quality

- Variable / function names that don't need a comment to explain.
- Dead code, copy-paste, magic numbers.
- Error handling — is anything silently swallowed?

## Style

- Lead with what works ("the migration is clean") before listing
  concerns. People hear feedback better that way.
- Flag blockers with "needs-change:" and nits with "nit:" so the
  author can triage.
`,
	"commit-author.md": `# commit-author

When the user asks you to write a commit message, follow this shape:

    <type>(<scope>): <imperative summary, lowercase, <= 72 chars>

    <one paragraph explaining the change and the why>

    <optional bullet list of mechanical changes>

    Fixes: ENG-123

## Types

feat | fix | refactor | docs | test | chore | perf

## Rules

- Imperative voice ("add X", not "added X" or "adds X").
- Summary line under 72 chars. Body wrapped at 72.
- Don't restate the diff. Explain the why.
- Reference ticket keys verbatim.
`,
	"bug-hunter.md": `# bug-hunter

When the user asks you to debug something, follow this loop:

1. Restate the bug in one sentence. Force precision.
2. Identify the smallest reproduction. If you can't repro, say so —
   don't guess.
3. List your hypotheses, ranked by probability. State the test that
   would falsify each.
4. Run the cheapest test first. Update the ranking.
5. When you've found the cause, write the fix AND a regression test.

## Anti-patterns to avoid

- Don't "fix" things you can't reproduce. Reproduce first.
- Don't blame the framework / library / OS before checking the obvious
  (typos, stale build, wrong branch).
- Don't ship the fix without writing down what was wrong — leave a
  trail (commit message + topic note in storage/memory) so the next
  person doesn't repeat the hunt.
`,
}
