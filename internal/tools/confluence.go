package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/harisaginting/goon/internal/atlassian"
	"github.com/harisaginting/goon/internal/logx"
)

// Confluence is a thin tool over Atlassian Cloud's Confluence REST API.
//
// It supports two operations selected by the "op" arg:
//
//   - "search":    op=search, query=<CQL or text>, limit=<int>
//   - "get_page":  op=get_page, page_id=<id>
//
// Auth: CONFLUENCE_EMAIL + CONFLUENCE_API_TOKEN, falling back to the shared
// ATLASSIAN_EMAIL + ATLASSIAN_API_TOKEN. The base URL falls back to
// ATLASSIAN_BASE_URL with "/wiki" appended (the standard Cloud layout).
type Confluence struct {
	BaseURL  string
	Email    string
	APIToken string
	HTTP     *http.Client
}

// NewConfluenceFromEnv reads config from env. The tool is registered even if
// not fully configured — it errors at Run time instead of at startup.
func NewConfluenceFromEnv() *Confluence {
	c := atlassian.Confluence()
	return &Confluence{
		BaseURL:  c.BaseURL,
		Email:    c.Email,
		APIToken: c.APIToken,
		HTTP:     logx.InstrumentClient("confluence", &http.Client{Timeout: 20 * time.Second}),
	}
}

func (*Confluence) Name() string { return "confluence" }
func (*Confluence) Description() string {
	return "search Confluence pages or fetch a page by id"
}
func (*Confluence) Schema() map[string]string {
	return map[string]string{
		"op":      `"search" or "get_page"`,
		"query":   "CQL or plain text (search only)",
		"page_id": "Confluence page id (get_page only)",
		"limit":   "max results (search only, default 10)",
	}
}

func (c *Confluence) Run(ctx context.Context, args map[string]string) (Result, error) {
	if c.BaseURL == "" || c.Email == "" || c.APIToken == "" {
		return Result{ToolName: "confluence"},
			errors.New("confluence: set CONFLUENCE_BASE_URL/CONFLUENCE_EMAIL/CONFLUENCE_API_TOKEN (or shared ATLASSIAN_BASE_URL/ATLASSIAN_EMAIL/ATLASSIAN_API_TOKEN)")
	}
	op := strings.ToLower(strings.TrimSpace(args["op"]))
	switch op {
	case "search", "":
		return c.search(ctx, args)
	case "get_page":
		return c.getPage(ctx, args)
	default:
		return Result{ToolName: "confluence"}, fmt.Errorf("confluence: unknown op %q", op)
	}
}

func (c *Confluence) search(ctx context.Context, args map[string]string) (Result, error) {
	q := strings.TrimSpace(args["query"])
	if q == "" {
		return Result{ToolName: "confluence"}, errors.New(`confluence search: "query" is required`)
	}
	limit := args["limit"]
	if limit == "" {
		limit = "10"
	}
	// Build CQL: if it doesn't look like CQL, wrap as text search.
	cql := q
	if !strings.Contains(q, "=") && !strings.Contains(q, "~") {
		cql = fmt.Sprintf(`text ~ "%s"`, escapeCQL(q))
	}
	u := c.BaseURL + "/rest/api/content/search?cql=" + url.QueryEscape(cql) + "&limit=" + url.QueryEscape(limit)
	body, err := c.do(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Result{ToolName: "confluence", Err: err}, err
	}
	var parsed struct {
		Results []struct {
			ID    string `json:"id"`
			Type  string `json:"type"`
			Title string `json:"title"`
			Links struct {
				WebUI string `json:"webui"`
			} `json:"_links"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Result{ToolName: "confluence", Err: err}, err
	}
	var b strings.Builder
	for _, r := range parsed.Results {
		fmt.Fprintf(&b, "%s\t%s\t%s%s\n", r.ID, r.Title, c.BaseURL, r.Links.WebUI)
	}
	if b.Len() == 0 {
		b.WriteString("(no results)\n")
	}
	return Result{ToolName: "confluence", Stdout: b.String()}, nil
}

func (c *Confluence) getPage(ctx context.Context, args map[string]string) (Result, error) {
	id := strings.TrimSpace(args["page_id"])
	if id == "" {
		return Result{ToolName: "confluence"}, errors.New(`confluence get_page: "page_id" is required`)
	}
	u := c.BaseURL + "/rest/api/content/" + url.PathEscape(id) + "?expand=body.storage,version,space"
	body, err := c.do(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Result{ToolName: "confluence", Err: err}, err
	}
	var parsed struct {
		ID    string `json:"id"`
		Title string `json:"title"`
		Body  struct {
			Storage struct {
				Value string `json:"value"`
			} `json:"storage"`
		} `json:"body"`
		Space struct {
			Key string `json:"key"`
		} `json:"space"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Result{ToolName: "confluence", Err: err}, err
	}
	out := fmt.Sprintf("# %s\n(space=%s, id=%s)\n\n%s\n",
		parsed.Title, parsed.Space.Key, parsed.ID, stripHTML(parsed.Body.Storage.Value))
	return Result{ToolName: "confluence", Stdout: out}, nil
}

func (c *Confluence) do(ctx context.Context, method, url string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Basic "+basicAuth(c.Email, c.APIToken))
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("confluence http %d: %s", resp.StatusCode, truncate(string(raw), 400))
	}
	return raw, nil
}

func basicAuth(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}

// escapeCQL escapes double quotes inside a CQL string literal.
func escapeCQL(s string) string {
	return strings.ReplaceAll(s, `"`, `\"`)
}

// stripHTML strips angle-bracket tags. Good enough for previews; not a real parser.
// It inserts a space wherever a tag is removed so adjacent words don't merge.
func stripHTML(s string) string {
	var b strings.Builder
	depth := 0
	tagJustClosed := false
	for _, r := range s {
		switch r {
		case '<':
			depth++
		case '>':
			if depth > 0 {
				depth--
				if depth == 0 {
					tagJustClosed = true
				}
			}
		default:
			if depth == 0 {
				if tagJustClosed {
					b.WriteByte(' ')
					tagJustClosed = false
				}
				b.WriteRune(r)
			}
		}
	}
	// Collapse runs of whitespace.
	out := strings.Fields(b.String())
	return strings.Join(out, " ")
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
