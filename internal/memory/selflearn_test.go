package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestQuestionKindFilters verifies the gate/learning split that drives the
// Workflows vs Questions tabs. Empty Kind must count as a gate (back-compat).
func TestQuestionKindFilters(t *testing.T) {
	dir := t.TempDir()
	m, err := New(filepath.Join(dir, "memory.json"))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	m.AskQuestion(Question{Kind: QuestionKindGate, TicketID: "ENG-1", Question: "approve plan?"})
	m.AskQuestion(Question{Kind: QuestionKindLearning, Question: "what's the deploy process?"})
	m.AskQuestion(Question{Question: "legacy gate (empty kind)"}) // empty == gate

	if got := len(m.PendingLearningQuestions()); got != 1 {
		t.Errorf("learning pending = %d, want 1", got)
	}
	if got := len(m.PendingGateQuestions()); got != 2 {
		t.Errorf("gate pending = %d, want 2 (incl. legacy empty kind)", got)
	}
}

// TestAnswerLearningPersistsToLearned is the core of the self-learning loop:
// answering a learning question must append it to LEARNED.md so the knowledge
// is durable. Gate answers must NOT touch LEARNED.md.
func TestAnswerLearningPersistsToLearned(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GOON_MEMORY_DIR", dir) // notes.New("") writes LEARNED.md here
	m, err := New(filepath.Join(dir, "memory.json"))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	lid := m.AskQuestion(Question{Kind: QuestionKindLearning, Question: "Which package owns auth?"})
	gid := m.AskQuestion(Question{Kind: QuestionKindGate, TicketID: "ENG-9", Question: "approve?"})

	if !m.AnswerQuestion(lid, "internal/auth") {
		t.Fatal("answer learning failed")
	}
	if !m.AnswerQuestion(gid, "yes") {
		t.Fatal("answer gate failed")
	}

	body, err := os.ReadFile(filepath.Join(dir, "LEARNED.md"))
	if err != nil {
		t.Fatalf("LEARNED.md not written: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, "Which package owns auth?") || !strings.Contains(got, "internal/auth") {
		t.Errorf("LEARNED.md missing the learning Q/A:\n%s", got)
	}
	if strings.Contains(got, "approve?") {
		t.Errorf("gate answer must not be written to LEARNED.md:\n%s", got)
	}
}

// TestLastReflectRoundtrip guards the daily-reflection throttle persistence.
func TestLastReflectRoundtrip(t *testing.T) {
	dir := t.TempDir()
	m, err := New(filepath.Join(dir, "memory.json"))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if !m.LastReflect().IsZero() {
		t.Errorf("expected zero LastReflect on a fresh store")
	}
	now := time.Now().Truncate(time.Second)
	m.SetLastReflect(now)
	if got := m.LastReflect(); !got.Equal(now) {
		t.Errorf("LastReflect = %v, want %v", got, now)
	}
	// Survives a reload from disk.
	m2, err := New(filepath.Join(dir, "memory.json"))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := m2.LastReflect(); !got.Equal(now) {
		t.Errorf("LastReflect after reload = %v, want %v", got, now)
	}
}
