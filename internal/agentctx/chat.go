package agentctx

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/harisaginting/goon/internal/boards"
	"github.com/harisaginting/goon/internal/githost"
	"github.com/harisaginting/goon/internal/llm"
	"github.com/harisaginting/goon/internal/memory"
)

// maxChatToolIterations caps the LLM ↔ tool loop per chat turn. Each
// iteration costs an LLM call and a board API call, so we bound it
// tight — three searches is more than enough to answer any realistic
// chat question, and prevents a misbehaving model from burning quota.
const maxChatToolIterations = 3

// chatToolBudget is the per-search Jira call timeout. Generous enough
// for cold-start latency, tight enough that a hung connection doesn't
// freeze the chat handler.
const chatToolBudget = 20 * time.Second

// ToolCall is the wire-format the LLM emits when it wants to invoke
// a board tool. The chat handlers parse the whole LLM reply against
// this struct; if it parses cleanly, we execute the tool and
// re-prompt. Otherwise the reply is treated as the final prose
// answer.
//
// We picked the JSON-only protocol (vs OpenAI's tool-call API)
// because it works across every provider goon supports — Ollama,
// Anthropic, and the mock LLM — without any provider-specific code.
//
// Supported actions:
//   - "jira_search"      → read JQL results
//   - "jira_comment"     → post comment on a ticket
//   - "jira_transition"  → change ticket status
//   - "jira_update"      → edit ticket title/description/labels
type ToolCall struct {
	Action string `json:"action"`

	// jira_search
	JQL   string `json:"jql,omitempty"`
	Limit int    `json:"limit,omitempty"`

	// jira_comment / jira_transition / jira_update — all use Key
	Key string `json:"key,omitempty"`

	// jira_comment
	Body string `json:"body,omitempty"`

	// jira_transition — value is one of: "open", "in_progress",
	// "in_review", "blocked", "done" (matches boards.Status). The
	// system prompt teaches the LLM to use these canonical values.
	Status string `json:"status,omitempty"`

	// jira_update — leave a field empty/omitted to skip it. To
	// explicitly clear a field, send a single space (the executor
	// detects that and sends an empty value). We use this odd
	// indirection because Go's JSON decoder can't distinguish
	// "field missing" from "field set to empty string" with plain
	// string fields, and we want the simpler wire format on the
	// LLM side.
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	Labels      []string `json:"labels,omitempty"`

	// pr_get / pr_comment / pr_approve / pr_request_changes — a PR
	// reference: a full PR/MR URL, or "owner/repo#number". pr_comment /
	// pr_approve / pr_request_changes also use Body for the comment text.
	PR string `json:"pr,omitempty"`
	// pr_list — optional repo to scope to ("owner/repo").
	Repo string `json:"repo,omitempty"`
	// pr_list — optional filter; "review-requested" lists PRs awaiting
	// the current user's review.
	Filter string `json:"filter,omitempty"`

	// confluence_search / web_search — a free-text (or CQL) query.
	Query string `json:"query,omitempty"`
	// web_fetch — the page URL to fetch.
	URL string `json:"url,omitempty"`
	// confluence_get — the Confluence page id.
	PageID string `json:"page_id,omitempty"`
}

// validActions is the closed set of action strings parseToolCall
// recognises. Keeping it explicit (vs a default-any-Action approach)
// prevents the LLM from inventing a "jira_delete" the server will
// silently accept.
var validActions = map[string]bool{
	"jira_search":        true,
	"jira_comment":       true,
	"jira_transition":    true,
	"jira_transitions":   true,
	"jira_update":        true,
	"pr_get":             true,
	"pr_list":            true,
	"pr_review":          true,
	"pr_comment":         true,
	"pr_approve":         true,
	"pr_request_changes": true,
	"confluence_search":  true,
	"confluence_get":     true,
	"web_search":         true,
	"web_fetch":          true,
}

// ChatTurnOptions configures one ChatTurn invocation.
type ChatTurnOptions struct {
	LLM           llm.Provider // required
	Memory        *memory.Memory
	Board         boards.Board  // may implement boards.Searcher; may be nil
	Host          githost.Host  // git host; may implement PRReviewer; may be nil
	NotesDir      string        // "" → derive from storage
	SystemPrompt  string       // base persona (telegram or web)
	History       []llm.Message
	UserMessage   string
	MaxTokens     int     // 0 → 800
	Temperature   float64 // 0 → 0.4
}

// ChatTurnResult is what a single chat turn returns to the caller.
type ChatTurnResult struct {
	Reply     string        // the model's final prose answer
	NewTurns  []llm.Message // user + assistant pair to append to caller's history
	ToolCalls []string      // human-readable summary of every tool call this turn
}

// ChatTurn runs one user→assistant turn with an LLM ↔ search-jira tool
// loop in the middle. Behaviour:
//
//  1. Append UserMessage to history.
//  2. Send system prompt + GOON-STATE block + history to the LLM.
//  3. Parse the response:
//     - If first non-empty line parses as ToolCall, run the tool,
//       append the tool result as a system message, loop (up to
//       maxChatToolIterations times).
//     - Otherwise the response IS the final answer; return.
//  4. The caller appends NewTurns to whatever history store it owns.
//
// Errors are returned ONLY for genuine failures (no LLM, network).
// Tool failures (board misconfigured, JQL invalid) are surfaced to
// the LLM as system messages so it can apologize gracefully — the
// chat never errors out just because Jira said no.
func ChatTurn(ctx context.Context, opt ChatTurnOptions) (ChatTurnResult, error) {
	if opt.LLM == nil {
		return ChatTurnResult{}, fmt.Errorf("chat: no LLM provider")
	}
	if opt.MaxTokens == 0 {
		// 2048 instead of the old 800. Gemini 2.5 spends invisible
		// "thinking" tokens before answering and an 800-token cap
		// regularly produced empty replies. Most providers ignore the
		// excess; only the ones that bill per token pay anything, and
		// even then we typically use <500.
		opt.MaxTokens = 2048
	}
	if opt.Temperature == 0 {
		// 0.2 instead of 0.4 — weaker models drift into narrating
		// ("TOOL: search ... RESULT: ...") at higher temperatures.
		// Tool calls need to be near-deterministic.
		opt.Temperature = 0.2
	}

	// Append the user turn up front so the LLM sees what it's answering.
	userMsg := llm.Message{Role: llm.RoleUser, Content: opt.UserMessage}
	working := append([]llm.Message(nil), opt.History...)
	working = append(working, userMsg)

	// Build the system block fresh on every turn. The state block
	// reflects the most recent daemon snapshot; the tool block tells
	// the LLM whether a search tool is available right now.
	stateBlock := Build(opt.Memory, opt.NotesDir)
	toolBlock := buildToolBlock(opt.Board, opt.Host)

	systemMsgs := []llm.Message{
		{Role: llm.RoleSystem, Content: opt.SystemPrompt},
		{Role: llm.RoleSystem, Content: toolBlock},
		{Role: llm.RoleSystem, Content: stateBlock},
	}

	var (
		toolLog []string
		final   string
	)
	for i := 0; i < maxChatToolIterations+1; i++ {
		msgs := append([]llm.Message(nil), systemMsgs...)
		msgs = append(msgs, working...)

		out, err := opt.LLM.Generate(ctx, msgs, llm.Options{
			Temperature: opt.Temperature,
			MaxTokens:   opt.MaxTokens,
		})
		if err != nil {
			return ChatTurnResult{}, err
		}
		out = strings.TrimSpace(out)
		if out == "" {
			// Empty response — common Gemini failure mode after a
			// tool result, especially if maxOutputTokens was tight.
			// If we have tool calls already AND iterations left,
			// inject a "just answer in plain English" nudge and try
			// once more. Otherwise surface as a useful error so the
			// user knows why.
			if len(toolLog) > 0 && i < maxChatToolIterations {
				working = append(working, llm.Message{
					Role:    llm.RoleAssistant,
					Content: "",
				}, llm.Message{
					Role:    llm.RoleSystem,
					Content: "Your previous reply was empty. The user is waiting. Use the SEARCH RESULTS / ACTION OK message(s) above and answer the user in plain English in 1–3 short sentences. Do NOT emit any JSON. Do NOT call any more tools.",
				})
				continue
			}
			final = "(no response from model — try rephrasing, or check `goon doctor` for provider issues)"
			break
		}

		// Try to parse the response as a ToolCall.
		call, rest, ok := parseToolCall(out)
		if !ok {
			// Soft self-correction: if the response LOOKS like the
			// model tried to emit a tool call but mangled it
			// ("TOOL: search ...", "I will run jira_search..."),
			// nudge it once and retry instead of giving up. The
			// nudge is appended only on first detection per turn so
			// we don't loop forever on a stubborn model.
			if i < maxChatToolIterations && looksLikeMangledToolCall(out) {
				working = append(working, llm.Message{
					Role:    llm.RoleAssistant,
					Content: out,
				}, llm.Message{
					Role: llm.RoleSystem,
					Content: "Your previous reply tried to call a tool but the format was wrong. The correct format is a SINGLE LINE of raw JSON — no 'TOOL:' or 'RESULT:' prefix, no code fences, no prose, just the JSON object. Example: {\"action\":\"jira_search\",\"jql\":\"order by updated desc\",\"limit\":50}. Retry now with the correct format, OR answer the user in prose if you don't actually need a tool.",
				})
				continue
			}
			// No tool call — treat the whole response as the answer.
			final = out
			break
		}
		// We've hit the iteration cap and the model is still tool-calling.
		// Refuse to loop further; force a prose answer next time. Surface
		// what happened in toolLog so the UI can show "tried 3 searches".
		if i == maxChatToolIterations {
			working = append(working, llm.Message{
				Role:    llm.RoleAssistant,
				Content: out,
			}, llm.Message{
				Role: llm.RoleSystem,
				Content: "TOOL BUDGET EXHAUSTED — answer the user's original question now using whatever you've gathered. Do NOT emit another tool call.",
			})
			continue
		}
		// Echo the assistant's tool-call message back as part of history
		// so the next iteration sees it.
		working = append(working, llm.Message{
			Role:    llm.RoleAssistant,
			Content: out,
		})
		// Execute the tool.
		result, summary := executeToolCall(ctx, opt.Board, opt.Host, opt.LLM, opt.Memory, call)
		toolLog = append(toolLog, summary)
		_ = rest // unused — we always feed the tool result back, not the prose
		working = append(working, llm.Message{
			Role:    llm.RoleSystem,
			Content: result,
		})
	}

	return ChatTurnResult{
		Reply: final,
		NewTurns: []llm.Message{
			userMsg,
			{Role: llm.RoleAssistant, Content: final},
		},
		ToolCalls: toolLog,
	}, nil
}

// buildToolBlock teaches the LLM about whatever tools are wired this
// turn. The set of tools is dynamic — we only advertise capabilities
// the board actually supports, so the LLM can't try to call something
// that won't work. When no board is wired at all the block tells the
// LLM to skip tool calls entirely and answer from cached state.
func buildToolBlock(board boards.Board, host githost.Host) string {
	_, hasSearch := board.(boards.Searcher)
	_, hasUpdate := board.(boards.Updater)
	hasBoard := board != nil
	_, hasPR := host.(githost.PRReviewer)
	hasConfluence := confluenceConfigured()
	var sb strings.Builder
	sb.WriteString("# TOOLS YOU CAN CALL\n\n")

	sb.WriteString(`## CRITICAL OUTPUT FORMAT

When you call a tool, your ENTIRE reply for that turn must be ONE
LINE of raw JSON. The server PARSES your reply as JSON — if it
isn't pure JSON, the parser fails and the user sees garbage.

CORRECT:
  {"action":"jira_search","jql":"order by updated desc","limit":50}

ALL OF THESE ARE WRONG (the parser fails and the user gets nothing
useful):

  WRONG: TOOL: jira_search ... RESULT: ...
  WRONG: I will call jira_search with the query "..."
  WRONG: ` + "```json\n{...}\n```" + `
  WRONG: Here is the tool call: {"action":...}
  WRONG: To answer this I need to query Jira. {"action":...}

NEVER write "TOOL:", "RESULT:", "I will", or any English text on the
same turn as a tool call. The JSON object is the WHOLE reply or it
is not a tool call at all.

After the server runs the tool, you'll receive a SEARCH RESULTS or
ACTION OK message. THAT is when you switch to prose and confirm the
result to the user.

You may call tools at most 3 times per user message.

Only call WRITE tools (comment, transition, update) when the user
explicitly asks for that action. If they say "comment on ENG-123
that the build is green", call jira_comment. If they say "what does
ENG-123 say", just read or answer from cache.

`)

	if hasSearch {
		sb.WriteString(`## jira_search — read live JQL results

  {"action":"jira_search","jql":"<JQL>","limit":50}

JQL examples:
  - "project = ENG AND statusCategory != Done"
  - "assignee = currentUser() AND updated >= -7d"
  - "labels = bug AND priority = High ORDER BY created DESC"

Use this for ANY ticket question whose answer might be incomplete
from the GOON STATE cache. If GOON STATE clearly answers it already,
skip and respond in prose.

`)
	}

	if hasBoard {
		sb.WriteString(`## jira_comment — post a comment on a ticket

  {"action":"jira_comment","key":"ENG-123","body":"the comment text"}

The body is sent as plain text. Newlines work. Quote the user's
exact wording where they asked you to.

## jira_transition — move a ticket to a new status

  {"action":"jira_transition","key":"ENG-123","status":"Ready to Test"}

"status" is the status name as it exists on the user's board — pass
what the user actually said ("ready to test", "in QA", "done", …).
The server matches it against the ticket's REAL workflow transitions,
not a fixed vocabulary. If it doesn't match, the error lists the
ticket's actual available statuses — show those to the user verbatim
and ask which they meant. Never silently substitute a different
status.

## jira_transitions — list the statuses a ticket can move to

  {"action":"jira_transitions","key":"ENG-123"}

Returns the ticket's real available statuses. Use it when the user
asks what statuses exist, or when you're unsure of the exact name
before calling jira_transition.

`)
	}

	if hasUpdate {
		sb.WriteString(`## jira_update — edit a ticket's fields

  {"action":"jira_update","key":"ENG-123","title":"new title","description":"new description","labels":["bug","p1"]}

Include only the fields you want to change; omit the rest. To clear
labels, send "labels": []. Title and description are sent verbatim.

`)
	}

	if hasPR {
		sb.WriteString(`## pr_get — read a pull request (incl. reviewers)

  {"action":"pr_get","pr":"<PR URL or owner/repo#number>"}

Returns title, author, state, branch, and the reviewer list with each
reviewer's status (approved / changes_requested / commented /
pending). Use this for "who is reviewing PR X", "did Y approve",
"status of <PR url>". The user often pastes a full Bitbucket / GitHub
/ GitLab PR URL — pass it through verbatim as "pr".

## pr_list — list pull requests

  {"action":"pr_list","repo":"owner/repo"}
  {"action":"pr_list","filter":"review-requested"}

With "repo" → open PRs in that repo. With "filter":"review-requested"
→ PRs awaiting the current user's review across all repos.

## pr_review — draft an AI review of a PR (and post on confirmation)

  {"action":"pr_review","pr":"<PR URL or owner/repo#number>"}

Use this when the user says "review PR X", "what do you think of <url>",
or pastes a PR URL with review intent. The tool fetches the diff, runs
the model, and hands back the draft with explicit instructions to:
  1. show the user the review verbatim,
  2. ask "post this as a comment on the PR?",
  3. on "yes", call pr_comment with body = the EXACT draft text.
Follow those instructions exactly — do NOT paraphrase the review when
posting; the body must match what you showed the user.

## pr_comment — post a comment on a pull request

  {"action":"pr_comment","pr":"<PR ref>","body":"the comment text"}

## pr_approve — approve a pull request

  {"action":"pr_approve","pr":"<PR ref>","body":"optional approval note"}

## pr_request_changes — request changes on a pull request

  {"action":"pr_request_changes","pr":"<PR ref>","body":"what needs to change"}

Only call pr_approve / pr_request_changes / pr_comment when the user
explicitly asks you to act. "who's reviewing PR 12" → pr_get;
"approve PR 12" → pr_approve.

`)
	}

	if hasConfluence {
		sb.WriteString(`## confluence_search — search the Confluence wiki

  {"action":"confluence_search","query":"<text or CQL>"}

## confluence_get — read one Confluence page in full

  {"action":"confluence_get","page_id":"<id from a confluence_search result>"}

`)
	}

	sb.WriteString(`## web_search — search the web

  {"action":"web_search","query":"<search text>"}

## web_fetch — fetch the readable text of a web page

  {"action":"web_fetch","url":"https://..."}

Use web_search / web_fetch for general knowledge your other tools and
the GOON STATE block can't answer — library docs, error messages,
current facts. Do NOT use them for the user's own tickets, PRs or wiki;
those have dedicated tools above.

`)

	sb.WriteString(`Rules:
  - The JSON MUST be the entire response — no prose before/after.
  - Tool failures come back as TOOL ERROR system messages — apologize
    in your next turn and either retry with a fix, or tell the user.
  - Action successes come back as ACTION OK system messages —
    confirm to the user in prose ("✓ commented on ENG-123 …").
  - TRUTHFULNESS: report exactly what the ACTION OK / TOOL ERROR
    message states — the real resulting status or outcome. NEVER claim
    a status, value or result the tool did not confirm; if a ticket
    landed in a different status than the user asked for, say so
    plainly. If a tool failed, report the actual error — do not invent
    a reason or a missing capability. You DO have live Jira / PR /
    Confluence / web access through these tools.
`)
	return sb.String()
}

// parseToolCall extracts a tool call from an LLM reply. It's
// deliberately forgiving: weaker local models (Ollama mistral, llama)
// love to narrate their tool calls ("TOOL: jira_search ...\nRESULT:
// ...\n```json\n{...}\n```"). The strict "JSON only, nothing else"
// system prompt fights this, but real-world traffic shows the model
// often ignores it, so the parser must rescue what it can.
//
// Strategy:
//   1. Strip Markdown code fences (```...```).
//   2. If the trimmed string starts with '{' and has a matching brace
//      that decodes into a known ToolCall, accept it. (Strict path.)
//   3. Otherwise scan the WHOLE reply for any `{"action":"<known>"…}`
//      blob, balance braces around it, and try to decode. (Salvage
//      path — handles "TOOL: search… ```json {action:...} ```".)
//   4. If none decodes into a recognised action, treat the reply as
//      prose.
//
// Returns (call, "", ok). The previously-returned "rest" is dropped;
// the caller doesn't use it (we always feed the tool result back as
// the next message, never the prose preamble).
func parseToolCall(s string) (ToolCall, string, bool) {
	// Quick-strip code fences anywhere in the string. We replace
	// rather than splitting because some models close the fence on
	// the same line as the JSON.
	cleaned := stripCodeFences(s)
	cleaned = strings.TrimSpace(cleaned)

	// Strict path: starts with '{' and decodes cleanly.
	if strings.HasPrefix(cleaned, "{") {
		end := matchingBrace(cleaned)
		if end > 0 {
			var c ToolCall
			if err := json.Unmarshal([]byte(cleaned[:end+1]), &c); err == nil {
				if validActions[c.Action] {
					return c, "", true
				}
			}
		}
	}

	// Salvage path: find any embedded `{"action":"..."}` blob. Look
	// for the canonical opener — `"action"` quoted — so we don't pick
	// up unrelated JSON in the prose. validActions filters the result,
	// so a generic marker is safe.
	for _, marker := range []string{`"action":"`, `"action": "`} {
		idx := strings.Index(cleaned, marker)
		if idx < 0 {
			continue
		}
		// Walk backwards to find the opening brace.
		open := -1
		for j := idx; j >= 0; j-- {
			if cleaned[j] == '{' {
				open = j
				break
			}
		}
		if open < 0 {
			continue
		}
		end := matchingBrace(cleaned[open:])
		if end < 0 {
			continue
		}
		blob := cleaned[open : open+end+1]
		var c ToolCall
		if err := json.Unmarshal([]byte(blob), &c); err == nil {
			if validActions[c.Action] {
				return c, "", true
			}
		}
	}

	return ToolCall{}, "", false
}

// stripCodeFences removes ```...``` blocks' fences (the triple-backtick
// markers themselves), keeping the content inside. Optional language
// tag after the opening fence is also dropped. We do this in-place so
// nested or multiple fences are all handled.
func stripCodeFences(s string) string {
	for {
		i := strings.Index(s, "```")
		if i < 0 {
			return s
		}
		// Find end of opening fence line.
		nl := strings.IndexByte(s[i:], '\n')
		var openEnd int
		if nl < 0 {
			openEnd = len(s) // unterminated fence; treat rest as content
		} else {
			openEnd = i + nl + 1
		}
		// Find closing fence.
		close := strings.Index(s[openEnd:], "```")
		if close < 0 {
			// No closer — drop the opener and stop. Salvage what we can.
			s = s[:i] + s[openEnd:]
			return s
		}
		body := s[openEnd : openEnd+close]
		s = s[:i] + body + s[openEnd+close+3:]
	}
}

// matchingBrace scans s starting at index 0 (which must be '{') and
// returns the index of the matching '}', or -1 on mismatch. Skips
// JSON strings so a brace inside a quoted value doesn't confuse it.
func matchingBrace(s string) int {
	if len(s) == 0 || s[0] != '{' {
		return -1
	}
	depth := 0
	inStr := false
	esc := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if esc {
			esc = false
			continue
		}
		if c == '\\' && inStr {
			esc = true
			continue
		}
		if c == '"' {
			inStr = !inStr
			continue
		}
		if inStr {
			continue
		}
		switch c {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// executeToolCall runs the requested tool and returns (system_message,
// human_summary). system_message is what we feed back into the next
// LLM turn; human_summary is a short string the UI can show ("ran
// jira_search jql=…").
func executeToolCall(ctx context.Context, board boards.Board, host githost.Host, llmProv llm.Provider, mem *memory.Memory, c ToolCall) (string, string) {
	switch c.Action {
	case "jira_search":
		return execJiraSearch(ctx, board, mem, c)
	case "jira_comment":
		return execJiraComment(ctx, board, c)
	case "jira_transition":
		return execJiraTransition(ctx, board, c)
	case "jira_transitions":
		return execJiraListTransitions(ctx, board, c)
	case "jira_update":
		return execJiraUpdate(ctx, board, c)
	case "pr_get":
		return execPRGet(ctx, host, c)
	case "pr_list":
		return execPRList(ctx, host, c)
	case "pr_comment":
		return execPRComment(ctx, host, c)
	case "pr_approve":
		return execPRApprove(ctx, host, c)
	case "pr_request_changes":
		return execPRRequestChanges(ctx, host, c)
	case "pr_review":
		return execPRReview(ctx, host, llmProv, c)
	case "confluence_search":
		return execConfluenceSearch(ctx, c)
	case "confluence_get":
		return execConfluenceGet(ctx, c)
	case "web_search":
		return execWebSearch(ctx, c)
	case "web_fetch":
		return execWebFetch(ctx, c)
	default:
		// Should never happen — parseToolCall already filters action.
		return fmt.Sprintf("TOOL ERROR: unknown action %q.", c.Action),
			"unknown action " + c.Action
	}
}

func execJiraSearch(ctx context.Context, board boards.Board, mem *memory.Memory, c ToolCall) (string, string) {
	if board == nil {
		return "TOOL ERROR: no board is configured — answer from GOON STATE only.",
			"jira_search skipped (no board)"
	}
	searcher, ok := board.(boards.Searcher)
	if !ok {
		return "TOOL ERROR: the configured board does not support live search — answer from GOON STATE only.",
			"jira_search skipped (board not searchable)"
	}
	jql := strings.TrimSpace(c.JQL)
	if jql == "" {
		return "TOOL ERROR: empty jql. Re-emit the tool call with a non-empty JQL string, or answer from GOON STATE.",
			"jira_search rejected (empty jql)"
	}
	searchCtx, cancel := context.WithTimeout(ctx, chatToolBudget)
	defer cancel()
	tickets, err := searcher.Search(searchCtx, jql, c.Limit)
	if err != nil {
		return fmt.Sprintf("TOOL ERROR: jira_search failed: %s. Try a simpler JQL, or answer from GOON STATE.", err.Error()),
			fmt.Sprintf("jira_search failed: %v", err)
	}
	// Persist results into memory so the next /tickets call (CLI or
	// web) sees them too. Cheap win — the user just paid the API call.
	if mem != nil {
		for _, t := range tickets {
			mem.SeenTicket(memory.TicketSnapshot{
				ID:        t.ID,
				Source:    t.Source,
				Key:       t.Key,
				Title:     t.Title,
				URL:       t.URL,
				Status:    string(t.Status),
				Project:   t.Project,
				Assignee:  t.Assignee,
				Labels:    t.Labels,
				LastSeen:  time.Now(),
				UpdatedAt: t.UpdatedAt,
			})
		}
	}
	// Format compactly for the LLM. We include all the fields that
	// the chat persona's "list KEY — title [status] assignee=…" line
	// needs, so the model can answer without re-asking.
	var sb strings.Builder
	fmt.Fprintf(&sb, "SEARCH RESULTS (jira_search, jql=%q): %d ticket(s)\n", jql, len(tickets))
	if len(tickets) == 0 {
		sb.WriteString("(no matches)\n")
	} else {
		for _, t := range tickets {
			fmt.Fprintf(&sb, "  %s [%s] assignee=%s project=%s — %s\n",
				t.Key,
				safeStr(string(t.Status), "unknown"),
				safeStr(t.Assignee, "—"),
				safeStr(t.Project, "—"),
				oneLine(t.Title),
			)
			if t.URL != "" {
				fmt.Fprintf(&sb, "    url: %s\n", t.URL)
			}
		}
	}
	sb.WriteString("\nNow answer the user in prose using these results. Do NOT emit another tool call unless you genuinely need a different query.")
	return sb.String(), fmt.Sprintf("jira_search ok (jql=%q, %d hits)", jql, len(tickets))
}

// execJiraComment posts a comment on a ticket. We use board.Comment
// directly (always available — it's part of Board, not optional).
func execJiraComment(ctx context.Context, board boards.Board, c ToolCall) (string, string) {
	if board == nil {
		return "TOOL ERROR: no board configured — cannot comment.",
			"jira_comment skipped (no board)"
	}
	key := strings.TrimSpace(c.Key)
	body := strings.TrimSpace(c.Body)
	if key == "" || body == "" {
		return "TOOL ERROR: jira_comment needs both \"key\" and \"body\". Retry with the ticket key and the comment text, or answer in prose.",
			"jira_comment rejected (missing key/body)"
	}
	actCtx, cancel := context.WithTimeout(ctx, chatToolBudget)
	defer cancel()
	if err := board.Comment(actCtx, key, body); err != nil {
		return fmt.Sprintf("TOOL ERROR: jira_comment on %s failed: %s. Tell the user what went wrong.", key, err.Error()),
			fmt.Sprintf("jira_comment %s failed: %v", key, err)
	}
	preview := oneLine(body)
	return fmt.Sprintf("ACTION OK: commented on %s — %q. Confirm this to the user in prose.", key, preview),
		fmt.Sprintf("jira_comment %s ok (%q)", key, preview)
}

// execJiraTransition moves a ticket to a new status. For boards with a
// real custom workflow (Jira) it resolves the user's wording against
// the ticket's actual transitions via TransitionByName — so "ready to
// test" reaches the genuine "Ready to Test" status instead of being
// bucketed to "open" by MapStatus. Boards with only the canonical
// lifecycle (GitHub Issues) fall back to the MapStatus path.
func execJiraTransition(ctx context.Context, board boards.Board, c ToolCall) (string, string) {
	if board == nil {
		return "TOOL ERROR: no board configured — cannot transition.",
			"jira_transition skipped (no board)"
	}
	key := strings.TrimSpace(c.Key)
	status := strings.TrimSpace(c.Status)
	if key == "" || status == "" {
		return `TOOL ERROR: jira_transition needs both "key" and "status".`,
			"jira_transition rejected (missing key/status)"
	}
	actCtx, cancel := context.WithTimeout(ctx, chatToolBudget)
	defer cancel()

	// Preferred path: boards that expose their real workflow. Pass the
	// user's wording straight through — the board matches it against
	// the ticket's actual transition names.
	if tr, ok := board.(boards.TransitionResolver); ok {
		applied, err := tr.TransitionByName(actCtx, key, status)
		if err != nil {
			return "TOOL ERROR: jira_transition on " + key + " failed: " + err.Error() +
					" — show the user the available statuses from this message verbatim and ask which one they meant. Do NOT retry with a guess.",
				fmt.Sprintf("jira_transition %s failed: %v", key, err)
		}
		return fmt.Sprintf("ACTION OK: %s is now in status %q. Tell the user exactly that, using the status name %q verbatim — do not paraphrase it or substitute the wording they originally used.", key, applied, applied),
			fmt.Sprintf("jira_transition %s → %s ok", key, applied)
	}

	// Fallback: boards with only the canonical open/closed lifecycle.
	target := boards.MapStatus(strings.ToLower(status))
	if target == boards.StatusUnknown {
		return fmt.Sprintf("TOOL ERROR: this board only understands the canonical lifecycle and %q didn't match. Use one of: open, in_progress, in_review, blocked, done.", status),
			fmt.Sprintf("jira_transition rejected (bad status %q)", status)
	}
	if err := board.Transition(actCtx, key, target); err != nil {
		return fmt.Sprintf("TOOL ERROR: jira_transition on %s → %s failed: %s.", key, target, err.Error()),
			fmt.Sprintf("jira_transition %s → %s failed: %v", key, target, err)
	}
	return fmt.Sprintf("ACTION OK: transitioned %s → %s. Confirm this to the user in prose.", key, target),
		fmt.Sprintf("jira_transition %s → %s ok", key, target)
}

// execJiraListTransitions lists the real status names a ticket can move
// to right now. Answers "what statuses are available" and lets the LLM
// learn the exact name before a transition.
func execJiraListTransitions(ctx context.Context, board boards.Board, c ToolCall) (string, string) {
	if board == nil {
		return "TOOL ERROR: no board configured.", "jira_transitions skipped (no board)"
	}
	tr, ok := board.(boards.TransitionResolver)
	if !ok {
		return "TOOL ERROR: the configured board doesn't expose its workflow transitions.",
			"jira_transitions skipped (unsupported)"
	}
	key := strings.TrimSpace(c.Key)
	if key == "" {
		return `TOOL ERROR: jira_transitions needs a ticket "key" — Jira transitions are per-ticket (they depend on the ticket's current status).`,
			"jira_transitions rejected (no key)"
	}
	lctx, cancel := context.WithTimeout(ctx, chatToolBudget)
	defer cancel()
	names, err := tr.ListTransitions(lctx, key)
	if err != nil {
		return "TOOL ERROR: jira_transitions failed: " + err.Error(),
			fmt.Sprintf("jira_transitions %s failed: %v", key, err)
	}
	if len(names) == 0 {
		return "AVAILABLE STATUSES for " + key + ": (none — the ticket is likely in a terminal status). Tell the user that.",
			"jira_transitions ok (0)"
	}
	return "AVAILABLE STATUSES for " + key + " — the ticket can move to any of these EXACT names: " +
			strings.Join(names, ", ") + "\nAnswer the user with this list; if they then ask to transition, use one of these names verbatim.",
		fmt.Sprintf("jira_transitions %s ok (%d)", key, len(names))
}

// execJiraUpdate edits one or more mutable fields on a ticket.
// Requires the board to implement boards.Updater. Empty / omitted
// fields are skipped (vs cleared) so the LLM only ever changes what
// the user asked it to.
func execJiraUpdate(ctx context.Context, board boards.Board, c ToolCall) (string, string) {
	if board == nil {
		return "TOOL ERROR: no board configured — cannot update.",
			"jira_update skipped (no board)"
	}
	updater, ok := board.(boards.Updater)
	if !ok {
		return "TOOL ERROR: the configured board does not support ticket updates. Apologize to the user; they will need to edit the ticket directly.",
			"jira_update skipped (board not updatable)"
	}
	key := strings.TrimSpace(c.Key)
	if key == "" {
		return "TOOL ERROR: jira_update needs \"key\". Retry with the ticket key.",
			"jira_update rejected (missing key)"
	}
	patch := boards.TicketPatch{}
	changed := []string{}
	if c.Title != "" {
		t := c.Title
		patch.Title = &t
		changed = append(changed, "title")
	}
	if c.Description != "" {
		d := c.Description
		patch.Description = &d
		changed = append(changed, "description")
	}
	if c.Labels != nil {
		patch.Labels = c.Labels
		changed = append(changed, fmt.Sprintf("labels=%v", c.Labels))
	}
	if len(changed) == 0 {
		return "TOOL ERROR: jira_update was called with nothing to change. Include title, description, and/or labels.",
			"jira_update rejected (no fields)"
	}
	actCtx, cancel := context.WithTimeout(ctx, chatToolBudget)
	defer cancel()
	if err := updater.Update(actCtx, key, patch); err != nil {
		return fmt.Sprintf("TOOL ERROR: jira_update on %s failed: %s. Tell the user what went wrong.", key, err.Error()),
			fmt.Sprintf("jira_update %s failed: %v", key, err)
	}
	return fmt.Sprintf("ACTION OK: updated %s (%s). Confirm this to the user in prose.",
			key, strings.Join(changed, ", ")),
		fmt.Sprintf("jira_update %s ok (%s)", key, strings.Join(changed, ", "))
}

// looksLikeMangledToolCall detects "the model tried to call a tool
// but wrote prose around it" — the failure mode that weaker Ollama
// models exhibit. Heuristics, not parsing:
//   - Mentions "TOOL:" or "RESULT:" on its own line (the narration
//     pattern observed in the wild).
//   - Contains a tool action name (jira_search, jira_comment, etc.)
//     somewhere AND an opening brace, but parseToolCall couldn't
//     extract a clean call from it.
//
// Conservative: false negatives are fine (the model gets to answer
// in prose), false positives cost one wasted LLM call per turn.
func looksLikeMangledToolCall(s string) bool {
	lower := strings.ToLower(s)
	// Obvious narration patterns.
	if strings.Contains(lower, "tool:") && strings.Contains(lower, "result") {
		return true
	}
	if strings.Contains(lower, "i will call") || strings.Contains(lower, "i'll call") ||
		strings.Contains(lower, "calling jira") || strings.Contains(lower, "calling the tool") {
		return true
	}
	// Tool name + opening brace but no clean parse → likely mangled.
	// Derived from validActions so every tool (jira_*, pr_*,
	// confluence_*, web_*) is covered without a second list to keep
	// in sync.
	if strings.Contains(s, "{") {
		for name := range validActions {
			if strings.Contains(lower, name) {
				return true
			}
		}
	}
	return false
}

func safeStr(s, fallback string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return fallback
	}
	return s
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", "")
	if len(s) > 160 {
		s = s[:157] + "..."
	}
	return s
}
