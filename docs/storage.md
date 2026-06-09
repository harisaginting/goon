# Where goon keeps its files (`./storage/`)

goon stores all of its state in a single folder called **`storage/`**, created
in whatever directory you run goon from. Nothing is written to your home folder
or anywhere global — delete `./storage/` and goon starts fresh.

> Want it somewhere else? Set `GOON_STORAGE_DIR=/absolute/path` (for example
> `GOON_STORAGE_DIR=$HOME/.goon` to share one state folder across projects).

---

## The layout

```
./storage/
├── memory.json          ← machine state goon manages for you (don't hand-edit)
├── usage.json           ← token usage counter, per model
├── goon.pid             ← present only while the daemon is running
├── logs/
│   └── goon.log         ← rotated, structured run log
├── memory/              ← notes goon reads & writes in plain Markdown (editable)
│   ├── SOUL.md          ← persona / standing context, injected into every prompt
│   ├── HISTORY.md       ← append-only log of past tasks + distilled lessons
│   ├── REPOSITORY.md    ← table mapping remote repos → local checkouts
│   └── LEARNED.md       ← durable findings from idle self-learning
└── skills/              ← specialist Markdown "skills" (loaded on demand)
    └── *.md
```

---

## What each one is for

| Path | What it holds | Edit by hand? |
|---|---|---|
| `memory.json` | The daemon's working state: the tickets it has seen, open workflows, your pending questions/approvals, daemon status, Telegram auth. | No — goon owns it. |
| `usage.json` | Running token counts per model (shown on the dashboard). | No. |
| `goon.pid` | The process id of the running daemon. Exists only while `goon start` is live; removed on stop. | No. |
| `logs/goon.log` | Structured logs (auto-rotated). Check here when something misbehaves. | Read-only. |
| `memory/SOUL.md` | Persona and standing instructions goon injects into every system prompt. Put "always do X, our stack is Y" here. | **Yes** — this is yours. |
| `memory/HISTORY.md` | Append-only record of completed tasks plus short distilled lessons. | Yes (mostly goon appends). |
| `memory/REPOSITORY.md` | `\| Remote \| Local \| Notes \|` table telling goon where each repo is checked out. Also editable from the **Repositories** tab. | Yes. |
| `memory/LEARNED.md` | Findings goon writes while idle (the self-learning loop). | Yes. |
| `skills/*.md` | Optional specialist playbooks goon can pull in for specific work. Not auto-injected. | Yes. |

**memory.json vs the memory/ folder:** `memory.json` is bookkeeping goon manages
automatically. The `memory/` *folder* is human-readable knowledge in Markdown —
if you'd hand-edit it, it lives there.

---

## Not under `./storage/`

A couple of files live in your project root, not in `storage/`:

- **`config.json`** — your settings and secrets (API keys, board, Google creds).
- **`workflow.json`** — your custom pipeline, if you've defined one.

---

## Moving or resetting things

Each location can be redirected with an env var if you need to:

| Env var | Overrides |
|---|---|
| `GOON_STORAGE_DIR` | the whole `storage/` root |
| `GOON_LOG_FILE` | `logs/goon.log` |
| `GOON_MEMORY_PATH` | `memory.json` |
| `GOON_MEMORY_DIR` | the `memory/` notes folder |
| `GOON_PID_FILE` | `goon.pid` |
| `GOON_WORKFLOW_FILE` | `workflow.json` |

To wipe everything and start clean, stop goon and delete `./storage/`. Your
`config.json` is separate, so your settings survive.
