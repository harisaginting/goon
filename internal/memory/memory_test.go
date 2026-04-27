package memory

import (
	"path/filepath"
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
