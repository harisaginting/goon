package agentctx

import (
	"context"
	"strings"
	"testing"

	"github.com/harisaginting/goon/internal/boards"
)

// TestExecJiraTransition_RealStatusName is the regression test for the
// Telegram-chat bug where "change EB-4978 to ready to test" silently
// moved the ticket to Backlog (because MapStatus matched the substring
// "ready"). The fix uses TransitionByName against the board's REAL
// status set.
func TestExecJiraTransition_RealStatusName(t *testing.T) {
	board := boards.NewMock([]boards.Ticket{{ID: "EB-4978", Key: "EB-4978"}})
	board.Transitions = []string{"Backlog", "In Progress", "Ready to Test", "Done"}
	out, summary := execJiraTransition(context.Background(), board,
		ToolCall{Action: "jira_transition", Key: "EB-4978", Status: "ready to test"})
	if !strings.Contains(out, "ACTION OK") || !strings.Contains(out, "Ready to Test") {
		t.Errorf("expected ACTION OK with real status name, got:\n%s", out)
	}
	if len(board.Transit) != 1 || !strings.Contains(board.Transit[0], "Ready to Test") {
		t.Errorf("recorded transitions: %v (must NOT be Backlog)", board.Transit)
	}
	if !strings.Contains(summary, "Ready to Test") {
		t.Errorf("summary: %q", summary)
	}
}

func TestExecJiraTransition_NoMatchListsOptions(t *testing.T) {
	board := boards.NewMock([]boards.Ticket{{ID: "EB-1", Key: "EB-1"}})
	board.Transitions = []string{"Backlog", "In Progress", "Done"}
	out, _ := execJiraTransition(context.Background(), board,
		ToolCall{Action: "jira_transition", Key: "EB-1", Status: "ready to test"})
	if !strings.Contains(out, "TOOL ERROR") {
		t.Errorf("expected TOOL ERROR for unmatched status, got: %q", out)
	}
	for _, want := range []string{"Backlog", "In Progress", "Done"} {
		if !strings.Contains(out, want) {
			t.Errorf("error should list available status %q, got: %q", want, out)
		}
	}
}

func TestExecJiraListTransitions(t *testing.T) {
	board := boards.NewMock([]boards.Ticket{{ID: "EB-1", Key: "EB-1"}})
	board.Transitions = []string{"Backlog", "Ready to Test"}
	out, summary := execJiraListTransitions(context.Background(), board,
		ToolCall{Action: "jira_transitions", Key: "EB-1"})
	if !strings.Contains(out, "Backlog") || !strings.Contains(out, "Ready to Test") {
		t.Errorf("expected status list, got: %q", out)
	}
	if !strings.Contains(summary, "ok (2)") {
		t.Errorf("summary: %q", summary)
	}
}
