package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/harisaginting/goon/internal/notes"
)

// runMemory dispatches `goon memory <action>`. Subactions:
//
//	goon memory                       alias for list
//	goon memory list                  list every note
//	goon memory read <name>           print one note
//	goon memory write <name>          read stdin, write to note
//	goon memory append <name>         read stdin, append to note
//	goon memory edit <name>           open note in $EDITOR (creates if missing)
//	goon memory delete <name>         remove a note (asks for confirmation)
//	goon memory path                  print the memory directory
//	goon memory init                  ensure dir exists + seed SOUL.md
//	goon memory search <query>        grep across notes
//
// All operations go through internal/notes which enforces path-safety, so
// the user can't escape the memory dir by typing "../../etc/passwd".
func runMemory(ctx context.Context, args []string, stdout, stderr io.Writer, stdin io.Reader) error {
	action := "list"
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		action = args[0]
		rest = args[1:]
	}

	store, err := notes.New("")
	if err != nil {
		return fmt.Errorf("memory: open store: %w", err)
	}

	switch action {
	case "list", "ls":
		return memList(store, stdout)
	case "read", "cat":
		return memRead(store, rest, stdout, stderr)
	case "write":
		return memWrite(store, rest, stdin, stdout, stderr)
	case "append":
		return memAppend(store, rest, stdin, stdout, stderr)
	case "edit":
		return memEdit(ctx, store, rest, stdin, stdout, stderr)
	case "delete", "rm":
		return memDelete(store, rest, stdin, stdout, stderr)
	case "path":
		fmt.Fprintln(stdout, store.Path())
		return nil
	case "init":
		return memInit(store, stdout)
	case "search", "grep":
		return memSearch(store, rest, stdout, stderr)
	case "help", "-h", "--help":
		printMemoryHelp(stdout)
		return nil
	default:
		printMemoryHelp(stderr)
		return fmt.Errorf("unknown memory action %q", action)
	}
}

func printMemoryHelp(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  goon memory                  list every note (alias for `list`)")
	fmt.Fprintln(w, "  goon memory list             list every note")
	fmt.Fprintln(w, "  goon memory read <name>      print one note")
	fmt.Fprintln(w, "  goon memory write <name>     write a note from stdin (replaces)")
	fmt.Fprintln(w, "  goon memory append <name>    append stdin to a note")
	fmt.Fprintln(w, "  goon memory edit <name>      open in $EDITOR (creates if missing)")
	fmt.Fprintln(w, "  goon memory delete <name>    remove a note (asks for confirmation)")
	fmt.Fprintln(w, "  goon memory search <query>   case-insensitive grep across notes")
	fmt.Fprintln(w, "  goon memory path             print the memory dir")
	fmt.Fprintln(w, "  goon memory init             create dir + seed SOUL.md")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Notes are markdown files in $GOON_MEMORY_DIR (default ./storage/memory under $GOON_STORAGE_DIR).")
	fmt.Fprintln(w, "SOUL.md is auto-loaded into every agent prompt — keep it short and high-signal.")
	fmt.Fprintln(w, "HISTORY.md is the running log of past tasks (auto-appended after every run).")
	fmt.Fprintln(w, "(Legacy PINNED.md is still read transparently and auto-migrates on `goon memory init`.)")
}

func memList(s *notes.Store, stdout io.Writer) error {
	names, err := s.List()
	if err != nil {
		return err
	}
	if len(names) == 0 {
		fmt.Fprintf(stdout, "(no notes yet — create one with `goon memory edit <name>`)\n")
		fmt.Fprintf(stdout, "memory dir: %s\n", s.Path())
		return nil
	}
	fmt.Fprintf(stdout, "memory dir: %s\n\n", s.Path())
	for _, n := range names {
		marker := "  "
		if n == notes.SoulFilename || n == "PINNED.md" {
			marker = "* " // soul note is auto-loaded
		}
		fmt.Fprintf(stdout, "%s%s\n", marker, n)
	}
	if hasSoul(names) {
		fmt.Fprintf(stdout, "\n* = auto-loaded into the agent's system prompt\n")
	}
	return nil
}

func hasSoul(names []string) bool {
	for _, n := range names {
		if n == notes.SoulFilename || n == "PINNED.md" {
			return true
		}
	}
	return false
}

func memRead(s *notes.Store, args []string, stdout, stderr io.Writer) error {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "usage: goon memory read <name>")
		return fmt.Errorf("missing note name")
	}
	body, err := s.Read(args[0])
	if err != nil {
		return err
	}
	_, _ = stdout.Write([]byte(body))
	if !strings.HasSuffix(body, "\n") {
		fmt.Fprintln(stdout)
	}
	return nil
}

func memWrite(s *notes.Store, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "usage: goon memory write <name> < content")
		return fmt.Errorf("missing note name")
	}
	data, err := io.ReadAll(stdin)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return fmt.Errorf("memory write: stdin was empty (use `goon memory edit` to write interactively)")
	}
	if err := s.Write(args[0], string(data)); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "wrote %d bytes to %s\n", len(data), args[0])
	return nil
}

func memAppend(s *notes.Store, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "usage: goon memory append <name> < content")
		return fmt.Errorf("missing note name")
	}
	data, err := io.ReadAll(stdin)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return fmt.Errorf("memory append: stdin was empty")
	}
	if err := s.Append(args[0], string(data)); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "appended %d bytes to %s\n", len(data), args[0])
	return nil
}

func memEdit(ctx context.Context, s *notes.Store, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "usage: goon memory edit <name>")
		return fmt.Errorf("missing note name")
	}
	full, err := s.Resolve(args[0])
	if err != nil {
		return err
	}
	// If the note doesn't exist yet, leave it that way and let the editor
	// create it on save. Same UX as `vim newfile.md`.
	if _, statErr := os.Stat(full); os.IsNotExist(statErr) {
		// Touch parent dir so $EDITOR doesn't fail on a missing folder.
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
	}
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = os.Getenv("VISUAL")
	}
	if editor == "" {
		editor = "vi"
	}
	cmd := exec.CommandContext(ctx, editor, full)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

func memDelete(s *notes.Store, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "usage: goon memory delete <name>")
		return fmt.Errorf("missing note name")
	}
	full, err := s.Resolve(args[0])
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(full); os.IsNotExist(statErr) {
		return fmt.Errorf("no such note: %s", args[0])
	}
	fmt.Fprintf(stdout, "delete %s? [y/N]: ", args[0])
	buf := make([]byte, 4)
	n, _ := stdin.Read(buf)
	answer := strings.ToLower(strings.TrimSpace(string(buf[:n])))
	if answer != "y" && answer != "yes" {
		fmt.Fprintln(stdout, "aborted.")
		return nil
	}
	if err := s.Delete(args[0]); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "deleted %s\n", args[0])
	return nil
}

func memInit(s *notes.Store, stdout io.Writer) error {
	created, err := s.SeedSoulTemplate()
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "memory dir: %s\n", s.Path())
	if created {
		fmt.Fprintf(stdout, "seeded starter %s — edit it with `goon memory edit %s`\n", notes.SoulFilename, notes.SoulFilename)
	} else {
		fmt.Fprintf(stdout, "(%s already exists — leaving it alone)\n", notes.SoulFilename)
	}
	return nil
}

func memSearch(s *notes.Store, args []string, stdout, stderr io.Writer) error {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "usage: goon memory search <query>")
		return fmt.Errorf("missing query")
	}
	q := strings.Join(args, " ")
	hits, err := s.Search(q, 100)
	if err != nil {
		return err
	}
	if len(hits) == 0 {
		fmt.Fprintln(stdout, "(no matches)")
		return nil
	}
	for _, h := range hits {
		fmt.Fprintf(stdout, "%s:%d: %s\n", h.Name, h.Line, h.Text)
	}
	return nil
}
