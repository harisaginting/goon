package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/harisaginting/goon/internal/memory"
)

// runStatus prints the current daemon status and a quick summary of pending
// questions, recent workflows, and last-seen tickets.
func runStatus(_ context.Context, _ []string, stdout, stderr io.Writer) error {
	mem, err := memory.New(os.Getenv("GOON_MEMORY_PATH"))
	if err != nil {
		return fmt.Errorf("memory: %w", err)
	}
	st := mem.GetStatus()

	pidPath := pidFilePath()
	pidLive := false
	pid := 0
	if p, err := readPIDFile(pidPath); err == nil {
		pid = p
		pidLive = processAlive(p)
	}

	fmt.Fprintf(stdout, "goon daemon\n")
	fmt.Fprintf(stdout, "  status:        %s\n", runningStr(st.Running && pidLive))
	if pid != 0 {
		fmt.Fprintf(stdout, "  pid:           %d (alive=%v)\n", pid, pidLive)
	}
	if !st.StartedAt.IsZero() {
		fmt.Fprintf(stdout, "  started:       %s (%s ago)\n",
			st.StartedAt.Format(time.RFC3339), time.Since(st.StartedAt).Round(time.Second))
	}
	if !st.LastPoll.IsZero() {
		fmt.Fprintf(stdout, "  last poll:     %s ago\n", time.Since(st.LastPoll).Round(time.Second))
	}
	if st.BoardName != "" {
		fmt.Fprintf(stdout, "  board:         %s\n", st.BoardName)
	}
	if st.HostName != "" {
		fmt.Fprintf(stdout, "  git host:      %s\n", st.HostName)
	}
	if st.WebAddr != "" {
		fmt.Fprintf(stdout, "  web ui:        http://%s\n", st.WebAddr)
	}
	if st.LastTicket != "" {
		fmt.Fprintf(stdout, "  last ticket:   %s\n", st.LastTicket)
	}

	pending := mem.PendingQuestions()
	fmt.Fprintf(stdout, "\npending questions: %d\n", len(pending))
	for _, q := range pending {
		fmt.Fprintf(stdout, "  [%s] %s — %s\n", q.ID, ticketLabel(q), oneLine(q.Question))
	}

	wfs := mem.ListWorkflows(5)
	fmt.Fprintf(stdout, "\nrecent workflows: %d\n", len(wfs))
	for _, w := range wfs {
		fmt.Fprintf(stdout, "  %s  %s  %-12s  %s\n",
			w.UpdatedAt.Format("2006-01-02 15:04"),
			truncFixed(w.ID, 14), w.State, w.TicketKey)
	}

	tks := mem.ListTickets()
	fmt.Fprintf(stdout, "\nlast-seen tickets: %d\n", len(tks))
	_ = stderr
	return nil
}

func runningStr(b bool) string {
	if b {
		return "RUNNING"
	}
	return "STOPPED"
}

func ticketLabel(q memory.Question) string {
	if q.TicketID == "" {
		return "(no ticket)"
	}
	return q.TicketID
}

func oneLine(s string) string {
	for _, r := range []string{"\r\n", "\n", "\r"} {
		s = replaceAll(s, r, " ")
	}
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}

func replaceAll(s, old, new string) string {
	for {
		i := indexOf(s, old)
		if i < 0 {
			return s
		}
		s = s[:i] + new + s[i+len(old):]
	}
}

func indexOf(s, sep string) int {
	for i := 0; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			return i
		}
	}
	return -1
}

func truncFixed(s string, n int) string {
	if len(s) <= n {
		return s + spaces(n-len(s))
	}
	return s[:n]
}

func spaces(n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = ' '
	}
	return string(out)
}
