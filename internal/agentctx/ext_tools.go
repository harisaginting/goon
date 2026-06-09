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

// execObsidianList lists notes in the Obsidian vault, optionally under a folder.
func execObsidianList(c ToolCall) (string, string) {
	notes, err := tools.ObsidianList(c.Folder)
	if err != nil {
		return "TOOL ERROR: obsidian_list failed: " + err.Error() + ". Tell the user what went wrong.",
			"obsidian_list failed: " + err.Error()
	}
	if notes == "" {
		if c.Folder != "" {
			return "OBSIDIAN NOTES (no notes found under " + c.Folder + "):\n\nTell the user the folder appears to be empty.",
				"obsidian_list empty"
		}
		return "OBSIDIAN NOTES (vault is empty):\n\nTell the user the vault has no markdown notes yet.",
			"obsidian_list empty"
	}
	return "OBSIDIAN NOTES:\n" + clampForChat(notes, maxChatToolResult) +
		"\n\nList these notes to the user. To read one in full, call obsidian_read with its path.",
		"obsidian_list ok"
}

// execObsidianRead reads one Obsidian note by vault-relative path.
func execObsidianRead(c ToolCall) (string, string) {
	if c.Note == "" {
		return `TOOL ERROR: obsidian_read needs a "note" path.`, "obsidian_read rejected (no note)"
	}
	content, err := tools.ObsidianRead(c.Note)
	if err != nil {
		return "TOOL ERROR: obsidian_read failed: " + err.Error() + ". Tell the user what went wrong.",
			"obsidian_read failed: " + err.Error()
	}
	return "OBSIDIAN NOTE (" + c.Note + "):\n" + clampForChat(content, maxChatToolResult) +
		"\n\nAnswer the user in prose from this note.",
		"obsidian_read ok"
}

// execObsidianSearch searches across all Obsidian vault notes.
func execObsidianSearch(c ToolCall) (string, string) {
	q := strings.TrimSpace(c.Query)
	if q == "" {
		return `TOOL ERROR: obsidian_search needs a "query".`, "obsidian_search rejected (no query)"
	}
	results, err := tools.ObsidianSearch(q, 30)
	if err != nil {
		return "TOOL ERROR: obsidian_search failed: " + err.Error() + ". Tell the user what went wrong.",
			"obsidian_search failed: " + err.Error()
	}
	if results == "" {
		return fmt.Sprintf("OBSIDIAN SEARCH: no matches for %q in the vault.\n\nTell the user nothing matched.", q),
			"obsidian_search empty"
	}
	return "OBSIDIAN SEARCH RESULTS (note:line: text):\n" + clampForChat(results, maxChatToolResult) +
		"\n\nSummarise the relevant hits for the user. Call obsidian_read to get the full note if needed.",
		"obsidian_search ok"
}

// execObsidianSync runs git pull + store reload and reports the result.
func execObsidianSync() (string, string) {
	msg := tools.ObsidianSync()
	return "OBSIDIAN SYNC RESULT:\n" + msg + "\n\nReport this to the user.",
		"obsidian_sync ok"
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
