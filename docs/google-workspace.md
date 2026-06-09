# Connect goon to your Google account

This lets you **ask goon about your Google Workspace** right in the Chat tab:

- "what meetings do I have today?"
- "what's my schedule this afternoon?"
- "what are my tasks?" / "what's on my plate today?"

It's **read-only** — goon can *look* at your calendar and tasks, but it can't
change, send, or delete anything.

> You only do the setup below **once**. It takes ~10 minutes. After that, you
> just ask goon things in chat.

---

## Part 1 — Get a "key" from Google (one time)

Google won't let an app read your calendar/tasks until you create a little
"app key" and say "yes, I allow it." Here's exactly what to click.

### 1. Open the Google Cloud Console
Go to **https://console.cloud.google.com** and sign in with the **same Google
account** whose calendar/tasks you want goon to read.

### 2. Make a project (a folder for your key)
- Top-left, click the **project dropdown** (says "Select a project").
- Click **New Project** → name it `goon` → **Create**.
- Wait a few seconds, then make sure `goon` is the **selected** project (top-left).

### 3. Turn on the things goon will read
For each of these, use the search bar at the top, open it, and click **Enable**:
- **Google Calendar API**
- **Google Tasks API**
- **Gmail API** *(only needed later, for the email feature — fine to enable now)*
- **Cloud Logging API** *(only needed later, for the log-search feature)*

(Search the name → click the result → big blue **Enable** button.)

### 4. Set up the "consent screen" (who's allowed)
- Search **OAuth consent screen** → open it.
- Choose **External** → **Create**.
- Fill the required boxes:
  - **App name:** `goon`
  - **User support email:** your email
  - **Developer contact email:** your email
- Click **Save and Continue** through the next pages (you can skip Scopes —
  click **Save and Continue**).
- On the **Test users** page, click **+ Add Users**, type **your own email**,
  click **Add** → **Save and Continue**.

> Why test users? Your app stays in "Testing" mode, which is perfect for personal
> use. Adding yourself means Google lets *you* use it without a lengthy review.

### 5. Create the actual key
- Search **Credentials** → open it.
- Click **+ Create Credentials** → **OAuth client ID**.
- **Application type:** choose **Desktop app**.
- Name it `goon` → **Create**.
- A box pops up with **Client ID** and **Client secret**. Keep this open (or
  click **Download JSON**) — you'll paste these into goon next.

✅ That's the Google side done.

---

## Part 2 — Tell goon the key, then connect

In your terminal, in the goon folder:

```sh
goon config set GOOGLE_OAUTH_CLIENT_ID     <paste the Client ID>
goon config set GOOGLE_OAUTH_CLIENT_SECRET <paste the Client secret>
# optional, only for the log-search feature later:
goon config set GOOGLE_CLOUD_PROJECT       <your project id, e.g. goon-123456>
```

Now connect (this opens your browser for a one-time "Allow"):

```sh
goon google auth
```

A browser tab opens. Two things to know:

1. You'll likely see a scary-looking **"Google hasn't verified this app"** screen.
   That's normal for a personal app. Click **Advanced** → **Go to goon (unsafe)**.
   (It's your own app — it's safe.)
2. Click **Allow** / **Continue** to grant read-only access.

When it says **"✓ goon is connected to Google"**, close the tab. Back in the
terminal you'll see **"✓ Connected."** Done.

---

## Part 3 — Use it

Open the **Chat** tab (or Telegram) and just ask. goon picks the right tool
from your sentence — you don't type any commands.

**Calendar & tasks**
- *"What meetings do I have today?"*
- *"Am I busy this afternoon?"*
- *"What are my tasks?"*
- *"What's on my plate today?"* (goon combines Google Tasks with your Jira work)

**Email** (read-only)
- *"Check my email from finance, last week."*
- *"Any unread from my manager?"*
- *"Open that one and tell me what it says."*

**Logs** (Google Cloud Logging — needs the extra step below)
- *"Check the log when user `harisa` registered."*
- *"Get the traceId for the login of username `harisa`."*
- *"Search the logs for `payment failed` in the last 6 hours."*
- *"Any errors in the logs today?"*

goon answers in plain language from your real data.

### Turning on log search (optional)

Log search reads **Google Cloud Logging** (the "Log Explorer"). Two things:

1. You already enabled the **Cloud Logging API** in Part 1, step 3.
2. Tell goon which project's logs to read:

```sh
goon config set GOOGLE_CLOUD_PROJECT <your project id, e.g. my-app-123456>
```

That's it — now the "logs" questions above work. (Your project id is shown in
the Google Cloud Console project dropdown.)

---

## If something goes wrong

| What you see | Fix |
|---|---|
| `GOOGLE_OAUTH_CLIENT_ID / SECRET not set` | You skipped Part 2's first two `config set` commands. |
| Browser: **"Access blocked: goon has not completed verification"** | You forgot **Test users** in step 4 — add your email there. |
| Browser: **"Access blocked: this app's request is invalid"** / redirect error | Your OAuth client must be type **Desktop app** (step 5), not "Web application". |
| `Google did not return a refresh token` | Re-run `goon google auth`; on the consent screen make sure you click **Allow**. |
| In chat: `403` / `API not enabled` | You missed **Enable** for that API in step 3 — enable it and ask again. |
| `goon: not connected — run 'goon google auth'` | The connect step didn't finish. Run `goon google auth` again. |

---

## Privacy & safety

- **Read-only.** goon requests only "view" permissions — it cannot send email,
  change events, or complete/delete tasks.
- Your tokens are stored locally in goon's `config.json` on **your** machine and
  are masked in the Setup screen. They aren't sent anywhere except Google.
- You can disconnect anytime: revoke access at
  **https://myaccount.google.com/permissions** (find "goon" → Remove), or run
  `goon config set GOOGLE_OAUTH_REFRESH_TOKEN ""`.

---

## What goon can read today

All read-only, all on the one connection above:

- **Calendar** — today's schedule, "am I free…".
- **Tasks** — your open Google Tasks (folded together with Jira for "my day").
- **Gmail** — search and read your mail ("from finance last week", "open it").
- **Cloud Logging** — search logs, find a register/login event, pull a traceId
  (needs `GOOGLE_CLOUD_PROJECT`, see Part 3).

_Not yet (deliberately): sending email, creating events, changing tasks. When
those land they'll always ask you to confirm first._
