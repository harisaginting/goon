package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/harisaginting/goon/internal/logx"
)

// FetchURL fetches an arbitrary URL and returns the (sanitised) body so
// the agent can read documentation, error messages, package READMEs
// etc. instead of hallucinating from training data.
//
// Behaviour:
//   - HTTPS only by default. Set GOON_FETCH_ALLOW_HTTP=1 to allow http://
//     (useful for localhost dev docs).
//   - Capped at 256 KB. Larger bodies are truncated with a notice.
//   - HTML content-type → naïve tag stripper (good enough for docs).
//   - text/* and application/json → pass through verbatim.
//   - Other types → "unsupported content-type" so the agent doesn't get
//     a screen of binary garbage.
type FetchURL struct{}

func (FetchURL) Name() string { return "fetch_url" }
func (FetchURL) Description() string {
	return "fetch the body of an https URL (docs, error pages, READMEs). https only unless GOON_FETCH_ALLOW_HTTP=1."
}
func (FetchURL) Schema() map[string]string {
	return map[string]string{
		"url": "absolute URL (https required unless allowed via env)",
	}
}

func (FetchURL) Run(ctx context.Context, args map[string]string) (Result, error) {
	raw := strings.TrimSpace(args["url"])
	if raw == "" {
		return Result{}, fmt.Errorf("fetch_url: url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Result{}, fmt.Errorf("fetch_url: bad url: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" {
		if !(scheme == "http" && os.Getenv("GOON_FETCH_ALLOW_HTTP") == "1") {
			return Result{}, fmt.Errorf("fetch_url: only https is allowed (got %q). Set GOON_FETCH_ALLOW_HTTP=1 to permit http://.", scheme)
		}
	}

	cli := fetchClient()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("User-Agent", "goon/1.0 (+https://github.com/harisaginting/goon)")
	req.Header.Set("Accept", "text/html, text/plain, application/json, text/*;q=0.9, */*;q=0.5")

	resp, err := cli.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("fetch_url: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return Result{}, fmt.Errorf("fetch_url: http %d: %s", resp.StatusCode, truncForLog(string(body), 400))
	}

	const maxBytes = 256 * 1024
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return Result{}, fmt.Errorf("fetch_url: read body: %w", err)
	}
	truncated := false
	if len(body) > maxBytes {
		body = body[:maxBytes]
		truncated = true
	}

	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	var out string
	switch {
	case strings.Contains(ct, "html"):
		out = stripHTMLDocs(string(body))
	case strings.HasPrefix(ct, "text/"), strings.Contains(ct, "json"), strings.Contains(ct, "xml"), strings.Contains(ct, "javascript"):
		out = string(body)
	case ct == "":
		out = string(body) // best-effort
	default:
		return Result{}, fmt.Errorf("fetch_url: unsupported content-type %q (use a URL that returns text or html)", ct)
	}

	if truncated {
		out += "\n\n[... truncated at 256 KB ...]"
	}
	return Result{ToolName: "fetch_url", Stdout: out}, nil
}

// WebSearch hits a real search engine so the agent can find pages it
// doesn't know the URL of. Two backends, in priority order:
//
//	1. Google Custom Search JSON API — when both GOOGLE_API_KEY and
//	   GOOGLE_CSE_ID are set. 100 free queries/day per CSE.
//	2. DuckDuckGo HTML endpoint — no auth, no quota, lower-quality
//	   ranking. Scraped as HTML; resilient to small layout changes.
//
// The tool never invents URLs. Each result has {title, url, snippet}.
type WebSearch struct{}

func (WebSearch) Name() string { return "web_search" }
func (WebSearch) Description() string {
	return "search the web for a query — uses Google CSE when GOOGLE_API_KEY+GOOGLE_CSE_ID are set, else falls back to DuckDuckGo. Returns up to 8 results."
}
func (WebSearch) Schema() map[string]string {
	return map[string]string{
		"query": "search query (plain text)",
	}
}

func (WebSearch) Run(ctx context.Context, args map[string]string) (Result, error) {
	q := strings.TrimSpace(args["query"])
	if q == "" {
		return Result{}, fmt.Errorf("web_search: query is required")
	}
	cseKey := strings.TrimSpace(os.Getenv("GOOGLE_API_KEY"))
	cseID := strings.TrimSpace(os.Getenv("GOOGLE_CSE_ID"))
	if cseKey != "" && cseID != "" {
		if results, err := googleCSE(ctx, q, cseKey, cseID); err == nil && len(results) > 0 {
			return Result{ToolName: "web_search", Stdout: formatResults(results) + "\n[via google_cse]"}, nil
		} else if err != nil {
			logx.Warn("web_search.google_cse_failed", "error", err.Error())
		}
	}
	results, err := duckDuckGoSearch(ctx, q)
	if err != nil {
		return Result{}, fmt.Errorf("web_search: %w", err)
	}
	if len(results) == 0 {
		return Result{ToolName: "web_search", Stdout: "(no results)\n[via duckduckgo]"}, nil
	}
	return Result{ToolName: "web_search", Stdout: formatResults(results) + "\n[via duckduckgo]"}, nil
}

// searchHit is the common shape both backends produce.
type searchHit struct {
	Title   string
	URL     string
	Snippet string
}

func formatResults(hits []searchHit) string {
	var b strings.Builder
	for i, h := range hits {
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, h.Title, h.URL)
		if s := strings.TrimSpace(h.Snippet); s != "" {
			fmt.Fprintf(&b, "   %s\n", truncForLog(s, 240))
		}
		if i >= 7 {
			break
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// googleCSE hits the Custom Search JSON API. Returns up to 10 items.
func googleCSE(ctx context.Context, q, key, cx string) ([]searchHit, error) {
	v := url.Values{}
	v.Set("key", key)
	v.Set("cx", cx)
	v.Set("q", q)
	v.Set("num", "8")
	endpoint := "https://customsearch.googleapis.com/customsearch/v1?" + v.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := fetchClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("google cse http %d: %s", resp.StatusCode, truncForLog(string(raw), 200))
	}
	var body struct {
		Items []struct {
			Title   string `json:"title"`
			Link    string `json:"link"`
			Snippet string `json:"snippet"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("google cse decode: %w", err)
	}
	out := make([]searchHit, 0, len(body.Items))
	for _, it := range body.Items {
		out = append(out, searchHit{Title: it.Title, URL: it.Link, Snippet: it.Snippet})
	}
	return out, nil
}

// duckDuckGoSearch scrapes html.duckduckgo.com (the no-JS version),
// which returns a clean static HTML page. We extract the result
// title + URL + snippet via permissive substring scans rather than a
// full HTML parser — keeps us stdlib-only and resilient.
//
// The page layout has been stable for years but if DuckDuckGo
// changes it, the parser falls back to "no results" cleanly rather
// than blowing up.
func duckDuckGoSearch(ctx context.Context, q string) ([]searchHit, error) {
	v := url.Values{}
	v.Set("q", q)
	endpoint := "https://html.duckduckgo.com/html/?" + v.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(v.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; goon/1.0; +https://github.com/harisaginting/goon)")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := fetchClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("duckduckgo http %d", resp.StatusCode)
	}
	return parseDDGHTML(string(raw)), nil
}

// parseDDGHTML extracts up to 8 results from a DuckDuckGo HTML page.
// We look for the well-known result class names. If the structure
// changes, this returns an empty slice — fail soft.
func parseDDGHTML(body string) []searchHit {
	var hits []searchHit
	// Each result block starts with <a class="result__a" href="..."> TITLE </a>
	// followed somewhere by <a class="result__snippet" ...> SNIPPET </a>.
	for i := 0; i < len(body) && len(hits) < 8; {
		anchor := `class="result__a"`
		ix := strings.Index(body[i:], anchor)
		if ix < 0 {
			break
		}
		ix += i
		// Walk back to the opening <a tag.
		open := strings.LastIndex(body[:ix], "<a ")
		if open < 0 {
			i = ix + len(anchor)
			continue
		}
		// Find href.
		hrefStart := strings.Index(body[open:ix+len(anchor)], `href="`)
		if hrefStart < 0 {
			i = ix + len(anchor)
			continue
		}
		hrefStart += open + len(`href="`)
		hrefEnd := strings.Index(body[hrefStart:], `"`)
		if hrefEnd < 0 {
			i = ix + len(anchor)
			continue
		}
		hrefEnd += hrefStart
		href := body[hrefStart:hrefEnd]
		// DDG often wraps real URLs in a /l/?uddg=... redirect — unwrap.
		if strings.Contains(href, "uddg=") {
			if pu, err := url.Parse(href); err == nil {
				if real := pu.Query().Get("uddg"); real != "" {
					href = real
				}
			}
		}
		// Title: text between > and </a> after the anchor.
		titleStart := strings.Index(body[hrefEnd:], ">")
		if titleStart < 0 {
			i = hrefEnd
			continue
		}
		titleStart += hrefEnd + 1
		titleEnd := strings.Index(body[titleStart:], "</a>")
		if titleEnd < 0 {
			i = titleStart
			continue
		}
		titleEnd += titleStart
		title := stripHTMLDocs(body[titleStart:titleEnd])
		// Snippet: nearest result__snippet block after this title.
		snippet := ""
		snipMarker := strings.Index(body[titleEnd:], `class="result__snippet"`)
		if snipMarker >= 0 && snipMarker < 4000 { // within reasonable distance
			snipMarker += titleEnd
			snipStart := strings.Index(body[snipMarker:], ">")
			if snipStart >= 0 {
				snipStart += snipMarker + 1
				snipEnd := strings.Index(body[snipStart:], "</a>")
				if snipEnd >= 0 {
					snippet = stripHTMLDocs(body[snipStart : snipStart+snipEnd])
				}
			}
		}
		if href != "" && title != "" {
			hits = append(hits, searchHit{
				Title:   strings.TrimSpace(title),
				URL:     href,
				Snippet: strings.TrimSpace(snippet),
			})
		}
		i = titleEnd
	}
	return hits
}

// stripHTMLDocs removes tags and decodes common entities. Not a real
// parser — good enough for converting docs / search snippets to
// readable text without pulling in golang.org/x/net/html.
func stripHTMLDocs(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	in := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '<' {
			in = true
			// Collapse common block tags into newlines for readability.
			tag := strings.ToLower(peekTag(s[i:]))
			switch tag {
			case "br", "p", "div", "li", "tr", "h1", "h2", "h3", "h4", "h5", "h6":
				b.WriteByte('\n')
			}
			continue
		}
		if c == '>' {
			in = false
			continue
		}
		if in {
			continue
		}
		b.WriteByte(c)
	}
	out := b.String()
	// Decode the small set of entities that actually show up in docs.
	out = strings.NewReplacer(
		"&amp;", "&",
		"&lt;", "<",
		"&gt;", ">",
		"&quot;", `"`,
		"&#39;", "'",
		"&nbsp;", " ",
	).Replace(out)
	// Collapse runs of blank lines.
	for strings.Contains(out, "\n\n\n") {
		out = strings.ReplaceAll(out, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(out)
}

func peekTag(s string) string {
	if !strings.HasPrefix(s, "<") {
		return ""
	}
	s = s[1:]
	end := 0
	for end < len(s) && s[end] != ' ' && s[end] != '>' && s[end] != '/' {
		end++
	}
	return s[:end]
}

// fetchClient is the shared HTTP client for both tools. Logged via
// logx so every outbound request is visible in ./storage/logs/goon.log.
func fetchClient() *http.Client {
	return logx.InstrumentClient("fetch", &http.Client{Timeout: 20 * time.Second})
}

// truncForLog is local to this file so we don't depend on internal/util
// from tool packages (kept self-contained).
func truncForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
