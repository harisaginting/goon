# goon — Go ON

**Autonomous AI engineer in a single Go binary.** Polls your ticket board
(Jira / GitHub Issues), plans the work, asks you to approve, codes it,
tests, verifies, updates its own memory, and opens a PR. Driveable from
the terminal, the web UI, or Telegram.

- **Single binary, zero dependencies.** Go stdlib only — no PyTorch, no
  Node, no daemon-of-daemons. Drop the binary on any machine, run.
- **Pluggable everything.** LLM provider (OpenAI, Anthropic, Ollama,
  mock), board (Jira, GitHub Issues), git host (GitHub, GitLab,
  Bitbucket), notification (Telegram).
- **Safe by default.** Strict-JSON tool contract, regex blocklist on
  every shell command, approval gates before code lands.
- **Persistent memory.** Markdown notes the agent reads and writes
  across runs, so the next ticket starts smarter than the last.

```
                 ┌─────────────── you ───────────────┐
                 │  approve / decline / review / chat │
                 └─────────┬──────────────────┬───────┘
                       CLI │ web UI           │ Telegram bot
                           ▼                  ▼
   board ──► triage ──► confirm_repo ──► approve_plan ──► execute ──► test ──►
   (Jira/GH)  (LLM)        (gate)            (gate)        (agent)   (make/go)
                                                                       │
              ◄── notify ◄── PR ◄── update_memory ◄── verify × N ◄─────┘
                  (Telegram)  (GitHub/GitLab/Bitbucket)  (LLM)

  Reject the plan with feedback ─► daemon re-triages with your feedback (capped
  at 3 rejections) so you don't have to start over.
```

---

## Pick your setup

| You want… | What to read |
|---|---|
| **Try goon in 60 seconds** with no API keys | [Run offline with the mock provider](#run-offline-no-keys) |
| **Use goon as a CLI assistant** for one-off tasks | [One-shot agent quick start](#one-shot-agent-quick-start) |
| **Run the autonomous daemon** that picks up tickets and files PRs | [Daemon quick start](#daemon-quick-start) |
| **Drive goon from Telegram** (review PRs, approve plans, chat) | [Telegram bot setup](#the-telegram-bot) |

All paths share the same setup steps. Pick the highest one that fits.

---

## Quick start

**Prereqs:** Go 1.21+, that's it.

**Supported platforms.** Linux, macOS, and Windows. CLI one-shot mode and
daemon mode both work on all three. The same memory.json multi-process
lock-file caveat applies everywhere — concurrent goon processes against
the same `./storage/` directory serialize through a sibling lockfile, and
on filesystems that don't support atomic file creation reliably (some
network mounts) you may see a "lockfile held by another process" warning.
Workflow hooks and `run_command` invoke the host shell (`sh -c` on POSIX,
`cmd /C` on Windows), so portable hooks should stick to commands both
shells understand or branch on platform inside the hook itself.

### 1. Build

```sh
git clone https://github.com/harisaginting/goon
cd goon
make build                       # → ./goon
cp .env.example .env             # then edit .env with your keys
```

`.env.example` is tiered — fill in only the sections you need. Required
vs optional is called out at the top of every section.

### 2. Verify your config

```sh
./goon doctor                    # live-probes every configured provider
```

You should see green checks for the LLM provider and any board/host
you set up. Anything red is missing creds — fix in `.env` and re-run.

---

### Run offline (no keys)

Just to see the agent loop work end-to-end:

```sh
GOON_LLM_PROVIDER=mock \
GOON_MOCK_REPLIES='{"tool":"finish","args":{"message":"hello from mock"}}' \
./goon "say hi"
```

The mock provider returns canned replies, so the agent loop runs without
hitting any network.

### One-shot agent quick start

Minimum `.env`:

```sh
GOON_LLM_PROVIDER=openai
OPENAI_API_KEY=sk-...
```

Then:

```sh
./goon "list every .go file under internal/" --explain   # plan only
./goon "tidy go.mod" --auto                              # run, no prompts
./goon "find all .log files under /tmp and delete them" --run   # ask y/N each step
```

See [Modes](#modes) for the difference between `--explain`, `--run`,
and `--auto`. By default goon does a **dry run** — prints what it would
do but never executes.

### Daemon quick start

Add a board and a git host to your `.env`:

```sh
# Atlassian (Jira board + Confluence wiki) — three lines cover both products
ATLASSIAN_BASE_URL=https://acme.atlassian.net
ATLASSIAN_EMAIL=me@acme.com
ATLASSIAN_API_TOKEN=...

# Tell goon to use them
GOON_BOARD=jira
GOON_GIT_HOST=bitbucket          # or github / gitlab

# Bitbucket has separate auth — generate at bitbucket.org/account/settings/app-passwords
BITBUCKET_USERNAME=me@acme.com
BITBUCKET_APP_PASSWORD=...
```

Then:

```sh
./goon start --web=:8080         # daemon + web UI
# open http://localhost:8080 to watch tickets stream through
```

The daemon now polls Jira every 5 minutes, picks the most-recently-updated
open ticket assigned to you, and runs it through the
[approval-gated pipeline](#how-a-ticket-flows). When a gate fires you'll
see it in the web UI under "pending questions" — answer there, in
Telegram, or with `goon train`.

---

## How a ticket flows

When the daemon picks up a ticket it runs this resumable pipeline. The
two **gates** pause the workflow until you reply (via `goon train`, the
web UI, or Telegram); the daemon then resumes from that exact stage on
the next tick.

```
1. Pull ticket          board.List → most-recently-updated open ticket
2. Triage               LLM produces an ordered plan + suggested repo
3. Confirm repo  ◀─gate "Confirm repo for ENG-123? (yes / change=<path>)"
4. Approve plan  ◀─gate "Approve work + test plan?  (yes / no)"
5. Execute              agent runs each plan step, safety-validated
6. Test                 best-effort `make test` (or repo-defined)
7. Verify × N           LLM re-checks the work N times
8. Update memory        agent distils learnings into PINNED.md / notes
9. Open PR              GitHub / GitLab / Bitbucket
10. Notify              Telegram + board comment + status transition
```

Want it fully unattended? Set `auto_approve: true` in `workflow.json`
or `GOON_AUTO_APPROVE=1` and the gates pass automatically.

---

## Install (run `goon` from anywhere)

```sh
make install               # → ~/.local/bin/goon (no sudo)
make install-system        # → /usr/local/bin/goon (needs sudo)
make install-go            # → $(go env GOPATH)/bin/goon
```

If `~/.local/bin` isn't on your `PATH`, the install command prints the
exact line to add. Or run it directly without installing:

```sh
go run github.com/harisaginting/goon@latest "summarize the .go files in internal/" --explain
```

To remove:

```sh
goon uninstall              # confirms first, leaves data
goon uninstall --yes --purge   # also wipe ~/.config/goon and ~/.goon
```

---

## Configure

All knobs are environment variables (or `.env`). The config file lives at
`~/.config/goon/.env`. Manage it with the `config` subcommand:

```sh
goon config              # show all (secrets masked)
goon config set KEY VAL  # KEY=VAL also works
goon config get KEY
goon config edit         # open in $EDITOR
goon config path         # print the config file path
```

Shell-exported values always win over the file. `goon doctor` live-probes
every configured provider so you can verify auth before kicking off the
daemon.

### Required for autonomous mode

| variable | purpose |
|---|---|
| `GOON_LLM_PROVIDER` | `openai` \| `anthropic` \| `ollama` \| `mock` |
| `GOON_BOARD`        | `jira` \| `github` |
| `GOON_GIT_HOST`     | `github` \| `gitlab` \| `bitbucket` *(optional — skip PR creation if unset)* |

### LLM providers

```sh
# OpenAI
OPENAI_API_KEY=sk-...
OPENAI_MODEL=gpt-4o-mini                # default

# Anthropic
ANTHROPIC_API_KEY=sk-ant-...
ANTHROPIC_MODEL=claude-sonnet-4-5       # default

# Ollama (local)
OLLAMA_BASE_URL=http://localhost:11434  # default
OLLAMA_MODEL=llama3                     # default — change to qwen2.5-coder:7b for code work
```

### Atlassian (Jira + Confluence share creds)

```sh
ATLASSIAN_BASE_URL=https://acme.atlassian.net
ATLASSIAN_EMAIL=me@acme.com
ATLASSIAN_API_TOKEN=...   # id.atlassian.com/manage-profile/security/api-tokens
```

`JIRA_*` / `CONFLUENCE_*` per-product overrides win when set — useful
for self-hosted Data Center where Jira and Confluence live on different
hosts.

### GitHub Issues + PRs

```sh
GITHUB_TOKEN=ghp_...        # repo + issues + pull_requests scopes
GITHUB_REPOS=owner/a,owner/b   # required when GOON_BOARD=github
GITHUB_LABEL=                  # filter to issues with this label (optional)
GITHUB_ASSIGNEE=@me            # default @me
GITHUB_API_URL=                # default https://api.github.com (set for GHES)
```

### Telegram bot

```sh
TELEGRAM_BOT_TOKEN=123:abc        # from @BotFather
GOON_TELEGRAM_SECRET=long-phrase  # required for inbound bot (see below)
TELEGRAM_CHAT_ID=987654321        # optional default chat for outbound notify
GOON_REVIEW_REPOS=owner/a,owner/b # default repos for /prs without args
```

### Storage knobs (project-local by default)

```sh
GOON_STORAGE_DIR=./storage   # everything below derives from this
GOON_LOG_FILE=               # default: $STORAGE/logs/goon.log
GOON_MEMORY_PATH=            # default: $STORAGE/memory.json
GOON_MEMORY_DIR=             # default: $STORAGE/memory
GOON_PID_FILE=               # default: $STORAGE/goon.pid
GOON_WORKFLOW_FILE=          # default: ./workflow.json
```

`cd ~/myproject && goon start` gets its own `myproject/storage/` —
override `GOON_STORAGE_DIR` to share across repos.

### Pipeline knobs

```sh
GOON_AUTO_APPROVE=1     # skip the confirm_repo + approve_plan gates
GOON_VERIFY_RUNS=3      # extra verify passes after execute (1..10)
GOON_MAX_STEPS=5        # agent loop bound (1..50)
GOON_POLL_SECONDS=300   # daemon poll interval
GOON_REPO_MAP="ENG=/repos/eng,WEB=/repos/web,*=/repos/default"
```

---

## The Telegram bot

A bidirectional bot — sends notifications **and** lets you drive goon
from your phone with `/status`, `/run`, `/prs`, `/review`, plus free-text
chat with the model.

### Setup (3 steps)

**1. Create a bot.** Open Telegram, search `@BotFather`, send `/newbot`,
follow the prompts. Copy the **HTTP API token** it gives you.

**2. Add to `.env`:**

```sh
TELEGRAM_BOT_TOKEN=123456:abcdefg-from-botfather
GOON_TELEGRAM_SECRET=any-long-random-phrase-you-pick

# Optional: default repos when you type /prs with no arguments
GOON_REVIEW_REPOS=harisaginting/goon,you/other-repo

# Optional: default chat for outbound notifications (workflow done, etc.)
TELEGRAM_CHAT_ID=987654321
```

**3. Restart goon:**

```sh
goon stop && goon start
# you'll see: → telegram bot ready: @your_bot_name
#             → telegram bot: 16 commands registered in menu
```

### First DM

Open a chat with your bot in Telegram. Tap the ☰ menu next to the input
bar — you'll see all commands. Then authenticate once:

```
/auth any-long-random-phrase-you-pick
```

The bot constant-time compares against `GOON_TELEGRAM_SECRET`. After
that, your chat ID is trusted until you `/logout` or wipe
`storage/memory.json`.

**Commands.**

```
monitor:    /status               daemon snapshot
            /logs [n]             last n log lines (default 30)
            /workflows [n]        recent workflow runs
            /memory list          notes index
            /memory read <name>
            /memory search <q>
            /queue                pending questions

approvals:  /answer <id> <text>   answers the gate question

PR review:  /prs [repo]           list open PRs
            /review <repo> <num>  ask the model to review the diff
            /approve <repo> <num> [body]
            /decline <repo> <num> <reason>
            /comment <repo> <num> <body>

agent:      /run <task>           one-shot agent task (like CLI)
            (any plain text)      chat with the model — 6-turn rolling history

session:    /whoami /logout /help

passthrough: /<any-other> → goon CLI subcommand
            (start / stop / uninstall / update are blocked)
```

**Typical session over Telegram:**

```
you →  /auth my-shared-secret
bot →  ✓ authenticated
you →  /queue
bot →  1 pending question: [q-3] Approve work + test plan for ENG-42?
you →  /answer q-3 yes
bot →  ✓ answered q-3
       (next poll, the daemon resumes ENG-42 from approve_plan)
you →  /prs
bot →  3 open PRs: …
you →  /review owner/repo 17
bot →  Review of #17 — Refactor auth …
       SUMMARY: …
       RISKS:   …
       NITS:    …
       RECOMMENDATION: request_changes
you →  /decline owner/repo 17 needs error-handling on token refresh
bot →  ✓ requested changes on owner/repo#17
```

---

## Daily commands

```sh
goon "<task>" [--run|--auto|--explain]   # one-shot agent run
goon start [--web=:8080] [--once]        # autonomous daemon
goon stop                                # stop the running daemon
goon status                              # daemon + queue snapshot
goon doctor [--json] [--quiet]           # live-probe every provider
goon train                               # answer questions queued by the agent
goon train answer <id> <answer>          # non-interactive
goon workflow init|show|path|edit|hooks  # customize the pipeline
goon memory init|list|read|write|append|search|edit|delete|path  # active markdown notes
goon repo list|forget <project>|clear    # learned project→repo mappings
goon pause | resume                      # toggle the daemon's poll loop
goon version                             # build info (commit, date, go version)
goon logs [--tail|--follow|--clear]      # browse the structured log
goon config show|get|set|unset|path|edit # ~/.config/goon/.env
goon update [<ref>]                      # rebuild from upstream
goon uninstall [--yes] [--purge]
```

### Modes

| flag | behavior |
|---|---|
| (none) | **dry-run** — print the planned action, never execute |
| `--run` | execute, ask `y/N` before each mutating step |
| `--auto` | execute every validated step automatically |
| `--explain` | plan only — produce a step-by-step explanation, no tool calls |
| `--debug` | extra diagnostic output |

---

## Customize the workflow

Drop a `workflow.json` in your repo root and goon picks it up on the
next poll — no rebuild, no restart.

```sh
goon workflow init    # writes a starter ./workflow.json
goon workflow show    # prints the resolved config
goon workflow edit    # open in $EDITOR
goon workflow hooks   # list every hook name + env vars
```

Lookup order (first match wins):

1. `$GOON_WORKFLOW_FILE`
2. `./workflow.json` *(repo root — recommended)*
3. `<repo>/workflow.json`, `<repo>/.goon/workflow.json` *(legacy)*
4. `~/.config/goon/workflow.json`

### Minimal example

```jsonc
{
  "version": 1,
  "name": "engineering-prod",
  "description": "PR-opening pipeline for the prod monorepo",
  "branch_prefix": "feature/",
  "test_command": "make ci",
  "verify_runs": 5,
  "auto_approve": false,                  // false = the two gates fire (default)
  "pr_title_template": "FIX({{.Key}}): {{.Title}}",
  "pr_body_template":  "Resolves {{.Key}}\n\nBranch: {{.Branch}}",
  "extra_labels":      ["customer-x"],
  "hooks": {
    "before_execute": ["echo 'goon picked up {{.Key}}'"],
    "before_test":    ["make build"],
    "before_pr":      ["go fmt ./...", "goimports -w ."],
    "after_pr":       ["echo 'PR up at $TICKET_URL'"],
    "on_failure":     ["echo \"goon failed on $TICKET_KEY\" | mail -s goon you@x"]
  }
}
```

### Hook commands

Every hook value is a list of shell commands run sequentially through
`sh -c` in the resolved repo directory, piped through goon's safety
validator. Each command receives `$TICKET_KEY`, `$TICKET_TITLE`,
`$TICKET_URL`, `$TICKET_SOURCE`, `$TICKET_PROJECT`, `$REPO`, `$BRANCH`
as env vars, plus full Go template substitution inside the command
itself (`{{.Key}}`, `{{.Branch}}`, etc.).

A failed hook fails the workflow phase. `on_failure` is best-effort and
never blocks anything.

### Replace the pipeline (stages)

Hooks run *around* goon's built-in pipeline. When you need a
fundamentally different shape — a marketing-brief workflow, a sales-lead
qualifier — declare a `stages` array. **`stages` replaces the built-in
pipeline wholesale** but PR/notify still fire at the end if configured.

```jsonc
{
  "version": 2,
  "stages": [
    { "name": "triage",  "type": "llm", "json_mode": true,
      "prompt": "Break {{.Key}} into 3-7 atomic steps. Reply JSON {\"steps\":[{\"title\":\"...\"}]}." },
    { "name": "execute", "type": "agent",
      "task": "Implement: {{(index (index .Stages.triage.steps 0) \"title\")}}" },
    { "name": "verify",  "type": "agent", "repeat": 3,
      "task": "Verify ticket {{.Key}} is done. List defects via finish." }
  ]
}
```

Stage fields: `name`, `type` (`llm` \| `agent`), `if`, `repeat`,
`on_error`, plus type-specific fields. See
[`examples/workflows/`](examples/workflows/) for marketing/sales/ops
presets.

---

## Memory

Two layers — **passive** runtime state (a JSON file) and **active**
persistent knowledge (markdown notes the agent reads + writes).

```
./storage/
├── memory.json     passive: tickets, workflows, queue, daemon status
├── memory/         active: PINNED.md + topic notes the agent maintains
│   ├── PINNED.md           ← always loaded into the system prompt
│   └── learnings/...md     ← topic notes the agent fetches as needed
├── logs/goon.log
└── goon.pid        present while the daemon runs
```

You don't edit `memory.json` by hand — goon manages it. The notes dir is
plain markdown; you can edit, version-control, or copy across repos.

```sh
goon memory init                        # creates memory/ + seeds PINNED.md
goon memory list                        # * = auto-loaded
goon memory edit PINNED.md
goon memory read learnings/regex.md
goon memory search "auth"               # case-insensitive grep
```

The agent gets five tools — `memory_list`, `memory_read`,
`memory_write`, `memory_append`, `memory_search` — and is nudged in the
system prompt to write down what it learned after each task. The
**update_memory** workflow phase makes this an explicit step before
every PR opens, so steady-state knowledge keeps growing.

**PINNED.md** is the always-loaded note. Keep it short and high-signal:
codebase conventions, names of services/people, "don't do this" rules,
pointers to other notes worth reading. Park bulky context in topic
notes (`learnings/oauth-flow.md`, `repos/webapp.md`) and let the agent
fetch them on demand.

---

## Tools, providers, contract

### Tools shipped

| tool | purpose |
|---|---|
| `run_command`   | shell command (safety-validated) |
| `read_file`     | up to 64KB |
| `list_dir`      | up to 100 entries |
| `confluence`    | search/get pages (Atlassian Cloud) |
| `telegram`      | send a message via Bot API |
| `ask_user`      | queue a question for the user (daemon mode) |
| `memory_*`      | five tools to read/write the markdown notes store |
| `finish`        | end the loop with a summary |

### LLM providers

| provider | switch | default model |
|---|---|---|
| OpenAI    | `GOON_LLM_PROVIDER=openai`    | `gpt-4o-mini` |
| Anthropic | `GOON_LLM_PROVIDER=anthropic` | `claude-sonnet-4-5` |
| Ollama    | `GOON_LLM_PROVIDER=ollama`    | `llama3` |
| Mock      | `GOON_LLM_PROVIDER=mock`      | offline fixtures |

### The model contract

The LLM **must** emit exactly one JSON object. Parse errors get fed back
so the model self-corrects.

```json
{
  "tool": "run_command",
  "args": { "command": "ls -la internal" },
  "rationale": "list packages before reading"
}
```

### Safety

`internal/safety` blocks (regex) the most dangerous patterns regardless
of mode: `rm -rf /`, `rm -rf ~`, `mkfs.*`, `dd of=/dev/*`, fork bombs,
`shutdown`/`reboot`/`halt`, `chmod -R 777 /`, `curl … | sh`. Hooks go
through the same validator. In `--run` mode the executor also asks
`y/N` before any mutating tool.

---

## Logs

Default location: `./storage/logs/goon.log`. Rotates at 10 MB; keeps 3
rotations. Mirrored to stderr in real time when running in the
foreground.

```sh
goon logs                # last 100 lines, then exit
goon logs --tail=500
goon logs --follow       # tail -f equivalent
goon logs --clear        # truncate (keeps rotations)
goon logs --path

GOON_LOG_FORMAT=json goon start | jq 'select(.level=="ERROR")'
```

| event | level | example attrs |
|---|---|---|
| LLM prompt / response   | debug | provider, message count, raw |
| Tool call / execution   | info  | tool, args, ok, latency_ms |
| HTTP request            | info  | component, method, url, status, latency_ms |
| Workflow start / end    | info  | wf, ticket, state, stage, pr_url |
| Daemon poll             | info  | poll_start, poll_end, duration_ms |
| Telegram bot events     | info  | chat, user, command |

Tokens are auto-redacted: `bot$TOKEN/...` becomes `bot***/...`,
`user:pass@host` becomes `***@host`.

---

## Self-update

Once `goon` is on your `PATH` it can rebuild itself. **Requires `git` and
`go` on `PATH`** — self-update clones the upstream repo and rebuilds, so
both tools must be installed. (If you originally got the binary from a
release archive without Go installed, use `goon uninstall` then
re-download the new release instead.)

```sh
goon update                         # latest commit on master
goon update v0.2.0                  # tag
goon update feature/new-tool        # branch
goon update 3288a2c02b              # specific commit
```

Clones [github.com/harisaginting/goon](https://github.com/harisaginting/goon)
to a temp dir, checks out the requested ref, runs `go build`, and
atomically replaces the running binary. Override the upstream:

```sh
GOON_UPSTREAM=https://github.com/yourfork/goon goon update
```

---

## Architecture (brief)

```
goon/
├── main.go
├── cmd/                       CLI entry points + subcommand handlers
├── internal/
│   ├── agent/                 multi-step LLM↔tool loop, system prompt
│   ├── atlassian/             shared env helper for Jira + Confluence
│   ├── boards/                ticket sources (jira, github)
│   ├── checkup/               `goon doctor` provider probes
│   ├── daemon/                poll loop + resume detection
│   ├── executor/              {dry-run | run | auto | explain} modes
│   ├── githost/               PR adapters (github, gitlab, bitbucket) + PRReviewer
│   ├── llm/                   provider adapters (openai, anthropic, ollama, mock)
│   ├── logx/                  slog wrapper, log rotation, HTTP transport
│   ├── memory/                passive runtime store (memory.json)
│   ├── notes/                 active markdown notes (./storage/memory/)
│   ├── safety/                command validator
│   ├── storage/               single source of truth for state paths
│   ├── telegram/              inbound bot — auth, /commands, chat, PR review
│   ├── tools/                 Tool interface + builtins + memory_* tools
│   ├── web/                   optional web UI (htmx, single embedded page)
│   └── workflow/              Engine.Run state machine + declarative stages
└── examples/
```

The workflow engine is a **resumable state machine**. When a gate fires,
it queues a question, sets `wf.State=WFAwaitingApproval` + the current
stage, and exits. The daemon's `Memory.ResumableWorkflow()` catches that
on the next tick once the user answers, and `Engine.Run` re-enters at
the same stage.

---

## Testing

The full suite uses a deterministic mock LLM and `httptest` servers so
it runs offline:

```sh
make check           # vet + go test -race ./...
go test ./...        # plain
```

Coverage includes: tool-call parser (plain JSON, fenced JSON, prose,
nested braces), safety blocklist, executor mode behavior (dry-run,
y/N, auto, --explain), agent loop (finish, multi-step, JSON
self-repair, max-steps), memory (append, frequency, persistence,
disabled mode), workflow gates + resume, daemon resume path, Telegram
bot dispatch (auth, /status, /queue/answer, /prs, /approve, refusal),
HTTP shapes for Telegram + Confluence + GitHub.

---

## Run on the fly (no install)

If you have Go 1.21+ you can skip the build step entirely:

```sh
go run . "list every .go file under internal"          # one-shot
go run . start --web=:8080                             # daemon
go run . workflow init                                 # any subcommand
```

> Don't prefix with `goon` when using `go run .` — `go run .` IS goon,
> so `go run . goon workflow init` would be "ask the agent to do
> workflow init". Goon catches this and strips the redundant prefix
> with a hint, but the cleaner form is just `go run . workflow init`.

There are Makefile shortcuts:

```sh
make run         TASK='list every .go file under internal'   # dry-run
make run-auto    TASK='tidy go.mod'
make run-explain TASK='delete every .log older than 30 days'
```

You can also run from anywhere on disk without cloning:

```sh
go run github.com/harisaginting/goon@latest "summarize the .go files" --explain
go run github.com/harisaginting/goon@v0.2.0  "tidy go.mod" --auto
```

> Add a shell alias for shorthand:
> `alias goon='go run github.com/harisaginting/goon@latest'`

---

## Failure modes the agent guards against

- **Executing raw LLM text** — every step must parse as a `ToolCall`.
- **Skipping JSON parsing** — bad JSON is fed back as a parse-error message.
- **No validation** — `run_command` always passes through `safety`.
- **Infinite loop** — `MaxSteps` (default 5, max 50).
- **Unattended runaway** — every code-touching workflow pauses at
  `approve_plan` until you say yes (unless you opted into `auto_approve`).

## Extending

Add a new tool by implementing `tools.Tool` and registering in
`DefaultRegistry`. The model picks it up automatically through the
manifest line in the system prompt.

Add a new board / git-host / LLM by implementing the matching interface
in `internal/boards`, `internal/githost`, or `internal/llm` and routing
in the corresponding `NewFromEnv`.

## License

MIT.
