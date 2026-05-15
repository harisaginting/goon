// Package personal holds goon's "soul" — character and voice
// instructions auto-injected into every LLM call alongside PINNED.md.
//
// Memory layers, in order of how often they're loaded:
//
//   PERSONAL  always — defines HOW goon talks and decides
//   KNOWLEDGE always (PINNED.md) + on demand (topic notes)
//             — facts about THIS project / org
//   SKILLS    on demand — specialist procedures invoked by name
//
// On disk this is a single markdown file at ./storage/personal.md
// (override via $GOON_PERSONAL_FILE). Editable from the web Memory
// tab and Telegram /personal commands.
//
// The personality file is small and high-signal by intent. Anything
// longer than a screen probably belongs in a topic note instead.
package personal

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/harisaginting/goon/internal/storage"
)

// Filename is the canonical basename. Kept exported so the web UI
// and Telegram commands can refer to it consistently.
const Filename = "personal.md"

// Path returns the resolved file path. Precedence:
//
//   1. $GOON_PERSONAL_FILE (explicit override)
//   2. <storage.Root()>/personal.md   (default — ./storage/personal.md)
func Path() string {
	if p := strings.TrimSpace(os.Getenv("GOON_PERSONAL_FILE")); p != "" {
		return p
	}
	return storage.Path(Filename)
}

// Read returns the trimmed file body, or "" if absent / unreadable.
// Caller should treat empty as "no personality block to inject" and
// fall back to whatever default tone the model has.
func Read() string {
	b, err := os.ReadFile(Path())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Write replaces the file's content. Creates the parent directory
// if missing. Atomic via tmp + rename so a crash mid-write doesn't
// leave a half-saved personality.
func Write(body string) error {
	p := Path()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.TrimRight(body, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// SeedDefault writes the default personality file ONLY if the file
// doesn't already exist. Safe to call on every startup — won't
// clobber user edits.
func SeedDefault() error {
	p := Path()
	if _, err := os.Stat(p); err == nil {
		return nil // already there
	}
	return Write(Default)
}

// Default is the out-of-the-box personality. Users edit this via the
// Memory tab or `goon memory write personal.md ...`. Kept short and
// opinionated — placeholder text invites users to make it theirs.
const Default = `# goon — character

You are GOON, an autonomous engineering co-pilot. You pair with the
user like a senior engineer would, not like a chatbot.

## Voice

- Direct. Lead with the answer, then the reasoning. Don't bury the lede.
- Plain English. No marketing tone, no "as an AI..." disclaimers, no apologies.
- When you don't know, say so plainly and propose the next step.
- When asked for an opinion on a tradeoff, give one. Pushback is fine.

## Defaults

- Read PINNED.md before answering project-specific questions.
- Read skills/*.md when the user invokes a named skill.
- Confirm before destructive actions (delete, force-push, merge).
- Quote ticket KEYs and PR numbers verbatim — never paraphrase IDs.

## What you care about

- Shipping working code with a paper trail (commits, PRs, tickets).
- Not creating tech debt when a small extra effort avoids it.
- Telling the user when their plan looks wrong, even if they just asked you to execute.

(Edit this file — via the web Memory tab → Personal segment, or
` + "`goon memory write personal.md ...`" + ` — to make it sound like
your team.)
`
