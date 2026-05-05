package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/harisaginting/goon/internal/memory"
)

func TestAskUser_QueuesQuestion(t *testing.T) {
	mem := memory.Disabled()
	au := NewAskUser(mem)

	res, err := au.Run(context.Background(), map[string]string{
		"question": "Which DB engine?",
		"ticket":   "ENG-1",
		"workflow": "wf-1",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(res.Stdout, "queued question") {
		t.Errorf("stdout: %q", res.Stdout)
	}
	pending := mem.PendingQuestions()
	if len(pending) != 1 || pending[0].Question != "Which DB engine?" {
		t.Fatalf("pending: %+v", pending)
	}
	if pending[0].TicketID != "ENG-1" || pending[0].WorkflowID != "wf-1" {
		t.Errorf("metadata: %+v", pending[0])
	}
}

func TestAskUser_ReusesPriorAnswer(t *testing.T) {
	mem := memory.Disabled()
	au := NewAskUser(mem)
	id := mem.AskQuestion(memory.Question{TicketID: "ENG-1", Question: "Which DB?"})
	mem.AnswerQuestion(id, "postgres")

	res, err := au.Run(context.Background(), map[string]string{
		"question": "Which DB?",
		"ticket":   "ENG-1",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(res.Stdout, "postgres") {
		t.Fatalf("expected reused answer, got %q", res.Stdout)
	}
	if len(mem.PendingQuestions()) != 0 {
		t.Errorf("expected no new question; got %d pending", len(mem.PendingQuestions()))
	}
}

func TestAskUser_RequiresQuestion(t *testing.T) {
	au := NewAskUser(memory.Disabled())
	_, err := au.Run(context.Background(), map[string]string{})
	if err == nil {
		t.Fatal("expected error for missing question")
	}
}

func TestAskUser_RequiresMemory(t *testing.T) {
	au := &AskUser{}
	_, err := au.Run(context.Background(), map[string]string{"question": "x"})
	if err == nil {
		t.Fatal("expected error when memory nil")
	}
}
