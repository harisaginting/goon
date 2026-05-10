package notes

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newStore is a test helper that opens a Store against a fresh temp dir,
// shielding tests from each other and from the user's real ~/.goon.
func newStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("GOON_MEMORY_DIR", "")
	t.Setenv("HOME", dir) // belt-and-braces in case env wins
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// TestNew_FallsBackToStorageRoot covers the new default: when neither
// the explicit dir nor GOON_MEMORY_DIR is set, notes lives under
// <storage.Root()>/memory. Per-project, gitignore-friendly. The old
// ~/.goon/memory fallback is gone.
func TestNew_FallsBackToStorageRoot(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("GOON_MEMORY_DIR", "")
	t.Setenv("GOON_STORAGE_DIR", tmp)
	t.Setenv("HOME", "/should-not-be-consulted")
	s, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	want := filepath.Join(tmp, "memory")
	if filepath.Clean(s.Path()) != filepath.Clean(want) {
		t.Errorf("Path: got %q want %q", s.Path(), want)
	}
}

func TestNew_RespectsEnvOverride(t *testing.T) {
	tmp := t.TempDir()
	override := filepath.Join(tmp, "elsewhere")
	t.Setenv("GOON_MEMORY_DIR", override)
	s, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.Path() != override {
		t.Errorf("Path: got %q want %q", s.Path(), override)
	}
	if _, err := os.Stat(override); err != nil {
		t.Errorf("memory dir not created: %v", err)
	}
}

// TestResolve_Safety covers the path-traversal attack surface: any name
// the LLM might emit gets sanitized to an absolute path inside the store
// or rejected outright.
func TestResolve_Safety(t *testing.T) {
	s := newStore(t)
	cases := []struct {
		name    string
		wantErr bool
	}{
		{"", true},
		{"   ", true},
		{"normal.md", false},
		{"NoExtension", false}, // .md is auto-appended
		{"with/subdir.md", false},
		{"../escape.md", true},
		{"a/../../escape.md", true},
		{"/etc/passwd", true},        // absolute
		{"/tmp/foo.md", true},        // absolute
		{"ok/../still-ok.md", true},  // contains ".." segment, refused
		{"weird name with spaces.md", false},
	}
	for _, c := range cases {
		_, err := s.Resolve(c.name)
		if (err != nil) != c.wantErr {
			t.Errorf("Resolve(%q) err=%v wantErr=%v", c.name, err, c.wantErr)
		}
	}
}

func TestRoundTrip_WriteReadList(t *testing.T) {
	s := newStore(t)
	if err := s.Write("alpha", "hello\n"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := s.Write("dir/beta.md", "world"); err != nil {
		t.Fatalf("Write subdir: %v", err)
	}
	got, err := s.Read("alpha")
	if err != nil || got != "hello\n" {
		t.Errorf("Read alpha: got=%q err=%v", got, err)
	}
	got, err = s.Read("dir/beta")
	if err != nil || got != "world" {
		t.Errorf("Read dir/beta: got=%q err=%v", got, err)
	}
	names, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"alpha.md", "dir/beta.md"}
	if !equalStringSlice(names, want) {
		t.Errorf("List: got %v want %v", names, want)
	}
}

func TestAppend_AddsNewlineWhenMissing(t *testing.T) {
	s := newStore(t)
	if err := s.Write("log", "line1"); err != nil { // no trailing newline
		t.Fatalf("Write: %v", err)
	}
	if err := s.Append("log", "line2"); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, _ := s.Read("log")
	if got != "line1\nline2" {
		t.Errorf("Append: got %q want %q", got, "line1\nline2")
	}
}

func TestAppend_NoExtraNewlineWhenAlreadyTerminated(t *testing.T) {
	s := newStore(t)
	if err := s.Write("log", "line1\n"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := s.Append("log", "line2"); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, _ := s.Read("log")
	if got != "line1\nline2" {
		t.Errorf("Append: got %q want %q", got, "line1\nline2")
	}
}

func TestAppend_CreatesIfMissing(t *testing.T) {
	s := newStore(t)
	if err := s.Append("fresh", "first content"); err != nil {
		t.Fatalf("Append fresh: %v", err)
	}
	got, _ := s.Read("fresh")
	if got != "first content" {
		t.Errorf("Append fresh: got %q", got)
	}
}

func TestSearch_FindsMatchesWithLineNumbers(t *testing.T) {
	s := newStore(t)
	_ = s.Write("a", "alpha line\nshared term\nbeta line")
	_ = s.Write("b", "another file\nwith shared term too")
	hits, err := s.Search("shared", 0)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits: got %d want 2 (%+v)", len(hits), hits)
	}
	if hits[0].Name != "a.md" || hits[0].Line != 2 {
		t.Errorf("hit[0]: %+v", hits[0])
	}
	if hits[1].Name != "b.md" || hits[1].Line != 2 {
		t.Errorf("hit[1]: %+v", hits[1])
	}
}

func TestSearch_CaseInsensitive(t *testing.T) {
	s := newStore(t)
	_ = s.Write("a", "FooBar")
	hits, _ := s.Search("foobar", 0)
	if len(hits) != 1 {
		t.Errorf("case-insensitive search failed: %+v", hits)
	}
}

func TestSearch_RespectsMaxHits(t *testing.T) {
	s := newStore(t)
	_ = s.Write("a", "x\nx\nx\nx\nx")
	hits, _ := s.Search("x", 2)
	if len(hits) != 2 {
		t.Errorf("maxHits ignored: got %d want 2", len(hits))
	}
}

func TestPinned_EmptyWhenAbsent(t *testing.T) {
	s := newStore(t)
	if got := s.Pinned(); got != "" {
		t.Errorf("Pinned with no file: got %q want empty", got)
	}
}

func TestPinned_ReadsContent(t *testing.T) {
	s := newStore(t)
	_ = s.Write(PinnedFilename, "remember this\n")
	if got := s.Pinned(); got != "remember this" {
		t.Errorf("Pinned: got %q", got)
	}
}

func TestSeedPinnedTemplate(t *testing.T) {
	s := newStore(t)
	created, err := s.SeedPinnedTemplate()
	if err != nil || !created {
		t.Fatalf("SeedPinnedTemplate: created=%v err=%v", created, err)
	}
	body, _ := s.Read(PinnedFilename)
	if !strings.Contains(body, "Pinned memory") {
		t.Errorf("template missing header: %q", body)
	}
	// Second call should NOT overwrite.
	created2, err := s.SeedPinnedTemplate()
	if err != nil || created2 {
		t.Errorf("SeedPinnedTemplate idempotency: created=%v err=%v", created2, err)
	}
}

func TestList_SkipsHiddenAndNonMd(t *testing.T) {
	s := newStore(t)
	_ = os.WriteFile(filepath.Join(s.Path(), "visible.md"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(s.Path(), ".hidden.md"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(s.Path(), "not_a_note.txt"), []byte("x"), 0o644)
	names, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !equalStringSlice(names, []string{"visible.md"}) {
		t.Errorf("List: got %v", names)
	}
}

func TestDelete_NonexistentReturnsError(t *testing.T) {
	s := newStore(t)
	err := s.Delete("ghost")
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Delete missing: got %v want ErrNotExist", err)
	}
}

// TestPinned_CaseInsensitiveFallback ensures that a user who writes
// pinned.md / Pinned.md / etc on a case-sensitive filesystem still gets
// the auto-load behaviour, instead of silently losing it. Reasonable
// since the whole point of PINNED is to be discoverable.
func TestPinned_CaseInsensitiveFallback(t *testing.T) {
	cases := []string{"pinned.md", "Pinned.md", "PiNnEd.Md"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			s := newStore(t)
			// Write directly with the exact case the user typed — bypass
			// Resolve, which would normalize to PINNED.md only on
			// case-insensitive volumes.
			body := "stay sharp"
			if err := os.WriteFile(filepath.Join(s.Path(), name), []byte(body), 0o644); err != nil {
				t.Fatalf("seed %s: %v", name, err)
			}
			got := s.Pinned()
			if got != body {
				t.Errorf("Pinned() = %q, want %q (file: %s)", got, body, name)
			}
		})
	}
}

// TestResolve_RejectsSymlinkEscape covers the audit's "symlink slips
// through Rel check" finding. A symlink inside the store pointing
// outside it must be rejected, not silently followed.
func TestResolve_RejectsSymlinkEscape(t *testing.T) {
	if runtimeIsWindows() {
		t.Skip("symlink creation requires admin on Windows; skip")
	}
	s := newStore(t)
	outside := t.TempDir()
	link := filepath.Join(s.Path(), "evil")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// Resolve a child of the symlinked dir should be rejected.
	if _, err := s.Resolve("evil/note.md"); err == nil {
		t.Error("Resolve should reject path through outward-pointing symlink")
	}
}

// runtimeIsWindows is split out so the symlink test can skip without an
// extra import on the happy path.
func runtimeIsWindows() bool {
	return filepath.Separator == '\\'
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
