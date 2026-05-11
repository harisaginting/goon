package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMemory_AppendAndRecent(t *testing.T) {
	dir := t.TempDir()
	m, err := New(filepath.Join(dir, "memory.json"))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	m.Append(Interaction{Input: "task1", ToolUsed: "run_command", Command: "ls", OK: true})
	m.Append(Interaction{Input: "task2", ToolUsed: "run_command", Command: "ls", OK: true})
	m.Append(Interaction{Input: "task3", ToolUsed: "run_command", Command: "pwd", OK: true})

	recent := m.RecentSummary(2)
	if len(recent) != 2 {
		t.Fatalf("expected 2 recent, got %d", len(recent))
	}
	if recent[1].Input != "task3" {
		t.Fatalf("expected most recent task3, got %q", recent[1].Input)
	}

	freq := m.FrequentCommands(2)
	if len(freq) == 0 || freq[0] != "ls" {
		t.Fatalf("expected ls as most frequent, got %v", freq)
	}
}

func TestMemory_Persistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.json")
	m1, err := New(path)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	m1.Append(Interaction{Input: "x", ToolUsed: "finish", OK: true})

	m2, err := New(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if recent := m2.RecentSummary(10); len(recent) != 1 || recent[0].Input != "x" {
		t.Fatalf("expected persisted entry, got %+v", recent)
	}
}

func TestMemory_Disabled_NoPanic(t *testing.T) {
	m := Disabled()
	m.Append(Interaction{Input: "y"})
	if r := m.RecentSummary(5); len(r) != 1 {
		t.Fatalf("expected 1 in-memory entry, got %d", len(r))
	}
}

// TestRepoChoices_RecordLookupForget covers the per-project repo memory
// that backs goon's "don't ask the gate again for this project" UX.
// Empty inputs are silently ignored; identical-write is a no-op so
// flush-on-poll doesn't write to disk every tick when nothing changed.
func TestRepoChoices_RecordLookupForget(t *testing.T) {
	dir := t.TempDir()
	m, err := New(filepath.Join(dir, "memory.json"))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, ok := m.LookupRepoChoice("ENG"); ok {
		t.Error("fresh memory should not have a learned repo for ENG")
	}
	m.RecordRepoChoice("ENG", "/r/eng")
	if got, ok := m.LookupRepoChoice("ENG"); !ok || got != "/r/eng" {
		t.Errorf("LookupRepoChoice(ENG) = (%q, %v), want (/r/eng, true)", got, ok)
	}
	// Empty inputs ignored.
	m.RecordRepoChoice("", "/r/eng")
	m.RecordRepoChoice("WEB", "")
	if all := m.RepoChoices(); len(all) != 1 {
		t.Errorf("RepoChoices = %v, want only ENG", all)
	}
	// ForgetRepoChoice returns true on hit, false on miss.
	if !m.ForgetRepoChoice("ENG") {
		t.Error("Forget(ENG) should return true")
	}
	if m.ForgetRepoChoice("ENG") {
		t.Error("second Forget(ENG) should return false")
	}
}

// TestForgetRepoChoice_SurvivesFlushAndReopen guards against the cycle-5
// silent-data-loss bug: ForgetRepoChoice removed an entry from mem,
// but the next flush+merge re-added it from disk because the merge
// policy was "disk wins for missing-from-mem" instead of "mem wins
// absolutely". A user running `goon repo forget ENG` from the CLI
// saw success, restarted the daemon, and ENG was still cached.
func TestForgetRepoChoice_SurvivesFlushAndReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.json")

	// Round 1: persist a learned mapping.
	m1, err := New(path)
	if err != nil {
		t.Fatalf("new1: %v", err)
	}
	m1.RecordRepoChoice("ENG", "/r/eng")
	if got, ok := m1.LookupRepoChoice("ENG"); !ok || got != "/r/eng" {
		t.Fatalf("setup precondition failed: lookup = (%q, %v)", got, ok)
	}

	// Round 2: simulate a separate-process forget — open Memory at
	// the same path, forget, allow the flush to merge with disk.
	m2, err := New(path)
	if err != nil {
		t.Fatalf("new2: %v", err)
	}
	if !m2.ForgetRepoChoice("ENG") {
		t.Fatal("ForgetRepoChoice should succeed on existing entry")
	}

	// Round 3: re-open and verify the deletion stuck.
	m3, err := New(path)
	if err != nil {
		t.Fatalf("new3: %v", err)
	}
	if got, ok := m3.LookupRepoChoice("ENG"); ok {
		t.Errorf("ENG should be forgotten, but lookup returned %q", got)
	}
	if got := m3.RepoChoices(); len(got) != 0 {
		t.Errorf("expected empty RepoChoices after forget, got %v", got)
	}
}

// TestForgetRepoChoice_DaemonReloadPicksUpDeletion is the cycle-6
// regression. The cycle-5 fix made flush mem-wins, but the daemon's
// in-memory state never picked up CLI-side deletions through Reload
// (mergeStores' mem-wins policy preserved the daemon's stale entry).
// Result: CLI deletes ENG, daemon's next write resurrects it. This
// test simulates the daemon process holding ENG, the CLI process
// deleting ENG, and the daemon's Reload picking up the deletion.
func TestForgetRepoChoice_DaemonReloadPicksUpDeletion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.json")

	// Daemon-process Memory: holds ENG.
	daemon, err := New(path)
	if err != nil {
		t.Fatalf("daemon new: %v", err)
	}
	daemon.RecordRepoChoice("ENG", "/r/eng")

	// Separate CLI-process Memory: forgets ENG.
	cli, err := New(path)
	if err != nil {
		t.Fatalf("cli new: %v", err)
	}
	if !cli.ForgetRepoChoice("ENG") {
		t.Fatal("CLI forget failed")
	}

	// Daemon reloads — must see disk's deleted state, not its own
	// stale in-memory copy.
	daemon.Reload()
	if got, ok := daemon.LookupRepoChoice("ENG"); ok {
		t.Errorf("daemon Reload should pick up CLI deletion; still has %q", got)
	}

	// Critical follow-up: the daemon's next write must NOT
	// resurrect the deleted entry. Trigger a flush by recording a
	// different mapping, then verify ENG stays gone.
	daemon.RecordRepoChoice("WEB", "/r/web")
	check, err := New(path)
	if err != nil {
		t.Fatalf("check new: %v", err)
	}
	if got, ok := check.LookupRepoChoice("ENG"); ok {
		t.Errorf("daemon's flush silently resurrected ENG = %q", got)
	}
	if got, ok := check.LookupRepoChoice("WEB"); !ok || got != "/r/web" {
		t.Errorf("daemon's new write missing: WEB = (%q, %v)", got, ok)
	}
}

// TestStatus_Paused covers the pause/resume flag plumbing. SetPaused
// flips the bit, IsPaused reads it, and the value persists across a
// New() reopen of the same memory.json.
func TestStatus_Paused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.json")
	m, err := New(path)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if m.IsPaused() {
		t.Error("fresh memory should not be paused")
	}
	m.SetPaused(true)
	if !m.IsPaused() {
		t.Error("after SetPaused(true), IsPaused should be true")
	}
	// Force a flush + re-open to verify the flag survives disk round-trip.
	m2, err := New(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if !m2.IsPaused() {
		t.Error("paused flag should persist across New() reopen")
	}
	m2.SetPaused(false)
	if m2.IsPaused() {
		t.Error("after SetPaused(false), IsPaused should be false")
	}
}

// TestNew_RejectsDirectoryWithActionableMessage covers the most common
// first-run footgun: a user sets GOON_MEMORY_PATH to the notes directory
// (e.g. ./storage/memory) instead of the JSON file (./storage/memory.json).
// The two names differ by one character. Without this guard the error
// surfaces as "read X: is a directory" with no hint about which env var
// to flip. With the guard, the error message names both env vars and
// the suggested fix verbatim.
func TestNew_RejectsDirectoryWithActionableMessage(t *testing.T) {
	dir := t.TempDir()
	notesDir := filepath.Join(dir, "memory")
	if err := os.MkdirAll(notesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := New(notesDir)
	if err == nil {
		t.Fatal("expected error when path is a directory")
	}
	for _, want := range []string{
		"is a directory",
		"GOON_MEMORY_PATH",
		"GOON_MEMORY_DIR",
		notesDir + ".json", // suggested fix
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected error to contain %q, got:\n%s", want, err.Error())
		}
	}
}
