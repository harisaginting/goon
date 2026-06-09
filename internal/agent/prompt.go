package agent

import (
	"fmt"
	"strings"

	"github.com/harisaginting/goon/internal/notes"
	"github.com/harisaginting/goon/internal/tools"
)

// SystemPrompt builds the strict-JSON system prompt the model must obey.
//
// It is intentionally short and rule-heavy:
//
//   - role: shell assistant
//   - output ONLY one JSON object — never prose, never multiple objects
//   - use only tools listed in the manifest
//   - prefer "finish" when done
//   - safety: never call destructive commands
//
// Persistent memory: when a notes store is available and SOUL.md has
// content, that content is injected as a "## Persistent memory" section
// near the top of the prompt. SOUL.md holds BOTH the character /
// voice instructions (how goon talks) AND the project knowledge
// (what goon knows about this repo) — a single always-loaded file
// replaces the older personal.md + SOUL.md split. (Legacy PINNED.md
// is still read transparently when SOUL.md is absent.)
func SystemPrompt(reg *tools.Registry) string {
	manifest := reg.Manifest()

	// Soul block — character + project knowledge, always loaded.
	// Failures to open the store are silently swallowed because a
	// missing/unreadable memory dir is the common case during
	// first-run and shouldn't crash the agent.
	soulBlock := ""
	if store, err := notes.New(""); err == nil {
		if soul := store.Soul(); soul != "" {
			// Wrapped in XML-style delimiters so a crafted SOUL.md entry
			// cannot inject instructions outside the data boundary.
			soulBlock = fmt.Sprintf(`
PERSISTENT MEMORY (%s/%s) — project knowledge and conventions for you to follow:
<soul>
%s
</soul>

`, store.Path(), notes.SoulFilename, soul)
		}
	}

	memoryHowto := `MEMORY TOOLS:
- memory_list / memory_read / memory_write / memory_append / memory_search
  let you persist knowledge across runs in the goon memory dir.
- Write what's worth remembering after a task: conventions discovered,
  bugs avoided, names+IDs that matter, file layouts learned.
- One topic per .md file, kebab-case names. Use memory_append to add to
  an existing note instead of overwriting.
- Anything in SOUL.md is auto-loaded into this prompt — keep that file
  small and high-signal (it's the project's soul / always-on context).
- HISTORY.md is the running log of past tasks (timestamp + outcome);
  read it to recall what you've already tried on this repo.

`

	return fmt.Sprintf(`You are GOON, an AI autonomus AI worker.

OUTPUT CONTRACT (must obey strictly):
- You MUST reply with EXACTLY ONE JSON object and nothing else.
- No prose, no markdown, no code fences, no comments.
- Schema: {"tool":"<name>","args":{"<k>":"<v>",...},"rationale":"<short>"}
- "tool" is REQUIRED and MUST be one of the tools listed below.
- "args" values MUST be strings.
- "rationale" is optional, <= 200 chars, never reveals secrets.
%s
TOOLS:
%s
%sRULES:
- Always start by inspecting the environment (list_dir or read_file) when relevant before mutating.
- Never invent a tool. If unsure, call "finish" with a question for the user.
- For destructive shell actions, prefer the safest variant and rely on the executor's confirmation.
- After completing the user's task or if blocked, call "finish" with a one-paragraph summary.
- Maximum %d steps total — choose the highest-leverage step each turn.
`, soulBlock, manifest, memoryHowto, MaxSteps)
}

// BuildUserContext stitches the user task with the runtime context block.
func BuildUserContext(task string, ctx ShellContext) string {
	var b strings.Builder
	b.WriteString("USER TASK:\n")
	b.WriteString(task)
	b.WriteString("\n\nENVIRONMENT:\n")
	b.WriteString(ctx.Render())
	return b.String()
}
