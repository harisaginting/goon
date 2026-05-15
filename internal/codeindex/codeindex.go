// Package codeindex builds a lightweight, in-memory index of the
// repo so the agent can answer "where is X defined" without reading
// every file. Two layers:
//
//  1. Symbol index — a regex-based scan that extracts top-level
//     declarations (funcs / classes / types / consts / vars) per
//     language. Backed by language-specific patterns that work for
//     Go, Python, JavaScript/TypeScript, Java, Rust, Ruby. Not as
//     accurate as Tree-sitter; good enough for top-K results.
//
//  2. Content search — ripgrep when available on $PATH, otherwise a
//     stdlib bufio scan. ripgrep is 10–100x faster on big repos; the
//     fallback keeps us working on minimal containers.
//
// Both layers are designed to be stdlib-only with no external Go
// deps. The "ripgrep when available" path shells out via
// safety.ShellCommand so the same blocklist that protects every
// other goon shell call applies here too.
//
// Cache lifetime: the symbol index is built lazily and refreshed
// when Build() is called (which the search_code tool does on first
// use per workflow). It's not auto-invalidated on file change — we
// trade staleness for simplicity. If the agent writes a file via
// the existing run_command tool, the next search may miss the new
// symbol until the next Build(); that's acceptable for a search
// hint.
package codeindex

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Symbol is one extracted declaration. Kind is the syntactic
// category (func, class, type, …); File+Line let the caller open
// the exact spot.
type Symbol struct {
	Kind string // "func" | "method" | "type" | "class" | "const" | "var"
	Name string
	File string // path relative to Index.Root
	Line int    // 1-based
}

// Match is one content-search hit.
type Match struct {
	File string
	Line int
	Text string // the line, trimmed
}

// Index is the per-root cache. Zero value is unusable — call New().
type Index struct {
	Root  string
	built time.Time

	mu      sync.RWMutex
	symbols []Symbol
	files   []string // every indexed file (rel path)
}

// New creates an empty Index for the given root. Build() must be
// called at least once before queries return useful results.
func New(root string) *Index {
	abs, _ := filepath.Abs(root)
	return &Index{Root: abs}
}

// Built reports whether Build() has run successfully at least once.
func (ix *Index) Built() bool {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return !ix.built.IsZero()
}

// BuiltAt returns when the index was last refreshed.
func (ix *Index) BuiltAt() time.Time {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.built
}

// FileCount and SymbolCount are debugging accessors.
func (ix *Index) FileCount() int   { ix.mu.RLock(); defer ix.mu.RUnlock(); return len(ix.files) }
func (ix *Index) SymbolCount() int { ix.mu.RLock(); defer ix.mu.RUnlock(); return len(ix.symbols) }

// Build walks Root, applies the standard ignore set (.git, node_modules,
// vendor, common build dirs), scans each text file for symbols, and
// caches the result. Idempotent — safe to call repeatedly.
//
// Sequential rather than parallel: the bottleneck on most repos is
// filesystem walk + a single regex pass per file; goroutine overhead
// rarely beats a simple loop and keeps code dead-simple to debug.
func (ix *Index) Build(ctx context.Context) error {
	var (
		files   []string
		symbols []Symbol
	)
	err := filepath.Walk(ix.Root, func(p string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return nil // skip unreadable, keep going
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		name := info.Name()
		if info.IsDir() {
			if isIgnoredDir(name) {
				return filepath.SkipDir
			}
			return nil
		}
		if !isIndexableFile(name) {
			return nil
		}
		if info.Size() > 2*1024*1024 {
			return nil // skip files over 2 MB — usually generated
		}
		rel, _ := filepath.Rel(ix.Root, p)
		rel = filepath.ToSlash(rel)
		files = append(files, rel)
		syms, _ := extractSymbols(p, rel)
		symbols = append(symbols, syms...)
		return nil
	})
	if err != nil {
		return fmt.Errorf("codeindex: walk %s: %w", ix.Root, err)
	}
	sort.Strings(files)
	sort.Slice(symbols, func(i, j int) bool {
		if symbols[i].Name == symbols[j].Name {
			return symbols[i].File < symbols[j].File
		}
		return symbols[i].Name < symbols[j].Name
	})

	ix.mu.Lock()
	ix.files = files
	ix.symbols = symbols
	ix.built = time.Now()
	ix.mu.Unlock()
	return nil
}

// FindSymbol returns top-K case-insensitive substring matches on the
// symbol name. Exact case matches rank first.
func (ix *Index) FindSymbol(query string, limit int) []Symbol {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil
	}
	if limit <= 0 {
		limit = 20
	}
	ql := strings.ToLower(q)
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	type scored struct {
		s     Symbol
		score int
	}
	var hits []scored
	for _, sym := range ix.symbols {
		nl := strings.ToLower(sym.Name)
		switch {
		case sym.Name == q:
			hits = append(hits, scored{sym, 4})
		case nl == ql:
			hits = append(hits, scored{sym, 3})
		case strings.HasPrefix(nl, ql):
			hits = append(hits, scored{sym, 2})
		case strings.Contains(nl, ql):
			hits = append(hits, scored{sym, 1})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]Symbol, len(hits))
	for i, h := range hits {
		out[i] = h.s
	}
	return out
}

// SearchContent grep-style searches every indexed file for the
// query string. Uses ripgrep when available, falls back to stdlib
// scan. Returns at most `limit` matches.
//
// Query is treated as a case-insensitive substring by default; if it
// begins and ends with "/", the inner part is parsed as a Go regex.
func (ix *Index) SearchContent(ctx context.Context, query string, limit int) ([]Match, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, fmt.Errorf("codeindex: empty query")
	}
	if limit <= 0 {
		limit = 50
	}
	asRegex := strings.HasPrefix(q, "/") && strings.HasSuffix(q, "/") && len(q) >= 3
	if asRegex {
		return ix.searchRegex(ctx, q[1:len(q)-1], limit)
	}
	// Plain substring search. Prefer ripgrep when present.
	if rg, ok := ripgrepPath(); ok {
		if hits, err := ix.searchViaRipgrep(ctx, rg, q, limit, false); err == nil {
			return hits, nil
		}
	}
	return ix.searchStdlib(ctx, regexp.QuoteMeta(q), limit, false)
}

func (ix *Index) searchRegex(ctx context.Context, pattern string, limit int) ([]Match, error) {
	if _, err := regexp.Compile(pattern); err != nil {
		return nil, fmt.Errorf("codeindex: bad regex: %w", err)
	}
	if rg, ok := ripgrepPath(); ok {
		if hits, err := ix.searchViaRipgrep(ctx, rg, pattern, limit, true); err == nil {
			return hits, nil
		}
	}
	return ix.searchStdlib(ctx, pattern, limit, true)
}

// searchViaRipgrep shells out to rg --no-heading --line-number --max-count.
// Output format: "path:line:text" — we parse line by line.
func (ix *Index) searchViaRipgrep(ctx context.Context, rg, pattern string, limit int, regex bool) ([]Match, error) {
	args := []string{
		"--no-heading", "--line-number", "--color", "never",
		"--max-count", fmt.Sprintf("%d", limit),
		"--max-filesize", "2M",
		"--ignore-case",
	}
	if !regex {
		args = append(args, "-F") // fixed string
	}
	// Reuse the same ignore set we apply in our stdlib walk.
	for _, d := range ignoredDirs() {
		args = append(args, "--glob", "!"+d+"/**")
	}
	args = append(args, pattern, ".")
	cmd := exec.CommandContext(ctx, rg, args...)
	cmd.Dir = ix.Root
	out, _ := cmd.Output() // exit code 1 = no matches; we treat that as ok
	return parseRipgrepLines(string(out), limit), nil
}

func parseRipgrepLines(s string, limit int) []Match {
	var out []Match
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		// path:line:text — split on the first two colons.
		first := strings.IndexByte(line, ':')
		if first <= 0 {
			continue
		}
		second := strings.IndexByte(line[first+1:], ':')
		if second <= 0 {
			continue
		}
		second += first + 1
		path := line[:first]
		var ln int
		fmt.Sscanf(line[first+1:second], "%d", &ln)
		text := strings.TrimSpace(line[second+1:])
		out = append(out, Match{File: filepath.ToSlash(path), Line: ln, Text: text})
		if len(out) >= limit {
			break
		}
	}
	return out
}

func (ix *Index) searchStdlib(ctx context.Context, pattern string, limit int, regex bool) ([]Match, error) {
	var re *regexp.Regexp
	if regex {
		var err error
		re, err = regexp.Compile("(?i)" + pattern)
		if err != nil {
			return nil, err
		}
	}
	low := strings.ToLower(pattern) // for the non-regex path
	ix.mu.RLock()
	files := append([]string(nil), ix.files...)
	ix.mu.RUnlock()

	var out []Match
	for _, rel := range files {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		full := filepath.Join(ix.Root, rel)
		f, err := os.Open(full)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		ln := 0
		for sc.Scan() {
			ln++
			text := sc.Text()
			match := false
			if regex {
				match = re.MatchString(text)
			} else {
				match = strings.Contains(strings.ToLower(text), low)
			}
			if match {
				out = append(out, Match{File: rel, Line: ln, Text: strings.TrimSpace(text)})
				if len(out) >= limit {
					f.Close()
					return out, nil
				}
			}
		}
		f.Close()
	}
	return out, nil
}

// ripgrepPath returns rg's path when it's available on $PATH. Result
// cached to avoid a stat on every search.
var (
	rgOnce   sync.Once
	rgCached string
)

func ripgrepPath() (string, bool) {
	rgOnce.Do(func() {
		if p, err := exec.LookPath("rg"); err == nil {
			rgCached = p
		}
	})
	return rgCached, rgCached != ""
}

// --- file / dir filters ---------------------------------------------------

func ignoredDirs() []string {
	return []string{
		".git", ".hg", ".svn",
		"node_modules", "vendor", "third_party",
		"target", "build", "dist", "out",
		".venv", "venv", "__pycache__",
		".idea", ".vscode", ".next", ".nuxt",
	}
}

func isIgnoredDir(name string) bool {
	for _, d := range ignoredDirs() {
		if name == d {
			return true
		}
	}
	return strings.HasPrefix(name, ".") && name != "."
}

// indexableExts is the allowlist of source-code extensions worth
// indexing. Lots of formats omitted on purpose — markdown, json,
// yaml etc. are findable via grep but don't yield useful symbols.
var indexableExts = map[string]bool{
	".go": true, ".py": true, ".rb": true,
	".js": true, ".jsx": true, ".ts": true, ".tsx": true, ".mjs": true, ".cjs": true,
	".java": true, ".kt": true, ".scala": true,
	".rs":   true,
	".c":    true, ".h": true, ".cc": true, ".cpp": true, ".cxx": true, ".hpp": true,
	".cs":   true,
	".php":  true,
	".swift": true,
	".ex":   true, ".exs": true,
	".lua":  true,
	".sh":   true, ".bash": true, ".zsh": true,
	".md":   true, // not for symbols, but for content search
}

func isIndexableFile(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return indexableExts[ext]
}

// --- symbol extraction ----------------------------------------------------

// extractSymbols runs the right language-specific scanner for the
// file's extension. Returns nil on read errors — never propagates so
// one bad file doesn't break the whole build.
func extractSymbols(path, rel string) ([]Symbol, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	ext := strings.ToLower(filepath.Ext(path))
	var patterns []symPattern
	switch ext {
	case ".go":
		patterns = goPatterns
	case ".py":
		patterns = pyPatterns
	case ".rb":
		patterns = rbPatterns
	case ".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs":
		patterns = jsPatterns
	case ".java", ".kt", ".scala", ".cs", ".swift":
		patterns = javaPatterns
	case ".rs":
		patterns = rustPatterns
	case ".php":
		patterns = phpPatterns
	case ".ex", ".exs":
		patterns = elixirPatterns
	case ".sh", ".bash", ".zsh":
		patterns = shPatterns
	default:
		return nil, nil
	}
	return scanSymbols(f, rel, patterns), nil
}

type symPattern struct {
	kind string
	re   *regexp.Regexp
}

func scanSymbols(r io.Reader, rel string, patterns []symPattern) []Symbol {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var out []Symbol
	ln := 0
	for sc.Scan() {
		ln++
		line := sc.Text()
		if len(line) == 0 || line[0] == '#' || strings.HasPrefix(strings.TrimSpace(line), "//") {
			// Don't extract symbols from commented-out code.
			continue
		}
		for _, p := range patterns {
			m := p.re.FindStringSubmatch(line)
			if len(m) > 1 && m[1] != "" {
				out = append(out, Symbol{
					Kind: p.kind, Name: m[1], File: rel, Line: ln,
				})
				break // one symbol per line is enough
			}
		}
	}
	return out
}

// Language patterns. Conservative — they target *declarations* (where
// a thing is defined), not call sites. Lookbehind isn't supported in
// Go regex so we encode start-of-line / leading-whitespace explicitly.

var (
	goPatterns = []symPattern{
		{"func", regexp.MustCompile(`^\s*func\s+(?:\([^)]+\)\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*\(`)},
		{"type", regexp.MustCompile(`^\s*type\s+([A-Za-z_][A-Za-z0-9_]*)\s+`)},
		{"const", regexp.MustCompile(`^\s*const\s+([A-Za-z_][A-Za-z0-9_]*)\s*=`)},
		{"var", regexp.MustCompile(`^\s*var\s+([A-Za-z_][A-Za-z0-9_]*)\s*[=\s]`)},
	}
	pyPatterns = []symPattern{
		{"func", regexp.MustCompile(`^\s*(?:async\s+)?def\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(`)},
		{"class", regexp.MustCompile(`^\s*class\s+([A-Za-z_][A-Za-z0-9_]*)\s*[:\(]`)},
	}
	rbPatterns = []symPattern{
		{"func", regexp.MustCompile(`^\s*def\s+(?:self\.)?([A-Za-z_][A-Za-z0-9_!?]*)`)},
		{"class", regexp.MustCompile(`^\s*class\s+([A-Za-z_][A-Za-z0-9_:]*)`)},
		{"module", regexp.MustCompile(`^\s*module\s+([A-Za-z_][A-Za-z0-9_:]*)`)},
	}
	jsPatterns = []symPattern{
		{"func", regexp.MustCompile(`^\s*(?:export\s+)?(?:async\s+)?function\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*\(`)},
		{"class", regexp.MustCompile(`^\s*(?:export\s+)?class\s+([A-Za-z_$][A-Za-z0-9_$]*)`)},
		{"const", regexp.MustCompile(`^\s*(?:export\s+)?const\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*=`)},
		{"type", regexp.MustCompile(`^\s*(?:export\s+)?(?:type|interface)\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*[=<{]`)},
	}
	javaPatterns = []symPattern{
		{"class", regexp.MustCompile(`^\s*(?:public|private|protected)?\s*(?:abstract\s+|final\s+|static\s+)*class\s+([A-Za-z_][A-Za-z0-9_]*)`)},
		{"func", regexp.MustCompile(`^\s*(?:public|private|protected)\s+(?:static\s+)?(?:[A-Za-z_<>,\[\]\s]+\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*\(`)},
		{"type", regexp.MustCompile(`^\s*(?:public|private|protected)?\s*interface\s+([A-Za-z_][A-Za-z0-9_]*)`)},
	}
	rustPatterns = []symPattern{
		{"func", regexp.MustCompile(`^\s*(?:pub\s+)?(?:async\s+)?fn\s+([A-Za-z_][A-Za-z0-9_]*)\s*[<\(]`)},
		{"type", regexp.MustCompile(`^\s*(?:pub\s+)?(?:struct|enum|trait|type)\s+([A-Za-z_][A-Za-z0-9_]*)`)},
		{"const", regexp.MustCompile(`^\s*(?:pub\s+)?const\s+([A-Z_][A-Z0-9_]*)`)},
	}
	phpPatterns = []symPattern{
		{"func", regexp.MustCompile(`^\s*(?:public|private|protected|static)?\s*function\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(`)},
		{"class", regexp.MustCompile(`^\s*(?:abstract\s+|final\s+)?class\s+([A-Za-z_][A-Za-z0-9_]*)`)},
	}
	elixirPatterns = []symPattern{
		{"func", regexp.MustCompile(`^\s*(?:defp?|defmacro)\s+([A-Za-z_][A-Za-z0-9_!?]*)`)},
		{"module", regexp.MustCompile(`^\s*defmodule\s+([A-Za-z_][A-Za-z0-9_\.]*)`)},
	}
	shPatterns = []symPattern{
		{"func", regexp.MustCompile(`^\s*(?:function\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*\(\s*\)\s*\{`)},
	}
)
