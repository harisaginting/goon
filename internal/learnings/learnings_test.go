package learnings

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCapture_AppendsHistoryWhenNoLLM: even when the LLM stack isn't
// configured (Options.LLM == nil), HISTORY.md should still gain a
// timestamp+task+outcome line. The history log is independent of the
// distillation step so users keep a record even on fully-offline runs
// (mock provider, --explain mode, etc).
func TestCapture_AppendsHistoryWhenNoLLM(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GOON_MEMORY_DIR", dir)
	t.Setenv("HOME", dir)
	t.Setenv("GOON_AUTO_LEARN", "")

	_ = Capture(context.Background(), Options{
		Task:    "fix the login redirect",
		Outcome: "ok",
	})

	body, err := os.ReadFile(filepath.Join(dir, HistoryFilename))
	if err != nil {
		t.Fatalf("HISTORY.md not written: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, "fix the login redirect") {
		t.Errorf("HISTORY.md missing task; got:\n%s", got)
	}
	if !strings.Contains(got, " · ok\n") {
		t.Errorf("HISTORY.md missing outcome separator; got:\n%s", got)
	}
}

// TestCapture_DefaultsOutcomeToOK confirms an empty Outcome lands as
// "ok" in the log — saves callers from having to remember to fill it
// in on the happy path.
func TestCapture_DefaultsOutcomeToOK(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GOON_MEMORY_DIR", dir)
	t.Setenv("HOME", dir)

	_ = Capture(context.Background(), Options{Task: "do a thing"})

	body, _ := os.ReadFile(filepath.Join(dir, HistoryFilename))
	if !strings.Contains(string(body), " · ok\n") {
		t.Errorf("empty Outcome should default to ok; got:\n%s", string(body))
	}
}

// TestCapture_MultiRunAppendsMultipleLines: two consecutive Capture
// calls must produce two lines in HISTORY.md, not one. Belt-and-braces
// for the notes.Store.Append newline-handling logic.
func TestCapture_MultiRunAppendsMultipleLines(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GOON_MEMORY_DIR", dir)
	t.Setenv("HOME", dir)

	_ = Capture(context.Background(), Options{Task: "first run"})
	_ = Capture(context.Background(), Options{Task: "second run"})

	body, _ := os.ReadFile(filepath.Join(dir, HistoryFilename))
	got := string(body)
	if !strings.Contains(got, "first run") || !strings.Contains(got, "second run") {
		t.Errorf("HISTORY.md missing one of the runs; got:\n%s", got)
	}
	if got := strings.Count(got, "\n"); got != 2 {
		t.Errorf("expected 2 history lines, got %d:\n%s", got, body)
	}
}

// TestAutoLearnEnabled_DefaultOn confirms the opt-in semantics: with
// the env var unset, the distillation step is enabled. Without this
// default-on guarantee the feature would be invisible to users who
// never read the docs.
func TestAutoLearnEnabled_DefaultOn(t *testing.T) {
	t.Setenv("GOON_AUTO_LEARN", "")
	if !autoLearnEnabled() {
		t.Errorf("autoLearnEnabled() = false with empty env; expected default on")
	}
}

// TestAutoLearnEnabled_OffValues covers every off-spelling the env
// parser accepts. A user who types "no" / "off" should disable the
// distillation pass without surprise.
func TestAutoLearnEnabled_OffValues(t *testing.T) {
	for _, v := range []string{"0", "false", "FALSE", "no", "No", "off", "OFF", "n"} {
		t.Setenv("GOON_AUTO_LEARN", v)
		if autoLearnEnabled() {
			t.Errorf("autoLearnEnabled() = true for %q; expected false", v)
		}
	}
}

// TestCapture_TruncatesLongTaskInHistory: a 500-char rambling task
// must not blow up HISTORY.md to half a megabyte per run. We cap at
// ~200 chars with a trailing ellipsis.
func TestCapture_TruncatesLongTaskInHistory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GOON_MEMORY_DIR", dir)
	t.Setenv("HOME", dir)

	long := strings.Repeat("x", 500)
	_ = Capture(context.Background(), Options{Task: long})

	body, _ := os.ReadFile(filepath.Join(dir, HistoryFilename))
	if !strings.Contains(string(body), "…") {
		t.Errorf("expected truncation ellipsis in long task; got:\n%s", string(body))
	}
	if strings.Count(string(body), "x") > 220 {
		t.Errorf("task field not truncated; got %d xs", strings.Count(string(body), "x"))
	}
}
