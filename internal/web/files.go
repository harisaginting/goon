// Package web — files.go is the in-browser file tree + editor. Lets
// the user browse and edit the codebase goon is working on without
// switching to a terminal or IDE.
//
// Three endpoints:
//
//	GET  /api/files/tree?path=<rel>     list a directory (JSON or HTML)
//	GET  /api/files/read?path=<rel>     read one file
//	POST /api/files/write               write one file (form: path, body)
//
// Safety:
//
//   - All paths are resolved relative to a "root" and the resolved
//     absolute path MUST stay inside that root. No "../" escapes,
//     no absolute paths in user input.
//   - Root resolution priority: GOON_WORKSPACE_DIR → GOON_WORKDIR → cwd.
//   - File size cap: 2 MB read, 4 MB write. Binary files (containing
//     a NUL byte in the first 8 KB) are refused for editing — the
//     editor is meant for source code, not images.
//   - No execution, no rename, no delete from this surface. Those
//     are intentionally NOT here so the agent stays the only thing
//     that can mutate the repo in non-obvious ways.
package web

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// filesRoot returns the absolute root directory the file API operates
// against. Trims trailing slash. Empty when no root can be resolved.
func filesRoot() string {
	for _, candidate := range []string{
		os.Getenv("GOON_WORKSPACE_DIR"),
		os.Getenv("GOON_WORKDIR"),
	} {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if abs, err := filepath.Abs(candidate); err == nil {
			return strings.TrimRight(abs, string(os.PathSeparator))
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		return strings.TrimRight(cwd, string(os.PathSeparator))
	}
	return ""
}

// resolveFilesPath validates a user-supplied relative path and
// returns the absolute path under the root. Empty rel means root.
// Rejects absolute paths and any ".." segment in the raw input.
func resolveFilesPath(root, rel string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("no workspace root configured (set GOON_WORKSPACE_DIR)")
	}
	rel = strings.TrimSpace(rel)
	if rel == "" || rel == "." || rel == "/" {
		return root, nil
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute paths are not allowed (%q)", rel)
	}
	for _, sep := range []string{"/", "\\"} {
		for _, seg := range strings.Split(rel, sep) {
			if seg == ".." {
				return "", fmt.Errorf("path escapes the workspace (%q)", rel)
			}
		}
	}
	clean := filepath.Clean(filepath.Join(root, rel))
	r, err := filepath.Rel(root, clean)
	if err != nil || strings.HasPrefix(r, "..") {
		return "", fmt.Errorf("path escapes the workspace (%q)", rel)
	}
	return clean, nil
}

// fileEntry is one row in a directory listing.
type fileEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size,omitempty"`
}

// handleFilesTree returns either JSON (for ?format=json) or an HTML
// fragment (the default — htmx-driven). The HTML shape is a flat list
// of clickable rows scoped to a single directory; clicking a folder
// reloads the panel with that path; clicking a file loads the editor.
func (s *Server) handleFilesTree(w http.ResponseWriter, r *http.Request) {
	root := filesRoot()
	rel := strings.TrimSpace(r.URL.Query().Get("path"))
	abs, err := resolveFilesPath(root, rel)
	if err != nil {
		filesError(w, err.Error())
		return
	}
	info, err := os.Stat(abs)
	if err != nil {
		filesError(w, err.Error())
		return
	}
	if !info.IsDir() {
		filesError(w, "not a directory: "+rel)
		return
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		filesError(w, err.Error())
		return
	}
	rows := make([]fileEntry, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if isHiddenOrIgnored(name) {
			continue
		}
		var size int64
		if !e.IsDir() {
			if fi, err := e.Info(); err == nil {
				size = fi.Size()
			}
		}
		childRel := filepath.ToSlash(filepath.Join(rel, name))
		rows = append(rows, fileEntry{
			Name: name, Path: childRel, IsDir: e.IsDir(), Size: size,
		})
	}
	// Directories first, alphabetical within each group.
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].IsDir != rows[j].IsDir {
			return rows[i].IsDir
		}
		return strings.ToLower(rows[i].Name) < strings.ToLower(rows[j].Name)
	})

	if r.URL.Query().Get("format") == "json" {
		writeJSON(w, map[string]any{"root": root, "path": rel, "entries": rows})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	renderFilesPanel(w, rel, rows)
}

// renderFilesPanel emits the HTML used by both the initial tab body
// and every subsequent navigation (htmx swaps the same target).
func renderFilesPanel(w io.Writer, rel string, rows []fileEntry) {
	// Breadcrumb — last segment is the current dir, others are
	// clickable to go up.
	parts := []string{}
	if rel != "" {
		parts = strings.Split(rel, "/")
	}
	fmt.Fprint(w, `<div class="flex items-center gap-1 text-xs font-mono text-gray-500 mb-3 flex-wrap">`)
	fmt.Fprintf(w, `<button type="button" class="hover:text-accent transition"
		hx-get="/api/files/tree" hx-target="#files-panel" hx-swap="innerHTML">workspace</button>`)
	cum := ""
	for i, p := range parts {
		if cum == "" {
			cum = p
		} else {
			cum += "/" + p
		}
		fmt.Fprint(w, ` <span class="text-gray-400">/</span> `)
		if i == len(parts)-1 {
			fmt.Fprintf(w, `<span class="text-gray-700 dark:text-gray-300">%s</span>`, html.EscapeString(p))
		} else {
			fmt.Fprintf(w, `<button type="button" class="hover:text-accent transition"
				hx-get="/api/files/tree?path=%s" hx-target="#files-panel" hx-swap="innerHTML">%s</button>`,
				html.EscapeString(urlQueryEscape(cum)), html.EscapeString(p))
		}
	}
	fmt.Fprint(w, `</div>`)

	if len(rows) == 0 {
		fmt.Fprint(w, `<div class="text-xs text-gray-500 italic">(empty)</div>`)
		return
	}

	fmt.Fprint(w, `<ul class="rounded-lg border border-gray-200 dark:border-surface-border bg-white dark:bg-surface-raised divide-y divide-gray-100 dark:divide-surface-border/60 text-sm">`)
	// Parent link if not at root.
	if rel != "" {
		parent := ""
		if i := strings.LastIndex(rel, "/"); i > 0 {
			parent = rel[:i]
		}
		fmt.Fprintf(w, `<li><button type="button" class="w-full text-left px-3 py-2 hover:bg-gray-50 dark:hover:bg-surface-sunken/40 transition text-gray-600 dark:text-gray-400"
			hx-get="/api/files/tree?path=%s" hx-target="#files-panel" hx-swap="innerHTML">⬆ .. (up)</button></li>`,
			html.EscapeString(urlQueryEscape(parent)))
	}
	for _, e := range rows {
		icon := "📄"
		if e.IsDir {
			icon = "📁"
		}
		sizeNote := ""
		if !e.IsDir {
			sizeNote = humanizeBytes(e.Size)
		}
		if e.IsDir {
			fmt.Fprintf(w, `<li><button type="button" class="w-full text-left px-3 py-2 hover:bg-gray-50 dark:hover:bg-surface-sunken/40 transition flex items-center gap-2"
				hx-get="/api/files/tree?path=%s" hx-target="#files-panel" hx-swap="innerHTML">
				<span>%s</span><span class="font-mono">%s</span>
			</button></li>`, html.EscapeString(urlQueryEscape(e.Path)), icon, html.EscapeString(e.Name))
		} else {
			fmt.Fprintf(w, `<li><button type="button" class="w-full text-left px-3 py-2 hover:bg-gray-50 dark:hover:bg-surface-sunken/40 transition flex items-center gap-2"
				hx-get="/api/files/read?path=%s" hx-target="#files-editor" hx-swap="innerHTML">
				<span>%s</span><span class="font-mono flex-1 truncate">%s</span>
				<span class="text-[11px] text-gray-400 font-mono">%s</span>
			</button></li>`,
				html.EscapeString(urlQueryEscape(e.Path)), icon,
				html.EscapeString(e.Name), html.EscapeString(sizeNote))
		}
	}
	fmt.Fprint(w, `</ul>`)
}

// handleFilesRead returns an HTML fragment with the editor for one
// file. Binary files render a "binary, not editable" notice. Files
// over 2 MB return a similar refusal.
func (s *Server) handleFilesRead(w http.ResponseWriter, r *http.Request) {
	root := filesRoot()
	rel := strings.TrimSpace(r.URL.Query().Get("path"))
	if rel == "" {
		filesError(w, "path required")
		return
	}
	abs, err := resolveFilesPath(root, rel)
	if err != nil {
		filesError(w, err.Error())
		return
	}
	info, err := os.Stat(abs)
	if err != nil {
		filesError(w, err.Error())
		return
	}
	if info.IsDir() {
		filesError(w, "path is a directory: "+rel)
		return
	}
	if info.Size() > 2*1024*1024 {
		filesError(w, fmt.Sprintf("file too large to edit (%s); use a terminal", humanizeBytes(info.Size())))
		return
	}
	body, err := os.ReadFile(abs)
	if err != nil {
		filesError(w, err.Error())
		return
	}
	if isBinary(body) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<div class="p-4 text-xs text-gray-500 italic">%s is a binary file — not editable here.</div>`,
			html.EscapeString(rel))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<div class="rounded-lg border border-gray-200 dark:border-surface-border bg-white dark:bg-surface-raised">
		<div class="flex items-center justify-between gap-2 px-3 py-2 border-b border-gray-200 dark:border-surface-border text-xs">
			<div class="font-mono text-gray-700 dark:text-gray-300 truncate">%s</div>
			<div class="text-gray-400 font-mono">%s · %d lines</div>
		</div>
		<form hx-post="/api/files/write" hx-target="#files-write-result" hx-swap="innerHTML" class="space-y-0">
			<input type="hidden" name="path" value="%s">
			<textarea name="body" rows="28" spellcheck="false"
				class="block w-full font-mono text-xs leading-snug bg-white dark:bg-surface px-3 py-2 focus:outline-none resize-y border-0">%s</textarea>
			<div class="flex items-center justify-between gap-2 px-3 py-2 border-t border-gray-200 dark:border-surface-border bg-gray-50/60 dark:bg-surface-sunken/40">
				<div id="files-write-result" class="text-xs"></div>
				<button type="submit" class="text-xs rounded-md bg-accent text-surface px-3 py-1 font-semibold hover:brightness-110 transition">save</button>
			</div>
		</form>
	</div>`,
		html.EscapeString(rel),
		humanizeBytes(info.Size()),
		strings.Count(string(body), "\n")+1,
		html.EscapeString(rel),
		html.EscapeString(string(body)),
	)
}

// handleFilesWrite persists an edited file. Atomic via tmp + rename
// so a crash mid-write doesn't corrupt source. Refuses to create new
// files outside an existing directory and refuses paths that would
// escape the root.
func (s *Server) handleFilesWrite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	root := filesRoot()
	rel := strings.TrimSpace(r.FormValue("path"))
	body := r.FormValue("body")
	if rel == "" {
		filesError(w, "path required")
		return
	}
	if len(body) > 4*1024*1024 {
		filesError(w, "body too large (>4 MB)")
		return
	}
	abs, err := resolveFilesPath(root, rel)
	if err != nil {
		filesError(w, err.Error())
		return
	}
	parent := filepath.Dir(abs)
	if _, err := os.Stat(parent); err != nil {
		filesError(w, "parent dir does not exist: "+filepath.Dir(rel))
		return
	}
	tmp := abs + ".goon.tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil {
		filesError(w, "write tmp: "+err.Error())
		return
	}
	if err := os.Rename(tmp, abs); err != nil {
		_ = os.Remove(tmp)
		filesError(w, "rename: "+err.Error())
		return
	}
	w.Header().Set("HX-Trigger", "filesChanged")
	s.events.Publish("filesChanged")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<span class="text-emerald-700 dark:text-emerald-400">✓ saved %s</span>`,
		html.EscapeString(rel))
}

// fragTabFiles renders the two-column files page: tree panel on the
// left, editor on the right. Sibling page-section in main, lazy-
// loaded on first reveal like every other tab.
func (s *Server) fragTabFiles(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	root := filesRoot()
	header := pageHeader("Files",
		"Browse and edit the repo goon is working in. Root: <code class=\"font-mono text-xs\">"+html.EscapeString(root)+"</code>. Click a file to open it, edit, then save.",
		"")
	fmt.Fprint(w, header)
	if root == "" {
		fmt.Fprint(w, `<div class="rounded-md bg-amber-500/10 border border-amber-500/30 px-3 py-2 text-sm text-amber-700 dark:text-amber-400">
			Set <code class="font-mono">GOON_WORKSPACE_DIR</code> (or <code class="font-mono">GOON_WORKDIR</code>) to enable the file browser.
		</div>`)
		return
	}
	fmt.Fprint(w, `<div class="grid grid-cols-1 lg:grid-cols-[minmax(260px,360px)_1fr] gap-4">
		<div id="files-panel" hx-get="/api/files/tree" hx-trigger="load" hx-swap="innerHTML">
			<div class="text-xs text-gray-500">Loading tree…</div>
		</div>
		<div id="files-editor" class="min-w-0">
			<div class="rounded-lg border border-dashed border-gray-300 dark:border-surface-border bg-gray-50/60 dark:bg-surface-sunken/40 p-6 text-center text-sm text-gray-500">
				Pick a file from the tree to edit.
			</div>
		</div>
	</div>`)
}

// --- helpers ---------------------------------------------------------------

func filesError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	fmt.Fprintf(w, `<div class="rounded-md bg-rose-500/10 border border-rose-500/30 px-3 py-2 text-sm text-rose-700 dark:text-rose-400">✗ %s</div>`,
		html.EscapeString(msg))
}

// isHiddenOrIgnored matches the same set as the codeindex package.
// Kept duplicated rather than importing because this is a tiny list
// and the two callers want slightly different semantics later.
func isHiddenOrIgnored(name string) bool {
	switch name {
	case "node_modules", "vendor", "third_party",
		"target", "build", "dist", "out",
		".venv", "venv", "__pycache__",
		".idea", ".vscode", ".next", ".nuxt":
		return true
	}
	return strings.HasPrefix(name, ".") && name != "." && name != ".env" && name != ".env.example"
}

// isBinary heuristically detects binary files by scanning the first
// 8 KB for a NUL byte. Faster than mime-type sniffing, matches
// what most editors use.
func isBinary(b []byte) bool {
	limit := len(b)
	if limit > 8192 {
		limit = 8192
	}
	for i := 0; i < limit; i++ {
		if b[i] == 0 {
			return true
		}
	}
	return false
}

func humanizeBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}

// writeJSON is kept local to avoid pulling in a shared helper.
// Identical to the helper used elsewhere in the package.
func writeFilesJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// Keep the unused writer alive — referenced via writeJSON elsewhere
// in the package, but Go's import-checker complains about
// encoding/json otherwise.
var _ = writeFilesJSON
