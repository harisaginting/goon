# goon

**Autonomous AI engineer.** Picks up your tickets, plans the work, asks before touching anything, codes it, tests it, opens the PR.

```
board → triage → confirm repo → approve plan → execute → test → verify → PR
          LLM        gate            gate        agent    make   LLM×N  GitHub/GitLab/BB
```

Two gates park the workflow until you say yes — via web, Telegram, or terminal. Reject with feedback and goon re-plans from scratch.

> **Goon gets smarter while it works.** While the daemon runs, goon continuously reads your repo, logs, and notes to build its own memory — and asks you to confirm anything it's unsure about. Every completed task sharpens its understanding of your codebase. It remembers what it learned. It knows when to ask.

---

## Install

```sh
go install github.com/harisaginting/goon@latest
```

Or build from source:

```sh
git clone https://github.com/harisaginting/goon
cd goon && make install
```

**Requires Go 1.21+. Zero runtime dependencies.**

---

## Quick start

**Run immediately — no config file needed:**

```sh
go run . start --web=:8080
# → open http://localhost:8080 → Settings to configure LLM + board + git host
```

Or if installed:

```sh
goon start --web=:8080
```

All settings are saved to `./config.json` through the web UI. Run `goon doctor` after configuring to verify connections.

**Try the agent without a board:**

```sh
# offline smoke test
GOON_LLM_PROVIDER=mock \
GOON_MOCK_REPLIES='{"tool":"finish","args":{"message":"done"}}' \
go run . "say hi"

# real task (set LLM key via web UI first, or via CLI)
goon config set GOON_LLM_PROVIDER openai
goon config set OPENAI_API_KEY sk-...

goon "list every .go file under internal/" --explain   # plan only
goon "tidy go.mod"                                     # dry-run (default)
goon "fix the typo in README.md" --auto                # execute automatically
```

---

## Interfaces

All three share the same `./storage/` state — switch freely.

| Interface | Best for | How |
|---|---|---|
| **CLI** | One-off tasks, scripts, CI | `goon "task..."` |
| **Web UI** | Approvals, PRs, workflow editor | `goon start --web=:8080` |
| **Telegram** | Mobile approvals, PR review, chat | Set `TELEGRAM_BOT_TOKEN` |

---

## LLM providers

Pick one in the web UI Settings tab, or via CLI:

```sh
goon config set GOON_LLM_PROVIDER openai     # gpt-4o-mini default
goon config set OPENAI_API_KEY sk-...

goon config set GOON_LLM_PROVIDER anthropic  # claude-sonnet-4-5 default
goon config set ANTHROPIC_API_KEY sk-ant-...

goon config set GOON_LLM_PROVIDER gemini     # gemini-2.5-flash default
goon config set GEMINI_API_KEY ...

goon config set GOON_LLM_PROVIDER ollama     # llama3 default, no key needed
```

Override the model: `goon config set OPENAI_MODEL gpt-4o`, etc.

Per-feature routing (optional):

```sh
GOON_TRIAGE_PROVIDER=anthropic  GOON_TRIAGE_MODEL=claude-opus-4-5
GOON_EXECUTE_PROVIDER=openai    GOON_EXECUTE_MODEL=gpt-4o
GOON_CHAT_MODEL=gpt-4o-mini
```

---

## Boards & git hosts

Configure via web UI (`goon start --web=:8080` → Settings), or via CLI:

```sh
# Jira
goon config set GOON_BOARD jira
goon config set ATLASSIAN_BASE_URL https://you.atlassian.net
goon config set ATLASSIAN_EMAIL me@you.com
goon config set ATLASSIAN_API_TOKEN ...

# GitHub Issues
goon config set GOON_BOARD github
goon config set GITHUB_TOKEN ghp_...
goon config set GITHUB_REPOS owner/repo,owner/other

# Git host (for PRs)
goon config set GOON_GIT_HOST github   # or gitlab | bitbucket
```

All values persist to `./config.json`. No restart needed — the daemon hot-reloads on save.

---

## Commands

```sh
goon "<task>" [--run|--auto|--explain]   # one-shot agent
goon start [--web=:8080]                 # autonomous daemon
goon stop | pause | resume
goon status                              # snapshot
goon doctor                              # live-probe every provider
goon train                               # answer queued questions
goon workflow init|show|edit             # manage pipeline
goon memory list|read|write|search       # manage knowledge notes
goon repo show|add|scan                  # manage repo registry
goon review-prs [--telegram]             # AI-draft PR reviews
goon logs [--follow]
goon config show|set|get          # read/write ./config.json
goon update [ref]
```

**Execution modes:**

| flag | behavior |
|---|---|
| (none) | dry-run — plans but never executes |
| `--run` | executes, asks `y/N` before each mutating step |
| `--auto` | executes every validated step automatically |
| `--explain` | plan only, no tool calls |

---

## Custom workflow

Drop `workflow.json` in your repo root. Goon picks it up on the next poll — no restart.

```jsonc
{
  "version": 1,
  "name": "engineering",
  "branch_prefix": "goon/",
  "test_command": "make ci",
  "verify_runs": 3,
  "auto_approve": false,
  "hooks": {
    "before_pr":  ["go fmt ./...", "goimports -w ."],
    "on_failure": ["notify-slack 'goon failed on {{.Key}}'"]
  }
}
```

Replace the built-in pipeline entirely with `stages`:

```jsonc
{
  "stages": [
    {
      "name": "plan",
      "type": "llm",
      "json_mode": true,
      "prompt": "Break {{.Key}} into steps. Reply JSON {\"steps\":[...]}."
    },
    {
      "name": "execute",
      "type": "agent",
      "task": "Implement: {{index .Stages.plan.steps 0}}"
    },
    {
      "name": "verify",
      "type": "agent",
      "repeat": 3,
      "reject_if": "{{eq .Stages.verify.ok false}}",
      "on_reject": "execute",
      "task": "Verify {{.Key}} is complete. List any defects."
    }
  ]
}
```

Stage types: `llm` · `agent` · `notify` · `http`

Routing fields: `on_next` · `on_reject` · `reject_if` · `ask_stage` · `max_loops`

---

## Memory

```
./storage/
├── memory.json          runtime state (tickets, queues, daemon status)
└── memory/
    ├── SOUL.md           always loaded into the system prompt — put conventions here
    ├── HISTORY.md        one-line log of every completed task
    ├── REPOSITORY.md     remote→local repo registry
    └── *.md              topic notes the agent fetches on demand
```

```sh
goon memory init           # create memory/ and seed SOUL.md
goon memory edit SOUL.md   # your codebase conventions, rules, context
goon memory read HISTORY.md
```

**Goon teaches itself.** While the daemon is idle between tickets, it reads your recent commits, logs, and notes — then distils new insights into `LEARNED.md`. When it encounters something it isn't sure about, it surfaces a question to you directly (web, Telegram, or `goon train`). Your answers become part of its permanent memory.

Opt out: `GOON_AUTO_LEARN=0`. Tune the reflection interval: `GOON_LEARN_INTERVAL_HOURS` (default 24).

For a full breakdown of every file and folder under `./storage/` — what each one holds and which you can safely hand-edit — see **[docs/storage.md](docs/storage.md)**.

---

## Telegram

```sh
TELEGRAM_BOT_TOKEN=...        # from @BotFather
GOON_TELEGRAM_SECRET=...      # any passphrase — used to authenticate chats
TELEGRAM_CHAT_ID=...          # optional default chat for outbound notifications
GOON_AUTO_REVIEW=1            # auto-draft PR reviews for PRs awaiting you
```

Authenticate once per chat: `/auth <secret>`. Then use `/status`, `/queue`, `/answer <id> yes`, `/prs`, `/review owner/repo 42`, `/run <task>`, or plain chat.

---

## Google Workspace (ask goon about your calendar, mail & logs)

Connect your Google account once and just ask goon, right in the **Chat** tab —
it routes the sentence to the right tool:

- "what meetings do I have today?" / "am I busy this afternoon?"  → **Calendar**
- "what are my tasks?" / "what's on my plate today?"  → **Tasks** (+ Jira)
- "check my email from finance last week" / "open it"  → **Gmail**
- "get the traceId for the login of username harisa", "search logs for payment
  failed", "when did user X register?"  → **GCP Cloud Logging**

**Read-only** — goon can look but never send, change, or delete.

```sh
# after creating an OAuth client in Google Cloud (see the guide):
goon config set GOOGLE_OAUTH_CLIENT_ID <id>
goon config set GOOGLE_OAUTH_CLIENT_SECRET <secret>
goon config set GOOGLE_CLOUD_PROJECT <project-id>   # only for log search
goon google auth                                    # one-time browser consent
```

Chat tools added: `calendar_today`, `tasks_list`, `gmail_search`, `gmail_get`,
`gcp_log_search` (each appears only once Google is connected).

**New to this? Follow the step-by-step, click-by-click guide:**
👉 **[docs/google-workspace.md](docs/google-workspace.md)** (≈10 min, no coding).

Built zero-dependency: `internal/google` (hand-rolled OAuth2 + Calendar/Tasks/
Gmail) and `internal/gcplog` (Cloud Logging) use only the Go stdlib.

---

## Parallel agents

The `spawn_agents` tool lets a workflow (or the agent itself) fan out tasks across isolated child processes:

```json
{ "tool": "spawn_agents", "args": { "tasks": "refactor auth\nwrite tests for auth", "wait": "true" } }
```

Each child gets its own `GOON_STORAGE_DIR`. Cap concurrency with `GOON_MAX_AGENTS` (default 4).

---

## Build & test

```sh
make build          # ./goon
make check          # vet + go test -race ./...
make install        # ~/.local/bin/goon
goon doctor         # verify all providers are reachable
```

---

## Extending

- **New tool** — implement `tools.Tool`, register in `DefaultRegistry`. The agent picks it up through the manifest in the system prompt.
- **New board** — implement `boards.Board` in `internal/boards/`, route in `NewFromEnv`.
- **New LLM** — implement `llm.Provider` in `internal/llm/`, route in `NewFromEnv`.

---

MIT
