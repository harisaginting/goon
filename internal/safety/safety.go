// Package safety validates shell commands before execution.
//
// The validator is intentionally conservative — it errs on the side of
// blocking. The agent loop surfaces validator errors back to the LLM so it
// can rewrite the command.
package safety

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Validator inspects a string command and returns nil for safe, error for blocked.
type Validator interface {
	Validate(cmd string) error
}

// Default returns the standard validator with sensible blocklists.
func Default() Validator {
	return &defaultValidator{
		// Block pattern -> human reason.
		blocked: []rule{
			{regexp.MustCompile(`(?i)\brm\s+-[rfRF]+\s+/(\s|$)`), "rm of root"},
			{regexp.MustCompile(`(?i)\brm\s+-[rfRF]+\s+/\*`), "rm of root glob"},
			{regexp.MustCompile(`(?i)\brm\s+-[rfRF]+\s+~(\s|$)`), "rm of $HOME"},
			{regexp.MustCompile(`(?i)\brm\s+-[rfRF]+\s+\$HOME(\s|$)`), "rm of $HOME"},
			{regexp.MustCompile(`(?i)\bmkfs\b`), "filesystem reformat"},
			{regexp.MustCompile(`(?i)\bdd\s+if=.*of=/dev/`), "dd to a device"},
			{regexp.MustCompile(`(?i):\(\)\s*\{\s*:\|:&\s*\}\s*;`), "fork bomb"},
			{regexp.MustCompile(`(?i)>\s*/dev/sd[a-z]`), "raw write to disk"},
			{regexp.MustCompile(`(?i)\bshutdown\b`), "shutdown"},
			{regexp.MustCompile(`(?i)\bhalt\b`), "halt"},
			{regexp.MustCompile(`(?i)\breboot\b`), "reboot"},
			{regexp.MustCompile(`\bcurl\s+[^|]*\|\s*sh\b`), "curl|sh — pipe-to-shell"},
			{regexp.MustCompile(`\bwget\s+[^|]*\|\s*sh\b`), "wget|sh — pipe-to-shell"},
			{regexp.MustCompile(`\b(chmod|chown)\s+-R\s+[0-7]+\s+/(\s|$)`), "recursive chmod/chown of /"},
		},
	}
}

type defaultValidator struct {
	blocked []rule
}

type rule struct {
	re     *regexp.Regexp
	reason string
}

// Validate checks the command. Returns nil if safe.
func (v *defaultValidator) Validate(cmd string) error {
	c := strings.TrimSpace(cmd)
	if c == "" {
		return errors.New("empty command")
	}
	// Empty rm path: `rm -rf ` with nothing after it.
	if m := regexp.MustCompile(`(?i)^rm\s+-[rfRF]+\s*$`).FindString(c); m != "" {
		return errors.New("safety: empty rm path")
	}
	for _, r := range v.blocked {
		if r.re.MatchString(c) {
			return fmt.Errorf("safety: blocked (%s)", r.reason)
		}
	}
	return nil
}

// AlwaysAllow is a Validator that approves everything. Use only in tests.
type AlwaysAllow struct{}

// Validate always returns nil.
func (AlwaysAllow) Validate(string) error { return nil }
