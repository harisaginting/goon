package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/harisaginting/goon/internal/logx"
	"github.com/harisaginting/goon/internal/notes"
)

// memoryTool is shared state for the five LLM-facing memory_* tools. Each
// tool is its own type (so Schema/Description can be tailored), but all
// share one *notes.Store via this embedded struct so they touch the same
// directory. Per-call New() would be wasteful and would dance around the
// MkdirAll cost on every tool call.
type memoryTool struct {
	store *notes.Store
}

// newMemoryTool builds a memoryTool, lazily opening the notes store. If
// the store can't be opened the tool returns an error on every Run() so
// the failure is visible to the agent (instead of silently dropping notes).
func newMemoryTool() *memoryTool {
	s, err := notes.New("")
	if err != nil {
		logx.Warn("memory.tool_init_failed", "error", err.Error())
		// Return a tool with nil store; Run() guards on it.
		return &memoryTool{store: nil}
	}
	return &memoryTool{store: s}
}

func (m *memoryTool) require() error {
	if m.store == nil {
		return errors.New("memory store unavailable (check $GOON_MEMORY_DIR perms or set $HOME)")
	}
	return nil
}

// MemoryList lists every note name in the store.
type MemoryList struct{ *memoryTool }

func (*MemoryList) Name() string { return "memory_list" }
func (*MemoryList) Description() string {
	return "list every persistent memory note (markdown files in the goon memory dir). Returns one name per line."
}
func (*MemoryList) Schema() map[string]string { return map[string]string{} }

func (m *MemoryList) Run(_ context.Context, _ map[string]string) (Result, error) {
	if err := m.require(); err != nil {
		return Result{ToolName: "memory_list", Err: err}, err
	}
	names, err := m.store.List()
	if err != nil {
		return Result{ToolName: "memory_list", Err: err}, err
	}
	if len(names) == 0 {
		return Result{ToolName: "memory_list", Stdout: "(no notes yet — write one with memory_write)\n"}, nil
	}
	var b strings.Builder
	for _, n := range names {
		// Annotate the special files so the agent knows what each is
		// for. SOUL/PINNED auto-load into the system prompt;
		// REPOSITORY/HISTORY have dedicated roles the agent should
		// treat carefully (don't overwrite the history log; respect
		// the table format in REPOSITORY).
		switch n {
		case notes.SoulFilename, "PINNED.md":
			b.WriteString(n + "  (auto-loaded into system prompt)\n")
		case "REPOSITORY.md":
			b.WriteString(n + "  (repo registry — table of remote→local mappings)\n")
		case "HISTORY.md":
			b.WriteString(n + "  (chronological run log — append-only; goon manages it)\n")
		default:
			b.WriteString(n + "\n")
		}
	}
	return Result{ToolName: "memory_list", Stdout: b.String()}, nil
}

// MemoryRead returns the contents of one note.
type MemoryRead struct{ *memoryTool }

func (*MemoryRead) Name() string { return "memory_read" }
func (*MemoryRead) Description() string {
	return "read the contents of a persistent memory note by name (e.g. \"SOUL.md\", \"HISTORY.md\", or \"learnings/regex.md\")"
}
func (*MemoryRead) Schema() map[string]string {
	return map[string]string{"name": "note name (relative path under the memory dir, .md auto-appended)"}
}

func (m *MemoryRead) Run(_ context.Context, args map[string]string) (Result, error) {
	if err := m.require(); err != nil {
		return Result{ToolName: "memory_read", Err: err}, err
	}
	name := args["name"]
	body, err := m.store.Read(name)
	if err != nil {
		if os.IsNotExist(err) {
			return Result{ToolName: "memory_read", Stdout: "(no such note: " + name + ")"}, nil
		}
		return Result{ToolName: "memory_read", Err: err}, err
	}
	// Cap stdout at 64KB to keep tool output bounded for the LLM.
	const maxBytes = 64 * 1024
	if len(body) > maxBytes {
		body = body[:maxBytes] + fmt.Sprintf("\n…(truncated; full note is %d bytes)", len(body))
	}
	return Result{ToolName: "memory_read", Stdout: body}, nil
}

// MemoryWrite replaces a note (creates if missing).
type MemoryWrite struct{ *memoryTool }

func (*MemoryWrite) Name() string { return "memory_write" }
func (*MemoryWrite) Description() string {
	return "create or REPLACE a persistent memory note. Use memory_append to add to an existing note instead of overwriting."
}
func (*MemoryWrite) Schema() map[string]string {
	return map[string]string{
		"name":    "note name (relative, .md auto-appended)",
		"content": "full markdown body to write — REPLACES any existing content",
	}
}

func (m *MemoryWrite) Run(_ context.Context, args map[string]string) (Result, error) {
	if err := m.require(); err != nil {
		return Result{ToolName: "memory_write", Err: err}, err
	}
	name := args["name"]
	content := args["content"]
	if content == "" {
		return Result{ToolName: "memory_write"}, errors.New("memory_write: \"content\" is required (use memory_delete to remove a note)")
	}
	// Block direct agent writes to SOUL.md / PINNED.md (the system-prompt
	// file). An agent that can freely overwrite its own system prompt is a
	// prompt-injection target: a poisoned ticket could instruct the agent to
	// write malicious instructions into SOUL.md, making them persistent.
	// Agents should write observations to topic notes (e.g.
	// "learnings/topic.md") which the user can review; SOUL.md is
	// user-maintained. Set GOON_ALLOW_SOUL_WRITE=1 to opt out (e.g. when
	// running goon trust as an admin to update its own context).
	if isSoulFile(name) && !allowSoulWrite() {
		e := fmt.Errorf("memory_write: writing directly to %s is restricted — "+
			"write observations to a topic note (e.g. learnings/topic.md) instead. "+
			"Set GOON_ALLOW_SOUL_WRITE=1 to override", name)
		return Result{ToolName: "memory_write", Err: e}, e
	}
	if err := m.store.Write(name, content); err != nil {
		return Result{ToolName: "memory_write", Err: err}, err
	}
	logx.Info("memory.write", "name", name, "bytes", len(content))
	return Result{ToolName: "memory_write", Stdout: fmt.Sprintf("wrote %d bytes to %s", len(content), name)}, nil
}

// MemoryAppend adds to an existing note (or creates one).
type MemoryAppend struct{ *memoryTool }

func (*MemoryAppend) Name() string { return "memory_append" }
func (*MemoryAppend) Description() string {
	return "append to a persistent memory note (creates the note if missing). Preferred over memory_write for adding new observations."
}
func (*MemoryAppend) Schema() map[string]string {
	return map[string]string{
		"name":    "note name",
		"content": "text to append (a newline is auto-inserted between old and new content)",
	}
}

func (m *MemoryAppend) Run(_ context.Context, args map[string]string) (Result, error) {
	if err := m.require(); err != nil {
		return Result{ToolName: "memory_append", Err: err}, err
	}
	name := args["name"]
	content := args["content"]
	if content == "" {
		return Result{ToolName: "memory_append"}, errors.New("memory_append: \"content\" is required")
	}
	// Same protection as memory_write — appending to SOUL.md is also blocked
	// because a sequence of small appends is equally effective for injection.
	if isSoulFile(name) && !allowSoulWrite() {
		e := fmt.Errorf("memory_append: appending to %s is restricted — "+
			"write observations to a topic note (e.g. learnings/topic.md) instead. "+
			"Set GOON_ALLOW_SOUL_WRITE=1 to override", name)
		return Result{ToolName: "memory_append", Err: e}, e
	}
	if err := m.store.Append(name, content); err != nil {
		return Result{ToolName: "memory_append", Err: err}, err
	}
	logx.Info("memory.append", "name", name, "bytes", len(content))
	return Result{ToolName: "memory_append", Stdout: fmt.Sprintf("appended %d bytes to %s", len(content), name)}, nil
}

// MemorySearch greps the notes for a substring.
type MemorySearch struct{ *memoryTool }

func (*MemorySearch) Name() string { return "memory_search" }
func (*MemorySearch) Description() string {
	return "case-insensitive substring search across every persistent memory note. Returns matching note:line: text lines."
}
func (*MemorySearch) Schema() map[string]string {
	return map[string]string{"query": "substring to search for"}
}

func (m *MemorySearch) Run(_ context.Context, args map[string]string) (Result, error) {
	if err := m.require(); err != nil {
		return Result{ToolName: "memory_search", Err: err}, err
	}
	query := args["query"]
	hits, err := m.store.Search(query, 50)
	if err != nil {
		return Result{ToolName: "memory_search", Err: err}, err
	}
	if len(hits) == 0 {
		return Result{ToolName: "memory_search", Stdout: "(no matches)"}, nil
	}
	var b strings.Builder
	for _, h := range hits {
		fmt.Fprintf(&b, "%s:%d: %s\n", h.Name, h.Line, h.Text)
	}
	return Result{ToolName: "memory_search", Stdout: b.String()}, nil
}

// isSoulFile reports whether name resolves to SOUL.md or the legacy PINNED.md.
// Path normalisation strips leading slashes and a single trailing ".md" so
// the check catches "SOUL", "SOUL.md", "soul.md", etc.
func isSoulFile(name string) bool {
	base := strings.ToUpper(strings.TrimSuffix(strings.TrimSpace(name), ".md"))
	return base == "SOUL" || base == "PINNED"
}

// allowSoulWrite reports whether the GOON_ALLOW_SOUL_WRITE env var is set to
// a truthy value, letting operators (not the agent) bypass the SOUL.md guard.
func allowSoulWrite() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("GOON_ALLOW_SOUL_WRITE")))
	return v == "1" || v == "true" || v == "yes"
}

// RegisterMemoryTools attaches the five memory_* tools to a registry,
// sharing one Store. Called from DefaultRegistry().
func RegisterMemoryTools(r *Registry) {
	mt := newMemoryTool()
	r.Register(&MemoryList{mt})
	r.Register(&MemoryRead{mt})
	r.Register(&MemoryWrite{mt})
	r.Register(&MemoryAppend{mt})
	r.Register(&MemorySearch{mt})
}
