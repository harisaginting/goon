package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/harisaginting/goon/internal/boards"
)

// TestDiscoverWorkspaceRepos seeds a fake workspace with mixed
// content (real repos, non-repo dirs, hidden dirs, a regular file)
// and verifies that only the real repos come back, alphabetised.
func TestDiscoverWorkspaceRepos(t *testing.T) {
	dir := t.TempDir()
	mustRepo := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Join(p, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mustDir := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}

	r1 := mustRepo("alpha-app")
	r2 := mustRepo("beta-svc")
	mustDir("not-a-repo")     // no .git → ignored
	mustDir(".hidden")        // hidden → ignored
	if err := os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	} // not a dir → ignored

	t.Setenv("GOON_WORKSPACE_DIR", dir)
	got := DiscoverWorkspaceRepos()
	want := []string{r1, r2}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d] got %q, want %q", i, got[i], want[i])
		}
	}
}

// TestDiscoverWorkspaceRepos_NoEnv returns nil cleanly when the env
// var isn't set, so callers can fall back to typed-path mode.
func TestDiscoverWorkspaceRepos_NoEnv(t *testing.T) {
	t.Setenv("GOON_WORKSPACE_DIR", "")
	if got := DiscoverWorkspaceRepos(); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

// TestBuildRepoGateQuestion_WithMenu verifies the rendered menu
// includes a numbered line per repo and marks the suggested one
// with "*". The web layer's renderRepoPickButtons keys off this
// exact shape.
func TestBuildRepoGateQuestion_WithMenu(t *testing.T) {
	ticket := boards.Ticket{Key: "ENG-1", Title: "fix flaky test"}
	repos := []repoCandidate{
		{Label: "alpha", Value: "/ws/alpha"},
		{Label: "beta", Value: "/ws/beta"},
		{Label: "gamma", Value: "/ws/gamma"},
		{Label: "owner/remote-svc", Value: "owner/remote-svc", IsRemote: true},
	}
	q := buildRepoGateQuestion(ticket, "/ws/beta", repos)
	if !strings.Contains(q, "1. alpha") {
		t.Errorf("missing alpha entry: %s", q)
	}
	if !strings.Contains(q, "* 2. beta") {
		t.Errorf("suggested marker missing: %s", q)
	}
	if !strings.Contains(q, "3. gamma") {
		t.Errorf("missing gamma entry: %s", q)
	}
	if !strings.Contains(q, "4. owner/remote-svc (remote)") {
		t.Errorf("missing remote tag: %s", q)
	}
	if !strings.Contains(q, "<n>") {
		t.Errorf("missing reply hint: %s", q)
	}
}

// TestBuildRepoGateQuestion_NoMenu falls back to the original prompt
// when no candidates are configured.
func TestBuildRepoGateQuestion_NoMenu(t *testing.T) {
	ticket := boards.Ticket{Key: "ENG-1", Title: "fix flaky test"}
	q := buildRepoGateQuestion(ticket, "/some/path", nil)
	if strings.Contains(q, "Available repos:") {
		t.Errorf("expected no menu, got: %s", q)
	}
	if !strings.Contains(q, "Suggested: /some/path") {
		t.Errorf("expected suggested path: %s", q)
	}
}

// TestPickWorkspaceRepo covers the legacy single-pick parser kept
// for backward compatibility.
func TestPickWorkspaceRepo(t *testing.T) {
	repos := []string{"/a", "/b", "/c"}
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"1", "/a", true},
		{"2", "/b", true},
		{"  3 ", "/c", true},
		{"0", "", false},
		{"4", "", false},
		{"abc", "", false},
		{"", "", false},
		{"yes", "", false},
	}
	for _, c := range cases {
		got, ok := pickWorkspaceRepo(c.in, repos)
		if ok != c.ok || got != c.want {
			t.Errorf("pick(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// TestPickWorkspaceReposMulti covers multi-pick selection
// (comma/space/plus separated numbers).
func TestPickWorkspaceReposMulti(t *testing.T) {
	cands := []repoCandidate{
		{Label: "a", Value: "/a"},
		{Label: "b", Value: "/b"},
		{Label: "c", Value: "/c"},
		{Label: "d", Value: "/d"},
	}
	cases := []struct {
		in   string
		want []string
		ok   bool
	}{
		{"1", []string{"/a"}, true},
		{"1,3", []string{"/a", "/c"}, true},
		{"1 3", []string{"/a", "/c"}, true},
		{"1+2+3", []string{"/a", "/b", "/c"}, true},
		{"4,1", []string{"/d", "/a"}, true},     // order preserved
		{"1,1,2", []string{"/a", "/b"}, true},    // dedup
		{"0", nil, false},                        // out of range
		{"1,5", nil, false},                      // one out of range fails the whole answer
		{"yes", nil, false},                      // non-numeric
		{"1,abc", nil, false},                    // mixed
		{"", nil, false},
	}
	for _, c := range cases {
		got, ok := pickWorkspaceReposMulti(c.in, cands)
		if ok != c.ok {
			t.Errorf("multi(%q) ok = %v, want %v", c.in, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("multi(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("multi(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}
