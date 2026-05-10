// Package notes is goon's *active* memory store: a directory of markdown
// files the LLM can read and write between runs to accumulate knowledge.
//
// This is intentionally separate from internal/memory, which is the *passive*
// JSON-backed runtime store for tickets, workflows, and queued questions.
// notes is for evolving knowledge — observations, conventions, learned
// patterns, project context — that any LLM provider can consult and grow
// across sessions.
//
// Design goals:
//
//   - One markdown file per topic. Filenames are kebab-case .md by convention.
//   - Path-safe: every operation is constrained to the memory root. No
//     accidental writes outside the directory, no relative escapes, no
//     absolute paths.
//   - Provider-agnostic. The contents are plain UTF-8 markdown — every LLM
//     can read it without special tooling.
//   - One file gets superpowers: PINNED.md, when present, is auto-included
//     in the agent's system prompt every run. That's how the agent
//     "remembers" things without being told to look them up.
//
// Storage location, in order of precedence:
//
//	1. explicit `dir` argument to New("...")
//	2. $GOON_MEMORY_DIR
//	3. <storage.Root()>/memory   (./storage/memory by default; relocates
//	   with $GOON_STORAGE_DIR for the whole project's state)
//
// The directory is named "memory" rather than "notes" to match the
// user-facing CLI verb (`goon memory`) and the existing GOON_MEMORY_DIR
// env var. The Go package is `notes` only because the *type* of store —
// markdown files the LLM evolves — is conceptually distinct from the
// passive JSON memory in internal/memory.
package notes

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/harisaginting/goon/internal/storage"
)

// Store is a disk-backed markdown notes store rooted at a single directory.
// The zero value is unusable — always go through New().
type Store struct {
	dir string
}

// PinnedFilename is the special note auto-loaded into the agent's system
// prompt. Keep the name in one place so CLI + agent agree.
const PinnedFilename = "PINNED.md"

// New opens (and creates) a Store. When dir is empty it falls back to
// $GOON_MEMORY_DIR, then <storage.Root()>/memory (i.e.
// ./storage/memory by default). The directory is created with 0o755
// perms if it doesn't exist.
//
// The resolved path is canonicalized via filepath.EvalSymlinks so the
// "stays inside root" invariant in Resolve() can't be defeated by a
// symlink hop higher up the tree.
//
// Returns an error only on filesystem failures — never on missing dir,
// since the whole point is to bootstrap from nothing.
func New(dir string) (*Store, error) {
	resolved := strings.TrimSpace(dir)
	if resolved == "" {
		resolved = strings.TrimSpace(os.Getenv("GOON_MEMORY_DIR"))
	}
	if resolved == "" {
		resolved = storage.Path("memory")
	}
	if err := os.MkdirAll(resolved, 0o755); err != nil {
		return nil, fmt.Errorf("notes: mkdir %s: %w", resolved, err)
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return nil, fmt.Errorf("notes: abs %s: %w", resolved, err)
	}
	// EvalSymlinks resolves the entire path, including symlink-targeted
	// parents. We need this canonical form so Resolve can detect symlink
	// hops *inside* the store that point outside of it. Best-effort: if
	// EvalSymlinks errors (rare — directory exists since we just made it)
	// fall back to abs.
	canonical := abs
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		canonical = real
	}
	return &Store{dir: canonical}, nil
}

// Path returns the resolved root directory.
func (s *Store) Path() string { return s.dir }

// Resolve turns a user-supplied note name into an absolute path inside the
// store. It enforces:
//
//   - non-empty after trimming
//   - no absolute paths (no leading "/" or drive letter)
//   - no ".." segments — even if they cancel out — to stop sneaky escapes
//   - .md suffix (auto-appended if missing)
//   - resolved path stays inside store root after symlink-aware filepath.Clean
//
// The intent is to make the LLM's tool calls *safe by construction*, even
// when the model hallucinates a weird name.
func (s *Store) Resolve(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("notes: name is required")
	}
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("notes: absolute paths are not allowed (%q)", name)
	}
	// Reject any ".." segment in the RAW input — checking only after
	// filepath.Clean would silently allow "ok/../still-ok.md" since Clean
	// normalizes it to "still-ok.md". A literal ".." anywhere is suspicious
	// regardless, and refusing it is cheaper than reasoning about edge
	// cases. Users can still reach subdirs via "sub/note.md".
	for _, sep := range []string{"/", "\\"} {
		for _, seg := range strings.Split(name, sep) {
			if seg == ".." {
				return "", fmt.Errorf("notes: %q escapes the memory root", name)
			}
		}
	}
	clean := filepath.ToSlash(filepath.Clean(name))
	if !strings.HasSuffix(strings.ToLower(clean), ".md") {
		clean += ".md"
	}
	full := filepath.Join(s.dir, clean)
	// Lexical sanity: the resolved path must still be under root.
	rel, err := filepath.Rel(s.dir, full)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("notes: %q escapes the memory root", name)
	}
	// Symlink sanity: if any parent of the target already exists and
	// points (via symlink) outside the canonical store root, refuse.
	// We check the closest existing ancestor — file may not exist yet
	// (Write creates it), but its parent dir likely does after MkdirAll.
	if parent, ok := closestExistingDir(full); ok {
		if real, err := filepath.EvalSymlinks(parent); err == nil {
			rel, err := filepath.Rel(s.dir, real)
			if err != nil || strings.HasPrefix(rel, "..") {
				return "", fmt.Errorf("notes: %q escapes the memory root via symlink", name)
			}
		}
	}
	return full, nil
}

// closestExistingDir walks up from p looking for the first directory that
// actually exists on disk. Used by Resolve to detect symlink hops in the
// path even when the leaf file hasn't been created yet.
func closestExistingDir(p string) (string, bool) {
	dir := filepath.Dir(p)
	for {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir, true
		}
		next := filepath.Dir(dir)
		if next == dir {
			return "", false
		}
		dir = next
	}
}

// List returns every .md file under the root (recursively), as
// root-relative names, sorted. Hidden files (starting with ".") and
// non-.md files are skipped.
func (s *Store) List() ([]string, error) {
	var out []string
	err := filepath.Walk(s.dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// Skip hidden subdirs (e.g. .git scratch).
			if path != s.dir && strings.HasPrefix(info.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(info.Name(), ".") {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(info.Name()), ".md") {
			return nil
		}
		rel, _ := filepath.Rel(s.dir, path)
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// Read returns the full contents of a note as a string. Errors with
// os.ErrNotExist if the note doesn't exist.
func (s *Store) Read(name string) (string, error) {
	p, err := s.Resolve(name)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Write replaces a note's contents. Creates parent dirs as needed.
// File mode is 0o644.
func (s *Store) Write(name, body string) error {
	p, err := s.Resolve(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(body), 0o644)
}

// Append adds body to an existing note (or creates it). When the existing
// file doesn't end in '\n', a newline is inserted between the old content
// and the new — keeps successive appends from running together.
func (s *Store) Append(name, body string) error {
	p, err := s.Resolve(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	existing, _ := os.ReadFile(p) // missing file -> empty bytes, no error
	var sep string
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		sep = "\n"
	}
	out := append(existing, []byte(sep+body)...)
	return os.WriteFile(p, out, 0o644)
}

// Delete removes a note. Returns os.ErrNotExist when the note is absent.
func (s *Store) Delete(name string) error {
	p, err := s.Resolve(name)
	if err != nil {
		return err
	}
	return os.Remove(p)
}

// SearchHit is one match returned by Search.
type SearchHit struct {
	Name string // root-relative note name
	Line int    // 1-based line number
	Text string // matched line, trimmed
}

// Search does a case-insensitive substring scan across every note. Returns
// up to maxHits hits (0 = unlimited). Cheap brute-force — fine for the
// expected note volume (dozens, not millions).
func (s *Store) Search(query string, maxHits int) ([]SearchHit, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil, errors.New("notes: search query is required")
	}
	names, err := s.List()
	if err != nil {
		return nil, err
	}
	var hits []SearchHit
	for _, n := range names {
		body, err := s.Read(n)
		if err != nil {
			continue
		}
		for i, line := range strings.Split(body, "\n") {
			if strings.Contains(strings.ToLower(line), q) {
				hits = append(hits, SearchHit{
					Name: n, Line: i + 1, Text: strings.TrimSpace(line),
				})
				if maxHits > 0 && len(hits) >= maxHits {
					return hits, nil
				}
			}
		}
	}
	return hits, nil
}

// Pinned returns the contents of PINNED.md, or "" when the file is absent
// or unreadable. Used by the agent's SystemPrompt to inject persistent
// context into every LLM call.
//
// On case-sensitive filesystems (Linux ext4, macOS APFS-cs) a user who
// creates `pinned.md` would otherwise get silent no-auto-load. We try the
// canonical name first (fast path; works on case-insensitive volumes for
// free) and fall back to a case-insensitive scan of the root if missing.
//
// Errors are deliberately swallowed: a missing pinned file is the common
// case and shouldn't break the agent. A corrupt/locked file is best
// reported via the dedicated Read("PINNED.md") path.
func (s *Store) Pinned() string {
	if body, err := s.Read(PinnedFilename); err == nil {
		return strings.TrimSpace(body)
	}
	// Fallback: case-insensitive scan of the root only (no recursion —
	// the convention is PINNED.md sits at the top).
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return ""
	}
	wantLower := strings.ToLower(PinnedFilename)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.ToLower(e.Name()) == wantLower {
			body, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
			if err != nil {
				return ""
			}
			return strings.TrimSpace(string(body))
		}
	}
	return ""
}

// SeedPinnedTemplate writes a starter PINNED.md if none exists. Returns
// (created, err) — created=false means the file was already there and was
// left untouched (notes are precious, never overwrite without intent).
func (s *Store) SeedPinnedTemplate() (bool, error) {
	p, err := s.Resolve(PinnedFilename)
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(p); err == nil {
		return false, nil // exists, leave it
	}
	tpl := `# Pinned memory

This file is automatically loaded into goon's system prompt every run, so
anything you write here is visible to the agent on every task.

Use it for facts that should always be top-of-mind:

- Conventions for this codebase / org
- Names of people, repos, services the agent should know about
- "Don't do this" rules learned the hard way
- Pointers to other notes worth reading on relevant tasks

Other notes live alongside this file as ` + "`*.md`" + ` and the agent can
read or write them with the memory_* tools. Edit this with:

    goon memory edit PINNED.md
`
	if err := s.Write(PinnedFilename, tpl); err != nil {
		return false, err
	}
	return true, nil
}
