package memory

import (
	"path/filepath"
	"testing"
	"time"
)

func TestMerge_PreferAnswered(t *testing.T) {
	pending := Question{ID: "q-1", Question: "x?"}
	answered := Question{ID: "q-1", Question: "x?", Answer: "yes", AnsweredAt: time.Now()}

	if got := preferAnswered(pending, answered); got.Answer != "yes" {
		t.Errorf("answered should win, got %+v", got)
	}
	if got := preferAnswered(answered, pending); got.Answer != "yes" {
		t.Errorf("answered should win regardless of order, got %+v", got)
	}
}

func TestMerge_MoreRecentAnswerWins(t *testing.T) {
	a := Question{ID: "q-1", Answer: "old", AnsweredAt: time.Now().Add(-time.Hour)}
	b := Question{ID: "q-1", Answer: "new", AnsweredAt: time.Now()}
	if got := preferAnswered(a, b); got.Answer != "new" {
		t.Errorf("new should win, got %+v", got)
	}
}

func TestMergeStores_QuestionsFromDisk(t *testing.T) {
	disk := storeFile{
		Questions: []Question{
			{ID: "q-1", Answer: "from-disk", AnsweredAt: time.Now()},
		},
	}
	mem := storeFile{
		Questions: []Question{
			{ID: "q-1", Question: "x?"}, // still pending in mem
		},
	}
	out := mergeStores(disk, mem)
	if len(out.Questions) != 1 || out.Questions[0].Answer != "from-disk" {
		t.Fatalf("merge: %+v", out.Questions)
	}
}

func TestMergeStores_NewQuestionsFromMem(t *testing.T) {
	disk := storeFile{
		Questions: []Question{{ID: "q-1", Question: "old"}},
	}
	mem := storeFile{
		Questions: []Question{
			{ID: "q-1", Question: "old"},
			{ID: "q-2", Question: "new"},
		},
	}
	out := mergeStores(disk, mem)
	if len(out.Questions) != 2 {
		t.Fatalf("expected 2 questions, got %d: %+v", len(out.Questions), out.Questions)
	}
}

func TestMergeStores_TicketRecencyWins(t *testing.T) {
	older := time.Now().Add(-time.Hour)
	newer := time.Now()
	disk := storeFile{
		Tickets: map[string]TicketSnapshot{
			"T-1": {ID: "T-1", LastSeen: newer, Title: "from-disk"},
		},
	}
	mem := storeFile{
		Tickets: map[string]TicketSnapshot{
			"T-1": {ID: "T-1", LastSeen: older, Title: "from-mem"},
		},
	}
	out := mergeStores(disk, mem)
	if out.Tickets["T-1"].Title != "from-disk" {
		t.Fatalf("expected disk to win on recency: %+v", out.Tickets["T-1"])
	}
}

func TestReload_PicksUpExternalAnswer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.json")

	// Daemon-side memory writes a question.
	daemonMem, _ := New(path)
	id := daemonMem.AskQuestion(Question{TicketID: "T-1", Question: "x?"})

	// Another process opens the same file and answers it.
	cliMem, _ := New(path)
	if !cliMem.AnswerQuestion(id, "ack") {
		t.Fatal("CLI failed to answer")
	}

	// Daemon reloads from disk → must see the answer.
	daemonMem.Reload()
	if ans, ok := daemonMem.FindAnswer("T-1", "x?"); !ok || ans != "ack" {
		t.Fatalf("daemon did not pick up external answer: ok=%v ans=%q", ok, ans)
	}
}

func TestFlush_DoesNotClobberExternalAnswer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.json")

	daemonMem, _ := New(path)
	id := daemonMem.AskQuestion(Question{TicketID: "T-1", Question: "x?"})

	// Another process answers.
	cliMem, _ := New(path)
	cliMem.AnswerQuestion(id, "ack")

	// Daemon now does an unrelated write (status update). The file must NOT
	// lose the answer.
	daemonMem.SetStatus(DaemonStatus{Running: true, BoardName: "jira"})

	freshMem, _ := New(path)
	if ans, ok := freshMem.FindAnswer("T-1", "x?"); !ok || ans != "ack" {
		t.Fatalf("answer was clobbered after daemon flush: ok=%v ans=%q", ok, ans)
	}
	if !freshMem.GetStatus().Running {
		t.Fatal("status update did not persist")
	}
}

func TestFlush_PreservesExternalNewQuestion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.json")

	daemonMem, _ := New(path)
	daemonMem.SetStatus(DaemonStatus{Running: true})

	// CLI adds a question externally.
	cliMem, _ := New(path)
	cliMem.AskQuestion(Question{TicketID: "T-2", Question: "y?"})

	// Daemon flushes (via SetStatus) — must preserve the CLI-added question.
	daemonMem.SetStatus(DaemonStatus{Running: true, LastTicket: "T-1"})

	freshMem, _ := New(path)
	pending := freshMem.PendingQuestions()
	if len(pending) != 1 || pending[0].TicketID != "T-2" {
		t.Fatalf("CLI-added question lost: %+v", pending)
	}
}
