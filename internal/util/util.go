// Package util holds tiny helpers shared by three or more packages.
//
// Anything that lives here must be:
//   - dependency-free (stdlib only — no internal/* imports either, so this
//     package can be imported from anywhere without creating a cycle);
//   - small enough to read in one screen;
//   - actually used by 3+ call sites (not "might be useful someday").
//
// If a helper outgrows this file or sprouts state, give it its own package.
package util

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// Truncate shortens s to at most max runes by appending an ellipsis
// (the literal "…") when the limit is exceeded. Returns s unchanged when
// it already fits. The byte length of the returned string is bounded by
// max + len("…").
//
// max is the maximum byte length of the original input that will be kept.
// We deliberately don't decode runes — every existing call site treats
// the input as bytes (HTTP response bodies, log fragments) and the
// ellipsis exists purely to flag truncation in human-readable output.
func Truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// EnvOr returns the trimmed value of the named environment variable, or
// def when the variable is unset or holds only whitespace.
//
// Trimming whitespace matches the user expectation that `EXPORT FOO=" "`
// is "not set" — most callers pass a default like "https://api.example.com"
// and a blank value would otherwise produce broken URLs.
func EnvOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// ConfirmTTY prints prompt to out, reads a single line from in, and
// returns true when the answer (case-insensitive, trimmed) is "y" or
// "yes". Any other input — including EOF — returns false.
//
// io.EOF is treated as a graceful "no" so non-interactive callers (CI,
// piped stdin) decline by default rather than erroring.
func ConfirmTTY(prompt string, in io.Reader, out io.Writer) bool {
	fmt.Fprint(out, prompt)
	br := bufio.NewReader(in)
	line, err := br.ReadString('\n')
	if err != nil && err != io.EOF {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}
