package memory

import (
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
