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
  pause.go          `goon pause` / `goon resume` (flips memory.json flag)
  repo.go           `goon repo list|forget|clear` (manages learned project→repo)
  version.go        `goon version` (resolves ldflags + debug.BuildInfo)
  status.go, stop.go

internal/agent/     LLM ↔ tool loop; SystemPrompt
internal/atlassian/ shared env-var helper for Jira + Confluence
internal/boards/    ticket-source adapters: jira, github
internal/checkup/   `goon doctor` provider probes
internal/codeindex/ regex symbol extractor + ripgrep/stdlib content
                    search; backs the search_code tool
internal/daemon/    poll loop, hot-reload via Reconfigure()
internal/executor/  runs tool calls in {dry-run|run|auto|explain} modes
internal/githost/   PR adapters: github, gitlab, bitbucket
internal/llm/       provider adapters: openai, anthropic, gemini, ollama, mock
internal/logx/      slog wrapper, log rotation, HTTP LoggingTransport
internal/memory/    PASSIVE runtime store (JSON file: tickets, workflows, questions)
internal/notes/     ACTIVE markdown notes store (./storage/memory/*.md)
internal/skills/    Specialist markdown store (./storage/skills/*.md)
                    Same Store type as notes; instantiated against a
                    different default root. Sibling of memory, NOT
                    auto-injected like SOUL.md.
internal/personal/  DEPRECATED. Was the single-file character store
                    (./storage/personal.md). Character + project
                    knowledge are now unified in SOUL.md (see
                    internal/notes). The directory remains as a
                    package-doc stub so external tooling that
                    `go list`s the repo doesn't 404 — safe to
                    `rm -rf` it locally.
internal/repository/ Owns REPOSITORY.md — the user-maintained mapping
                    of remote git slugs to local checkout paths. Lives
                    at ./storage/memory/REPOSITORY.md (excluded from
                    topic-note index). Read by workflow triage so the
                    LLM can suggest specific repos by name; read by
                    confirm_repo gate to build the candidate menu;
                    write surface via `goon repo show/edit/scan/add`.
                    Lookup() resolves an LLM-supplied name (e.g.
                    "backend-api") back to its canonical local path.
                    SeedDefault() writes the starter table + preamble
                    on first boot.
internal/learnings/ Capture(): auto-runs after every agent.Run.
                    Appends a HISTORY.md line (timestamp · task ·
                    outcome) and fires a short distillation pass that
                    lets the LLM write durable knowledge to SOUL.md
                    or topic notes via memory_* tools. Shared between
                    the one-shot path (cmd/root.go) and the workflow
                    update_memory phase (internal/workflow). Opt out
                    with GOON_AUTO_LEARN=0.
internal/review/    Host-agnostic engine for the "PRs awaiting my
                    review" + "forward my notifications" features.
                    Runner.PendingReviews drafts an LLM review for each
                    review-requested PR whose diff changed; Runner.
                    Notifications dedups + digests the inbox. Depends
                    only on the githost companion interfaces, never on
                    cmd/telegram. Used by cmd/review.go and the bot's
                    autoreview.go loop.
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

- **Proactive PR review + notification forwarding (`internal/review`).**
  Two user-facing features built on three new pieces:
  - **githost companion interfaces** (`internal/githost/githost.go`):
    `ReviewRequester{ReviewRequestedPRs(ctx)}` and
    `Notifier{Notifications(ctx)}`, plus a `Notification` struct.
    GitHub implements both (Search API `review-requested:@me`;
    `/notifications` filtered to review_requested/mention/team_mention).
    GitLab implements both AND gained the whole `PRReviewer` surface it
    never had — `ListPRs`, `GetPRDetails` (diff reconstructed from
    `/merge_requests/{iid}/diffs`), `CommentPR`, `ApprovePR`,
    `RequestChangesPR` (no native "request changes" REST event — posts
    a blocking note instead), `ReviewRequestedPRs`
    (`merge_requests?reviewer_username=`), `Notifications` (`/todos`).
    Bitbucket implements `ReviewRequester` (per-repo `q=reviewers.uuid`
    over GOON_REVIEW_REPOS / discovered repos) but deliberately NOT
    `Notifier` — Bitbucket Cloud has no notification-inbox API, so the
    type assertion fails and callers degrade. Mock implements both.
  - **`internal/review.Runner`** — host-agnostic. `PendingReviews`
    drafts an LLM review (same prompt shape as telegram/pr.go) for each
    review-requested PR whose diff fingerprint (sha256[:16]) changed
    since last pass; `Notifications` dedups + adds an LLM digest when
    >1 is new. Caller marks dedup state AFTER successful delivery so a
    failed send retries.
  - **dedup in `internal/memory`** — `ReviewSeen map[string]ReviewMark`
    (key `host:repo#number`, stores diff hash) + `NotifSeen
    map[string]time.Time` (key `host:id`). Caps 500/2000, pruned oldest
    first, merged as a union in `mergeStores`.
  Delivery: `goon review-prs` / `goon notifications` CLI subcommands
  (both take `--watch --interval --telegram --all`, so they double as a
  cron-free scheduler) and the Telegram bot's `autoreview.go` loop —
  a ticker goroutine started from `Bot.Start`, gated by
  `GOON_AUTO_REVIEW` / `GOON_AUTO_NOTIFY` (default OFF so an upgrade
  never starts messaging existing daemon users), cadence
  `GOON_AUTO_INTERVAL` (default 15m). The bot owns the auto loop because
  it already holds Host+LLM+Memory+send+authorized-chat-list — daemon.go
  was NOT touched. Review drafts go out via `SendWithButtons` with a
  `rv:repo:number` callback; tapping "✅ Post as comment" runs
  `callbackHandleReview` (claimed early in interactive.go's
  `handleCallback`, mirroring `callbackHandleRepos`) which extracts the
  fenced draft from the message text and posts it via `CommentPR` — the
  user's one-tap approval. **Verify on real hosts:** the Bitbucket
  `q=reviewers.uuid="..."` PR filter is the one query I couldn't test;
  if Bitbucket rejects it the per-repo call is skipped + logged
  (`bitbucket.review_requested_skip`) and review-request detection
  silently returns empty for that host.
- **Codebase index (`internal/codeindex` + `internal/tools/search_code.go`).**
  First call to `search_code` builds a per-process index of the
  current repo: regex symbol extraction (Go/Python/JS/TS/Java/Rust/
  Ruby/PHP/Elixir/Shell) + a content searcher that prefers ripgrep
  when on PATH and falls back to stdlib bufio. Query shape picks the
  mode: bare word → symbol lookup + content fallback; `/pat/` →
  regex; anything else → substring. Single shared `SearchCode` is
  registered in `DefaultRegistry` so the index isn't rebuilt per
  call. Files >2 MB and ignored dirs (.git, node_modules, vendor,
  target, build, dist, .venv …) are skipped. No Tree-sitter because
  CGo would break the "single binary, zero deps" rule — regex was
  good enough and matches 9 langs with ~50 lines per lang.
- **Browser tools (`internal/tools/fetch.go`).** `fetch_url`
  retrieves a URL (HTTPS-only by default, `GOON_FETCH_ALLOW_HTTP=1`
  unlocks plain http), 256 KB cap, strips HTML tags via a hand-
  written stripper so the agent gets readable docs without an x/net
  dep. `web_search` prefers Google CSE when `GOOGLE_API_KEY` +
  `GOOGLE_CSE_ID` are set; falls back to `html.duckduckgo.com`
  scraping (substring-based, no html.Parser). Both clients are
  `logx.InstrumentClient("fetch", ...)` so every outbound request
  shows up in `./storage/logs/goon.log`. Use case: the agent can
  read error messages, library docs, and Stack Overflow answers
  autonomously instead of guessing.
- **Web UI file browser (`internal/web/files.go`).** New "Files"
  sidebar entry under a "Workspace" section. Endpoints:
  `/api/files/tree` (directory listing — JSON or HTML fragment),
  `/api/files/read` (returns the editor pane with a textarea),
  `/api/files/write` (atomic tmp+rename, fires
  `HX-Trigger: filesChanged` and SSE), `/fragments/tab-files` (the
  two-column composer). Root resolves
  `GOON_WORKSPACE_DIR → GOON_WORKDIR → cwd`. Path safety: rejects
  absolute paths, any literal `..` in the raw input, and resolves
  must stay under root. 2 MB read cap; binary files (NUL byte in
  first 8 KB) refused for editing. No execute/rename/delete from
  this surface — the agent stays the only thing that can mutate the
  repo in non-obvious ways. Letter shortcut `f`, also in cmd-K.
- **Daemon wake channel.** `(*daemon.Daemon).Wake()` pushes onto a
  buffer-1 `wakeCh` that `Run()` selects on alongside the poll ticker.
  Used by the web `/api/answer` handler and the Telegram `/answer`
  command so a workflow paused at an approval gate resumes in <1s
  instead of waiting up to PollInterval (default 5 min) for the next
  scheduled tick. Both the web and telegram packages define a local
  `Waker` interface (just `Wake()`) and accept the daemon as that
  interface — no import-cycle pain. Calling Wake on a daemon whose
  wakeCh is already full is a no-op (we coalesce bursts).
- **Gemini provider (`internal/llm/gemini.go`).** Google's
  generativelanguage v1beta REST API, stdlib-only like every other
  adapter. Env vars: `GEMINI_API_KEY` (or `GOOGLE_API_KEY` as
  fallback), `GEMINI_MODEL` (default `gemini-2.5-flash`),
  `GEMINI_BASE_URL` (default `https://generativelanguage.googleapis.com/v1beta`).
  URL shape: `{base}/models/{model}:generateContent?key={KEY}` for
  non-stream, `:streamGenerateContent?key={KEY}&alt=sse` for stream.
  Auth via query param (Google's public API style) — no OAuth.
  Roles map: system → `system_instruction.parts[*].text`, assistant
  → "model", user/tool → "user". `Stream` parses `alt=sse` events;
  each event carries a full `{candidates:[{content:{parts:[...]}}]}`
  fragment, NOT a delta type like Anthropic. `probeGemini` in
  checkup sends a 1-token generateContent ping to verify auth + model
  in one round-trip. Wired into NewFromEnv, doctor probe,
  cmd/config.go's known keys, web config form's groupings, README,
  .env.example, and docs.html.
- **Chat agent has tool use (`internal/agentctx/chat.go`).** The web
  and Telegram chat handlers no longer call `LLM.Generate` directly —
  they delegate to `agentctx.ChatTurn`, which runs an LLM↔tool loop
  with up to `maxChatToolIterations=3` iterations. The LLM emits a
  single JSON line on stdout to invoke a tool; everything else is
  treated as the final prose answer. `parseToolCall` strips a leading
  ```json fence if the model adds one. Tools:
    - `jira_search` (read JQL, requires `boards.Searcher`)
    - `jira_comment` (always available on any `boards.Board`)
    - `jira_transition` (always available)
    - `jira_update` (requires `boards.Updater`)
  Read results feed back as `SEARCH RESULTS` system messages; writes
  feed back as `ACTION OK …` or `TOOL ERROR …` so the LLM knows
  what happened and can confirm in prose on the next turn. Search
  hits are persisted into `Memory.SeenTicket` so the next /tickets
  call sees them too.
- **Two new optional board interfaces** (`internal/boards/board.go`):
  `Searcher{Search(ctx, query, limit)}` and `Updater{Update(ctx, id,
  TicketPatch)}`. Both are optional companions to the base `Board`
  interface; non-implementing boards degrade gracefully (the tool
  loop surfaces "not supported" to the LLM). `Mock` implements both
  and records calls in `Mock.Searches` / `Mock.Updates` for tests.
- **Jira Transition is now real** (`internal/boards/jira.go`). The
  former stub returning `nil` is replaced with the proper two-call
  Jira flow: GET `/rest/api/3/issue/{key}/transitions` lists the
  project's workflow-defined transitions, then POST with the
  best-matched transition id. Matching prefers `to.name → MapStatus`,
  falls back to `transition.name → MapStatus`, and on no match
  returns an error listing what WAS available so the chat agent can
  show the user the choices.
- **Jira Update** (`internal/boards/jira.go::(*Jira).Update`) is the
  Updater implementation — PUT `/rest/api/3/issue/{key}` with a
  `fields` object holding the diff. Description is wrapped in
  minimal ADF (same shape as Comment). `TicketPatch` uses
  pointer-to-string for Title/Description so nil = leave alone vs
  non-nil = set; `Labels []string` uses nil-vs-empty-slice for the
  same distinction.
- **Pause/resume control surface.** Three drivers, one source of truth.
  `Memory.Status.Paused bool` is flipped by `goon pause` (cmd/pause.go),
  the web UI's POST `/api/daemon/pause` (renders the alternate
  resume button so the htmx swap is non-destructive), and the
  Telegram bot's `/pause` command. The daemon's `pollAndRun` checks
  `IsPaused()` after `Reload()` every tick and skips the cycle. The
  bot itself stays responsive while paused (it lives in the same
  process as the daemon but on a different goroutine). `daemon.stop()`
  clears the flag so a fresh `goon start` always starts un-paused.
- **Per-project repo learning.** When the user confirms a repo at
  the `confirm_repo` gate, `Memory.RecordRepoChoice(project, repo)`
  persists it to `Memory.RepoChoices`. The next ticket from the same
  project skips the gate via `lookupLearnedRepo` (env-explicit
  `GOON_REPO_MAP` still wins, learned beats wildcard, raw project
  name is last resort). `goon repo list|forget <project>|clear`
  manages the cache. `Engine.pickRepoForTicket` is the priority-aware
  resolver that replaces calls to the legacy `pickRepo()`.
- **Rejected plans re-plan instead of failing.** Cycle-2/3:
  `phaseApprovePlan` no longer returns WFFailed on a non-yes answer.
  Instead it stores `Approvals["replan_feedback"] = ans`, sets
  `Plan = nil`, `Stage = "triage"`, and returns errPaused. The
  daemon's `ResumableWorkflow()` was extended to pick up workflows
  in `WFTriaging` with `replan_feedback` set, so re-plans cycle
  through the daemon naturally. `triageWithFeedback` weaves the
  feedback into the next prompt under a `PREVIOUS PLAN WAS REJECTED`
  block. The approve_plan question text includes
  `"REVISED plan (attempt N)"` so `FindAnswer` can't auto-replay
  the previous "no". Capped at `maxRePlans = 3` rejections before
  the workflow gives up with WFFailed. Tests:
  `TestEngine_RejectedPlanRePlansWithFeedback`,
  `TestEngine_RejectedPlanGivesUpAfterMaxRePlans`.
- **Question history cap.** `maxQuestions = 500` in
  `internal/memory/memory.go`. `pruneQuestions` evicts oldest
  answered first, never drops pending. Re-plan loops + months of
  uptime would otherwise unbound the slice.
- **`/api/config` fires both triggers.** POST returns
  `HX-Trigger: configChanged, statusChanged` so the header status
  pill (which polls `statusChanged`) refreshes alongside the config
  form's success panel. `fragTabConfig` deliberately does NOT listen
  to configChanged — it would wipe the user's verify/save output.
- **Telegram subprocess env scrub.** `runGoonCLI` strips every key
  whose name ends in `_TOKEN`/`_API_KEY`/`_SECRET`/`_PASSWORD` plus
  the explicit goon/atlassian secrets, before passing env to a
  passthrough subprocess. Without this, a misbehaving CLI subcommand
  could dump credentials to a Telegram chat.
- **Dry-run lets read-only tools through.** Cycle-2:
  `internal/executor/executor.go` now only short-circuits dry-run
  for `isMutating` tools. `read_file`, `list_dir`, `memory_read/list/search`,
  and friends execute even in dry-run so the LLM has real data to
  reason about. Without this, a fresh user typing `goon "summarize
  the .go files"` got hallucinated answers.
- **`/start` Telegram convention** is special-cased in
  `internal/telegram/commands.go::handleCommand` — for already-auth'd
  chats it sends a friendly greeting instead of "✗ command not
  allowed." Tests: `TestBot_StartIsFriendlyForAuthenticated`,
  `TestBot_StartStillRequiresAuth`.
- **Comprehensive `goon workflow init`.** Cycle-3:
  `internal/workflow/config.go::starterConfig()` writes a
  self-documenting starter with every hook key + populated educational
  echo commands. `examples/workflows/` library: minimal,
  engineering, engineering-stages, unattended, marketing-brief,
  sales-lead.
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
  markdown notes store (SOUL.md / topic notes) and append a HISTORY.md
  line via `internal/learnings.Capture` — failures here are non-fatal. Set `cfg.AutoApprove: true` in workflow.json or env
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
- **SOUL.md is auto-injected into `agent.SystemPrompt`.** Located at
  `./storage/memory/SOUL.md` (or `$GOON_MEMORY_DIR/SOUL.md`).
  Whitespace-only files are treated as absent — no empty banner.
  Renamed from PINNED.md for clarity — `notes.Store.Soul()` still
  reads the legacy `PINNED.md` filename transparently, and
  `SeedSoulTemplate()` auto-renames PINNED.md → SOUL.md on first
  call (one-shot migration). `notes.PinnedFilename` and `Store.Pinned()`
  are kept as deprecated aliases so out-of-tree code keeps compiling.
- **personal.md was folded into SOUL.md.** Previously goon shipped two
  always-loaded files — `personal.md` for character/voice and `SOUL.md`
  for project knowledge. Users found the split confusing ("which one
  do I edit?"). SOUL.md now holds both halves in one file, with the
  default template carrying a `## Character` section and a `## Project
  knowledge` section side-by-side. On boot, `notes.Store.MergePersonalIntoSoul()`
  detects a pre-existing `./storage/personal.md`, prepends its content
  into SOUL.md under "## Character (migrated from personal.md)" behind
  a dated banner, and renames the original to `personal.md.bak` so the
  user can verify the migration. The `internal/personal` package is a
  deprecated empty stub now — nothing imports it. Telegram `/personal`
  command became a one-line redirect pointing at `/knowledge` /
  `/memory edit SOUL.md`. Web Memory tab dropped the Personal segment;
  Knowledge tab (SOUL.md card) covers everything that used to be there.
  Env var `GOON_PERSONAL_FILE` removed from `.env.example`.
- **REPOSITORY.md is the canonical "where do my repos live" file.**
  Lives at `./storage/memory/REPOSITORY.md`. Markdown table format:
  `| Remote | Local | Notes |`. Read by `triageWithFeedback` so the
  LLM can suggest specific repos by name (the prompt embeds the raw
  body verbatim). Read by `buildRepoCandidates` so the confirm_repo
  gate's menu starts with the user's hand-curated list, then layers
  workspace + git-host repos underneath. The new `parseTriage` schema
  adds `needs_repo` (bool) + `repos` (array) so the LLM can:
  (a) classify a ticket as not needing a repo at all — research/docs/
  comms work skips confirm_repo + test + open_pr entirely; (b) pre-
  pick one or more repos that the gate then surfaces as `→ marked`
  recommended picks. Persisted on the workflow as `*bool NeedsRepo`
  (nil = legacy/pre-feature → assume true). Helpers: `memory.WorkflowNeedsRepo(wf)`,
  `repository.Lookup(name)`, `repository.RawBody()`, `repository.SeedDefault()`.
  CLI surface: `goon repo show/edit/scan/add` for REPOSITORY.md;
  `goon repo list/forget/clear` for the legacy learned mappings in
  memory.json. Auto-seed runs alongside personal/SOUL on first boot.
- **Repo selection is now strictly per-ticket.** The old
  `Memory.RepoChoices` cache (project key → single repo, written
  after a confirm_repo gate, read on every subsequent ticket to
  auto-skip the gate) was the bug behind "ENG-1 and ENG-2 forced to
  the same single repo." It's gone from the runtime hot path:
  `phaseConfirmRepo` no longer calls `lookupLearnedRepo`, no longer
  calls `rememberRepo` after a confirm, and `pickRepoForTicket` no
  longer consults `Memory.LookupRepoChoice` (it's just a soft hint
  built from `GOON_REPO_MAP` + ticket project key now). The
  `Memory.RecordRepoChoice` / `LookupRepoChoice` / `ForgetRepoChoice`
  methods + `memory.json` storage stay so legacy state loads cleanly
  but nothing writes fresh entries. `goon repo list / forget / clear`
  print a deprecation banner pointing at REPOSITORY.md and will
  silently drop any stale legacy entries the user wants cleared.
  The gate fires for EVERY ticket where `needs_repo=true` and
  autoApprove is off — each ticket gets its own multi-select.
- **Self-improvement loop (`internal/learnings`).** Every successful
  `agent.Run` from the one-shot CLI path now goes through
  `learnings.Capture(ctx, opts)` which (a) appends a single
  `YYYY-MM-DD HH:MM · task · outcome` line to `./storage/memory/HISTORY.md`
  and (b) fires a short follow-up agent task asking the LLM to distil
  durable knowledge into SOUL.md / topic notes via the existing
  `memory_*` tools. Same helper is called by `workflow.phaseUpdateMemory`
  so the daemon path and the one-shot path share one rule for what
  "remembering" means. Opt out with `GOON_AUTO_LEARN=0`. The mock LLM
  provider auto-skips the distillation step so tests stay fast and
  hermetic; HISTORY.md still gets the entry. The agent's system
  prompt mentions HISTORY.md so the LLM knows to consult it before
  re-trying something that's already been attempted.
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
