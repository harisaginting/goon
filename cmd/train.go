package cmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/harisaginting/goon/internal/memory"
)

// runTrain walks pending questions and prompts the user for an answer per
// question. With "--all" it shows the full question log including answered.
//
//	goon train                  # answer pending questions interactively
//	goon train --list           # only print the pending questions, don't prompt
//	goon train --all            # print the full question log
//	goon train answer <id> <a>  # set the answer for one question non-interactively
func runTrain(_ context.Context, args []string, stdout, stderr io.Writer, stdin io.Reader) error {
	mem, err := memory.New(os.Getenv("GOON_MEMORY_PATH"))
	if err != nil {
		return fmt.Errorf("memory: %w", err)
	}

	// Subsubcommand: `goon train answer <id> <answer>`
	if len(args) >= 1 && args[0] == "answer" {
		if len(args) < 3 {
			return fmt.Errorf("usage: goon train answer <id> <answer>")
		}
		id := args[1]
		ans := strings.Join(args[2:], " ")
		if !mem.AnswerQuestion(id, ans) {
			return fmt.Errorf("question %q not found or already answered", id)
		}
		fmt.Fprintf(stdout, "✓ answered %s\n", id)
		return nil
	}

	listOnly := false
	all := false
	for _, a := range args {
		switch a {
		case "--list", "-l":
			listOnly = true
		case "--all", "-a":
			all = true
		}
	}

	if all {
		printAllQuestions(stdout, mem)
		return nil
	}

	pending := mem.PendingQuestions()
	if len(pending) == 0 {
		fmt.Fprintln(stdout, "no pending questions ✓")
		return nil
	}
	fmt.Fprintf(stdout, "%d pending question(s):\n\n", len(pending))
	if listOnly {
		for _, q := range pending {
			fmt.Fprintf(stdout, "  [%s] (%s) %s\n", q.ID, ticketLabel(q), q.Question)
		}
		return nil
	}

	br := bufio.NewReader(stdin)
	for _, q := range pending {
		fmt.Fprintf(stdout, "─── %s ───\n", q.ID)
		if q.TicketID != "" {
			fmt.Fprintf(stdout, "ticket: %s\n", q.TicketID)
		}
		fmt.Fprintf(stdout, "Q: %s\n", q.Question)
		fmt.Fprint(stdout, "A: ")
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		line = strings.TrimSpace(line)
		if line == "" || line == "skip" {
			fmt.Fprintln(stderr, "(skipped)")
			continue
		}
		if !mem.AnswerQuestion(q.ID, line) {
			fmt.Fprintln(stderr, "(could not record answer)")
			continue
		}
		fmt.Fprintln(stdout, "✓ recorded")
	}
	left := mem.PendingQuestions()
	fmt.Fprintf(stdout, "\n%d still pending after this session\n", len(left))
	return nil
}

func printAllQuestions(w io.Writer, mem *memory.Memory) {
	all := mem.AllQuestions()
	if len(all) == 0 {
		fmt.Fprintln(w, "no questions yet")
		return
	}
	for _, q := range all {
		state := "PENDING"
		if !q.Pending() {
			state = "ANSWERED"
		}
		fmt.Fprintf(w, "[%s] %s (%s)\n", state, q.ID, ticketLabel(q))
		fmt.Fprintf(w, "  Q: %s\n", q.Question)
		if q.Answer != "" {
			fmt.Fprintf(w, "  A: %s\n", q.Answer)
		}
	}
}
