package tools

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/harisaginting/goon/internal/codeindex"
)

// SearchCode is the agent's "where is X" / "find every place that does Y"
// tool. Backed by internal/codeindex: extracts symbols on first call,
// then answers symbol lookups + content searches against the cached
// index.
//
// Single registered instance shared across the registry so the index
// is built once per goon process — building per-call would scan the
// repo on every search and defeat the purpose.
//
// Modes (auto-detected by query shape):
//
//   query starts with "/" and ends with "/"   regex content search
//   query looks like a symbol (one word)      symbol lookup + content fallback
//   anything else                             plain substring content search
//
// The agent gets a single, simple knob; the tool picks the right backend.
type SearchCode struct {
	mu  sync.Mutex
	idx *codeindex.Index
}

func (s *SearchCode) Name() string { return "search_code" }
func (s *SearchCode) Description() string {
	return "search the working repo: symbol lookup (one word), substring content search, or /regex/ between slashes. Cached index built on first call."
}
func (s *SearchCode) Schema() map[string]string {
	return map[string]string{
		"query": "symbol name, substring, or /regex/",
		"limit": "max results (default 20)",
		"root":  "repo root to index (default: cwd or $GOON_WORKDIR)",
	}
}

func (s *SearchCode) Run(ctx context.Context, args map[string]string) (Result, error) {
	query := strings.TrimSpace(args["query"])
	if query == "" {
		return Result{}, fmt.Errorf("search_code: query is required")
	}
	limit := 20
	if v := strings.TrimSpace(args["limit"]); v != "" {
		fmt.Sscanf(v, "%d", &limit)
	}
	if limit <= 0 {
		limit = 20
	}
	root := strings.TrimSpace(args["root"])
	if root == "" {
		root = WorkDirFrom(ctx) // the selected repo, when the workflow set one
	}
	if root == "" {
		root = strings.TrimSpace(os.Getenv("GOON_WORKDIR"))
	}
	if root == "" {
		var err error
		root, err = os.Getwd()
		if err != nil {
			return Result{}, fmt.Errorf("search_code: getcwd: %w", err)
		}
	}

	ix, err := s.indexFor(ctx, root)
	if err != nil {
		return Result{}, err
	}

	// Symbol lookup for single-word queries, then content fallback.
	looksLikeSymbol := isSymbolish(query)
	var sb strings.Builder
	wrote := false

	if looksLikeSymbol {
		syms := ix.FindSymbol(query, limit)
		if len(syms) > 0 {
			fmt.Fprintf(&sb, "Symbols (%d):\n", len(syms))
			for _, sym := range syms {
				fmt.Fprintf(&sb, "  %s  %s:%d  [%s]\n", sym.Name, sym.File, sym.Line, sym.Kind)
			}
			wrote = true
		}
	}

	matches, err := ix.SearchContent(ctx, query, limit)
	if err != nil {
		return Result{}, err
	}
	if len(matches) > 0 {
		if wrote {
			sb.WriteString("\n")
		}
		fmt.Fprintf(&sb, "Matches (%d):\n", len(matches))
		for _, m := range matches {
			text := m.Text
			if len(text) > 200 {
				text = text[:197] + "…"
			}
			fmt.Fprintf(&sb, "  %s:%d  %s\n", m.File, m.Line, text)
		}
		wrote = true
	}

	if !wrote {
		return Result{ToolName: "search_code", Stdout: fmt.Sprintf("(no matches for %q in %d files)", query, ix.FileCount())}, nil
	}
	fmt.Fprintf(&sb, "\n[index: %d files, %d symbols]\n", ix.FileCount(), ix.SymbolCount())
	return Result{ToolName: "search_code", Stdout: sb.String()}, nil
}

// indexFor returns the cached Index for the given root, building it
// on first request. Re-builds when the root changes (rare — almost
// always the same per process).
func (s *SearchCode) indexFor(ctx context.Context, root string) (*codeindex.Index, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idx != nil && s.idx.Root == root && s.idx.Built() {
		return s.idx, nil
	}
	ix := codeindex.New(root)
	if err := ix.Build(ctx); err != nil {
		return nil, fmt.Errorf("search_code: build index: %w", err)
	}
	s.idx = ix
	return ix, nil
}

// isSymbolish returns true for queries that look like a single
// identifier — letters, digits, underscores, no spaces. We treat
// those as symbol candidates AND fall back to content search.
func isSymbolish(q string) bool {
	if q == "" || strings.ContainsAny(q, " \t/") {
		return false
	}
	for _, r := range q {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_' || r == '$':
			continue
		default:
			return false
		}
	}
	return true
}
