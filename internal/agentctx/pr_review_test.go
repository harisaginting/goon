package agentctx

import (
	"context"
	"strings"
	"testing"

	"github.com/harisaginting/goon/internal/githost"
	"github.com/harisaginting/goon/internal/llm"
)

// TestExecPRReview covers the chat flow:
//
//	user → "review pr <url>"
//	tool  → fetches diff, drafts review, hands back the draft +
//	         instructions to show + ask + post-on-confirm.
//
// The tool result must include the drafted text, the BEGIN/END fences
// the assistant uses to find the exact body to repost, and a pointer
// to pr_comment for the confirmation step.
func TestExecPRReview(t *testing.T) {
	host := githost.NewMock()
	host.OpenPRs = []githost.PR{{
		Number: 1269, Repo: "meditap/internal-portal-service",
		Title: "Add cache", Author: "wind", State: "open",
		URL: "https://bitbucket.org/meditap/internal-portal-service/pull-requests/1269",
	}}
	host.Diffs[1269] = "diff --git a/x b/x\n+y\n"
	mockLLM := llm.NewMock([]string{
		"SUMMARY — Adds a caching layer.\nRISKS — none obvious.\nRECOMMENDATION: approve",
	})

	out, summary := execPRReview(context.Background(), host, mockLLM,
		ToolCall{Action: "pr_review", PR: "https://bitbucket.org/meditap/internal-portal-service/pull-requests/1269"})
	for _, want := range []string{"PR REVIEW DRAFT", "Adds a caching layer", "BEGIN REVIEW", "END REVIEW", "pr_comment"} {
		if !strings.Contains(out, want) {
			t.Errorf("pr_review result missing %q in:\n%s", want, out)
		}
	}
	if !strings.Contains(summary, "ok") {
		t.Errorf("summary: %q", summary)
	}
}

func TestExecPRReview_BadRef(t *testing.T) {
	out, _ := execPRReview(context.Background(), githost.NewMock(), llm.NewMock(nil),
		ToolCall{Action: "pr_review", PR: "garbage"})
	if !strings.Contains(out, "TOOL ERROR") {
		t.Errorf("expected TOOL ERROR for a bad ref, got: %q", out)
	}
}

func TestExecPRReview_NoHost(t *testing.T) {
	out, _ := execPRReview(context.Background(), nil, llm.NewMock(nil),
		ToolCall{Action: "pr_review", PR: "o/r#1"})
	if !strings.Contains(out, "TOOL ERROR") {
		t.Errorf("expected TOOL ERROR with no host, got: %q", out)
	}
}

func TestExecPRReview_NoLLM(t *testing.T) {
	host := githost.NewMock()
	host.OpenPRs = []githost.PR{{Number: 1, Repo: "o/r"}}
	host.Diffs[1] = "diff"
	out, summary := execPRReview(context.Background(), host, nil,
		ToolCall{Action: "pr_review", PR: "o/r#1"})
	if !strings.Contains(out, "TOOL ERROR") || !strings.Contains(out, "LLM") {
		t.Errorf("expected TOOL ERROR about LLM, got: %q", out)
	}
	if !strings.Contains(summary, "no llm") {
		t.Errorf("summary: %q", summary)
	}
}
