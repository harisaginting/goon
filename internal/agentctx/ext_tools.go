package agentctx

// ext_tools.go wires the env-driven external-service tools into the chat
// loop: Confluence (search the wiki, read a page) and the web tools
// (search, fetch a page). Unlike the jira_* and pr_* tools these have no
// board/host dependency — each reads its own config from the
// environment — so casual chat can reach them whenever they're set up.

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/harisaginting/goon/internal/atlassian"
	"github.com/harisaginting/goon/internal/tools"
)

// maxChatToolResult caps how much text a tool result feeds back into the
// chat context — a fetched web page can be 256 KB, which would blow the
// LLM context window. 8000 bytes is ~2000 tokens: plenty to answer from.
const maxChatToolResult = 8000

// confluenceConfigured reports whether Confluence credentials are set.
func confluenceConfigured() bool {
	return atlassian.Confluence().Filled()
}

// execConfluenceSearch searches Confluence pages (CQL or plain text).
func execConfluenceSearch(ctx context.Context, c ToolCall) (string, string) {
	q := strings.TrimSpace(c.Query)
	if q == "" {
		return `TOOL ERROR: confluence_search needs a "query".`, "confluence_search rejected (no query)"
	}
	cctx, cancel := context.WithTimeout(ctx, chatToolBudget)
	defer cancel()
	res, err := tools.NewConfluenceFromEnv().Run(cctx, map[string]string{
		"op": "search", "query": q, "limit": "10",
	})
	if err != nil {
		return "TOOL ERROR: confluence_search failed: " + err.Error() + ". Tell the user what went wrong.",
			fmt.Sprintf("confluence_search failed: %v", err)
	}
	out := strings.TrimSpace(res.Stdout)
	if out == "" {
		out = "(no results)"
	}
	return "CONFLUENCE SEARCH RESULTS (each line: id, title, url):\n" + clampForChat(out, maxChatToolResult) +
			"\n\nAnswer the user in prose. To read a page in full, call confluence_get with its id.",
		"confluence_search ok"
}

// execConfluenceGet fetches one Confluence page by id.
func execConfluenceGet(ctx context.Context, c ToolCall) (string, string) {
	id := strings.TrimSpace(c.PageID)
	if id == "" {
		return `TOOL ERROR: confluence_get needs a "page_id".`, "confluence_get rejected (no id)"
	}
	cctx, cancel := context.WithTimeout(ctx, chatToolBudget)
	defer cancel()
	res, err := tools.NewConfluenceFromEnv().Run(cctx, map[string]string{
		"op": "get_page", "page_id": id,
	})
	if err != nil {
		return "TOOL ERROR: confluence_get failed: " + err.Error() + ". Tell the user what went wrong.",
			fmt.Sprintf("confluence_get failed: %v", err)
	}
	body := strings.TrimSpace(res.Stdout)
	if body == "" {
		body = "(empty page)"
	}
	return "CONFLUENCE PAGE:\n" + clampForChat(body, maxChatToolResult) +
			"\n\nAnswer the user in prose from this page.",
		"confluence_get ok"
}

// execWebSearch runs a web search.
func execWebSearch(ctx context.Context, c ToolCall) (string, string) {
	q := strings.TrimSpace(c.Query)
	if q == "" {
		return `TOOL ERROR: web_search needs a "query".`, "web_search rejected (no query)"
	}
	wctx, cancel := context.WithTimeout(ctx, chatToolBudget)
	defer cancel()
	res, err := tools.WebSearch{}.Run(wctx, map[string]string{"query": q})
	if err != nil {
		return "TOOL ERROR: web_search failed: " + err.Error() + ". Tell the user what went wrong.",
			fmt.Sprintf("web_search failed: %v", err)
	}
	out := strings.TrimSpace(res.Stdout)
	if out == "" {
		out = "(no results)"
	}
	return "WEB SEARCH RESULTS:\n" + clampForChat(out, maxChatToolResult) +
			"\n\nAnswer the user in prose, citing the relevant result URLs.",
		"web_search ok"
}

// execWebFetch fetches a web page's readable text.
func execWebFetch(ctx context.Context, c ToolCall) (string, string) {
	u := strings.TrimSpace(c.URL)
	if u == "" {
		return `TOOL ERROR: web_fetch needs a "url".`, "web_fetch rejected (no url)"
	}
	wctx, cancel := context.WithTimeout(ctx, chatToolBudget)
	defer cancel()
	res, err := tools.FetchURL{}.Run(wctx, map[string]string{"url": u})
	if err != nil {
		return "TOOL ERROR: web_fetch failed: " + err.Error() + ". Tell the user what went wrong.",
			fmt.Sprintf("web_fetch failed: %v", err)
	}
	body := strings.TrimSpace(res.Stdout)
	if body == "" {
		body = "(empty page)"
	}
	return "WEB PAGE (" + u + "):\n" + clampForChat(body, maxChatToolResult) +
			"\n\nAnswer the user in prose from this page.",
		"web_fetch ok"
}

// clampForChat truncates a tool result so it can't blow the chat
// context window. Rune-safe so the result stays valid UTF-8.
func clampForChat(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n\n[... truncated for chat ...]"
}
