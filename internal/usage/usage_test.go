package usage

import (
	"path/filepath"
	"testing"
)

func TestRecordAggregatesPerModel(t *testing.T) {
	m := New(filepath.Join(t.TempDir(), "usage.json"))
	m.Record("gpt-4o-mini", 100, 40)
	m.Record("gpt-4o-mini", 50, 10)
	m.Record("claude-sonnet", 200, 80)

	snap := m.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("models = %d, want 2", len(snap))
	}
	// Snapshot is sorted by total desc → claude (280) before gpt (200).
	if snap[0].Model != "claude-sonnet" {
		t.Errorf("first model = %q, want claude-sonnet (higher total)", snap[0].Model)
	}
	var gpt ModelStat
	for _, s := range snap {
		if s.Model == "gpt-4o-mini" {
			gpt = s
		}
	}
	if gpt.Calls != 2 || gpt.PromptTokens != 150 || gpt.CompletionTokens != 50 {
		t.Errorf("gpt stat = %+v, want calls=2 prompt=150 completion=50", gpt)
	}
	if gpt.TotalTokens() != 200 {
		t.Errorf("gpt total = %d, want 200", gpt.TotalTokens())
	}

	calls, prompt, completion := m.Totals()
	if calls != 3 || prompt != 350 || completion != 130 {
		t.Errorf("totals = (%d,%d,%d), want (3,350,130)", calls, prompt, completion)
	}
}

func TestRecordClampsAndNamesUnknown(t *testing.T) {
	m := New(filepath.Join(t.TempDir(), "usage.json"))
	m.Record("", -5, -3) // empty model + negative counts
	snap := m.Snapshot()
	if len(snap) != 1 || snap[0].Model != "unknown" {
		t.Fatalf("expected one 'unknown' model, got %+v", snap)
	}
	if snap[0].PromptTokens != 0 || snap[0].CompletionTokens != 0 {
		t.Errorf("negative counts not clamped: %+v", snap[0])
	}
	if snap[0].Calls != 1 {
		t.Errorf("calls = %d, want 1", snap[0].Calls)
	}
}

func TestPersistenceRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	m1 := New(path)
	m1.Record("gpt-4o", 1000, 500)

	// Reopen from the same file — totals must survive.
	m2 := New(path)
	calls, prompt, completion := m2.Totals()
	if calls != 1 || prompt != 1000 || completion != 500 {
		t.Errorf("after reload totals = (%d,%d,%d), want (1,1000,500)", calls, prompt, completion)
	}
}

func TestNilMeterSafe(t *testing.T) {
	var m *Meter
	m.Record("x", 1, 1) // must not panic
	if snap := m.Snapshot(); snap != nil {
		t.Errorf("nil meter Snapshot = %v, want nil", snap)
	}
	if c, p, comp := m.Totals(); c != 0 || p != 0 || comp != 0 {
		t.Errorf("nil meter Totals = (%d,%d,%d), want zeros", c, p, comp)
	}
}
