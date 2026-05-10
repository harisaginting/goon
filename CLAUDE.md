# CLAUDE.md — context for future me

You're Claude, picking up work on **goon** with Harisa. Read this before
touching code. It captures project context, conventions, and the
not-immediately-obvious things that have already cost time to learn.

---

## What goon is

A Go CLI for autonomous engineering work. Two operating modes:

1. **One-shot agent** — `goon "do this thing"` runs a single agent loop
   (LLM picks tools, executor runs them, repeat until `finish`).
2. **Daemon** — `goon start` polls a board (Jira / GitHub Issues), picks
   one ticket per tick, runs the workflow pipeline against it, opens a
   PR, notifies via Telegram.

Workflows are JSON-configurable. A single `workflow.json` can override
the built-in pipeline with arbitrary `stages` (typed: `llm` or `agent`),
hooks (sh commands at phase boundaries), branch prefix, PR templates.
Designed so the same binary works for engineering, marketing, sales,
ops — not just code.

## Module + build

- Module path: `github.com/harisaginting/goon`
- Go 1.21+. No external dependencies — only stdlib + internal packages.
  When touching code, **don't add deps**. We deliberately ship zero.
- Build: `go build ./...`. Tests: `go test ./...`. Race: `go test -race ./...`.
- Smoke test: `GOON_LLM_PROVIDER=mock GOON_MOCK_REPLIES='{"tool":"finish","args":{"message":"hi"}}' go run . "test"`

## Package map (rough)

```
cmd/                CLI entry points + subcommand handlers
  root.go           Execute() → run() → flag parse + subcommand dispatch
  start.go          `goon start` — daemon launcher
  workflow.go       `goon workflow show/path/init/edit/hooks`
  memory.go         `goon memory list/read/write/...` (active markdown notes)
  logs.go           `goon logs [--tail|--follow|--clear]`
  config.go         `goon config get/set/...` against ~/.config/goon/.env (legacy ~/.goon/.env still loaded)
  doctor.go         live-probe every configured provider
  train.go          answer LLM-queued questions
  update.go         self-update from upstream
  status.go, stop.go

internal/agent/     LLM ↔ tool loop; SystemPrompt
internal/atlassian/ shared env-var helper for Jira + Confluence
internal/boards/    ticket-source adapters: jira, github
internal/checkup/   `goon doctor` provider probes
internal/daemon/    poll loop, hot-reload via Reconfigure()
internal/executor/  runs tool calls in {dry-run|run|auto|explain} modes
internal/githost/   PR adapters: github, gitlab, bitbucket
internal/llm/       provider adapters: openai, anthropic, ollama, mock
internal/logx/      slog wrapper, log rotation, HTTP LoggingTransport
internal/memory/    PASSIVE runtime store (JSON file: tickets, workflows, questions)
internal/notes/     ACTIVE markdown notes store (./storage/memory/*.md)
internal/safety/    command validator (blocks rm -rf / etc)
internal/storage/   single source of truth for the per-project state root
internal/telegram/  inbound bot — auth, /commands, chat, PR review (cmd/start spawns)
internal/tools/     Tool interface + builtins + memory_* tools
internal/util/      tiny stdlib-only helpers shared by 3+ packages
                    (Truncate, EnvOr, ConfirmTTY). MUST stay zero-dep
                    (no internal/* imports) so anything can pull it in.
internal/web/       optional web UI for `goon start --web=:8080`
internal/workflow/  Engine.Run, declarative stage runner, hooks
```

## Where state lives (the storage layout)

Goon writes everything under one per-project directory — `./storage/`
relative to the working directory. **Don't reintroduce `~/.goon` as a
fallback anywhere.** That used to share state across repos and
confused everyone.

```
./storage/
├── logs/goon.log     rotated structured log (logx)
├── memory.json       PASSIVE runtime state (internal/memory)
├── memory/           ACTIVE markdown notes (internal/notes — PINNED.md, etc.)
└── goon.pid          present while daemon runs (cmd/pidfile.go)
```

`./workflow.json` (repo root) is the new canonical workflow location —
also tried in legacy `<repo>/.goon/workflow.json` etc. for backwards compat.

Resolution flows through `internal/storage` (`Root()` and `Path(parts...)`).
Every package that needs a default path delegates there. If you find
yourself writing `os.UserHomeDir()` in a default-path function, you're
doing it wrong — call `storage.Path(...)`.

Env overrides:
- `GOON_STORAGE_DIR` — relocate the whole tree
- `GOON_LOG_FILE`, `GOON_MEMORY_PATH`, `GOON_MEMORY_DIR`, `GOON_PID_FILE`,
  `GOON_WORKFLOW_FILE` — relocate individual files

> **memory vs notes — easy to confuse.** `internal/memory` holds the
> JSON-backed runtime state (tickets the daemon processes, the question
> queue, workflow records, status). `internal/notes` is the new
> markdown-based persistent knowledge store the LLM reads and writes via
> `memory_*` tools. The user-facing word is "memory" for both. When in
> doubt about which one to touch, the test is: *does the user edit it
> with their own hands?* If yes → notes. If no (it's just runtime
> bookkeeping) → memory.

## How a request flows

**One-shot agent (`goon "task..."`):**

```
cmd.Execute → run() → loadDotEnv → logx.New → strip "goon" prefix →
splitSubcommand (none) → parse flags → llm.NewFromEnv → tools.DefaultRegistry →
memory.New → safety.Default → executor.New → agent.New → ag.Run(ctx, task)
```

**Daemon (`goon start`):**

```
cmd.Execute → run() → ... → splitSubcommand("start") → runStart →
workflow.Announce(stdout) → daemon.New → d.Reconfigure → web.NewServer →
d.Run → tick → pollAndRun → memory.ResumableWorkflow? (resume) :
  (boards.List → pick ticket) → workflow.Engine.Run →
  triage → confirm_repo (gate) → approve_plan (gate) → execute → test →
  verify → update_memory → openPR → notify
```

The two gate phases (`confirm_repo`, `approve_plan`) queue a question via
`memory.AskQuestion`, set `wf.State=WFAwaitingApproval` + `wf.Stage=<gate>`,
and exit cleanly. The next poll tick calls `Memory.ResumableWorkflow()`, which
returns the most recently-updated paused workflow whose `PendingQuestionID`
has been answered; the daemon re-enters `Engine.Run` with the same ticket and
the state machine picks up at `wf.Stage`.

## Conventions to honor

1. **Never add a third-party Go dep.** If you reach for a package, find
   another way. Stdlib only. Goon's value prop includes "single binary,
   zero deps".
2. **Wrap every `http.Client` in `logx.InstrumentClient(component, c)`.**
   Every outbound HTTP request shows up in `./storage/logs/goon.log` with
   method, URL, status, latency, byte counts. We did the audit — every
   provider already does this; keep it that way.
3. **Use structured logging via `logx.Info / Warn / Debug / Error`**,
   never `log.Println`. Keys are snake_case (`ticket_key`, `wf_id`).
4. **Errors: wrap with `fmt.Errorf("context: %w", err)`.** Don't
   swallow — we paid for that lesson once with `runAgent` returning
   `(nil, nil)`.
5. **Commands are run through `internal/safety` validator.** Hooks too.
   Don't bypass even for "trusted" code paths.
6. **Backwards-compatible env vars.** When unifying (e.g. ATLASSIAN_*
   over JIRA_*/CONFLUENCE_*), per-product wins when set; shared is the
   fallback. Never silently break old configs.
7. **Tests must run without network.** The `mock` LLM provider exists
   for this. CI uses it for the smoke test.
8. **Don't create files in the repo root** unless explicitly asked
   (no README spam, no scratch files). `examples/`, `internal/`, `cmd/`,
   `.github/` are the homes.

## Recent decisions worth knowing

- **`internal/util` shared helpers.** `Truncate`, `EnvOr`, `ConfirmTTY`
  live in `internal/util/util.go` and replace four-or-more in-package
  duplicates each. Rule: util has stdlib-only imports (no `internal/*`
  deps), so it can be imported from anywhere without cycles. If you find
  a fourth duplicate of some helper, that's the bar to add it here.
- **`memory.json` pruning caps (`internal/memory`).** Two new bounds to
  stop unbounded JSON growth: `maxTicketSnapshots = 500` and
  `maxTelegramAuth = 100`. `SeenTicket` evicts the oldest by `LastSeen`,
  `AuthorizeChat` evicts the oldest by `AuthorizedAt`. Caps are
  re-applied inside `flush()` after the disk-merge so a fresh process
  loading an old (unbounded) memory.json gets cleaned up on first
  write. New helper `Memory.PruneStaleAuth(maxAge time.Duration)` is
  exposed for future admin commands. Tests:
  `TestMemory_TicketsPrune`, `TestMemory_AuthorizeChatPrunesOldest`,
  `TestMemory_PruneStaleAuth`.
- **`goon doctor` ollama probe is no longer a liar.** When the Ollama
  server is reachable but the configured `OLLAMA_MODEL` isn't pulled,
  `probeOllama` now returns `OK = false` with a `run: ollama pull X`
  hint. Previously it returned `OK = true` (server-reachable was good
  enough) and the agent loop only discovered the missing model on first
  generate(). The `git_host` check also distinguishes "no host
  configured" (skipped, blue dot) from "no host but a board IS
  configured" (skipped with a yellow ⚠ hint to set `GOON_GIT_HOST`).
  `probeOllama` test was updated to assert `!OK`.
- **`internal/checkup.newReq` no longer hides errors.** It used to
  return `(*http.Request, error)` but every call site did `req, _ :=
  newReq(...)` and then `req.Header.Set(...)` — a bad URL would crash
  with a nil-pointer panic. `newReq` now returns a placeholder
  non-nil request on error AND every call site checks the err and
  returns the failure as a `Result.Detail`. Defensive-but-explicit.
- **Inbound Telegram bot (`internal/telegram`).** When both
  `TELEGRAM_BOT_TOKEN` and `GOON_TELEGRAM_SECRET` are set,
  `cmd/start.go::startTelegramBot` spins up a long-poll goroutine. Auth is
  a single shared secret: users DM `/auth <secret>` once, the chat ID is
  saved via `Memory.AuthorizeChat`, and from then on all messages from
  that chat are accepted. `/logout` revokes. Surface: `/help /status
  /logs /workflows /memory /queue /answer /run /whoami /logout` plus a
  GitHub-only PR review flow (`/prs /review /approve /decline /comment`)
  and full CLI parity for everything else (a `/<subcmd>` not in the
  builtin/disallow lists shells out to the goon binary at
  `os.Executable()`). Plain text → LLM chat with a 6-turn rolling history
  per chat (in-process; lost on restart). Disallowed commands:
  `start, stop, uninstall, update`. New schema: `memory.ChatAuth`,
  `Memory.AuthorizeChat / IsChatAuthorized / TouchChat / RevokeChat /
  AuthorizedChats`. Tests in `internal/telegram/bot_test.go` use a fake
  Telegram server (`httptest`).
- **`githost.PRReviewer` companion interface.** `internal/githost/githost.go`
  now exposes `PRReviewer` with `ListPRs / GetPRDetails / CommentPR /
  ApprovePR / RequestChangesPR`. GitHub implements it (added in
  `github.go`). Mock implements it (used by bot tests). Hosts that don't
  implement it gracefully degrade — the bot's PR commands report
  "PR review unsupported on the configured git host". `PR` struct grew
  `Author`, `State`, `Body`, `Repo` fields. New env var
  `GOON_REVIEW_REPOS=owner/a,owner/b` provides the default repo set when
  `/prs` is called without args.
- **Approval-gated workflow as the new default.** `internal/workflow/workflow.Run`
  is now a resumable state machine: `triage → confirm_repo → approve_plan →
  execute → test → verify → update_memory → open_pr → notify`. The two gates
  use `ask_user`-style questions stored via `memory.AskQuestion` and pause the
  workflow with `wf.State=WFAwaitingApproval` + `wf.Stage=<gate>` +
  `wf.PendingQuestionID`. The daemon's `pollAndRun` checks
  `Memory.ResumableWorkflow()` before fetching new tickets and resumes once
  the user replies via `goon train` or the web UI. New `update_memory` phase
  runs an agent task that asks the LLM to distil what it learned into the
  markdown notes store (PINNED.md / topic notes) — failures here are
  non-fatal. Set `cfg.AutoApprove: true` in workflow.json or env
  `GOON_AUTO_APPROVE=1` to skip both gates for unattended runs (tests use
  `Engine.AutoApprove = true` for the same reason). New states added to
  `internal/memory`: `WFAwaitingApproval`, `WFUpdatingMemory`. New fields
  on `memory.Workflow`: `Stage`,
  `PendingQuestionID`, `Approvals`. New helpers: `Memory.OpenWorkflowFor`,
  `Memory.ResumableWorkflow`. Tests in `internal/workflow/workflow_test.go`
  (TestEngine_PausesAtConfirmRepoGate, TestEngine_ResumesAfterApproval,
  TestEngine_RejectedPlanFailsWorkflow) and `internal/daemon/daemon_test.go`
  (TestDaemon_ResumesPausedWorkflow).
- **Per-project storage at `./storage/`** replaces the old `~/.goon/`
  global directory. Centralized in `internal/storage` (Root + Path).
  Logs, `memory.json`, the markdown notes dir, and the PID file all
  derive from `storage.Root()`. Workflow defaults to `./workflow.json`
  at the repo root. Legacy paths (`~/.goon/...`, `.goon/workflow.json`)
  are read for backwards compat but never written. Tests in
  `internal/storage/storage_test.go`, `internal/notes/notes_test.go`
  (TestNew_FallsBackToStorageRoot), and `internal/workflow/config_test.go`
  (TestLoadConfig_RepoRootWins, TestDefaultConfigFilePath_RepoRoot).
- **Workflow `name` + `description` fields** were added so `goon start`
  prints the active workflow on its first stdout line, and every
  per-ticket `workflow.start` log includes it. Default name is `"default"`.
- **`workflow.Announce(repoDir, w)`** is the helper that prints +
  logs the loaded workflow at startup. Call it from any new entry point.
- **PINNED.md is auto-injected into `agent.SystemPrompt`.** Located at
  `./storage/memory/PINNED.md` (or `$GOON_MEMORY_DIR/PINNED.md`).
  Whitespace-only files are treated as absent — no empty banner.
- **`memory_*` tools share a single `notes.Store`** via `RegisterMemoryTools()`
  in `internal/tools/memory.go`. Don't construct stores per-call.
- **Path safety in notes:** rejects absolute paths, any literal `..`
  segment in the raw input (before `filepath.Clean`), and resolves
  must end up inside the store root. Tests in `internal/notes/notes_test.go`.
- **Atlassian env vars are unified.** `ATLASSIAN_BASE_URL` /
  `ATLASSIAN_EMAIL` / `ATLASSIAN_API_TOKEN` cover both Jira and
  Confluence; `JIRA_*`/`CONFLUENCE_*` per-product vars override.
  Helper: `internal/atlassian.Jira()` and `.Confluence()`.
- **Jira search uses `/rest/api/3/search/jql`** (Atlassian CHANGE-2046
  removed `/rest/api/3/search`). Pagination is cursor-based via
  `nextPageToken` / `isLast`. Don't auto-paginate — daemon picks one
  ticket per tick anyway.
- **`go run . goon workflow init` is fixed** — root.go strips a
  redundant leading `"goon"` arg with a one-line hint to stderr. Tests
  in `cmd/root_test.go`.
- **Windows is a supported platform.** Cross-platform shells go through
  `safety.ShellCommand(ctx, cmd)` (new helper in `internal/safety/shell.go`)
  which picks `sh -c` on POSIX and `cmd /C` on Windows. Both
  `internal/workflow/hooks.go::runOne` and `internal/tools/run_command.go`
  call it instead of hard-coding `sh`. `cmd/pidfile.go::processAlive`
  branches on `runtime.GOOS`: Unix keeps the signal-0 probe; Windows uses
  `os.FindProcess` (which actually opens a handle on Windows and fails
  for missing pids) plus a 24h pid-file mtime backstop against pid reuse.
  `cmd/stop.go::stopSignal` returns `os.Interrupt` on Windows (translated
  to `CTRL_BREAK_EVENT`) and `SIGTERM` elsewhere. `internal/memory/lock_windows.go`
  is no longer a no-op: it implements multi-process locking using
  `os.OpenFile(... O_CREATE|O_EXCL ...)` on a sibling `.lock` file, with
  50ms backoff up to a 5s deadline and a 2-minute stale-lock eviction.
  Same `lockFile(path) (release, err)` API as `lock_unix.go`.

## Common pitfalls

- **No Go toolchain in the Cowork sandbox.** Don't try `go build` —
  it'll fail. Static brace/paren balance check via Python is the
  best you can do here. The user runs `go test ./...` on their
  machine. Be confident before reporting "shipped".
- **FUSE filesystem artifacts.** The user's machine sometimes drops
  `.fuse_hidden*` files. They're in `.gitignore`. Don't ever
  `rm -rf` blindly — make a `tar` backup first. We deleted the
  whole repo once. Never again.
- **The web fetch tool can't reach `go.dev`.** Allowlist blocks it.
  Don't try to install Go from the sandbox.
- **`internal/memory.Memory` flush uses flock**; warns once on
  failure, then continues. Don't treat the warn as an error.
- **The `mu sync.Mutex` in `daemon.Daemon`** only protects `pollAndRun`.
  Reconfigure uses a separate `rcMu` (RWMutex). Don't introduce a
  third lock; use `Snapshot()` to read providers safely.
- **Tools ALWAYS receive `map[string]string` args** — never a typed
  struct. The LLM emits strings, period. If you need an int, parse it
  inside `Run()` and return a clear error if invalid.

## Verification ritual when you're done

1. Brace/paren balance on every `.go` file you touched
   (Python token-aware check; example in conversation history).
2. JSON-validate any sample/example JSONs you edited
   (`python3 -c "import json; json.load(open('...'))"`).
3. Search for orphaned imports / unused symbols in your new code.
4. Update `README.md` if user-visible behavior changed.
5. If you added a new package, mention it in this file's package map.
6. Tell the user **what to verify on their machine** — usually a
   `go test ./internal/<pkg>/... -v` or `go build ./...` invocation.

## Notes for future Claude on the working relationship

When asked to ship, ship — but verify first. They prefer
the code be simple over clever, and they care about onboarding ergonomics
(env var unification, friendly error messages, sensible defaults).

When in doubt: read the code, then ask one focused question. Don't
guess at things that are easy to look up.

**Standing instruction from Harisa: read this file at the start of every
goon session, and update it at the end of every session.** New decisions
go under "Recent decisions worth knowing"; new gotchas under "Common
pitfalls"; new packages get a line in the package map. The point is
that the next instance of you doesn't repeat mistakes I've already
made.
