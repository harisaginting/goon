package cmd

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// runUninstall removes the running goon binary and (optionally) its local
// state directories. The OS allows deleting a running binary on Linux/macOS
// because the kernel pins the inode while the path is unlinked.
//
// Flags:
//
//	--yes    skip the y/N confirmation
//	--purge  also remove ~/.goon, ~/.config/goon, $XDG_CONFIG_HOME/goon
func runUninstall(_ context.Context, args []string, stdout, stderr io.Writer, stdin io.Reader) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	fs.SetOutput(stderr)
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	purge := fs.Bool("purge", false, "also remove ~/.goon and ~/.config/goon")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: goon uninstall [--yes] [--purge]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("uninstall: cannot find current binary: %w", err)
	}
	if resolved, lerr := filepath.EvalSymlinks(self); lerr == nil {
		self = resolved
	}

	dataPaths := stateDirs()

	fmt.Fprintf(stdout, "About to remove:\n")
	fmt.Fprintf(stdout, "  %s\n", self)
	if *purge {
		for _, p := range dataPaths {
			if exists(p) {
				fmt.Fprintf(stdout, "  %s\n", p)
			}
		}
	}

	if !*yes {
		ok, err := confirm(stdin, stdout, "Continue? (y/N) ")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(stdout, "aborted.")
			return errors.New("uninstall: declined")
		}
	}

	// Remove state first so we don't lose info if removing the binary races
	// with another goon invocation.
	if *purge {
		for _, p := range dataPaths {
			if !exists(p) {
				continue
			}
			if err := os.RemoveAll(p); err != nil {
				fmt.Fprintf(stderr, "warning: could not remove %s: %v\n", p, err)
			} else {
				fmt.Fprintf(stdout, "removed %s\n", p)
			}
		}
	}

	if err := os.Remove(self); err != nil {
		return fmt.Errorf("uninstall: cannot remove %s: %w", self, err)
	}
	fmt.Fprintf(stdout, "removed %s\n", self)
	fmt.Fprintln(stdout, "✓ goon uninstalled")
	return nil
}

// stateDirs returns goon's local state directories, in priority order.
func stateDirs() []string {
	var paths []string
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".goon"))
		if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
			paths = append(paths, filepath.Join(xdg, "goon"))
		} else {
			paths = append(paths, filepath.Join(home, ".config", "goon"))
		}
	}
	return paths
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// confirm prompts and returns true on "y" / "yes" (case-insensitive).
// It is duplicated from the executor package to keep cmd self-contained.
func confirm(stdin io.Reader, stdout io.Writer, prompt string) (bool, error) {
	fmt.Fprint(stdout, prompt)
	br := bufio.NewReader(stdin)
	line, err := br.ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes", nil
}
