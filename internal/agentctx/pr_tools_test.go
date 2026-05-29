package agentctx

import (
	"context"
	"strings"
	"testing"

	"github.com/harisaginting/goon/internal/githost"
)

func TestParsePRReference(t *testing.T) {
	cases := []struct {
		in       string
		wantRepo string
		wantNum  int
		wantErr  bool
	}{
		{"https://bitbucket.org/meditap/data-aggregator-service/pull-requests/629", "meditap/data-aggregator-service", 629, false},
		{"https://github.com/owner/repo/pull/42", "owner/repo", 42, false},
		{"https://gitlab.com/group/sub/project/-/merge_requests/7", "group/sub/project", 7, false},
		{"owner/repo#12", "owner/repo", 12, false},
		{"owner/repo 12", "owner/repo", 12, false},
		{"meditap/data-aggregator-service#629", "meditap/data-aggregator-service", 629, false},
		{"", "", 0, true},
		{"not a pr", "", 0, true},
		{"https://example.com/foo/bar", "", 0, true},
	}
	for _, c := range cases {
		repo, num, err := parsePRReference(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parsePRReference(%q): expected error, got %s#%d", c.in, repo, num)
			}
			continue
		}
		if err != nil {
			t.Errorf("parsePRReference(%q): unexpected error %v", c.in, err)
			continue
		}
		if repo != c.wantRepo || num != c.wantNum {
			t.Errorf("parsePRReference(%q) = %s#%d, want %s#%d", c.in, repo, num, c.wantRepo, c.wantNum)
		}
	}
}

func TestExecPRGet(t *testing.T) {
	host := githost.NewMock()
	host.OpenPRs = []githost.PR{{
		Number: 629, Repo: "meditap/data-aggregator-service", Title: "Add cache",
		Author: "wind", State: "open",
		Reviewers: []githost.Reviewer{
			{Name: "Alice", State: "approved", Approved: true},
			{Name: "Bob", State: "pending"},
		},
	}}
	host.Diffs[629] = "diff --git a/x b/x\n+y\n"

	out, summary := execPRGet(context.Background(), host,
		ToolCall{Action: "pr_get", PR: "https://bitbucket.org/meditap/data-aggregator-service/pull-requests/629"})
	if !strings.Contains(summary, "ok") {
		t.Errorf("summary: %q", summary)
	}
	for _, want := range []string{"Add cache", "Alice", "approved", "Bob", "pending"} {
		if !strings.Contains(out, want) {
			t.Errorf("pr_get output missing %q:\n%s", want, out)
		}
	}
}

func TestExecPRGet_BadRef(t *testing.T) {
	out, _ := execPRGet(context.Background(), githost.NewMock(), ToolCall{Action: "pr_get", PR: "garbage"})
	if !strings.Contains(out, "TOOL ERROR") {
		t.Errorf("expected TOOL ERROR for a bad ref, got: %q", out)
	}
}

func TestExecPRGet_NoHost(t *testing.T) {
	out, _ := execPRGet(context.Background(), nil, ToolCall{Action: "pr_get", PR: "o/r#1"})
	if !strings.Contains(out, "TOOL ERROR") || !strings.Contains(out, "git host") {
		t.Errorf("expected no-host error, got: %q", out)
	}
}

func TestExecPRComment(t *testing.T) {
	host := githost.NewMock()
	host.OpenPRs = []githost.PR{{Number: 5, Repo: "o/r"}}
	out, summary := execPRComment(context.Background(), host,
		ToolCall{Action: "pr_comment", PR: "o/r#5", Body: "looks good"})
	if !strings.Contains(out, "ACTION OK") {
		t.Errorf("pr_comment output: %q", out)
	}
	if !strings.Contains(summary, "ok") {
		t.Errorf("summary: %q", summary)
	}
	if len(host.Comments) != 1 || host.Comments[0].Body != "looks good" || host.Comments[0].Number != 5 {
		t.Errorf("recorded comments: %+v", host.Comments)
	}
}

func TestExecPRComment_NoBody(t *testing.T) {
	host := githost.NewMock()
	out, _ := execPRComment(context.Background(), host, ToolCall{Action: "pr_comment", PR: "o/r#5"})
	if !strings.Contains(out, "TOOL ERROR") {
		t.Errorf("expected TOOL ERROR for empty body, got: %q", out)
	}
}

func TestExecPRApprove(t *testing.T) {
	host := githost.NewMock()
	host.OpenPRs = []githost.PR{{Number: 8, Repo: "o/r"}}
	out, _ := execPRApprove(context.Background(), host, ToolCall{Action: "pr_approve", PR: "o/r#8"})
	if !strings.Contains(out, "ACTION OK") {
		t.Errorf("pr_approve output: %q", out)
	}
	if len(host.Approved) != 1 || host.Approved[0] != 8 {
		t.Errorf("recorded approvals: %v", host.Approved)
	}
}

func TestExecPRRequestChanges(t *testing.T) {
	host := githost.NewMock()
	host.OpenPRs = []githost.PR{{Number: 9, Repo: "o/r"}}
	out, _ := execPRRequestChanges(context.Background(), host,
		ToolCall{Action: "pr_request_changes", PR: "o/r#9", Body: "needs tests"})
	if !strings.Contains(out, "ACTION OK") {
		t.Errorf("pr_request_changes output: %q", out)
	}
	if len(host.ChangesAsked) != 1 || host.ChangesAsked[0].Body != "needs tests" {
		t.Errorf("recorded change requests: %+v", host.ChangesAsked)
	}
}

func TestExecPRList(t *testing.T) {
	host := githost.NewMock()
	host.OpenPRs = []githost.PR{
		{Number: 1, Repo: "o/r", Title: "First", State: "open", Author: "a"},
		{Number: 2, Repo: "o/r", Title: "Second", State: "open", Author: "b"},
	}
	out, summary := execPRList(context.Background(), host, ToolCall{Action: "pr_list"})
	if !strings.Contains(out, "o/r#1") || !strings.Contains(out, "o/r#2") {
		t.Errorf("pr_list output:\n%s", out)
	}
	if !strings.Contains(summary, "2 hits") {
		t.Errorf("summary: %q", summary)
	}
}

func TestExecPRList_ReviewRequested(t *testing.T) {
	host := githost.NewMock()
	host.ReviewPRs = []githost.PR{{Number: 7, Repo: "o/r", Title: "Review me", State: "open"}}
	out, _ := execPRList(context.Background(), host,
		ToolCall{Action: "pr_list", Filter: "review-requested"})
	if !strings.Contains(out, "o/r#7") || !strings.Contains(out, "awaiting your review") {
		t.Errorf("pr_list review-requested output:\n%s", out)
	}
}

// TestPRActionsAreValid guards that every pr_* action the prompt
// advertises is in the parser's allow-list.
func TestPRActionsAreValid(t *testing.T) {
	for _, a := range []string{"pr_get", "pr_list", "pr_review", "pr_comment", "pr_approve", "pr_request_changes"} {
		if !validActions[a] {
			t.Errorf("action %q missing from validActions", a)
		}
	}
}
