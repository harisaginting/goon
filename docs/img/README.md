# Screenshot directory

The repo's `README.md` references images in this folder. They aren't
checked in yet — capture them from your live `goon start --web=:8080`
dashboard and drop them here. Suggested set + sizes (1440×900 viewport
gives a crisp 16:10 frame):

| Path                       | What to capture |
|----------------------------|------------------|
| `home.png`                 | Home tab — stats strip, live workflow, recent tickets |
| `repositories.png`         | Repositories tab — repo cards with open-PR badges, one expanded showing PRs + map-to-local + clone form |
| `questions.png`            | Questions tab — an approval gate card with the repo-pick checkboxes |
| `workflows.png`            | Workflows tab — the new config band ("Workflow: default · ⏸ gated") + a few in-flight workflow cards |
| `chat.png`                 | Chat tab — left rail with past threads, repo-context picker, an active conversation |
| `tickets.png`              | Tickets tab — the action drawer expanded for one ticket, showing comment / move / edit / ignore controls |
| `telegram.png`             | Optional: a Telegram DM with the goon bot showing the `/prs` list or an AI-drafted review |

How to capture on macOS: `cmd + shift + 4` then drag, or use the
browser's built-in screenshot from devtools for an exact viewport
crop. PNG is preferred (lossless), JPEG is fine if file size matters.
