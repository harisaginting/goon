package tools

import (
	"context"
	"os"
	"strings"
	"testing"
)

func osWriteFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

func TestParseToolCall_Plain(t *testing.T) {
	in := `{"tool":"run_command","args":{"command":"ls -la"}}`
	got, err := ParseToolCall(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Tool != "run_command" || got.Args["command"] != "ls -la" {
		t.Fatalf("bad parse: %+v", got)
	}
}

func TestParseToolCall_FencedAndProse(t *testing.T) {
	in := "Sure, here you go:\n```json\n{\"tool\":\"finish\",\"args\":{\"message\":\"done\"}}\n```\nbye"
	got, err := ParseToolCall(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Tool != "finish" {
		t.Fatalf("bad parse: %+v", got)
	}
}

func TestParseToolCall_RejectEmptyTool(t *testing.T) {
	in := `{"tool":"","args":{"x":"y"}}`
	if _, err := ParseToolCall(in); err == nil {
		t.Fatal("expected error on empty tool")
	}
}

func TestParseToolCall_NoJSON(t *testing.T) {
	if _, err := ParseToolCall("just talking, no JSON here"); err == nil {
		t.Fatal("expected error when no JSON present")
	}
}

func TestParseToolCall_NestedBraces(t *testing.T) {
	in := `prefix {"tool":"run_command","args":{"command":"echo {hello}"}} suffix`
	got, err := ParseToolCall(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Args["command"] != "echo {hello}" {
		t.Fatalf("bad command: %q", got.Args["command"])
	}
}

func TestRegistry_DefaultsHaveAllExpectedTools(t *testing.T) {
	reg := DefaultRegistry()
	expected := []string{
		"run_command", "read_file", "list_dir", "finish",
		"confluence", "telegram",
		// memory_* are part of the default registry — every agent run
		// gets persistent-memory access without extra wiring.
		"memory_list", "memory_read", "memory_write",
		"memory_append", "memory_search",
	}
	for _, name := range expected {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("missing tool %q", name)
		}
	}
	m := reg.Manifest()
	if !strings.Contains(m, "run_command") || !strings.Contains(m, "telegram") || !strings.Contains(m, "memory_write") {
		t.Fatalf("manifest missing tools:\n%s", m)
	}
}

// TestMemoryTools_RoundTrip drives all five memory tools end-to-end via
// the Tool interface — the same surface the LLM hits — to verify they
// share state through the underlying notes.Store.
func TestMemoryTools_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GOON_MEMORY_DIR", dir)
	t.Setenv("HOME", dir)

	reg := DefaultRegistry()
	w, _ := reg.Get("memory_write")
	a, _ := reg.Get("memory_append")
	r, _ := reg.Get("memory_read")
	l, _ := reg.Get("memory_list")
	s, _ := reg.Get("memory_search")
	ctx := context.Background()

	if _, err := w.Run(ctx, map[string]string{"name": "first", "content": "hello\n"}); err != nil {
		t.Fatalf("memory_write: %v", err)
	}
	if _, err := a.Run(ctx, map[string]string{"name": "first", "content": "world"}); err != nil {
		t.Fatalf("memory_append: %v", err)
	}
	got, err := r.Run(ctx, map[string]string{"name": "first"})
	if err != nil || got.Stdout != "hello\nworld" {
		t.Errorf("memory_read after write+append: stdout=%q err=%v", got.Stdout, err)
	}
	listOut, _ := l.Run(ctx, nil)
	if !strings.Contains(listOut.Stdout, "first.md") {
		t.Errorf("memory_list missing first.md: %q", listOut.Stdout)
	}
	hits, _ := s.Run(ctx, map[string]string{"query": "world"})
	if !strings.Contains(hits.Stdout, "first.md:") {
		t.Errorf("memory_search missing hit: %q", hits.Stdout)
	}
}

// TestMemoryWrite_RejectsEmptyContent guards against the LLM accidentally
// nuking a note by calling memory_write with no content.
func TestMemoryWrite_RejectsEmptyContent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GOON_MEMORY_DIR", dir)
	t.Setenv("HOME", dir)
	w, _ := DefaultRegistry().Get("memory_write")
	_, err := w.Run(context.Background(), map[string]string{"name": "x", "content": ""})
	if err == nil {
		t.Error("expected error on empty content")
	}
}

// TestMemoryRead_MissingNote returns a friendly placeholder, not an
// error — the LLM should be able to probe for a note's existence
// without poisoning the conversation with a tool failure.
func TestMemoryRead_MissingNote(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GOON_MEMORY_DIR", dir)
	t.Setenv("HOME", dir)
	r, _ := DefaultRegistry().Get("memory_read")
	res, err := r.Run(context.Background(), map[string]string{"name": "ghost"})
	if err != nil {
		t.Fatalf("missing note should not error: %v", err)
	}
	if !strings.Contains(res.Stdout, "no such note") {
		t.Errorf("expected friendly placeholder, got %q", res.Stdout)
	}
}

func TestListDir_Stdout(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := osWriteFile(dir+"/foo.txt", "hi"); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	res, err := (&ListDir{}).Run(context.Background(), map[string]string{"path": dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res.Stdout, "foo.txt") {
		t.Fatalf("expected foo.txt in listing, got %q", res.Stdout)
	}
}

func TestListDir_EmptyDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	res, err := (&ListDir{}).Run(context.Background(), map[string]string{"path": dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Stdout != "" {
		t.Fatalf("expected empty listing for empty dir, got %q", res.Stdout)
	}
}

func TestRunCommand_RequiresArg(t *testing.T) {
	_, err := (&RunCommand{}).Run(context.Background(), map[string]string{})
	if err == nil {
		t.Fatal("expected error when command arg is missing")
	}
}

func TestFinish_DefaultMessage(t *testing.T) {
	res, err := (&Finish{}).Run(context.Background(), map[string]string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Stdout != "done" {
		t.Fatalf("expected 'done', got %q", res.Stdout)
	}
}
