package agentctx

import (
	"strings"
	"testing"
)

// TestParseToolCall_ValidShapes covers the happy path: pure JSON,
// JSON with surrounding whitespace, JSON inside a fenced block, JSON
// with an action the parser accepts.
func TestParseToolCall_ValidShapes(t *testing.T) {
	cases := []struct {
		name, input, wantAction, wantJQL string
	}{
		{
			name:       "pure single line",
			input:      `{"action":"jira_search","jql":"order by updated desc","limit":50}`,
			wantAction: "jira_search",
			wantJQL:    "order by updated desc",
		},
		{
			name:       "with leading whitespace and newline",
			input:      "   \n\n{\"action\":\"jira_search\",\"jql\":\"project = ENG\"}",
			wantAction: "jira_search",
			wantJQL:    "project = ENG",
		},
		{
			name:       "wrapped in code fence",
			input:      "```json\n{\"action\":\"jira_search\",\"jql\":\"abc\"}\n```",
			wantAction: "jira_search",
			wantJQL:    "abc",
		},
		{
			name:       "comment action",
			input:      `{"action":"jira_comment","key":"ENG-1","body":"hello"}`,
			wantAction: "jira_comment",
		},
		{
			name:       "transition action",
			input:      `{"action":"jira_transition","key":"ENG-1","status":"done"}`,
			wantAction: "jira_transition",
		},
		{
			name:       "update with labels array",
			input:      `{"action":"jira_update","key":"ENG-1","title":"new","labels":["bug","p1"]}`,
			wantAction: "jira_update",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _, ok := parseToolCall(tc.input)
			if !ok {
				t.Fatalf("expected ok=true, got false for %q", tc.input)
			}
			if c.Action != tc.wantAction {
				t.Errorf("action: got %q, want %q", c.Action, tc.wantAction)
			}
			if tc.wantJQL != "" && c.JQL != tc.wantJQL {
				t.Errorf("jql: got %q, want %q", c.JQL, tc.wantJQL)
			}
		})
	}
}

// TestParseToolCall_SalvageNarratedCall is the regression test for the
// real-world failure case: weaker models narrate the tool call as
// "TOOL: jira_search ... RESULT: ..." and embed the JSON anyway. The
// parser must salvage the JSON instead of returning it as prose.
func TestParseToolCall_SalvageNarratedCall(t *testing.T) {
	cases := []struct{ name, input string }{
		{
			name: "tool: prefix with embedded json",
			input: `TOOL: jira_search "recent tickets" limit=50
RESULT:
{
  "action": "jira_search",
  "jql": "order by updated desc",
  "limit": 50
}`,
		},
		{
			name: "prose preamble before json",
			input: `I will call jira_search to get the data.
{"action":"jira_search","jql":"project = ENG"}`,
		},
		{
			name: "markdown fence around dirty narration",
			input: "Here is the tool call you requested:\n```json\n{\"action\":\"jira_search\",\"jql\":\"project = ENG\"}\n```\nLet me know if you need anything else.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _, ok := parseToolCall(tc.input)
			if !ok {
				t.Fatalf("expected salvage to succeed; got prose for input:\n%s", tc.input)
			}
			if c.Action != "jira_search" {
				t.Errorf("action: got %q, want jira_search", c.Action)
			}
		})
	}
}

// TestParseToolCall_NotACall confirms we don't false-positive on real
// prose answers that happen to mention tools or contain braces.
func TestParseToolCall_NotACall(t *testing.T) {
	cases := []string{
		"There are 3 open tickets: ENG-1, ENG-2, ENG-3.",
		"You can use {} braces in JQL but I'm answering from cache.",
		"Sorry, I can't help with that.",
	}
	for _, s := range cases {
		if _, _, ok := parseToolCall(s); ok {
			t.Errorf("false positive on prose: %q", s)
		}
	}
}

// TestLooksLikeMangledToolCall covers the heuristic that triggers a
// soft self-correction nudge before giving up on tool use.
func TestLooksLikeMangledToolCall(t *testing.T) {
	mangled := []string{
		"TOOL: search\nRESULT: nothing yet",
		"I will call jira_search with the query",
		"calling the tool now",
		"jira_search { ...stuff... }",
	}
	clean := []string{
		"There are 3 tickets.",
		"Done — no action needed.",
		"",
	}
	for _, s := range mangled {
		if !looksLikeMangledToolCall(s) {
			t.Errorf("expected mangled flag for %q", s)
		}
	}
	for _, s := range clean {
		if looksLikeMangledToolCall(s) {
			t.Errorf("unexpected mangled flag for %q", s)
		}
	}
}

// TestStripCodeFences sanity checks fence removal.
func TestStripCodeFences(t *testing.T) {
	in := "before\n```json\n{\"a\":1}\n```\nafter"
	out := stripCodeFences(in)
	if !strings.Contains(out, `{"a":1}`) {
		t.Errorf("lost content: %q", out)
	}
	if strings.Contains(out, "```") {
		t.Errorf("fences still present: %q", out)
	}
}
