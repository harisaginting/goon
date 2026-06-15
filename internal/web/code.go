// Package web — code.go is the in-browser agentic coding surface. It
// lets the user pick a working directory and run goon's agent loop
// against a free-form task, streaming the transcript live — the same
// engine as `goon "task"` on the command line, but in the dashboard
// and scoped to a directory they choose. Think "Claude Code in a tab".
//
// Two endpoints:
//
//	GET  /fragments/tab-code   the page (workdir picker + task + transcript)
//	POST /api/code/run         run one agent session, stream stdout (form: workdir, task)
//
// Safety — this surface EXECUTES shell commands and EDITS files, so the
// guards matter and must stay:
//
//   - Workdir confinement: the posted workdir MUST be one of the
//     whitelisted candidates from codeWorkdirs() (the workspace root +
//     local checkouts from REPOSITORY.md). Arbitrary paths are refused,
//     so the picker — not the client — decides where the agent can run.
//   - safety.Default() validator on the executor blocks dangerous
//     commands (rm -rf, etc.) exactly as the CLI agent does.
//   - tools.WithWorkDir confines run_command's cwd + search_code root to
//     the chosen directory.
//   - Step cap (agent.MaxSteps, env GOON_MAX_STEPS) + a wall-clock
//     timeout bound every run.
//   - The UI states plainly that it runs commands and edits files.
package web

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/harisaginting/goon/internal/agent"
	"github.com/harisaginting/goon/internal/executor"
	"github.com/harisaginting/goon/internal/repository"
	"github.com/harisaginting/goon/internal/safety"
	"github.com/harisaginting/goon/internal/tools"
	"github.com/harisaginting/goon/internal/usage"
)

// codeRunTimeout bounds a single code session so a wedged LLM or a
// runaway command can't hold a connection (and a goroutine) forever.
// Generous because real multi-step coding takes minutes.
const codeRunTimeout = 15 * time.Minute

// codeDefaultSteps is the per-run step cap the Code tab uses when the
// user doesn't pick one. Higher than the daemon's default (5) because
// building or iterating on a project takes many turns. Clamped to 50.
const codeDefaultSteps = 25

// codeDir is one selectable working directory in the picker.
type codeDir struct {
	Label string // human label, e.g. "backend-api (repo)"
	Path  string // absolute path on disk
}

// codeWorkdirs returns the convenience "quick pick" directories shown
// in the Code tab's dropdown: the current working directory (where goon
// was launched — usually the goon checkout itself), the configured
// workspace root, then every local checkout mapped in REPOSITORY.md
// that still exists on disk. Deduped by absolute path.
//
// This list is for convenience only — it is NOT the access gate. The
// user may also type any directory path (see resolveCodeWorkdir), so
// they can code in goon's own root, a subdirectory, or any project on
// the machine without first mapping it.
func codeWorkdirs() []codeDir {
	var out []codeDir
	seen := map[string]bool{}
	add := func(label, path string) {
		path = strings.TrimSpace(path)
		if path == "" {
			return
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			return
		}
		abs = strings.TrimRight(abs, string(os.PathSeparator))
		if abs == "" || seen[abs] {
			return
		}
		if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
			return
		}
		seen[abs] = true
		out = append(out, codeDir{Label: label, Path: abs})
	}

	// Current working dir first — this is goon's own root in the common
	// "launched from the repo" setup, and a sensible default to code in.
	if cwd, err := os.Getwd(); err == nil {
		add("current directory — "+filepath.Base(cwd), cwd)
	}
	if root := filesRoot(); root != "" {
		add("workspace root — "+filepath.Base(root), root)
	}
	if entries, err := repository.Read(); err == nil {
		for _, e := range entries {
			local := e.Resolve()
			if local == "" {
				continue // remote-only, no checkout to run in
			}
			add(e.Name()+" (repo)", local)
		}
	}
	return out
}

// resolveCodeWorkdir turns a user-supplied workdir into an absolute
// directory path. Accepts:
//
//   - empty → the first quick-pick (current dir / goon root)
//   - an absolute path to any existing directory (full freedom — this
//     is a local, single-operator tool with the same trust level as the
//     `goon "task"` CLI, which already runs in the cwd)
//   - a path starting with ~ → home-expanded, then treated as absolute
//   - a relative path → resolved under the workspace root, with no ".."
//     escape, so the convenience field can target a subdirectory
//
// The directory must exist. Command safety is still enforced downstream
// by safety.Default() on the executor, and run_command is confined to
// this directory via tools.WithWorkDir.
func resolveCodeWorkdir(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		dirs := codeWorkdirs()
		if len(dirs) == 0 {
			return "", fmt.Errorf("no working directory available — type a path, set GOON_WORKSPACE_DIR, or map a repo in Repositories")
		}
		return dirs[0].Path, nil
	}

	// ~ expansion.
	if raw == "~" || strings.HasPrefix(raw, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			raw = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(raw, "~"), "/"))
		}
	}

	var abs string
	if filepath.IsAbs(raw) {
		abs = filepath.Clean(raw)
	} else {
		// Relative → under the workspace root, no escape.
		root := filesRoot()
		if root == "" {
			return "", fmt.Errorf("relative path needs a workspace root — set GOON_WORKSPACE_DIR or type an absolute path")
		}
		abs = filepath.Clean(filepath.Join(root, raw))
		if rel, err := filepath.Rel(root, abs); err != nil || strings.HasPrefix(rel, "..") {
			return "", fmt.Errorf("path escapes the workspace (%q) — type an absolute path instead", raw)
		}
	}
	abs = strings.TrimRight(abs, string(os.PathSeparator))
	fi, err := os.Stat(abs)
	if err != nil || !fi.IsDir() {
		return "", fmt.Errorf("not an existing directory: %s", raw)
	}
	return abs, nil
}

// flushWriter wraps an http.ResponseWriter so each Write is pushed to
// the client immediately, giving the browser a live transcript instead
// of one buffered dump at the end.
type flushWriter struct {
	w io.Writer
	f http.Flusher
}

func (fw flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if fw.f != nil {
		fw.f.Flush()
	}
	return n, err
}

// handleCodeRun runs one agent session and streams its stdout to the
// browser as plain text. The agent stack mirrors the one-shot CLI
// (cmd/root.go): DefaultRegistry + safety-validated auto executor +
// agent.New, with the context carrying the chosen workdir and a usage
// label so the run shows up in the dashboard's live sessions card.
func (s *Server) handleCodeRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.opts.LLM == nil {
		http.Error(w, "no LLM provider configured — set one up in Setup first", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	task := strings.TrimSpace(r.FormValue("task"))
	if task == "" {
		http.Error(w, "task is required", http.StatusBadRequest)
		return
	}
	workdir, err := resolveCodeWorkdir(r.FormValue("workdir"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Per-run step cap. Default higher than the daemon's (real coding
	// needs more turns); clamp 1..50. Zero/blank → codeDefaultSteps.
	steps := codeDefaultSteps
	if v := strings.TrimSpace(r.FormValue("max_steps")); v != "" {
		if n, e := strconv.Atoi(v); e == nil && n > 0 {
			steps = n
		}
	}
	if steps > 50 {
		steps = 50
	}

	// Stream as plain text. Clear the server's 30s WriteTimeout for
	// this connection — coding sessions run for minutes and there's no
	// auto-reconnect on a fetch() stream the way EventSource has.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Accel-Buffering", "no") // nginx: don't buffer
	// no write deadline; best-effort (the std server supports this).
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	flusher, _ := w.(http.Flusher)
	fw := flushWriter{w: w, f: flusher}

	fmt.Fprintf(fw, "▶ workdir : %s\n", workdir)
	fmt.Fprintf(fw, "▶ task    : %s\n", task)
	fmt.Fprintf(fw, "▶ steps   : capped at %d\n", steps)
	fmt.Fprint(fw, strings.Repeat("─", 56)+"\n\n")

	// Build the agent stack — same wiring as the CLI one-shot agent.
	reg := tools.DefaultRegistry()
	exec := executor.New(executor.Options{
		Mode:      executor.ModeAuto, // validate, then run (no TTY prompt)
		Validator: safety.Default(),
		Stdin:     strings.NewReader(""), // never block on input
		Stdout:    fw,
		Stderr:    fw,
	})
	ag := agent.New(agent.Options{
		LLM:      s.opts.LLM,
		Tools:    reg,
		Executor: exec,
		Memory:   s.opts.Memory,
		Stdout:   fw,
		Stderr:   fw,
		MaxSteps: steps,
	})

	// Context: tie to the request (so the browser's Stop/abort cancels
	// the run), scope tools to the workdir, label it for the sessions
	// card, and bound it with a wall-clock timeout.
	ctx := r.Context()
	ctx = tools.WithWorkDir(ctx, workdir)
	ctx = usage.WithLabel(ctx, "code · "+filepath.Base(workdir))
	ctx, cancel := context.WithTimeout(ctx, codeRunTimeout)
	defer cancel()

	runErr := ag.Run(ctx, task)

	fmt.Fprint(fw, "\n"+strings.Repeat("─", 56)+"\n")
	switch {
	case runErr != nil && ctx.Err() == context.Canceled:
		fmt.Fprint(fw, "■ stopped\n")
	case runErr != nil && ctx.Err() == context.DeadlineExceeded:
		fmt.Fprintf(fw, "■ timed out after %s\n", codeRunTimeout)
	case runErr != nil:
		fmt.Fprintf(fw, "✗ %s\n", runErr.Error())
	default:
		if res := strings.TrimSpace(ag.Result()); res != "" {
			fmt.Fprintf(fw, "✓ done\n\n%s\n", res)
		} else {
			fmt.Fprint(fw, "✓ done\n")
		}
	}
}

// fragTabCode renders the Code page: a workdir picker, a task box, and
// a live transcript area. Lazy-loaded on first reveal like every other
// tab. Degrades gracefully when no LLM or no workdir is available.
func (s *Server) fragTabCode(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, pageHeader("Code",
		"Run an agentic coding session in a directory you choose — the same engine as <code class=\"font-mono text-xs\">goon \"task\"</code>. It can read and edit files and run commands, streaming each step live.",
		""))

	if s.opts.LLM == nil {
		fmt.Fprint(w, `<div class="rounded-md bg-amber-500/10 border border-amber-500/30 px-3 py-2 text-sm text-amber-700 dark:text-amber-400">
			No LLM provider configured. Set one up in <button type="button" class="underline" onclick="showPage('setup')">Setup</button> first.
		</div>`)
		return
	}

	dirs := codeWorkdirs()

	// Picker + task + controls.
	fmt.Fprint(w, `<div class="grid grid-cols-1 xl:grid-cols-[minmax(0,1fr)_minmax(0,1.4fr)] gap-4">`)

	// Left column: inputs.
	fmt.Fprint(w, `<div class="space-y-3">`)
	fmt.Fprint(w, `<div class="rounded-xl border border-surface-border bg-surface-raised p-4 space-y-3">`)

	// Workdir: optional quick-pick dropdown + a free-form path field so
	// the user can code in goon's own root, a subdirectory, or any
	// project on the machine without mapping it first.
	fmt.Fprint(w, `<div>
		<label for="code-workdir" class="block text-xs font-medium text-muted mb-1">Working directory</label>`)
	if len(dirs) > 0 {
		fmt.Fprint(w, `<select id="code-workdir" class="block w-full rounded-md border border-surface-border bg-surface text-ink text-sm px-2.5 py-1.5 focus:outline-none focus:ring-2 focus:ring-accent/40">`)
		for _, d := range dirs {
			fmt.Fprintf(w, `<option value="%s">%s</option>`,
				html.EscapeString(d.Path), html.EscapeString(d.Label))
		}
		fmt.Fprint(w, `</select>`)
	} else {
		fmt.Fprint(w, `<input type="hidden" id="code-workdir" value="">`)
	}
	fmt.Fprint(w, `<input id="code-workdir-manual" type="text" spellcheck="false" autocapitalize="off" autocomplete="off"
			placeholder="…or type any directory (absolute, ~/path, or relative to workspace)"
			class="block w-full mt-2 rounded-md border border-surface-border bg-surface text-ink text-xs font-mono px-2.5 py-1.5 focus:outline-none focus:ring-2 focus:ring-accent/40">
		<p id="code-workdir-path" class="mt-1 text-[11px] font-mono text-muted truncate"></p>
	</div>`)

	// Task textarea.
	fmt.Fprint(w, `<div>
		<label for="code-task" class="block text-xs font-medium text-muted mb-1">Task</label>
		<textarea id="code-task" rows="5" spellcheck="false" placeholder="e.g. Add a --json flag to the status command and update its help text."
			class="block w-full rounded-md border border-surface-border bg-surface text-ink text-sm px-2.5 py-2 font-mono leading-snug focus:outline-none focus:ring-2 focus:ring-accent/40 resize-y"></textarea>
	</div>`)

	// Controls + warning.
	fmt.Fprintf(w, `<div class="flex flex-wrap items-center gap-2">
		<button type="button" id="code-run" onclick="goonCodeRun()"
			class="rounded-md bg-accent text-surface text-sm px-4 py-1.5 font-semibold hover:brightness-110 transition disabled:opacity-50 disabled:cursor-not-allowed">Run</button>
		<button type="button" id="code-stop" onclick="goonCodeStop()" style="display:none"
			class="rounded-md bg-rose-600 text-white text-sm px-4 py-1.5 font-semibold hover:brightness-110 transition">Stop</button>
		<label for="code-steps" class="text-xs text-muted flex items-center gap-1 ml-1">steps
			<input id="code-steps" type="number" min="1" max="50" value="%d"
				class="w-16 rounded-md border border-surface-border bg-surface text-ink text-xs px-2 py-1 focus:outline-none focus:ring-2 focus:ring-accent/40">
		</label>
		<span id="code-status" class="text-xs text-muted"></span>
	</div>
	<p class="text-[11px] text-amber-700 dark:text-amber-400 flex items-start gap-1.5">
		<svg class="w-3.5 h-3.5 shrink-0 mt-px" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z"/><line x1="12" y1="9" x2="12" y2="13"/><line x1="12" y1="17" x2="12.01" y2="17"/></svg>
		<span>This runs shell commands and edits files in the chosen directory. Each run is capped at the step count above (max 50) and times out after %d min.</span>
	</p>`, codeDefaultSteps, int(codeRunTimeout.Minutes()))

	fmt.Fprint(w, `</div></div>`) // close card + left column

	// Right column: transcript.
	fmt.Fprint(w, `<div class="rounded-xl border border-surface-border bg-surface-raised overflow-hidden flex flex-col min-h-[420px]">
		<div class="flex items-center justify-between gap-2 px-3 py-2 border-b border-surface-border text-xs">
			<span class="font-medium text-muted">Transcript</span>
			<button type="button" onclick="document.getElementById('code-transcript').textContent=''" class="text-muted hover:text-ink transition">clear</button>
		</div>
		<pre id="code-transcript" class="flex-1 overflow-auto scrollbar-thin text-xs leading-relaxed font-mono text-ink px-3 py-2 whitespace-pre-wrap break-words m-0">Pick a directory, describe a task, then press Run.</pre>
	</div>`)

	fmt.Fprint(w, `</div>`) // close grid

	// Streaming controller. Defined idempotently on window so repeated
	// fragment loads don't stack listeners.
	fmt.Fprint(w, `<script>
(function(){
  var ctrl = null;
  function el(id){ return document.getElementById(id); }
  function chosenDir(){
    var manual = el('code-workdir-manual');
    if (manual && manual.value.trim()) return manual.value.trim();
    var sel = el('code-workdir');
    return sel ? sel.value : '';
  }
  function syncPath(){
    var p = el('code-workdir-path');
    if (p) p.textContent = chosenDir() || '';
  }
  window.goonCodeRun = function(){
    var task = el('code-task'), out = el('code-transcript');
    var runBtn = el('code-run'), stopBtn = el('code-stop'), status = el('code-status');
    if (!task.value.trim()) { task.focus(); return; }
    var dir = chosenDir();
    if (!dir) { var m = el('code-workdir-manual'); if (m) m.focus(); status.textContent = 'pick or type a directory'; return; }
    runBtn.disabled = true; stopBtn.style.display = '';
    status.textContent = 'running…';
    out.textContent = '';
    ctrl = new AbortController();
    var steps = el('code-steps') ? el('code-steps').value : '';
    var body = new URLSearchParams({ workdir: dir, task: task.value, max_steps: steps });
    fetch('/api/code/run', { method:'POST', body: body, signal: ctrl.signal })
      .then(function(resp){
        if (!resp.ok) { return resp.text().then(function(t){ throw new Error(t || ('HTTP '+resp.status)); }); }
        var reader = resp.body.getReader(), dec = new TextDecoder();
        function pump(){
          return reader.read().then(function(r){
            if (r.done) return;
            out.textContent += dec.decode(r.value, { stream:true });
            var nearBottom = (out.scrollTop + out.clientHeight + 40) >= out.scrollHeight;
            if (nearBottom) out.scrollTop = out.scrollHeight;
            return pump();
          });
        }
        return pump();
      })
      .then(function(){ status.textContent = 'finished'; })
      .catch(function(e){
        if (e && e.name === 'AbortError') { out.textContent += '\n■ stopped'; status.textContent = 'stopped'; }
        else { out.textContent += '\n✗ ' + (e && e.message ? e.message : 'error'); status.textContent = 'error'; }
      })
      .finally(function(){
        runBtn.disabled = false; stopBtn.style.display = 'none'; ctrl = null;
        out.scrollTop = out.scrollHeight;
      });
  };
  window.goonCodeStop = function(){ if (ctrl) ctrl.abort(); };
  var sel = el('code-workdir'), manual = el('code-workdir-manual');
  if (sel && sel.tagName === 'SELECT' && !sel.dataset.bound) { sel.dataset.bound = '1'; sel.addEventListener('change', syncPath); }
  if (manual && !manual.dataset.bound) { manual.dataset.bound = '1'; manual.addEventListener('input', syncPath); }
  syncPath();
})();
</script>`)
}
