package cmd

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/harisaginting/goon/internal/logx"
)

// runLogs handles `goon logs [...]`.
//
// Modes (mutually exclusive):
//
//	(none)         tail the last 100 lines and exit
//	--tail=N       tail the last N lines and exit
//	--follow       tail and follow (like `tail -f`)
//	--clear        truncate the log file (keeps rotations)
//	--path         print the log file path and exit
func runLogs(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		tailN   = fs.Int("tail", 100, "print the last N lines")
		follow  = fs.Bool("follow", false, "tail and follow (like tail -f)")
		clear   = fs.Bool("clear", false, "truncate the log file (keeps rotations)")
		pathOnly = fs.Bool("path", false, "print the log file path and exit")
	)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: goon logs [--tail=N | --follow | --clear | --path]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	path := logx.Default().Path()

	if *pathOnly {
		fmt.Fprintln(stdout, path)
		return nil
	}
	if *clear {
		f, err := os.OpenFile(path, os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Errorf("clear: %w", err)
		}
		_ = f.Close()
		fmt.Fprintf(stdout, "✓ truncated %s\n", path)
		return nil
	}

	if *follow {
		return tailFollow(ctx, path, *tailN, stdout)
	}
	return tailLast(path, *tailN, stdout)
}

// tailLast prints the last n lines of the log file. If the file is shorter
// than n lines, prints the whole file. Reads the entire file (cheap for
// 10MB rotation cap) and slices in memory rather than reverse-seeking,
// which keeps the code obvious.
func tailLast(path string, n int, w io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(w, "(no log file yet at %s — run goon to generate one)\n", path)
			return nil
		}
		return err
	}
	defer f.Close()

	lines := make([]string, 0, n)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024) // allow long lines
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if len(lines) > n {
			lines = lines[1:] // drop oldest, keep last n
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	for _, line := range lines {
		fmt.Fprintln(w, line)
	}
	return nil
}

// tailFollow does an initial tailLast(n) and then polls for new bytes,
// printing them as they appear. Exits cleanly on ctx.Done(). Polls every
// 250ms — cheap and good enough for a log of this size.
func tailFollow(ctx context.Context, path string, n int, w io.Writer) error {
	if err := tailLast(path, n, w); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	// Seek to end; start streaming new appends.
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}

	r := bufio.NewReader(f)
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		for {
			line, err := r.ReadString('\n')
			if line != "" {
				_, _ = io.WriteString(w, line)
			}
			if err != nil {
				if err == io.EOF {
					break // no new data; wait for next tick
				}
				return err
			}
		}
		// If the file got truncated/rotated under us, reopen it.
		if st, err := os.Stat(path); err == nil {
			cur, _ := f.Seek(0, io.SeekCurrent)
			if st.Size() < cur {
				_ = f.Close()
				f, err = os.Open(path)
				if err != nil {
					return err
				}
				r = bufio.NewReader(f)
			}
		}
	}
}

// formatLogPath is a tiny helper used by `goon doctor` to display where
// logs go. Kept here so doctor doesn't have to import os/filepath itself.
func formatLogPath() string {
	p := logx.Default().Path()
	if strings.TrimSpace(p) == "" {
		return "(disabled)"
	}
	return p
}
