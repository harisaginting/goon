package memory

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestMemory_QuestionsRoundtrip(t *testing.T) {
	dir := t.TempDir()
	m, err := New(filepath.Join(dir, "memory.json"))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	id1 := m.AskQuestion(Question{TicketID: "ENG-1", Question: "Which DB?"})
	id2 := m.AskQuestion(Question{TicketID: "ENG-2", Question: "Migrate now?"})
	if id1 == "" || id1 == id2 {
		t.Fatalf("ids not unique: %q %q", id1, id2)
	}
	pending := m.PendingQuestions()
	if len(pending) != 2 {
		t.Fatalf("pending: %d", len(pending))
	}
	if !m.AnswerQuestion(id1, "postgres") {
		t.Fatal("expected to answer id1")
	}
	if m.AnswerQuestion(id1, "again") {
		t.Fatal("should not re-answer")
	}
	if v, ok := m.FindAnswer("ENG-1", "Which DB?"); !ok || v != "postgres" {
		t.Fatalf("FindAnswer: %v %q", ok, v)
	}
	pending = m.PendingQuestions()
	if len(pending) != 1 || pending[0].ID != id2 {
		t.Fatalf("pending after answer: %+v", pending)
	}
	all := m.AllQuestions()
	if len(all) != 2 {
		t.Fatalf("all: %d", len(all))
	}
}

func TestMemory_Workflows(t *testing.T) {
	dir := t.TempDir()
	m, _ := New(filepath.Join(dir, "memory.json"))

	w := Workflow{ID: "wf-1", TicketID: "ENG-1", State: WFTriaging}
	m.UpsertWorkflow(w)

	if !m.HasOpenWorkflowFor("ENG-1") {
		t.Fatal("expected open workflow")
	}

	w.State = WFDone
	m.UpsertWorkflow(w)
	if m.HasOpenWorkflowFor("ENG-1") {
		t.Fatal("expected no open workflow after done")
	}

	got, ok := m.GetWorkflow("wf-1")
	if !ok || got.State != WFDone {
		t.Fatalf("get: %+v", got)
	}
	wfs := m.ListWorkflows(10)
	if len(wfs) != 1 {
		t.Fatalf("list: %d", len(wfs))
	}
}

func TestMemory_Tickets(t *testing.T) {
	dir := t.TempDir()
	m, _ := New(filepath.Join(dir, "memory.json"))
	m.SeenTicket(TicketSnapshot{ID: "ENG-1", Title: "first"})
	m.SeenTicket(TicketSnapshot{ID: "ENG-2", Title: "second"})
	m.SeenTicket(TicketSnapshot{ID: "ENG-1", Title: "first-updated"})
	tks := m.ListTickets()
	if len(tks) != 2 {
		t.Fatalf("expected 2 tickets after dedupe, got %d", len(tks))
	}
}

func TestMemory_DaemonStatus(t *testing.T) {
	dir := t.TempDir()
	m, _ := New(filepath.Join(dir, "memory.json"))
	m.SetStatus(DaemonStatus{Running: true, PID: 1234, BoardName: "jira"})
	got := m.GetStatus()
	if !got.Running || got.PID != 1234 || got.BoardName != "jira" {
		t.Fatalf("status: %+v", got)
	}
}

func TestMemory_PersistenceWithEngineerData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.json")
	m1, _ := New(path)
	m1.AskQuestion(Question{TicketID: "T-1", Question: "x?"})
	m1.UpsertWorkflow(Workflow{ID: "wf-1", TicketID: "T-1", State: WFExecuting})
	m1.SeenTicket(TicketSnapshot{ID: "T-1", Title: "t"})
	m1.SetStatus(DaemonStatus{Running: true, StartedAt: time.Now()})

	m2, _ := New(path)
	if len(m2.PendingQuestions()) != 1 {
		t.Fatal("questions did not persist")
	}
	if !m2.HasOpenWorkflowFor("T-1") {
		t.Fatal("workflow did not persist")
	}
	if len(m2.ListTickets()) != 1 {
		t.Fatal("tickets did not persist")
	}
	if !m2.GetStatus().Running {
		t.Fatal("status did not persist")
	}
}

func TestMemory_TicketsPrune(t *testing.T) {
	dir := t.TempDir()
	m, _ := New(filepath.Join(dir, "memory.json"))
	// Insert maxTicketSnapshots+50 unique tickets with monotonic LastSeen.
	base := time.Now().Add(-24 * time.Hour)
	total := maxTicketSnapshots + 50
	for i := 0; i < total; i++ {
		m.SeenTicket(TicketSnapshot{
			ID:       fmt.Sprintf("T-%04d", i),
			LastSeen: base.Add(time.Duration(i) * time.Second),
		})
	}
	tks := m.ListTickets()
	if len(tks) != maxTicketSnapshots {
		t.Fatalf("expected ticket cap of %d, got %d", maxTicketSnapshots, len(tks))
	}
	// The 50 oldest (T-0000..T-0049) should have been evicted.
	for _, tk := range tks {
		if tk.ID == "T-0000" || tk.ID == "T-0049" {
			t.Errorf("oldest ticket %q should have been pruned", tk.ID)
		}
	}
}

func TestMemory_AuthorizeChatPrunesOldest(t *testing.T) {
	dir := t.TempDir()
	m, _ := New(filepath.Join(dir, "memory.json"))
	for i := 0; i < maxTelegramAuth+10; i++ {
		m.AuthorizeChat(int64(i+1), "user", "User")
	}
	got := m.AuthorizedChats()
	if len(got) != maxTelegramAuth {
		t.Fatalf("expected auth cap of %d, got %d", maxTelegramAuth, len(got))
	}
}

func TestMemory_PruneStaleAuth(t *testing.T) {
	dir := t.TempDir()
	m, _ := New(filepath.Join(dir, "memory.json"))
	// Hand-roll two entries with explicit AuthorizedAt so we don't depend on
	// time.Now() ordering in the test.
	m.AuthorizeChat(1, "old", "Old")
	m.AuthorizeChat(2, "new", "New")
	m.mu.Lock()
	m.store.TelegramAuth[0].AuthorizedAt = time.Now().Add(-48 * time.Hour)
	m.mu.Unlock()
	dropped := m.PruneStaleAuth(24 * time.Hour)
	if dropped != 1 {
		t.Fatalf("expected 1 drop, got %d", dropped)
	}
	if !m.IsChatAuthorized(2) {
		t.Error("recent auth should survive prune")
	}
	if m.IsChatAuthorized(1) {
		t.Error("stale auth should have been dropped")
	}
}

