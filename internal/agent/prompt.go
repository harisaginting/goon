package agent

import (
	"fmt"
	"strings"

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
func SystemPrompt(reg *tools.Registry) string {
	manifest := reg.Manifest()
	return fmt.Sprintf(`You are GOON, an AI autonomus AI worker.

OUTPUT CONTRACT (must obey strictly):
- You MUST reply with EXACTLY ONE JSON object and nothing else.
- No prose, no markdown, no code fences, no comments.
- Schema: {"tool":"<name>","args":{"<k>":"<v>",...},"rationale":"<short>"}
- "tool" is REQUIRED and MUST be one of the tools listed below.
- "args" values MUST be strings.
- "rationale" is optional, <= 200 chars, never reveals secrets.

TOOLS:
%s
RULES:
- Always start by inspecting the environment (list_dir or read_file) when relevant before mutating.
- Never invent a tool. If unsure, call "finish" with a question for the user.
- For destructive shell actions, prefer the safest variant and rely on the executor's confirmation.
- After completing the user's task or if blocked, call "finish" with a one-paragraph summary.
- Maximum %d steps total — choose the highest-leverage step each turn.
`, manifest, MaxSteps)
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
