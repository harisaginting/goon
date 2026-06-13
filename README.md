<p align="center">
  <img src="docs/logo.svg" alt="goon" width="72" height="72">
</p>

<h1 align="center">goon</h1>

<p align="center">
  <strong>Autonomous AI worker for your team.</strong><br>
  Run any workflow with AI — write code, open PRs, summarize emails, post daily standups, monitor logs, or build any custom automation. Runs as a daemon. Learns your context. Asks before acting.
</p>

<p align="center">
  <a href="https://github.com/harisaginting/goon/releases"><img src="https://img.shields.io/github/v/release/harisaginting/goon?color=6366F1&label=latest" alt="Latest release"></a>
  <a href="https://github.com/harisaginting/goon/blob/main/LICENSE"><img src="https://img.shields.io/badge/license-MIT-6366F1" alt="MIT license"></a>
  <img src="https://img.shields.io/badge/Go-1.21%2B-00ADD8?logo=go" alt="Go 1.21+">
  <img src="https://img.shields.io/badge/zero_dependencies-stdlib_only-10B981" alt="Zero dependencies">
</p>

<p align="center">
  <a href="https://harisaginting.github.io/goon/">Website</a> ·
  <a href="#quick-start">Quick start</a> ·
  <a href="#custom-workflow">Custom workflows</a> ·
  <a href="#how-goon-compares">How it compares</a> ·
  <a href="docs/">Docs</a>
</p>

---

Goon is a **general-purpose AI automation daemon**. The built-in pipeline ships code end-to-end, but the same engine runs any workflow you describe in JSON:

| Use case | What goon does |
|---|---|
| 🧑‍💻 **Software engineering** | Poll Jira/GitHub → plan → write code → test → open PR |
| 📧 **Email digest** | Read Gmail → summarize unread threads → post to Telegram |
| 📋 **Daily standup** | Fetch yesterday's commits + tickets → draft standup → send to Slack |
| 🔍 **Log monitoring** | Search GCP logs for errors → triage → open incident ticket |
| 📊 **Weekly report** | Aggregate metrics → write Markdown report → email to team |
| 🤖 **Any custom job** | Chain LLM, HTTP, agent, notify, and loop stages however you need |

Two gates pause workflows until **you say yes** — via web UI, Telegram, or terminal. Reject with feedback and goon re-plans from scratch.

> **Goon gets smarter while it works.** While the daemon idles, it reads your commits, logs, and notes, distils insights into `LEARNED.md`, and asks you directly when it's unsure. Every run sharpens its understanding of your context.

---

## Why goon?

Most AI tools answer questions or complete one-off tasks. **Goon is different** — it's a long-running daemon you deploy once and it keeps working:

- 🎫 **Reads your board** — polls Jira or GitHub Issues and picks the next task automatically
- ⚙️ **Runs any workflow** — code, email, reports, monitoring — defined in a single `workflow.json`
- 🔒 **Asks permission** — human-approval gates before touching anything sensitive
- 🛠 **Writes and tests** — for code workflows: runs your test suite, retries on failure, verifies output
- 🔀 **Opens the PR** — branches, commits, pushes to GitHub, GitLab, or Bitbucket
- 🧠 **Builds memory** — learns your conventions, repo layout, and past decisions across runs
- 💻 **Codes in your browser** — pick a directory and run an agentic coding session in the dashboard, like Claude Code in a tab
- 📱 **Mobile-friendly** — approve or reject from Telegram while you're away from your desk
- 🔌 **Google Workspace** — reads your Calendar, Gmail, Tasks, and GCP logs on demand

---

## Quick start

**One command — no config file required:**

```sh
go install github.com/harisaginting/goon@latest
goon start --web=:8080
# open http://localhost:8080 → Settings → configure your LLM + board
```

Or build from source:

```sh
git clone https://github.com/harisaginting/goon
cd goon && make install
```

**Try the agent immediately without a board:**

```sh
# offline smoke test (no API key needed)
GOON_LLM_PROVIDER=mock \
GOON_MOCK_REPLIES='{"tool":"finish","args":{"message":"done"}}' \
goon "say hi"

# real one-shot tasks
goon config set GOON_LLM_PROVIDER openai
goon config set OPENAI_API_KEY sk-...

goon "list every .go file under internal/" --explain   # plan only, no execution
goon "tidy go.mod"                                     # dry-run (default)
goon "fix the typo in README.md" --auto                # execute automatically
```

**Requires Go 1.21+. Zero runtime dependencies.**

---

## Interfaces

All three share the same `./storage/` state — switch freely.

| Interface | Best for | How |
|---|---|---|
| **Web UI** | Approvals, PRs, workflow editor, chat, in-browser coding (Code tab) | `goon start --web=:8080` |
| **CLI** | One-off tasks, scripts, CI pipelines | `goon "task..."` |
| **Telegram** | Mobile approvals, PR review, natural chat | Set `TELEGRAM_BOT_TOKEN` |

---

## LLM providers

Pick any provider in the web UI Settings tab, or via CLI:

```sh
goon config set GOON_LLM_PROVIDER openai     # gpt-4o-mini default
goon config set OPENAI_API_KEY sk-...

goon config set GOON_LLM_PROVIDER anthropic  # claude-sonnet-4-5 default
goon config set ANTHROPIC_API_KEY sk-ant-...

goon config set GOON_LLM_PROVIDER gemini     # gemini-2.5-flash default
goon config set GEMINI_API_KEY ...

goon config set GOON_LLM_PROVIDER ollama     # llama3 default, no key needed
```

**Per-role model routing** — route planning, coding, and review to different models:

```sh
goon config set GOON_LLM_CODE   anthropic:claude-sonnet-4-5   # writes code
goon config set GOON_LLM_PLAN   gemini:gemini-2.5-flash       # ticket planning
goon config set GOON_LLM_CHAT   gpt-4o-mini                   # chat tab
goon config set GOON_LLM_REVIEW anthropic                     # PR review drafts
```

---

## Boards & git hosts

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

## Custom workflow

Goon ships with a built-in software-engineering pipeline, but you can replace it entirely or run multiple named workflows for completely different jobs.

Drop `workflow.json` in your project directory. Goon picks it up on the next poll — no restart.

**Engineering pipeline (default):**

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

**Daily email digest** — reads Gmail, summarizes, sends to Telegram every morning:

```jsonc
{
  "name": "email-digest",
  "auto_approve": true,
  "stages": [
    {
      "name": "fetch_emails",
      "type": "agent",
      "task": "Search Gmail for unread emails from the last 24 hours. Return a JSON list of {from, subject, summary}."
    },
    {
      "name": "summarize",
      "type": "llm",
      "prompt": "Given these emails: {{.Stages.fetch_emails.result}}\n\nWrite a concise daily digest grouped by topic. Plain text, bullet points."
    },
    {
      "name": "send",
      "type": "notify",
      "message": "📬 Daily digest:\n\n{{.Stages.summarize.result}}"
    }
  ]
}
```

**Log monitoring** — scans GCP logs for errors, opens a ticket if found:

```jsonc
{
  "name": "log-monitor",
  "auto_approve": true,
  "stages": [
    {
      "name": "scan",
      "type": "agent",
      "task": "Search GCP logs for ERROR severity in the last hour. Return JSON {has_errors, count, samples[]}."
    },
    {
      "name": "triage",
      "type": "llm",
      "json_mode": true,
      "prompt": "Logs: {{.Stages.scan.result}}\nDecide: should_alert (bool), severity (low/medium/high), summary (string).",
      "reject_if": "{{eq .Stages.triage.should_alert false}}",
      "on_reject": "end"
    },
    {
      "name": "alert",
      "type": "notify",
      "message": "🚨 {{.Stages.triage.severity}} alert: {{.Stages.triage.summary}}"
    }
  ]
}
```

**Replace the built-in pipeline entirely with custom stages:**

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

Stage types: `llm` · `agent` · `notify` · `http` · `loop`

Routing fields: `on_next` · `on_reject` · `reject_if` · `ask_stage` · `max_loops` · `on_done`

- `on_next` accepts a single stage name **or an array** for fan-out — `"on_next": ["review", "docs"]` runs both branches sequentially.
- `on_reject` can point at the stage itself for a bounded retry (capped by `max_loops`, default 3).
- `type: "loop"` is a pure routing node: loops back to `on_next` until `max_loops` iterations, then exits via `on_done`. Build review → fix cycles with a hard cap.

---

## Memory & self-learning

```
./storage/
├── memory.json          runtime state (tickets, queues, daemon status)
└── memory/
    ├── SOUL.md           always loaded into the system prompt — put your conventions here
    ├── HISTORY.md        one-line log of every completed task
    ├── REPOSITORY.md     remote → local repo registry
    ├── LEARNED.md        insights distilled from idle-time reflection
    └── *.md              topic notes the agent reads and writes on demand
```

```sh
goon memory init           # create memory/ and seed SOUL.md
goon memory edit SOUL.md   # add your codebase conventions, rules, context
goon memory read HISTORY.md
```

While the daemon idles between tickets, it reads recent commits, logs, and notes — then distils insights into `LEARNED.md`. It asks you directly when it's unsure. Your answers become permanent memory.

Opt out: `GOON_AUTO_LEARN=0`. Tune the interval: `GOON_LEARN_INTERVAL_HOURS` (default 24).

See **[docs/storage.md](docs/storage.md)** for a full breakdown of every file.

---

## Telegram

```sh
TELEGRAM_BOT_TOKEN=...        # from @BotFather
GOON_TELEGRAM_SECRET=...      # any passphrase — used to authenticate chats
TELEGRAM_CHAT_ID=...          # optional default chat for outbound notifications
GOON_AUTO_REVIEW=1            # auto-draft PR reviews for PRs awaiting you
```

Authenticate once: `/auth <secret>`. Then use `/status`, `/queue`, `/answer <id> yes`, `/prs`, `/review owner/repo 42`, `/run <task>`, or plain chat.

---

## Google Workspace

Connect once and ask goon in the **Chat** tab:

- *"what meetings do I have today?"* → **Calendar**
- *"what are my tasks?"* → **Tasks** (+ Jira)
- *"check my email from finance last week"* → **Gmail**
- *"get the trace for the login of user harisa"* → **GCP Cloud Logging**

```sh
goon config set GOOGLE_OAUTH_CLIENT_ID <id>
goon config set GOOGLE_OAUTH_CLIENT_SECRET <secret>
goon config set GOOGLE_CLOUD_PROJECT <project-id>   # only for log search
goon google auth                                    # one-time browser consent
```

Read-only — goon can look but never send, change, or delete. Zero-dependency: hand-rolled OAuth2 + Calendar/Tasks/Gmail/Logging using only the Go stdlib.

👉 **[Step-by-step setup guide (≈10 min)](docs/google-workspace.md)**

---

## Parallel agents

Fan out tasks across isolated child processes:

```json
{ "tool": "spawn_agents", "args": { "tasks": "refactor auth\nwrite tests for auth", "wait": "true" } }
```

Each child gets its own `GOON_STORAGE_DIR`. Cap concurrency with `GOON_MAX_AGENTS` (default 4).

---

## How goon compares

| | **goon** | OpenHands | Devin | LangChain/LangGraph | Cursor |
|---|---|---|---|---|---|
| **Deployment** | Binary, self-hosted | Docker / cloud | SaaS only | Python library | Desktop app |
| **Zero dependencies** | ✅ stdlib only | ❌ Python + npm | ❌ SaaS | ❌ Python ecosystem | ❌ Electron |
| **Board integration** | ✅ Jira, GitHub Issues | ❌ manual | ✅ Jira (paid) | ❌ | ❌ |
| **Approval gates** | ✅ web, Telegram, CLI | partial | ✅ | manual | ❌ |
| **Opens PRs** | ✅ GitHub, GitLab, BB | ✅ GitHub | ✅ | ❌ | ❌ |
| **Self-learning** | ✅ HISTORY + reflect | ❌ | limited | ❌ | ❌ |
| **Custom workflows** | ✅ workflow.json | ❌ | ❌ | ✅ (code) | ❌ |
| **Multi-LLM routing** | ✅ per-role | ✅ | ❌ | ✅ | ❌ |
| **Local / offline** | ✅ Ollama | partial | ❌ | ✅ | partial |
| **Telegram interface** | ✅ | ❌ | ❌ | ❌ | ❌ |
| **Pricing** | free / open source | open source | $500+/mo | open source | $20/mo |

**The key difference:** goon is a daemon that runs *continuously* alongside your team, polls your board, and closes tickets end-to-end with your approval at each gate. It persists memory across runs, learns from your codebase, and routes each job to the right model automatically — not a one-shot assistant you invoke manually.

---

## Commands

```sh
goon "<task>" [--run|--auto|--explain]   # one-shot agent
goon start [--web=:8080]                 # autonomous daemon
goon stop | pause | resume
goon status                              # snapshot of current state
goon doctor                              # live-probe every provider
goon train                               # answer queued learning questions
goon workflow init|show|edit             # manage pipeline
goon memory list|read|write|search       # manage knowledge notes
goon repo show|add|scan                  # manage repo registry
goon review-prs [--telegram]             # AI-draft PR reviews
goon logs [--follow]
goon config show|set|get                 # read/write ./config.json
goon update [ref]
```

| Mode flag | Behavior |
|---|---|
| *(none)* | Dry-run — plans but never executes |
| `--run` | Executes, asks `y/N` before each mutating step |
| `--auto` | Executes every validated step automatically |
| `--explain` | Plan only, no tool calls |

---

## Build & test

```sh
make build    # ./goon
make check    # vet + go test -race ./...
make install  # ~/.local/bin/goon
goon doctor   # verify all providers are reachable
```

---

## Extending

- **New tool** — implement `tools.Tool`, register in `DefaultRegistry`.
- **New board** — implement `boards.Board` in `internal/boards/`, route in `NewFromEnv`.
- **New LLM** — implement `llm.Provider` in `internal/llm/`, route in `NewFromEnv`.

---

## Contributing

Issues and PRs welcome. Run `make check` before submitting. See [CLAUDE.md](CLAUDE.md) for architecture notes and coding conventions.

---

<p align="center">MIT · Built with Go · Zero runtime dependencies</p>
