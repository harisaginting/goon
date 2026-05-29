// Package chats persists web-chat threads to disk so the user can
// reopen, continue, delete, or distill prior conversations into a
// permanent knowledge note (./storage/memory/<slug>.md). Each thread
// is a self-contained JSON file under ./storage/chats/ — no central
// index file to coordinate, so threads are atomically discoverable
// just by listing the directory.
//
// Why this lives in its own package, not in internal/memory or
// internal/notes:
//   - internal/memory is for daemon runtime state (tickets, workflow
//     status, question queue). Mixing chat threads into it would
//     bloat the always-loaded memory.json that the daemon flushes
//     on every state change.
//   - internal/notes is for active markdown knowledge — files the
//     LLM reads on every run. Chats are conversation history, not
//     knowledge; we only promote a thread INTO notes when the user
//     explicitly asks via SaveAsNote.
//
// Each thread file is named "<id>.json"; id is a UTC timestamp in
// "20060102-150405-<rand>" form so the natural lexical sort of
// ls(1) is also a chronological sort.
package chats

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/harisaginting/goon/internal/llm"
	"github.com/harisaginting/goon/internal/storage"
)

// Thread is one saved conversation. Messages preserve the rolling
// transcript in the same {role, content} shape the LLM provider
// adapters speak, so reopening a thread is a verbatim resume.
type Thread struct {
	ID          string        `json:"id"`
	Title       string        `json:"title"`
	RepoContext string        `json:"repo_context,omitempty"` // optional "owner/repo" the conversation is scoped to
	Created     time.Time     `json:"created"`
	Updated     time.Time     `json:"updated"`
	Messages    []llm.Message `json:"messages"`
}

// Summary is the lightweight metadata view used by the sidebar list.
// Skips Messages so listing 100 threads doesn't load 100 transcripts
// into memory.
type Summary struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	RepoContext string    `json:"repo_context,omitempty"`
	Updated     time.Time `json:"updated"`
	MessageN    int       `json:"message_count"`
}

// dirName is the on-disk directory under storage.Root() where threads
// live. Exported as a constant so tests + docs can reference one
// source of truth.
const dirName = "chats"

// titleMaxLen caps the auto-derived title from the first user message
// so the sidebar entries stay one-line.
const titleMaxLen = 60

// mu serialises the directory-level operations (list / write / delete).
// Per-thread reads don't take the lock — the JSON file is the atomic
// unit, and a partially-written file is impossible because Write uses
// tmp + rename.
var mu sync.Mutex

// Dir returns the resolved directory path. Created on first use by
// Write; List returns nil cleanly when the directory is missing
// (fresh installs).
func Dir() string {
	return storage.Path(dirName)
}

// NewID returns a fresh thread id — UTC timestamp + 4 random hex
// chars so two threads created in the same second don't collide.
func NewID() string {
	stamp := time.Now().UTC().Format("20060102-150405")
	var b [2]byte
	_, _ = rand.Read(b[:])
	return stamp + "-" + hex.EncodeToString(b[:])
}

// DeriveTitle pulls a one-line title out of the first user message.
// Falls back to a generic "Chat (<timestamp>)" when the message is
// empty or only whitespace.
func DeriveTitle(messages []llm.Message, fallback time.Time) string {
	for _, m := range messages {
		if m.Role != "user" {
			continue
		}
		text := strings.TrimSpace(m.Content)
		// First newline-delimited line; many chat prompts start with a
		// directive line and a body, the title should be the directive.
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = strings.TrimSpace(text[:i])
		}
		if text == "" {
			continue
		}
		if len(text) > titleMaxLen {
			text = strings.TrimRight(text[:titleMaxLen], " \t") + "…"
		}
		return text
	}
	return "Chat " + fallback.UTC().Format("Jan 2 15:04")
}

// Write persists the thread to disk atomically (tmp + rename). Updates
// the Updated timestamp to time.Now and auto-derives the title when
// one isn't set. Safe to call on every chat turn — small JSON, local
// disk write.
func Write(t Thread) error {
	if strings.TrimSpace(t.ID) == "" {
		return errors.New("chats: missing thread id")
	}
	mu.Lock()
	defer mu.Unlock()
	if t.Created.IsZero() {
		t.Created = time.Now()
	}
	t.Updated = time.Now()
	if strings.TrimSpace(t.Title) == "" {
		t.Title = DeriveTitle(t.Messages, t.Created)
	}
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		return fmt.Errorf("chats: mkdir: %w", err)
	}
	target := filepath.Join(Dir(), t.ID+".json")
	tmp, err := os.CreateTemp(Dir(), ".chat.*.tmp")
	if err != nil {
		return fmt.Errorf("chats: tmp: %w", err)
	}
	defer func() {
		// best-effort cleanup if rename fails
		if _, statErr := os.Stat(tmp.Name()); statErr == nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(t); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chats: encode: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("chats: close tmp: %w", err)
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return fmt.Errorf("chats: rename: %w", err)
	}
	return nil
}

// Read loads a thread by id. Returns os.ErrNotExist for unknown ids
// so callers can use errors.Is(err, os.ErrNotExist) for the missing
// case without parsing the underlying file-system error string.
func Read(id string) (Thread, error) {
	var t Thread
	if strings.TrimSpace(id) == "" {
		return t, errors.New("chats: empty id")
	}
	// Guard against path traversal — id is user input on the read
	// path; we treat anything with "/" or ".." as invalid.
	if strings.ContainsAny(id, "/\\") || strings.Contains(id, "..") {
		return t, fmt.Errorf("chats: invalid id %q", id)
	}
	data, err := os.ReadFile(filepath.Join(Dir(), id+".json"))
	if err != nil {
		return t, err
	}
	if err := json.Unmarshal(data, &t); err != nil {
		return t, fmt.Errorf("chats: parse %s: %w", id, err)
	}
	return t, nil
}

// Delete removes a thread file. No-op (nil error) when the thread
// is already gone, so DELETE-style endpoints can be idempotent.
func Delete(id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("chats: empty id")
	}
	if strings.ContainsAny(id, "/\\") || strings.Contains(id, "..") {
		return fmt.Errorf("chats: invalid id %q", id)
	}
	mu.Lock()
	defer mu.Unlock()
	err := os.Remove(filepath.Join(Dir(), id+".json"))
	if err != nil && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// List returns thread summaries sorted by Updated descending (newest
// first). Walks the directory, opens each file just enough to extract
// the metadata fields — does NOT load the full Messages array, so a
// 1000-thread directory still lists in milliseconds.
func List() ([]Summary, error) {
	entries, err := os.ReadDir(Dir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("chats: readdir: %w", err)
	}
	out := make([]Summary, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		data, rerr := os.ReadFile(filepath.Join(Dir(), e.Name()))
		if rerr != nil {
			continue // skip unreadable files rather than failing the whole list
		}
		var t Thread
		if err := json.Unmarshal(data, &t); err != nil {
			continue
		}
		out = append(out, Summary{
			ID:          id,
			Title:       t.Title,
			RepoContext: t.RepoContext,
			Updated:     t.Updated,
			MessageN:    len(t.Messages),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out, nil
}

// SaveAsNote distils a thread into a markdown file under the notes
// store so it gets injected into future LLM context. Caller supplies
// a kebab-case name (without .md extension). Returns the path the
// note was written to so the UI can hint at where it lives.
//
// Format: H1 with the thread title + a meta line + each message as a
// blockquote with the role as a small caps label. Plain markdown; the
// active notes store reads anything under ./storage/memory/.
func SaveAsNote(id, name string) (string, error) {
	t, err := Read(id)
	if err != nil {
		return "", err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		// Fall back to a slug of the title.
		name = slugify(t.Title)
	}
	if !strings.HasSuffix(strings.ToLower(name), ".md") {
		name += ".md"
	}
	// Path safety: same rules as the notes store — no slashes, no
	// parent-dir traversal. Anything funny → reject.
	if strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return "", fmt.Errorf("chats: invalid note name %q", name)
	}
	target := storage.Path("memory", name)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", fmt.Errorf("chats: mkdir: %w", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", t.Title)
	fmt.Fprintf(&b, "_Saved from chat thread `%s` on %s._\n",
		t.ID, time.Now().UTC().Format("2006-01-02 15:04 UTC"))
	if t.RepoContext != "" {
		fmt.Fprintf(&b, "_Repo context: `%s`._\n", t.RepoContext)
	}
	b.WriteString("\n---\n\n")
	for _, m := range t.Messages {
		label := strings.ToUpper(m.Role)
		fmt.Fprintf(&b, "### %s\n\n%s\n\n", label, strings.TrimSpace(m.Content))
	}
	if err := os.WriteFile(target, []byte(b.String()), 0o644); err != nil {
		return "", fmt.Errorf("chats: write note: %w", err)
	}
	return target, nil
}

// slugify turns a free-text title into a kebab-case file-safe slug.
// Restricted to [a-z0-9-]; runs of non-allowed chars collapse to a
// single dash; leading/trailing dashes are stripped.
func slugify(s string) string {
	var b strings.Builder
	prevDash := true
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "chat"
	}
	return out
}

