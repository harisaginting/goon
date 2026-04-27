// Package web exposes goon's status, tickets, questions, and config over HTTP
// with a single embedded htmx page. It's designed to be a perfect mirror of
// the CLI: every action available from the command line is also available
// here, backed by the same memory store.
package web

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"goon/internal/boards"
	"goon/internal/daemon"
	"goon/internal/memory"
)

// Options bundles dependencies for the Server.
type Options struct {
	Addr   string
	Memory *memory.Memory
	Board  boards.Board
	Daemon *daemon.Daemon // may be nil
	Stdout io.Writer
	Stderr io.Writer
}

// Server is goon's web frontend.
type Server struct {
	opts Options
	srv  *http.Server
	mu   sync.Mutex
}

// NewServer wires the Server.
func NewServer(opts Options) *Server {
	return &Server{opts: opts}
}

// mux builds the routing table. Split out so tests can use it directly via
// httptest.NewRecorder without binding a real port.
func (s *Server) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/status", s.handleAPIStatus)
	mux.HandleFunc("/api/tickets", s.handleAPITickets)
	mux.HandleFunc("/api/workflows", s.handleAPIWorkflows)
	mux.HandleFunc("/api/questions", s.handleAPIQuestions)
	mux.HandleFunc("/api/answer", s.handleAnswer)
	mux.HandleFunc("/api/config", s.handleAPIConfig)
	mux.HandleFunc("/htmx.min.js", s.handleHTMX)
	mux.HandleFunc("/fragments/status", s.fragStatus)
	mux.HandleFunc("/fragments/tickets", s.fragTickets)
	mux.HandleFunc("/fragments/questions", s.fragQuestions)
	mux.HandleFunc("/fragments/workflows", s.fragWorkflows)
	return mux
}

// Start begins serving. Blocks until ListenAndServe returns.
func (s *Server) Start() error {
	s.mu.Lock()
	s.srv = &http.Server{
		Addr:              s.opts.Addr,
		Handler:           s.mux(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	srv := s.srv
	s.mu.Unlock()

	st := s.opts.Memory.GetStatus()
	st.WebAddr = s.opts.Addr
	s.opts.Memory.SetStatus(st)

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Stop gracefully shuts down the server.
func (s *Server) Stop() {
	s.mu.Lock()
	srv := s.srv
	s.mu.Unlock()
	if srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

// --- Index page ------------------------------------------------------------

//go:embed static/index.html
var indexHTML string

//go:embed static/htmx.min.js
var htmxJS []byte

func (s *Server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, indexHTML)
}

func (s *Server) handleHTMX(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(htmxJS)
}

// --- JSON API --------------------------------------------------------------

func (s *Server) handleAPIStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.opts.Memory.GetStatus())
}

func (s *Server) handleAPITickets(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.opts.Memory.ListTickets())
}

func (s *Server) handleAPIWorkflows(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.opts.Memory.ListWorkflows(50))
}

func (s *Server) handleAPIQuestions(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("all") == "1" {
		writeJSON(w, s.opts.Memory.AllQuestions())
		return
	}
	writeJSON(w, s.opts.Memory.PendingQuestions())
}

func (s *Server) handleAnswer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id := r.FormValue("id")
	answer := r.FormValue("answer")
	if id == "" || answer == "" {
		http.Error(w, "id and answer required", http.StatusBadRequest)
		return
	}
	if !s.opts.Memory.AnswerQuestion(id, answer) {
		http.Error(w, "question not found or already answered", http.StatusNotFound)
		return
	}
	// htmx reload trigger.
	w.Header().Set("HX-Trigger", "questionsChanged")
	_, _ = io.WriteString(w, "<div class='ok'>recorded ✓</div>")
}

func (s *Server) handleAPIConfig(w http.ResponseWriter, _ *http.Request) {
	keys := []string{
		"GOON_LLM_PROVIDER", "GOON_BOARD", "GOON_GIT_HOST",
		"OPENAI_MODEL", "ANTHROPIC_MODEL", "OLLAMA_MODEL",
		"GOON_MAX_STEPS", "GOON_POLL_SECONDS", "GOON_VERIFY_RUNS",
		"GOON_REPO_MAP",
	}
	out := map[string]string{}
	for _, k := range keys {
		out[k] = envEcho(k)
	}
	writeJSON(w, out)
}

// --- HTML fragments (htmx) -------------------------------------------------

func (s *Server) fragStatus(w http.ResponseWriter, _ *http.Request) {
	st := s.opts.Memory.GetStatus()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	cls := "stopped"
	state := "STOPPED"
	if st.Running {
		cls, state = "running", "RUNNING"
	}
	last := "—"
	if !st.LastPoll.IsZero() {
		last = time.Since(st.LastPoll).Round(time.Second).String() + " ago"
	}
	fmt.Fprintf(w, `<div class="kv"><span class="k">status</span><span class="v %s">%s</span></div>`, cls, state)
	fmt.Fprintf(w, `<div class="kv"><span class="k">board</span><span class="v">%s</span></div>`, html.EscapeString(st.BoardName))
	fmt.Fprintf(w, `<div class="kv"><span class="k">git host</span><span class="v">%s</span></div>`, html.EscapeString(st.HostName))
	fmt.Fprintf(w, `<div class="kv"><span class="k">last poll</span><span class="v">%s</span></div>`, last)
	if st.LastTicket != "" {
		fmt.Fprintf(w, `<div class="kv"><span class="k">last ticket</span><span class="v">%s</span></div>`, html.EscapeString(st.LastTicket))
	}
	if st.PID != 0 {
		fmt.Fprintf(w, `<div class="kv"><span class="k">pid</span><span class="v">%d</span></div>`, st.PID)
	}
}

func (s *Server) fragTickets(w http.ResponseWriter, _ *http.Request) {
	tks := s.opts.Memory.ListTickets()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if len(tks) == 0 {
		_, _ = io.WriteString(w, `<div class="empty">No tickets seen yet.</div>`)
		return
	}
	_, _ = io.WriteString(w, `<table class="tickets"><tr><th>key</th><th>title</th><th>status</th><th>updated</th></tr>`)
	for _, t := range tks {
		fmt.Fprintf(w, `<tr><td><a href="%s">%s</a></td><td>%s</td><td>%s</td><td>%s</td></tr>`,
			html.EscapeString(t.URL), html.EscapeString(t.Key),
			html.EscapeString(t.Title), html.EscapeString(t.Status),
			fuzzyTime(t.UpdatedAt))
	}
	_, _ = io.WriteString(w, `</table>`)
}

func (s *Server) fragWorkflows(w http.ResponseWriter, _ *http.Request) {
	wfs := s.opts.Memory.ListWorkflows(20)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if len(wfs) == 0 {
		_, _ = io.WriteString(w, `<div class="empty">No workflows yet.</div>`)
		return
	}
	_, _ = io.WriteString(w, `<table class="workflows"><tr><th>id</th><th>ticket</th><th>state</th><th>plan</th><th>PR</th><th>updated</th></tr>`)
	for _, wf := range wfs {
		done, total := 0, len(wf.Plan)
		for _, s := range wf.Plan {
			if s.Done {
				done++
			}
		}
		pr := "—"
		if wf.PRURL != "" {
			pr = fmt.Sprintf(`<a href="%s">PR</a>`, html.EscapeString(wf.PRURL))
		}
		fmt.Fprintf(w,
			`<tr><td>%s</td><td>%s</td><td><span class="state state-%s">%s</span></td><td>%d/%d</td><td>%s</td><td>%s</td></tr>`,
			html.EscapeString(wf.ID),
			html.EscapeString(wf.TicketKey),
			string(wf.State), string(wf.State),
			done, total,
			pr,
			fuzzyTime(wf.UpdatedAt))
	}
	_, _ = io.WriteString(w, `</table>`)
}

func (s *Server) fragQuestions(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	pending := s.opts.Memory.PendingQuestions()
	if len(pending) == 0 {
		_, _ = io.WriteString(w, `<div class="ok">No pending questions ✓</div>`)
		return
	}
	for _, q := range pending {
		fmt.Fprintf(w, `<form class="qrow" hx-post="/api/answer" hx-target="this" hx-swap="outerHTML">
  <div class="q">[%s] %s</div>
  <div class="ticket">%s</div>
  <input type="hidden" name="id" value="%s">
  <input type="text" name="answer" placeholder="your answer" autofocus>
  <button type="submit">answer</button>
</form>`,
			html.EscapeString(q.ID),
			html.EscapeString(q.Question),
			html.EscapeString(q.TicketID),
			html.EscapeString(q.ID),
		)
	}
}

// --- helpers ---------------------------------------------------------------

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func envEcho(key string) string {
	if v := strings.TrimSpace(get(key)); v != "" {
		return v
	}
	return ""
}

// get is a small indirection to allow tests to monkey-patch env reads.
var get = func(k string) string { return getenv(k) }

func fuzzyTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return time.Since(t).Round(time.Second).String() + " ago"
}
