package agent

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// ShellContext is a snapshot of the environment injected into the prompt.
type ShellContext struct {
	CWD        string
	Files      []string
	LastOutput string
	Frequent   []string
}

// Snapshot captures the current shell context. It NEVER returns an error —
// missing data is logged into the snapshot itself for transparency. When
// workDir is non-empty it overrides the process cwd, so the snapshot the
// LLM sees matches the directory where its commands actually run (the
// selected repo).
func Snapshot(workDir, lastOutput string, frequent []string) ShellContext {
	cwd := strings.TrimSpace(workDir)
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			cwd = "(unavailable: " + err.Error() + ")"
		}
	}
	files := listFilesLimited(cwd, 20)
	return ShellContext{
		CWD:        cwd,
		Files:      files,
		LastOutput: lastOutput,
		Frequent:   frequent,
	}
}

// Render formats the snapshot as a compact human-readable block.
func (s ShellContext) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Current dir: %s\n", s.CWD)
	if len(s.Files) > 0 {
		fmt.Fprintf(&b, "Files (max 20): %s\n", strings.Join(s.Files, ", "))
	}
	if s.LastOutput != "" {
		fmt.Fprintf(&b, "Last output:\n%s\n", truncateLines(s.LastOutput, 30))
	}
	if len(s.Frequent) > 0 {
		fmt.Fprintf(&b, "Frequent commands: %s\n", strings.Join(s.Frequent, " | "))
	}
	return b.String()
}

func listFilesLimited(dir string, limit int) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() {
			n += "/"
		}
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) > limit {
		names = names[:limit]
	}
	return names
}

func truncateLines(s string, max int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= max {
		return s
	}
	return strings.Join(lines[:max], "\n") + fmt.Sprintf("\n…(+%d more lines)", len(lines)-max)
}
