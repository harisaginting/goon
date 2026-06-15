# Workflow presets

Copy any of these to your repo root as `workflow.json` (or set
`GOON_WORKFLOW_FILE=/path/to/preset.json`) and goon picks it up on the
next poll cycle. No restart needed.

| File | When to use |
|---|---|
| [`minimal.json`](minimal.json) | The smallest config that works. Everything else inherits from defaults. |
| [`engineering.json`](engineering.json) | All-purpose eng pipeline with every hook populated + fmt/vet on PR. **This is the recommended starting point for code-tickets.** |
| [`engineering-stages.json`](engineering-stages.json) | Same job as `engineering.json` but expressed as an editable **role-graph** — executor → human reviewer → rework loop → open PR + notify, plus an analyst the executor can `ask`. Copy this to tune prompts/wiring. |
| [`unattended.json`](unattended.json) | `auto_approve: true` — no human gates. For trusted CI / a long-running daemon you don't want to babysit. |
| [`marketing-brief.json`](marketing-brief.json) | Non-engineering work. A role-graph (draft → human review → publish + notify) that opens no PR. |
| [`sales-lead.json`](sales-lead.json) | Inbound-lead role-graph: qualify → draft → human review → push-to-CRM, with a rework loop on reject. |

## Cheatsheet — what every field controls

```jsonc
{
  "version": 1,                          // pin to 1 (current schema)
  "name": "engineering-prod",            // shown at startup + every workflow.start log
  "description": "...",                  // human-readable summary; printed by `goon workflow show`
  "branch_prefix": "goon/",              // PR branch = <prefix><lowercased ticket key>
  "test_command": "make ci",             // empty = auto-detect (make test or go test ./...)
  "verify_runs": 5,                      // 1..10. Extra LLM passes between execute and PR.
  "auto_approve": false,                 // true skips confirm_repo + approve_plan gates
  "pr_title_template": "...",            // Go text/template; data: {Key,Title,URL,Source,Project,Branch,Repo,Plan}
  "pr_body_template":  "...",
  "extra_labels": ["customer-x"],        // appended to ["goon","auto"]
  "hooks": {                             // every value is a list of `sh -c` (POSIX) or `cmd /C` (Windows) commands
    "before_triage":  [],                //   fires before the planner LLM call
    "before_execute": [],                //   ...before the agent loops on each plan step
    "after_execute":  [],
    "before_test":    [],
    "after_test":     [],                //   only fires on test success
    "before_verify":  [],
    "after_verify":   [],
    "before_pr":      [],                //   great for fmt/lint/format
    "after_pr":       [],
    "on_failure":     []                 //   best-effort; doesn't block anything
  },
  "stages": [...]                        // optional: REPLACES the built-in pipeline wholesale (see "Stage roles" below)
}
```

## Stage roles (the role-graph)

When you set `stages`, goon runs a **role-graph** instead of the built-in
phases: typed nodes wired by the action each one emits. Five roles:

| role | does | routes via |
|---|---|---|
| `executor` | does the work — runs the agent loop on a `task`, or `"do": "open_pr"` to ship. Can finish a turn with `ASK: <question>` to consult an analyst. | `on_next` (fan-out allowed), `ask` |
| `analyst` | answers with a single LLM call (`prompt`, `json_mode`, `temperature`, optional `urls` fetched first). Reached via another node's `ask`, or wired forward with `on_next`. | `on_next` |
| `reviewer` | gates the flow. `"mode": "human"` (default) pauses for your approval; `"mode": "llm"` lets the model decide. | `on_approve` (fan-out), `on_reject` |
| `loop` | bounded rework — sends work back to `on_next` up to `max_loops` times, then exits via `on_done`. | `on_next`, `on_done` |
| `notify` | sends a `message` (Telegram). Terminal by default. | `on_next` |

Wiring fields take a stage name, `"end"` (finish), or an array (fan-out). Use
`"version": 2` for stage graphs. A node with no explicit `on_next` falls
through to the next **non-analyst** stage in array order, then to `end` —
analysts are ask-only sidecars, never the implicit next step. The web
**Pipeline editor** (Workflows tab) draws and edits this graph visually.

## Hooks — env vars you can use

Every hook command runs with these exported:

| variable | example |
|---|---|
| `$TICKET_KEY` | `ENG-123` |
| `$TICKET_TITLE` | `Add login` |
| `$TICKET_URL` | `https://acme.atlassian.net/browse/ENG-123` |
| `$TICKET_SOURCE` | `jira` |
| `$TICKET_PROJECT` | `ENG` |
| `$REPO` | `/home/me/repos/eng` |
| `$BRANCH` | `goon/eng-123` |

You can also use Go template syntax inside the command itself:

```json
"before_execute": ["git fetch origin {{.Branch}} || true"]
```

## Need help?

- `goon workflow show` — print the resolved config (defaults + your overrides)
- `goon workflow path` — print where goon is looking
- `goon workflow hooks` — list every supported hook name + env vars
- `goon doctor` — verify your providers are configured correctly
