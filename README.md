# goon — Go ON

An autonomous AI engineer in a single Go binary. Polls your ticket board
(Jira or GitHub Issues), triages each ticket, plans, executes, verifies its
own work N times, opens a PR (GitHub, GitLab, or Bitbucket), and pings you
on Telegram.
Same memory backs the CLI and the htmx-driven web UI, so everything you can
see in one is reflected in the other.

Pluggable LLM providers (OpenAI, Anthropic, Ollama for local models, mock for
tests). Strict-JSON tool-call contract. Layered safety checks. Persistent
memory at `~/.goon/memory.json`.

## The autonomous mode

```sh
goon config set GOON_LLM_PROVIDER ollama
goon config set GOON_BOARD jira
goon config set GOON_GIT_HOST github
goon config set ATLASSIAN_BASE_URL https://acme.atlassian.net   # covers Jira + Confluence
# (set the rest via 'goon config' or ~/.config/goon/.env)

goon start --web=:8080            # starts the daemon + web UI
# leave it running. open http://localhost:8080 in a browser.
```

What happens, every `GOON_POLL_SECONDS` (default 5 minutes):

```
Board   ──► Triage ──► Plan ──► Execute ──► Test ──► Verify×N ──► PR ──► Notify
(Jira/GH)   (LLM)     (LLM)    (agent)    (make)    (LLM)       (GH/GL) (Telegram)
                                  │
                                  └─► may call ask_user → queue question
                                       (workflow blocks; you answer via
                                        `goon train` or the web UI; daemon
                                        resumes on the next poll)
```

Everything (status, tickets, workflows, plan progress, pending questions,
PR links, history) lives in one JSON file at `~/.goon/memory.json` so the
CLI (`goon status`, `goon train`) and the web UI agree on every byte.

## Subcommands

```
goon "<task>" [--run|--auto|--explain]   # one-shot agent run
goon start [--web=:8080] [--once]        # autonomous daemon
goon stop                                # stop the running daemon
goon status                              # daemon + queue snapshot
goon train [--list|--all]                # answer questions queued by the agent
goon train answer <id> <answer>          # non-interactive form
goon doctor [--json] [--quiet]           # live probe every configured provider
goon workflow <action>                   # customize the per-ticket workflow (hooks, templates, …)
goon logs [--tail|--follow|--clear|--path] # browse the structured log file
goon config <action> [args]              # show/get/set/unset/path/edit ~/.config/goon/.env
goon update [<ref>]                      # rebuild from upstream
goon uninstall [--yes] [--purge]         # remove the binary and (optional) state
```

A Go CLI that turns natural-language tasks into structured tool calls and
executes them safely. Strict JSON contract from the model, layered safety
checks, multi-step agent loop, persistent memory, and pluggable LLM
providers (OpenAI, Anthropic, Ollama for local models, mock for tests).

## Quick start

```sh
git clone https://github.com/harisaginting/goon
cd goon
cp .env.example .env       # then edit: set OPENAI_API_KEY (or ANTHROPIC_API_KEY)
make build                 # produces ./goon
./goon "summarize the .go files in internal/" --explain
```

**Want to try it without any keys?** Use the mock provider for an offline
smoke test (it returns a canned reply, so the agent loop runs end-to-end):

```sh
GOON_LLM_PROVIDER=mock \
GOON_MOCK_REPLIES='{"tool":"finish","args":{"message":"hello from mock"}}' \
go run . "say hi"
```

## Run on the fly (no install)

`goon` has zero external dependencies, so if you have Go 1.21+ you can
just run it from a checkout — no `make build`, no copying anything onto
`PATH`, no `go mod download`. Same flags, same subcommands:

```sh
# from inside the repo
go run . "list every .go file under internal"     # one-shot agent run
go run . "tidy go.mod" --auto                     # ditto, no prompt
go run . start --web=:8080                        # autonomous daemon
go run . doctor                                   # health probe
go run . workflow init                            # any subcommand
go run . logs --follow                            # tail the log
```

`go run main.go "..."` works too — it's the same thing because `main.go`
is the only file in the root package.

> **Don't** prefix with `goon` when using `go run .` — `go run .` IS already
> goon, so `go run . goon workflow init` would mean "ask the agent to do
> workflow init" rather than running the subcommand. (Goon catches this
> common mistake and strips the redundant prefix with a hint, but the
> cleaner form is just `go run . workflow init`.)

There are Makefile shortcuts so you don't have to remember the flags:

```sh
make run         TASK='list every .go file under internal'   # dry-run
make run-auto    TASK='tidy go.mod'                           # validated, no prompt
make run-explain TASK='delete every .log older than 30 days'  # plan only
```

You can also run it from anywhere on disk without cloning, by pointing
`go run` at the upstream module. Go fetches, compiles, and executes in
one step:

```sh
go run github.com/harisaginting/goon@latest "summarize the .go files in internal/" --explain
go run github.com/harisaginting/goon@v0.1.0  "tidy go.mod" --auto
go run github.com/harisaginting/goon@main    "list .log files older than 30 days"
```

The first remote run takes a few seconds while Go downloads and compiles.
Every run after that is cached, so it's near-instant. Configuration still
lives in `~/.config/goon/.env` and state in `~/.goon/` — both shared with
the installed binary.

> Want a shorter command? Add a shell alias:
> `alias goon='go run github.com/harisaginting/goon@latest'`

## Install (so you can run `goon` from anywhere)

Pick whichever fits your machine — none of them touch your Go module cache,
they just copy the built binary somewhere on `PATH`.

```sh
make install               # default: copies to ~/.local/bin/goon (no sudo)
make install-system        # /usr/local/bin/goon (needs sudo on most systems)
make install-go            # $(go env GOPATH)/bin/goon via `go install`
```

If `~/.local/bin` is not yet on your `PATH`, the install command prints the
exact line to add to your shell rc. Quickest:

```sh
echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.zshrc   # or ~/.bashrc
exec $SHELL                                                # reload
goon --version                                             # → goon 0.1.0
goon "tell my Telegram bot we shipped" --auto
```

To remove it later, either ask `goon` to remove itself:

```sh
goon uninstall              # confirms first, leaves data alone
goon uninstall --yes        # skip the confirmation
goon uninstall --yes --purge  # also wipe ~/.goon and ~/.config/goon
```

…or use the Makefile (no source repo? just delete `$(command -v goon)`):

```sh
make uninstall             # removes ~/.local/bin/goon
make uninstall-system      # removes /usr/local/bin/goon (needs sudo)
make uninstall-go          # removes $GOPATH/bin/goon
```

### `.env` lookup once installed

`goon` reads `.env` from the **current working directory**, so once it's on
your PATH you can either:

- keep your `.env` in the project you're working in, or
- export the variables in your shell rc (`OPENAI_API_KEY`, `TELEGRAM_BOT_TOKEN`, …),
- or move the file to `~/.config/goon/.env` and `cd` there before running.

## Customizing the workflow

Every phase of the autonomous pipeline is hookable. Drop a `workflow.json`
into one of the standard locations and goon will pick it up — no rebuild,
no restart needed (the daemon's poll loop reloads on every tick).

```sh
goon workflow init       # writes a starter ~/.config/goon/workflow.json
goon workflow show       # prints the resolved config (defaults + overrides)
goon workflow path       # prints where it's looking
goon workflow edit       # opens it in $EDITOR
goon workflow hooks      # lists every supported hook name + env vars
```

Resolution order (first match wins):

1. `$GOON_WORKFLOW_FILE`
2. `<repo>/.goon/workflow.json` (per-repo override)
3. `./.goon/workflow.json` (current dir)
4. `$XDG_CONFIG_HOME/goon/workflow.json`
5. `~/.config/goon/workflow.json`
6. `~/.goon/workflow.json`

### `workflow.json` shape

```jsonc
{
  "version": 1,
  "branch_prefix": "feature/",
  "test_command": "make ci",
  "verify_runs": 5,
  "pr_title_template": "FIX({{.Key}}): {{.Title}}",
  "pr_body_template":  "Resolves {{.Key}}\n\nBranch: {{.Branch}}",
  "extra_labels":      ["customer-x", "needs-review"],
  "hooks": {
    "before_triage":  [],
    "before_execute": ["echo 'goon picked up {{.Key}} — {{.Title}}'"],
    "after_execute":  [],
    "before_test":    ["make build"],
    "after_test":     [],
    "before_verify":  [],
    "after_verify":   [],
    "before_pr":      ["go fmt ./...", "goimports -w .", "git diff --stat"],
    "after_pr":       ["echo 'PR up at $TICKET_URL'"],
    "on_failure":     ["echo \"goon failed on $TICKET_KEY\" | mail -s goon you@x"]
  }
}
```

### Hook commands

Every hook value is a list of shell commands. They run sequentially through
`sh -c`, in the **repo directory** (when a repo is resolved via
`GOON_REPO_MAP`), and are piped through goon's safety validator — so an
accidental `rm -rf /` is still blocked.

Each command receives these env vars:

| variable | example |
|---|---|
| `$TICKET_KEY` | `ENG-123` |
| `$TICKET_TITLE` | `Add login` |
| `$TICKET_URL` | `https://acme.atlassian.net/browse/ENG-123` |
| `$TICKET_SOURCE` | `jira` |
| `$TICKET_PROJECT` | `ENG` |
| `$REPO` | `/home/me/repos/eng` |
| `$BRANCH` | `goon/eng-123` |

…or use Go template syntax inside the command itself:

```json
"before_execute": ["git fetch origin {{.Branch}} || true"]
```

A failed hook fails the workflow phase (the workflow is marked `failed` in
memory and surfaces in the web UI). The exception is `on_failure`, which is
best-effort and never blocks anything.

### PR templates

`pr_title_template` and `pr_body_template` are full Go `text/template`
strings. The data passed in is `{Key, Title, URL, Source, Project, Branch,
Repo, Plan}` where `.Plan` is `[]PlanStep` with `.Title` and `.Done`:

```jsonc
"pr_body_template": "Resolves {{.Key}}.\n\nPlan:\n{{range .Plan}}- {{if .Done}}✓{{else}}✗{{end}} {{.Title}}\n{{end}}\n— opened by goon 🤖"
```

If the template fails to compile or render, goon falls back to the built-in
default rather than blocking the PR.

### Built-in defaults

`goon workflow show` (with no overrides) prints the baked-in defaults:

```jsonc
{
  "version": 1,
  "branch_prefix": "goon/",
  "pr_title_template": "[{{.Key}}] {{.Title}}",
  "pr_body_template":  "Resolves {{.Key}}.\n\nPlan:\n{{range .Plan}}- {{if .Done}}✓{{else}}✗{{end}} {{.Title}}\n{{end}}\n— opened by goon 🤖",
  "hooks": { ...all 10 hook keys with empty arrays... }
}
```

### Replacing the pipeline (stages)

Hooks let you run shell commands *around* goon's built-in 7-phase pipeline
(triage → execute → test → verify → PR → notify). When you need a
genuinely different pipeline — a marketing brief workflow, a sales-lead
qualifier, an ops runbook — declare a `stages` array. **When `stages` is
present, the built-in pipeline is replaced wholesale** with the stages
you list. Hooks (before_pr, on_failure, …) and PR/notify still fire at
the equivalent boundaries.

A stage has a `name`, a `type` (currently `llm` or `agent`), and the
fields its type needs. Earlier stage outputs are available to later
stages as `{{.Stages.NAME.field}}`:

```jsonc
{
  "version": 2,
  "stages": [
    {
      "name": "triage",
      "type": "llm",
      "json_mode": true,
      "temperature": 0.1,
      "prompt": "Break ticket {{.Key}} ({{.Title}}) into 3-7 atomic steps. Reply JSON {\"steps\":[{\"title\":\"...\"}]}."
    },
    {
      "name": "execute",
      "type": "agent",
      "task": "Implement: {{(index (index .Stages.triage.steps 0) \"title\")}}"
    },
    {
      "name": "verify",
      "type": "agent",
      "repeat": 3,
      "task": "Verify ticket {{.Key}} is done. List defects via finish."
    }
  ]
}
```

**Stage fields**

| field | applies to | meaning |
|---|---|---|
| `name`        | both  | unique id; later stages reference output as `{{.Stages.NAME.…}}` |
| `type`        | both  | `llm` or `agent` |
| `if`          | both  | Go template; stage is skipped when it renders to `""`/`false`/`no`/`0` |
| `repeat`      | both  | run the stage N times (1 if omitted). Useful for verify-style passes |
| `on_error`    | both  | `fail` (default) / `continue` / `warn` |
| `prompt`      | llm   | prompt template (required) |
| `system`      | llm   | optional system message template |
| `json_mode`   | llm   | request strict JSON; output is parsed into a value usable by later stages |
| `temperature` | llm   | float, default 0 |
| `max_tokens`  | llm   | int, default provider default |
| `output`      | llm   | override the key under `.Stages` (defaults to `name`) |
| `task`        | agent | task template (required) |
| `max_steps`   | agent | reserved (use `GOON_MAX_STEPS` for now) |

**Templating data** — every template (prompt, task, if, system) sees:

```
.Key, .Title, .URL, .Source, .Project, .Repo, .Branch  (HookCtx fields)
.Stages.<name>                                          (output of earlier stages)
.Attempt                                                (1-based pass index when Repeat > 1)
```

**Example presets** (in [`examples/workflows/`](examples/workflows/)):

- [`engineering.json`](examples/workflows/engineering.json) — reproduces
  the built-in eng pipeline as explicit stages. Good starting point for
  customizing prompts.
- [`marketing-brief.json`](examples/workflows/marketing-brief.json) —
  brief → review → publish. No PR, no test command.
- [`sales-lead.json`](examples/workflows/sales-lead.json) — qualify →
  draft outreach → push to CRM, with `if` skipping cold leads.

Drop one in `~/.config/goon/workflow.json` (or set
`GOON_WORKFLOW_FILE=/path/to/your.json`) and the next ticket goon picks
up will run through it. Same hot-reload as everything else: edit the
JSON, save, the daemon picks it up on the next poll.

## Built-in subcommands

```sh
goon update [<ref>]                # rebuild from upstream master / branch / tag / commit
goon uninstall [--yes] [--purge]   # remove the binary, optionally wipe state
goon config <action> [args]        # manage ~/.config/goon/.env (see below)
```

## `goon config`

```sh
goon config                  # alias for `show` — prints all config (secrets masked)
goon config show [--reveal]  # --reveal prints secret values verbatim
goon config get <KEY>        # print a single value
goon config set <KEY> <VAL>  # write to ~/.config/goon/.env
goon config set KEY=VAL      # KEY=VAL form also accepted
goon config unset <KEY>      # remove a key from the config file
goon config path             # print path to the config file
goon config edit             # open the config file in $EDITOR
```

The config file is `$XDG_CONFIG_HOME/goon/.env` (defaults to `~/.config/goon/.env`).
Values exported in your shell always win over values in the config file — `goon config show` labels each row `[shell]`, `[config-file]`, `[default]`, or `[unset]`.

## Local models with Ollama

`goon` can run against a local [Ollama](https://ollama.com) server — no API key, no network.

```sh
ollama serve                          # start the daemon (often auto-started)
ollama pull qwen2.5-coder:7b          # or llama3, mistral, etc.

goon config set GOON_LLM_PROVIDER ollama
goon config set OLLAMA_MODEL qwen2.5-coder:7b
goon "list every .go file under internal/" --explain
```

Internally `goon` posts to `http://localhost:11434/api/chat` with `format=json` so the model is forced into the strict-JSON tool-call contract. Tune via `OLLAMA_BASE_URL` and `OLLAMA_MODEL`.

## Self-update

Once `goon` is on your `PATH`, it can rebuild itself in place:

```sh
goon update                         # latest commit on master
goon update v0.2.0                  # tag
goon update feature/new-tool        # branch
goon update 3288a2c02b              # specific commit (7–40 hex chars)
```

Under the hood it clones [github.com/harisaginting/goon](https://github.com/harisaginting/goon)
to a temp dir, checks out the requested ref, runs `go build`, and atomically
replaces the running binary. Requires `git` and `go` on `PATH`.

Override the upstream (useful for forks or air-gapped mirrors):

```sh
GOON_UPSTREAM=https://github.com/yourfork/goon goon update
```

## Modes

| flag        | behavior                                                                |
| ----------- | ----------------------------------------------------------------------- |
| (none)      | **dry-run** — print the planned action, never execute                   |
| `--run`     | execute, but ask `y/N` before each mutating step                        |
| `--auto`    | execute every validated step automatically                              |
| `--explain` | plan only — produce a step-by-step explanation, no tool calls           |
| `--debug`   | extra diagnostic output                                                 |

## Tools shipped

| tool          | purpose                                              |
| ------------- | ---------------------------------------------------- |
| `run_command` | run a shell command (safety-validated)               |
| `read_file`   | read up to 64KB of a file                            |
| `list_dir`    | list a directory (max 100 entries)                   |
| `confluence`  | search pages or fetch a page by id (Atlassian Cloud) |
| `telegram`    | send a message to a Telegram chat via Bot API        |
| `ask_user`    | queue a question for the user (daemon mode)          |
| `finish`      | end the loop with a summary                          |

## LLM providers shipped

| provider    | env switch                          | default model      |
| ----------- | ----------------------------------- | ------------------ |
| OpenAI      | `GOON_LLM_PROVIDER=openai`          | `gpt-4o-mini`      |
| Anthropic   | `GOON_LLM_PROVIDER=anthropic`       | `claude-sonnet-4-5`|
| Ollama      | `GOON_LLM_PROVIDER=ollama`          | `llama3`           |
| Mock        | `GOON_LLM_PROVIDER=mock`            | (offline fixtures) |

## Configuration

All via environment (or `.env`):

```ini
GOON_LLM_PROVIDER=openai|anthropic|mock
OPENAI_API_KEY=...
OPENAI_MODEL=gpt-4o-mini
ANTHROPIC_API_KEY=...
CONFLUENCE_BASE_URL=https://acme.atlassian.net/wiki
CONFLUENCE_EMAIL=you@acme.com
CONFLUENCE_API_TOKEN=...
TELEGRAM_BOT_TOKEN=...
TELEGRAM_CHAT_ID=123456
GOON_MAX_STEPS=5
GOON_MEMORY_PATH=~/.goon/memory.json
```

## Architecture

```
goon/
├── main.go                          # entry: defers to cmd.Execute
├── cmd/
│   ├── root.go                      # flags, .env, signal-aware ctx, subcommand dispatch
│   ├── start.go                     # `goon start` — autonomous daemon
│   ├── stop.go status.go train.go   # cmds backed by the same memory file as the daemon
│   ├── update.go uninstall.go config.go
│   └── pidfile.go                   # ~/.goon/goon.pid bookkeeping
├── internal/
│   ├── llm/                         # Provider interface + impls
│   │   ├── llm.go openai.go anthropic.go ollama.go mock.go
│   ├── tools/                       # tool registry — what the agent can call
│   │   ├── tools.go run_command.go read_file.go
│   │   ├── confluence.go telegram.go
│   │   └── ask_user.go              # queues questions to memory
│   ├── boards/                      # ticket sources
│   │   ├── board.go                 # interface + Ticket + status mapping
│   │   ├── jira.go github.go        # adapters
│   ├── githost/                     # PR / MR creation
│   │   ├── githost.go github.go gitlab.go bitbucket.go
│   ├── workflow/                    # the autonomous pipeline
│   │   ├── workflow.go              # Triage → Plan → Execute → Test → Verify×N → PR → Notify
│   │   └── parse.go                 # strict-JSON triage parser
│   ├── daemon/                      # poll loop, status persistence
│   │   └── daemon.go
│   ├── web/                         # htmx UI (single embedded page)
│   │   ├── server.go
│   │   ├── getenv.go
│   │   └── static/                  # index.html + htmx.min.js (embedded)
│   ├── safety/safety.go             # regex blocklist
│   ├── executor/executor.go         # mode-aware execution + confirmation
│   ├── agent/                       # multi-step loop, prompt, context engine
│   │   ├── agent.go context.go prompt.go
│   └── memory/memory.go             # interactions, questions, workflows, tickets, status
└── pkg/                             # reserved for public packages
```

### Daemon poll loop (in pseudo-code)

```go
for ; !ctx.Done(); <-ticker.C {
    tickets := board.List(ctx)
    for t := range tickets { memory.SeenTicket(t) }

    pick := pickMostRecentlyUpdatedOpenTicket(tickets)
    if pick == nil || hasUnansweredQuestion(pick) { continue }
    if memory.HasOpenWorkflowFor(pick) || memory.HasCompletedWorkflowFor(pick) { continue }

    workflow.Run(ctx, pick)   // see workflow phases below
}
```

### Workflow phases

1. **Triage** — one focused LLM call, strict-JSON, returns `{steps:[…], repo}`.
2. **Plan** — already inside Triage in v1; persisted to `Workflow.Plan`.
3. **Execute** — for each plan step, the existing agent loop is run with that step as the task. Each tool call is safety-validated.
4. **Test** — best-effort `make test` (or `go test ./...`) inside the repo.
5. **Verify** — re-run the agent `GOON_VERIFY_RUNS` more times (default 3, max 10) to catch regressions before opening a PR.
6. **OpenPR** — pushes `goon/<ticket-key>` and creates a PR/MR via `internal/githost`.
7. **Notify** — Telegram message with a link to the PR.

The board ticket gets a comment ("✓ goon completed this ticket. PR: …") and is transitioned to **In Review** if the board adapter supports it.

## The contract

The LLM **must** emit exactly one JSON object. The agent feeds parse errors
back into the chat so the model can self-correct.

```json
{
  "tool": "run_command",
  "args": { "command": "ls -la internal" },
  "rationale": "list packages before reading"
}
```

Allowed `tool` values are exactly the registered tools. Unknown tools are
rejected and surfaced back to the model.

## Safety

`internal/safety` blocks (regex) the most dangerous patterns regardless of
mode: `rm -rf /`, `rm -rf ~`, `rm -rf $HOME`, `mkfs.*`, `dd of=/dev/*`,
fork bombs, `shutdown`/`reboot`/`halt`, recursive `chmod -R 777 /`, and
`curl … | sh`. The list is intentionally short and conservative — extend
in `safety.go` as you discover new patterns.

In `--run` mode the executor also asks `y/N` before any mutating tool
(`run_command`, `telegram`).

## Telegram adapter

```sh
export TELEGRAM_BOT_TOKEN=123:abc
export TELEGRAM_CHAT_ID=987654321
./goon "tell my Telegram bot we shipped" --auto
```

Internally the model emits:
```json
{"tool":"telegram","args":{"text":"We shipped 🎉"}}
```
The executor calls `https://api.telegram.org/bot$TOKEN/sendMessage`.

## Atlassian (Jira + Confluence) — share one set of credentials

Jira and Confluence both live under the same Atlassian Account, so they use
the **same email and API token**. goon lets you set them once via shared
`ATLASSIAN_*` env vars and falls back to them automatically:

```sh
# Set once — covers Jira AND Confluence
export ATLASSIAN_BASE_URL=https://acme.atlassian.net
export ATLASSIAN_EMAIL=me@acme.com
export ATLASSIAN_API_TOKEN=...     # id.atlassian.com/manage-profile/security/api-tokens
```

That's it. Both products work — Jira as the daemon's board source, Confluence
as a tool the agent can call. Internally:

- `JIRA_BASE_URL` defaults to `ATLASSIAN_BASE_URL`
- `JIRA_EMAIL` / `JIRA_API_TOKEN` default to `ATLASSIAN_EMAIL` / `ATLASSIAN_API_TOKEN`
- `CONFLUENCE_BASE_URL` defaults to `ATLASSIAN_BASE_URL` + `/wiki` (the Cloud convention)
- `CONFLUENCE_EMAIL` / `CONFLUENCE_API_TOKEN` default to the shared values

**Per-product overrides** still win when set — useful for self-hosted Data
Center installs where Jira and Confluence live on different hosts:

```sh
export ATLASSIAN_EMAIL=me@acme.com           # auth shared
export ATLASSIAN_API_TOKEN=...
export JIRA_BASE_URL=https://jira.internal/  # Jira on its own server
export CONFLUENCE_BASE_URL=https://wiki.internal/  # Confluence elsewhere
```

### Using Confluence as a tool

```sh
./goon "find the Q3 roadmap on Confluence and summarize the goals"
```

The model picks `confluence` with `op=search` to find pages, then
`op=get_page` to fetch the body, then `finish` with a summary.

## Logs — every prompt, tool call, HTTP request, workflow phase

Everything goon does that touches the outside world or makes a non-trivial
decision is captured to a structured log file. Default location:

```
~/.goon/logs/goon.log
```

Rotates at 10 MB; keeps 3 rotations (`goon.log.1`, `.2`, `.3`). Mirrored to
stderr in real time when goon runs in the foreground.

### What gets logged

| event              | level | example attrs                                                           |
|--------------------|-------|-------------------------------------------------------------------------|
| LLM prompt         | debug | provider, message count, task                                           |
| LLM response       | debug | provider, raw_bytes, raw (truncated)                                    |
| Tool call (parsed) | info  | tool, args, rationale                                                   |
| Tool execution     | info  | tool, args, ok, latency_ms, stdout_bytes                                |
| Tool stdout/stderr | debug | full output (truncated to 4 KB)                                         |
| HTTP request       | info  | component, method, url, status, req_bytes, resp_bytes, latency_ms       |
| HTTP body          | debug | request body + response body (truncated)                                |
| Workflow start/end | info  | wf, ticket, state, pr_url, duration_ms                                  |
| Daemon poll        | info  | poll_start, poll_end, duration_ms                                       |
| HTTP errors (≥500) | error | full error message + URL                                                |

Sensitive bits are redacted automatically: Telegram bot tokens in URLs
become `/bot***/...`; basic-auth `user:pass@host` becomes `***@host`.

### Browse logs

```sh
goon logs                    # last 100 lines, then exit
goon logs --tail=500         # last 500 lines
goon logs --follow           # tail -f equivalent
goon logs --path             # print the file path
goon logs --clear            # truncate (keeps rotations)
```

### Pipe to your log stack

```sh
GOON_LOG_FORMAT=json goon start    # newline-delimited JSON
tail -f ~/.goon/logs/goon.log | jq 'select(.level=="ERROR")'
tail -f ~/.goon/logs/goon.log | vector --config /etc/vector/cowork.toml
```

### Knobs

```
GOON_LOG_FILE     ~/.goon/logs/goon.log    where the file lives
GOON_LOG_LEVEL    info                     debug | info | warn | error
GOON_LOG_FORMAT   text                     text | json
```

`debug` captures full prompt text, full LLM responses, full HTTP request and
response bodies (each truncated to 4 KB). Useful for debugging a single
workflow; not recommended for steady-state production.

## Memory

`~/.goon/memory.json` (override with `GOON_MEMORY_PATH`) holds the last
200 interactions and a per-command counter. The agent injects the top-5
frequent commands into the prompt so the model can match your style.

## Testing

The full test suite uses a deterministic mock LLM and `httptest` servers
so it runs offline:

```sh
make check        # vet + go test -race ./...
```

What is covered:

- `tools.ParseToolCall`: plain JSON, fenced JSON, prose around JSON,
  nested braces, empty-tool rejection, missing-JSON rejection.
- `safety.Default`: blocks `rm -rf /`, `mkfs`, `dd`, fork bombs,
  pipe-to-shell, recursive chmod of `/`; allows benign commands.
- `executor`: dry-run does not execute; `--run` honors y/N; `--auto`
  skips the prompt; safety wins over mode; `--explain` never mutates.
- `agent`: finish-immediately, multi-step + tool result feedback, JSON
  self-repair, dangerous-command back-off, max-steps bound, unknown-tool
  recovery.
- `memory`: append, recent-summary, frequency, persistence across
  reopens, no-op disabled mode.
- `tools.Telegram` / `tools.Confluence`: full HTTP request shape against
  `httptest.Server`, error and config-missing paths.

## Acceptance test (manual)

```sh
./goon "find all .log files under /tmp and delete them" --run
```

Expected:
1. Step 1: `list_dir` or `run_command` (`find /tmp -name '*.log'`)
2. Confirmation prompt for the destructive step
3. Step 2: `rm` of the matched files (only if you say `y`)
4. Step 3: `finish` with a summary

## Failure modes the agent guards against

- Executing raw LLM text — every step must parse as a `ToolCall`.
- Skipping JSON parsing — bad JSON is fed back as a parse-error message.
- No validation layer — `run_command` always passes through `safety`.
- Infinite loop — `MaxSteps` (default 5, max 50).

## Extensions

Add a new tool by implementing `tools.Tool` and registering it in
`DefaultRegistry`. The model picks it up automatically through the
manifest line in the system prompt.

## License

MIT — go nuts.
