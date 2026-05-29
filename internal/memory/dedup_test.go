package memory

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestMemory_ReviewDedup(t *testing.T) {
	m, err := New(filepath.Join(t.TempDir(), "m.json"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const key = "github:o/r#1"
	if _, ok := m.ReviewMarkFor(key); ok {
		t.Fatal("expected no review mark initially")
	}
	m.RecordReview(key, "hash-aaa")
	mk, ok := m.ReviewMarkFor(key)
	if !ok || mk.DiffHash != "hash-aaa" {
		t.Fatalf("review mark: %+v ok=%v", mk, ok)
	}
	if mk.When.IsZero() {
		t.Error("review mark When should be set")
	}
	m.RecordReview(key, "hash-bbb")
	if mk, _ := m.ReviewMarkFor(key); mk.DiffHash != "hash-bbb" {
		t.Errorf("expected updated hash, got %q", mk.DiffHash)
	}
}

func TestMemory_NotificationDedup(t *testing.T) {
	m, err := New(filepath.Join(t.TempDir(), "m.json"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if m.NotificationSeen("github:42") {
		t.Fatal("expected unseen initially")
	}
	m.MarkNotificationSeen("github:42")
	if !m.NotificationSeen("github:42") {
		t.Error("expected seen after MarkNotificationSeen")
	}
}

func TestMemory_DedupPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.json")
	m1, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m1.RecordReview("github:o/r#7", "h7")
	m1.MarkNotificationSeen("github:n9")

	m2, err := New(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if mk, ok := m2.ReviewMarkFor("github:o/r#7"); !ok || mk.DiffHash != "h7" {
		t.Errorf("review mark not persisted: %+v ok=%v", mk, ok)
	}
	if !m2.NotificationSeen("github:n9") {
		t.Error("notification dedup not persisted across reopen")
	}
}

func TestPruneOldestNotifSeen(t *testing.T) {
	mp := map[string]time.Time{}
	base := time.Now()
	for i := 0; i < 10; i++ {
		mp[fmt.Sprintf("k%d", i)] = base.Add(time.Duration(i) * time.Minute)
	}
	pruneOldestNotifSeen(mp, 4)
	if len(mp) != 4 {
		t.Fatalf("want 4 entries after prune, got %d", len(mp))
	}
	for _, k := range []string{"k6", "k7", "k8", "k9"} {
		if _, ok := mp[k]; !ok {
			t.Errorf("expected newest entry %s to survive prune", k)
		}
	}
	for _, k := range []string{"k0", "k5"} {
		if _, ok := mp[k]; ok {
			t.Errorf("expected oldest entry %s to be evicted", k)
		}
	}
}

func TestPruneOldestReviewMarks(t *testing.T) {
	mp := map[string]ReviewMark{}
	base := time.Now()
	for i := 0; i < 8; i++ {
		mp[fmt.Sprintf("k%d", i)] = ReviewMark{
			DiffHash: "h",
			When:     base.Add(time.Duration(i) * time.Minute),
		}
	}
	pruneOldestReviewMarks(mp, 3)
	if len(mp) != 3 {
		t.Fatalf("want 3 entries after prune, got %d", len(mp))
	}
	if _, ok := mp["k7"]; !ok {
		t.Error("newest review mark k7 should survive")
	}
	if _, ok := mp["k0"]; ok {
		t.Error("oldest review mark k0 should be evicted")
	}
}
