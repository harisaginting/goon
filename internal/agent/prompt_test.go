package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/harisaginting/goon/internal/notes"
	"github.com/harisaginting/goon/internal/tools"
)

// TestSystemPrompt_NoSoulFile_NoBlock confirms that with no SOUL.md
// the prompt has no PERSISTENT MEMORY section — we must not leak fake
// header bytes into every prompt when the user hasn't set anything up.
func TestSystemPrompt_NoSoulFile_NoBlock(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GOON_MEMORY_DIR", dir)
	t.Setenv("HOME", dir)

	got := SystemPrompt(tools.DefaultRegistry())
	if strings.Contains(got, "PERSISTENT MEMORY") {
		t.Errorf("prompt should not contain PERSISTENT MEMORY block when SOUL.md is absent:\n%s", got)
	}
	// Sanity: the memory_* tool how-to should still be there since the tools
	// are always available.
	if !strings.Contains(got, "MEMORY TOOLS:") {
		t.Errorf("prompt missing MEMORY TOOLS section:\n%s", got)
	}
}

// TestSystemPrompt_WithSoulFile_Injects asserts the actual content of
// SOUL.md ends up in the model's system prompt verbatim. This is the
// load-bearing mechanism for "agent remembers across runs".
func TestSystemPrompt_WithSoulFile_Injects(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GOON_MEMORY_DIR", dir)
	t.Setenv("HOME", dir)

	soulBody := "Always run `make verify` before opening a PR.\nThe prod DB is in eu-west-1."
	if err := os.WriteFile(filepath.Join(dir, notes.SoulFilename), []byte(soulBody), 0o644); err != nil {
		t.Fatalf("write soul: %v", err)
	}

	got := SystemPrompt(tools.DefaultRegistry())
	if !strings.Contains(got, "PERSISTENT MEMORY") {
		t.Errorf("prompt missing PERSISTENT MEMORY header:\n%s", got)
	}
	if !strings.Contains(got, "Always run `make verify`") {
		t.Errorf("prompt missing first soul line:\n%s", got)
	}
	if !strings.Contains(got, "eu-west-1") {
		t.Errorf("prompt missing second soul line:\n%s", got)
	}
}

// TestSystemPrompt_EmptySoulFile_NoBlock: a file that exists but is
// effectively empty (whitespace only) should not produce a banner with
// no body — that would just confuse the model.
func TestSystemPrompt_EmptySoulFile_NoBlock(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GOON_MEMORY_DIR", dir)
	t.Setenv("HOME", dir)

	if err := os.WriteFile(filepath.Join(dir, notes.SoulFilename), []byte("\n\n  \n"), 0o644); err != nil {
		t.Fatalf("write soul: %v", err)
	}

	got := SystemPrompt(tools.DefaultRegistry())
	if strings.Contains(got, "PERSISTENT MEMORY") {
		t.Errorf("prompt should not include header for whitespace-only SOUL.md:\n%s", got)
	}
}

// TestSystemPrompt_LegacyPinnedFile_StillInjects: someone upgrading
// from the pre-rename build has PINNED.md but no SOUL.md — we must
// still inject the contents so they don't silently lose context.
func TestSystemPrompt_LegacyPinnedFile_StillInjects(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GOON_MEMORY_DIR", dir)
	t.Setenv("HOME", dir)

	if err := os.WriteFile(filepath.Join(dir, "PINNED.md"), []byte("legacy soul body"), 0o644); err != nil {
		t.Fatalf("write legacy: %v", err)
	}
	got := SystemPrompt(tools.DefaultRegistry())
	if !strings.Contains(got, "legacy soul body") {
		t.Errorf("prompt should still inject legacy PINNED.md when SOUL.md is absent:\n%s", got)
	}
}
