// Package repository owns REPOSITORY.md — the user's hand-maintained
// mapping from remote git slugs to local checkout paths.
//
// Why this exists:
//
//   Before: the confirm_repo gate guessed candidates from
//   GOON_WORKSPACE_DIR + whatever the git host returned, and the LLM
//   triage step had no way to suggest a specific repo by name from the
//   user's actual list. So every new ticket asked "which repo?" even
//   when the answer was obvious from the ticket text.
//
//   Now: REPOSITORY.md is the single source of truth for "what repos
//   does this user work on, and where do they live locally." Triage
//   reads it so the LLM can name specific repos confidently. The gate
//   reads it so the candidate menu is pre-populated with the user's
//   real list. The agent can read or write it like any other memory
//   note via memory_* tools.
//
// File format: a markdown table with three columns. Tolerant parser
// so users can edit freely — extra columns are ignored, header row is
// optional, comment lines start with `>` or `#`.
//
//	# REPOSITORY.md
//	| Remote                        | Local                       | Notes |
//	|-------------------------------|-----------------------------|-------|
//	| github.com/myorg/backend-api  | /Users/me/code/backend-api  | Go    |
//	| github.com/myorg/web          | /Users/me/code/web          | React |
//
// On disk: ./storage/memory/REPOSITORY.md (alongside SOUL.md,
// HISTORY.md). The notes store knows about this filename and
// excludes it from the topic-note index.
package repository

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/harisaginting/goon/internal/notes"
)

// Filename is the canonical basename of the repository registry.
// Exported so notes / agentctx / web / docs surfaces can reference
// the same string and exclude it from the topic-note index without
// hard-coding "REPOSITORY.md" in five places.
const Filename = "REPOSITORY.md"

// Entry is one row in REPOSITORY.md. All fields are trimmed; Notes
// may be empty.
type Entry struct {
	Remote string // remote slug or URL, e.g. "github.com/myorg/api"
	Local  string // absolute or ~-expanded local path
	Notes  string // free-form note column (language, owner, etc.)
}

// Name returns a stable short name derived from the remote slug —
// the last path component. Used to render candidate menus.
//
//	github.com/myorg/backend-api → backend-api
//	myorg/web                    → web
//	just-a-name                  → just-a-name
func (e Entry) Name() string {
	s := strings.Trim(e.Remote, "/")
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// Resolve returns the local path with $HOME / ~ expansion. Empty
// when Local is empty; callers should treat that as "remote-only,
// needs cloning."
func (e Entry) Resolve() string {
	p := strings.TrimSpace(e.Local)
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// Read returns every parsed entry, or an empty slice when the file
// is missing or unreadable. Errors only surface for genuinely
// broken stores (notes.New failure) — a missing or empty
// REPOSITORY.md is the common case and shouldn't crash anything.
func Read() ([]Entry, error) {
	store, err := notes.New("")
	if err != nil {
		return nil, fmt.Errorf("repository: notes store: %w", err)
	}
	body, err := store.Read(Filename)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("repository: read %s: %w", Filename, err)
	}
	return dedup(Parse(body)), nil
}

// dedup removes entries with identical Remote+Local pairs, keeping the first
// occurrence. This prevents duplicate rows in the UI when REPOSITORY.md has
// been written twice (e.g. after auto-clone runs twice on the same repo).
func dedup(entries []Entry) []Entry {
	seen := map[string]bool{}
	out := entries[:0:len(entries)]
	for _, e := range entries {
		key := strings.ToLower(strings.TrimSpace(e.Remote)) + "\x00" + strings.TrimSpace(e.Local)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, e)
	}
	return out
}

// RawBody returns the file as a string for prompt injection. Empty
// when the file is missing — callers should skip injection in that
// case so the prompt stays terse.
func RawBody() string {
	store, err := notes.New("")
	if err != nil {
		return ""
	}
	body, err := store.Read(Filename)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(body)
}

// Parse extracts entries from REPOSITORY.md body. Forgiving by
// design: skip blank lines, comment lines (#, >, html comments),
// separator rows (|---|---|), and the header row (detected by
// containing the literal "remote" or "repository" in the first
// cell, case-insensitive). Any row with at least two non-empty
// cells is accepted as an entry — extra cells past the third
// collapse into Notes.
func Parse(body string) []Entry {
	var out []Entry
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		// Skip markdown comments / blockquotes / fences.
		if strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, ">") {
			continue
		}
		if strings.HasPrefix(line, "<!--") {
			continue
		}
		if strings.HasPrefix(line, "```") {
			continue
		}
		// Must look like a table row.
		if !strings.HasPrefix(line, "|") {
			continue
		}
		// Separator row: only |, -, :, spaces.
		if isSeparatorRow(line) {
			continue
		}
		cells := splitRow(line)
		if len(cells) < 2 {
			continue
		}
		first := strings.ToLower(cells[0])
		// Header row heuristic — skip if the first cell is one of the
		// usual labels. Users editing by hand often keep the header.
		if first == "remote" || first == "repository" || first == "name" || first == "repo" {
			continue
		}
		remote := cells[0]
		local := ""
		notesCol := ""
		if len(cells) >= 2 {
			local = cells[1]
		}
		if len(cells) >= 3 {
			notesCol = strings.Join(cells[2:], " · ")
		}
		if remote == "" {
			continue
		}
		out = append(out, Entry{Remote: remote, Local: local, Notes: notesCol})
	}
	return out
}

// isSeparatorRow returns true for table separator rows like
// "|---|---|" or "|:---:|---|". Empty rows count too.
func isSeparatorRow(line string) bool {
	stripped := strings.Map(func(r rune) rune {
		switch r {
		case '|', '-', ':', ' ', '\t':
			return -1
		}
		return r
	}, line)
	return stripped == ""
}

// splitRow splits a pipe-delimited markdown table row into trimmed
// cells, dropping the leading + trailing empty cells produced by
// surrounding pipes.
func splitRow(line string) []string {
	parts := strings.Split(line, "|")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	// Drop leading/trailing empties created by edge pipes.
	for len(out) > 0 && out[0] == "" {
		out = out[1:]
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

// Write replaces REPOSITORY.md with the given entries rendered as a
// fresh markdown table. The starter comment block is preserved
// (re-rendered from Default's preamble) so the file always
// self-documents how to edit it. Pass an empty slice to clear it
// back to just the preamble.
func Write(entries []Entry) error {
	store, err := notes.New("")
	if err != nil {
		return fmt.Errorf("repository: notes store: %w", err)
	}
	return store.Write(Filename, render(entries))
}

// Add appends one entry to REPOSITORY.md (no duplicate check on
// Remote — Lookup returns the first match, so a deliberate
// duplicate row is harmless if the user wants two paths for the
// same remote). Returns the full updated entry list.
func Add(e Entry) ([]Entry, error) {
	existing, _ := Read()
	existing = append(existing, e)
	if err := Write(existing); err != nil {
		return nil, err
	}
	return existing, nil
}

// Lookup returns the first entry whose Remote slug, last-segment
// name, OR resolved local path matches the query. Case-insensitive.
// Returns (Entry{}, false) when no match. Used by the workflow
// triage step and the confirm_repo gate to resolve LLM-provided
// names back to concrete paths.
func Lookup(query string) (Entry, bool) {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return Entry{}, false
	}
	entries, _ := Read()
	// Pass 1: exact remote or local match.
	for _, e := range entries {
		if strings.ToLower(e.Remote) == q || strings.ToLower(e.Resolve()) == q {
			return e, true
		}
	}
	// Pass 2: last-segment name.
	for _, e := range entries {
		if strings.ToLower(e.Name()) == q {
			return e, true
		}
	}
	// Pass 3: substring on the remote (catches "myorg/api" → "github.com/myorg/api").
	for _, e := range entries {
		if strings.Contains(strings.ToLower(e.Remote), q) {
			return e, true
		}
	}
	return Entry{}, false
}

// Names returns just the short names from every entry, sorted.
// Handy for prompt context where the full table would be noisy
// and the user just needs the LLM to know which slugs exist.
func Names() []string {
	entries, _ := Read()
	out := make([]string, 0, len(entries))
	seen := map[string]bool{}
	for _, e := range entries {
		n := e.Name()
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// SeedDefault writes the starter REPOSITORY.md ONLY if no file
// exists yet. Returns (created, err); created=false means the file
// was already there (we never overwrite a populated file). Designed
// to be called on every boot — idempotent.
func SeedDefault() (bool, error) {
	store, err := notes.New("")
	if err != nil {
		return false, err
	}
	full, err := store.Resolve(Filename)
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(full); err == nil {
		return false, nil
	}
	if err := store.Write(Filename, Default); err != nil {
		return false, err
	}
	return true, nil
}

// render builds the on-disk markdown body from the preamble + a
// fresh table of entries. Even when entries is empty we still write
// the preamble + an empty table header so users see the format
// without guessing.
func render(entries []Entry) string {
	var sb strings.Builder
	sb.WriteString(preamble)
	sb.WriteString("\n")
	sb.WriteString("| Remote | Local | Notes |\n")
	sb.WriteString("|--------|-------|-------|\n")
	for _, e := range entries {
		fmt.Fprintf(&sb, "| %s | %s | %s |\n",
			escapeCell(e.Remote), escapeCell(e.Local), escapeCell(e.Notes))
	}
	return sb.String()
}

func escapeCell(s string) string {
	s = strings.TrimSpace(s)
	// Pipes inside cells would break the table; backslash-escape.
	return strings.ReplaceAll(s, "|", `\|`)
}

// Default is the starter REPOSITORY.md content. The preamble teaches
// the user the format so the file is self-documenting on first
// inspection.
const Default = preamble + `
| Remote | Local | Notes |
|--------|-------|-------|
`

const preamble = `# REPOSITORY.md — where do my repos live?

This file tells goon which remote repositories you work on and where
their local checkouts live. Triage reads it before deciding what work
needs to happen, and the confirm_repo gate uses it to pre-populate
the multi-select menu — so you stop typing paths after the first time.

How to use:

- **Remote**: the slug / URL the git host knows (e.g.
  ` + "`github.com/myorg/backend-api`" + ` or just ` + "`myorg/backend-api`" + `).
- **Local**: the absolute path to the checkout on your machine
  (` + "`~`" + ` expansion is supported). Leave blank for repos that aren't
  cloned yet — goon will tell you they're remote-only.
- **Notes**: free text. Useful for "Go", "Python", "owner=alice",
  "deprecated", etc.

Add one row per repo. Goon scans this file automatically; no restart
needed. Quick adds via CLI:

    goon repo scan           # auto-discover from GOON_WORKSPACE_DIR
    goon repo show           # print the parsed table
    goon repo edit           # open in $EDITOR

The agent can also read or update this file with the ` + "`memory_*`" + ` tools.
`

// ErrNotFound is returned by helpers that distinguish "no file" from
// "file but no matching row." Currently only the public API uses
// bool/ok returns; this is here for future use.
var ErrNotFound = errors.New("repository: no matching entry")
