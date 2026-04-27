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
	for _, name := range []string{"run_command", "read_file", "list_dir", "finish", "confluence", "telegram"} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("missing tool %q", name)
		}
	}
	m := reg.Manifest()
	if !strings.Contains(m, "run_command") || !strings.Contains(m, "telegram") {
		t.Fatalf("manifest missing tools:\n%s", m)
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
