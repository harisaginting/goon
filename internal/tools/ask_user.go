package tools

import (
	"context"
	"errors"
	"fmt"

	"github.com/harisaginting/goon/internal/memory"
)

// AskUser is the tool the agent calls when it cannot proceed without user
// input. The question is queued in memory; the daemon will skip the ticket
// until the user answers via `goon train` or the web UI.
//
// args:
//
//	question  (required) the question to ask
//	ticket    (optional) ticket id to attach the question to
//	workflow  (optional) workflow id
type AskUser struct {
	Memory *memory.Memory
}

// NewAskUser builds an AskUser tool bound to a memory store.
func NewAskUser(m *memory.Memory) *AskUser { return &AskUser{Memory: m} }

func (*AskUser) Name() string { return "ask_user" }
func (*AskUser) Description() string {
	return "queue a question for the user; the agent should call finish next"
}
func (*AskUser) Schema() map[string]string {
	return map[string]string{
		"question": "the question to ask the user",
		"ticket":   "ticket id (optional)",
		"workflow": "workflow id (optional)",
		"kind":     `"learning" (standby self-learning, shows in Questions tab) or "gate" (blocks a ticket, shows in Workflows tab). Defaults to gate when a ticket/workflow is set, else learning.`,
	}
}

func (a *AskUser) Run(_ context.Context, args map[string]string) (Result, error) {
	q := args["question"]
	if q == "" {
		return Result{ToolName: "ask_user"}, errors.New(`ask_user: "question" is required`)
	}
	if a.Memory == nil {
		return Result{ToolName: "ask_user"}, errors.New("ask_user: memory not configured")
	}
	// Reuse a previously-answered question if the same one was asked before.
	if ans, ok := a.Memory.FindAnswer(args["ticket"], q); ok {
		return Result{ToolName: "ask_user", Stdout: "previous answer: " + ans}, nil
	}
	// Decide the kind: explicit arg wins; otherwise a question tied to a
	// ticket/workflow is a blocking gate, and a free-standing one (e.g. from
	// the standby reflection) is a learning question.
	kind := args["kind"]
	switch kind {
	case memory.QuestionKindLearning, memory.QuestionKindGate:
		// explicit, keep as-is
	default:
		if args["ticket"] != "" || args["workflow"] != "" {
			kind = memory.QuestionKindGate
		} else {
			kind = memory.QuestionKindLearning
		}
	}
	id := a.Memory.AskQuestion(memory.Question{
		Kind:       kind,
		TicketID:   args["ticket"],
		WorkflowID: args["workflow"],
		Question:   q,
	})
	return Result{ToolName: "ask_user", Stdout: fmt.Sprintf("queued question %s; awaiting user", id)}, nil
}
