# opencode-telegram

A Telegram daemon for [OpenCode](https://opencode.ai). It listens to OpenCode's
live event stream for **all sessions** and notifies your allowlisted Telegram
chats when:

- a session finishes, fails, or is interrupted;
- the agent asks a **question** (an OpenCode form that needs a human answer);
- the agent requests a **permission**;
- (optionally) a provider retry is scheduled.

When you explicitly enable it, the same chat can answer questions, decide
permissions, bind to a session, and send new prompts to OpenCode.

Notifications are **off by default**. Arbitrary answers and new prompts are
**off by default** too, because a Telegram account is not a trusted prompt
source.

Backend: **Go**. Control/install scripts: **bash**.

---

## Architecture

```
┌──────────────────────┐        SSE /api/event         ┌────────────────────────┐
│  OpenCode background │ ◄──────────────────────────── │  opencode-telegram     │
│  service (v2 API)    │                               │  Go daemon             │
│                      │  REST: sessions, forms,       │                        │
│  sessions / forms /  │  permissions, prompts         │  state machine         │
│  permissions         │ ────────────────────────────► │  allowlist + settings  │
└──────────────────────┘                               └───────────┬────────────┘
                                                                   │ Telegram Bot API
                                                                   ▼
                                                      ┌────────────────────────┐
                                                      │  allowlisted chats     │
                                                      └────────────────────────┘
                                                                  ▲
                                                                  │ PATCH settings
                                                      ┌───────────┴────────────┐
                                                      │ OpenCode plugin (TUI)  │
                                                      │ /telegram              │
                                                      └────────────────────────┘
```

The daemon discovers the local OpenCode service from
`~/.local/state/opencode/service.json`, authenticates with its password, and
subscribes to the filtered event stream. It reconnects with backoff and
reconciles pending forms/permissions after a reconnect.

The companion plugin lives in this repository at
`plugins/opencode-telegram`. The
[`opencode-tools`](https://github.com/Kevin-Ramirez127/opencode-tools)
repository links to it with a `plugins/opencode-telegram` symlink, so existing
config paths there keep working. The plugin edits the daemon settings through
a loopback HTTP API.

---

## Install

Requirements: Go >= 1.23, `jq`, and a Telegram bot token from
[@BotFather](https://t.me/BotFather).

```bash
git clone <this repo>
cd <this repo>
./install.sh
```

The installer builds `bin/opencode-telegram`, installs a self-contained copy to
`~/.local/share/opencode-telegram`, and creates the
`~/.local/bin/opencode-telegram` command.

```bash
# 1. Allowlist yourself (numeric IDs from @userinfobot)
opencode-telegram config --adduser <your-user-id>

# 2. Start the daemon
opencode-telegram start
opencode-telegram logs -f

# 3. Status / reload / stop
opencode-telegram status
opencode-telegram reload
opencode-telegram stop
```

For a persistent user service:

```bash
./install.sh --systemd
# or copy packaging/opencode-telegram.service manually and:
systemctl --user enable --now opencode-telegram
```

### Configuration

Copy `.env.example` to `.env`, or use the installed
`~/.local/share/opencode-telegram/.env`:

```ini
TELEGRAM_BOT_TOKEN=...

# Optional: omit to auto-discover the local OpenCode service.
# OPENCODE_URL=http://127.0.0.1:4096
# OPENCODE_PASSWORD=...

# Loopback settings API used by the plugin.
# LISTEN_ADDR=127.0.0.1:8787
# PLUGIN_TOKEN=
# DATA_DIR=./data
```

Discovery paths, in order:

1. `OPENCODE_SERVICE_FILE` (if set);
2. `$XDG_STATE_HOME/opencode/service.json` or
   `~/.local/state/opencode/service.json`;
3. `$XDG_CONFIG_HOME/opencode/service.json` or
   `~/.config/opencode/service.json`.

`OPENCODE_URL` / `OPENCODE_PASSWORD` override discovery.

---

## Allowlist

`data/allowlist.json` requires **both** the sending user **and** the chat:

```json
{
  "users": [123456789],
  "chats": [123456789]
}
```

In a DM the user id and chat id are the same, so `config --adduser` adds the id
to both lists. For a group/channel use `config --addchat <chat-id>` as well.

`data/allowlist.json` and `data/settings.json` are local state and are
gitignored; the tracked templates are `data/allowlist.example.json` and
`data/settings.example.json`. `./install.sh` seeds the local files on first
install.

The daemon is fail-closed: a missing or corrupt file denies everyone, and
unauthorized updates are dropped silently.

Manage it without editing JSON:

```bash
opencode-telegram config --adduser 123456789
opencode-telegram config --addchat -1001234567890
opencode-telegram config --list
opencode-telegram config --deluser 123456789
```

Changes are reloaded automatically (SIGHUP is sent by the config command).

---

## Settings and the plugin

Notifications are controlled from the OpenCode plugin:

```jsonc
// ~/.config/opencode/opencode.jsonc
{
  "$schema": "https://opencode.ai/config.json",
  "plugins": [
    "/home/<you>/opencode-telegram/plugins/opencode-telegram"
  ]
}
```

Then run `/telegram` in the TUI (or the palette command **OpenCode Telegram
notifications**). It edits `data/settings.json` through the daemon API:

```json
{
  "notifications": {
    "enabled": false,
    "session_done": true,
    "session_failed": true,
    "session_interrupted": true,
    "question_asked": true,
    "permission_asked": true,
    "retry": false,
    "include_child_sessions": false,
    "include_last_message": true,
    "include_directory": true,
    "notify_chats": [],
    "long_message_mode": "split",
    "show_summarize_thinking": true
  },
  "security": {
    "allow_custom_form_answers": false,
    "allow_new_messages": false,
    "allow_model_switching": false
  }
}
```

The footer **✈ Telegram** icon shows **off** whenever the daemon is stopped.
Clicking it (or opening `/telegram`) starts the daemon through
`opencode-telegram start`; if the icon was off, it also enables notifications.
Set the plugin option `autoStart` to `true` to start the service when the
plugin loads.

`notify_chats` empty means “every allowlisted chat”. Entries must themselves be
allowlisted; the API rejects anything else.

`include_child_sessions` is off by default: subagent/child sessions finish
often and usually should not page you.

### Long messages

Notifications and bot replies are sent with Telegram `sendRichMessage`, so
headings, lists, tables, links, and code blocks in OpenCode output render
natively instead of appearing as raw Markdown. If the Telegram server does not
support rich messages, the daemon falls back to plain text automatically.

Telegram rejects regular messages over 4096 bytes. `long_message_mode` decides
what the daemon does when the last assistant message would not fit:

| Value | Behavior |
|---|---|
| `split` (default) | Send one message per assistant text block. A single oversized block is split at paragraph/line boundaries. Nothing is silently truncated. |
| `summary` | If the message is oversized, send a short preview instead of splitting it. The **Summarize output** button is available on every notification with assistant output, regardless of mode. |

### Output and thinking buttons

Notifications that include assistant output get:

- **📄 Summarize output** — asks OpenCode to summarize that exact output and
  sends the summary. The request uses the session's configured model and does
  not add a message to the session history.

When the assistant message carries reasoning, notifications also get:

- **🧠 Show thinking** — sends the raw reasoning text. Long reasoning is split
  the same way as normal text.

The raw-thinking message ends with:

- **📝 Summarize thinking** — asks OpenCode to summarize that exact reasoning
  block. It is enabled by default and can be turned off with
  `show_summarize_thinking` in the plugin.

Every button targets the assistant message that produced the notification, and
is single-use so repeated taps cannot start multiple generations.

### Security switches

| Setting | Default | Effect |
|---|---|---|
| `allow_custom_form_answers` | `false` | Adds **“Type your own response”** to questions. Without it, only the options offered by the form (or answering inside OpenCode) are possible. |
| `allow_new_messages` | `false` | Lets free-text Telegram messages become prompts for the chat's bound session. |
| `allow_model_switching` | `false` | Lets `/models` change the model used by the bound session. Without it, `/models` explains that the configuration does not allow choosing models. |

They are intentionally **not** exposed as Telegram commands: a compromised
Telegram session must not be able to grant itself prompt-injection rights.
Change them from the plugin (which asks for confirmation) or by editing
`data/settings.json` locally.

---

## Telegram commands

| Command | Meaning |
|---|---|
| `/start`, `/help` | Command list and security notes |
| `/status` | Daemon, OpenCode, allowlist, and pending-work status |
| `/sessions` | Recent sessions with bind buttons |
| `/bind <session-id>` | Bind this chat to a session |
| `/unbind` | Remove the binding |
| `/agents [agent-id]` | List or switch the bound session's agent |
| `/models` | List every available chat model with a switch button for each (requires `allow_model_switching`) |
| `/usage` | Show OpenCode Go and Console usage limits |
| `/interrupt` | Interrupt the bound session's current run |
| `/notify on\|off` | Toggle notification delivery |
| `/settings` | Show current notification/security settings |
| `/test` | Send a test notification to the allowed chats |
| `/id` | Show your Telegram user/chat ids |
| `/exit` | Stop the daemon, after an inline **Stop daemon** confirmation |
| `/poweroff` | Power off the computer, after an inline **Power off** confirmation |

`/exit` only stops the `opencode-telegram` process; OpenCode keeps running.
`/poweroff` asks the operating system to shut down the machine. Both commands
are available only to allowlisted users/chats, and neither runs until the
confirmation button is pressed. Cancel buttons dismiss the request.

`/agents` lists the primary agents for the bound session and lets you switch
with a button (or pass an agent id directly). `/models` lists every enabled
chat model with one switch button per model, marking the model currently
selected for the bound session; it requires `allow_model_switching`, otherwise
it replies that the configuration does not allow choosing models. Both commands
show a temporary loading message if the OpenCode API takes longer than 250 ms.
Finished-session notifications include an `Agent:` line showing the agent that
handled the run.

`/usage` reads the OpenCode Console usage-limits API and renders the workspace,
rolling/weekly/monthly OpenCode Go percentages, reset times, and any Console
budgets as Markdown tables. API failures are reported in the chat.

The daemon also adds its commands to the bot menu if they are missing. Existing
bot commands are preserved, so commands registered with BotFather are not
overwritten.

After `/sessions` + bind, free text is sent to that session **only when**
`allow_new_messages` is on. Otherwise the bot replies with a reminder.

### Question flow

When OpenCode creates a form:

1. the daemon posts the question with inline buttons for options, boolean
   fields, or multiselect toggles;
2. if `allow_custom_form_answers` is on, a **“Type your own response”** button
   starts a one-shot text prompt;
3. the answer is submitted to the exact form/session via
   `POST /api/session/{id}/form/{formID}/reply`;
4. the form is removed when OpenCode reports `form.replied` or
   `form.cancelled`.

Multi-field forms are walked field by field. Optional fields get a **Skip**
button.

### Permission flow

Permission requests show **Allow once**, **Always allow**, and **Reject**
buttons. The decision is sent to
`POST /api/session/{id}/permission/{requestID}/reply`.

---

## Local settings API

The daemon listens on `127.0.0.1:8787` by default. With `PLUGIN_TOKEN` set,
every request needs `Authorization: Bearer <token>`.

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/health` | Daemon/OpenCode status |
| `GET` | `/api/plugin/status` | Same status, plugin-friendly name |
| `GET` | `/api/plugin/settings` | `{settings, status}` |
| `PATCH` | `/api/plugin/settings` | Partial settings update |
| `POST` | `/api/plugin/test-notification` | Send a test message |
| `GET` | `/api/plugin/sessions` | Recent sessions |

Example patch:

```bash
curl -X PATCH http://127.0.0.1:8787/api/plugin/settings \
  -H 'content-type: application/json' \
  -d '{"notifications":{"enabled":true}}'
```

---

## How completion is detected

The daemon subscribes to these OpenCode events (plus the always-sent
`server.connected` marker):

```
session.status
session.idle
session.execution.started
session.execution.succeeded
session.execution.failed
session.execution.interrupted
form.created / form.replied / form.cancelled
permission.asked / permission.replied
session.created / session.renamed / session.deleted
```

`session.execution.*` is the primary outcome signal; `session.idle` is the
fallback for runs that only report idleness. A per-session flag prevents the
usual `execution.succeeded` → `idle` pair from producing two notifications.
On reconnect, active sessions and pending forms/permissions are reconciled.

---

## Development

```bash
make build       # bin/opencode-telegram
make test        # go test ./...
make vet
make fmt
make run         # foreground
make check       # validate config + connectivity
make simulate    # send premade conversations to Telegram
```

### Simulating Telegram output

The simulator sends real, premade OpenCode conversations to the allowlisted
Telegram chats without creating an OpenCode session:

```bash
./scripts/simulate.sh list          # scenario names
./scripts/simulate.sh               # everything
./scripts/simulate.sh text          # rich Markdown formatting
./scripts/simulate.sh done          # finished-session notification
./scripts/simulate.sh question1     # question with 1 field
./scripts/simulate.sh question2     # question with 2 fields
./scripts/simulate.sh question3     # question with 3 fields
./scripts/simulate.sh permission    # permission request
./scripts/simulate.sh text,done     # comma-separated scenarios
```

The installed command exposes the same thing:

```bash
opencode-telegram simulate question3
```

Question, permission, thinking, and summary buttons stay active while the
simulator process runs. Stop the real daemon first: Telegram delivers each
update to only one poller, so a running daemon would consume the simulated
button presses. The simulator uses a temporary settings file and a fake
OpenCode API, so it never changes your real settings or session state.

Layout:

```
main.go                     wiring, flags, discovery
internal/config             env/.env + paths
internal/occlient           OpenCode API client, SSE stream, discovery
internal/app                daemon state, Telegram, forms, permissions, HTTP API
internal/settings           settings file + validation
internal/whitelist          fail-closed user/chat allowlist
internal/ratelimit          one update per chat per second
internal/safeio             atomic 0600 writes
plugins/opencode-telegram   OpenCode TUI settings plugin
data/*.example.json         tracked allowlist/settings templates
data/*.json                 local state (gitignored)
scripts/daemon.sh           start/stop/logs/config/settings/simulate
scripts/simulate.sh         premade Telegram QA conversations
packaging/                  systemd user unit
```

---

## Security notes

- **Two-key gate:** a message is accepted only if both the user and the chat
  are allowlisted. Unauthorized updates get no response at all.
- **Deny-all on error:** missing/corrupt allowlist = nobody can use the bot.
- **Notifications off:** no session data leaves the machine until enabled.
- **No arbitrary injection by default:** custom answers and new prompts are
  separate opt-ins.
- **Loopback API:** the settings API binds to `127.0.0.1`; add a
  `PLUGIN_TOKEN` if other local users are a concern, and do not expose it to
  the internet.
- **No secrets in logs:** tokens and passwords are never logged.
- **Rate limiting:** at most one typed message per chat per second. Callback
  presses (buttons) are not throttled here so multiselect forms stay usable.
- **Callback tokens:** Telegram `callback_data` is a short random token mapped
  server-side, so no session/question content is embedded in buttons.

See [`safe_bot_advices.md`](safe_bot_advices.md) for bot-specific hygiene.

## License

GPL-3.0. See [LICENCE](LICENCE).
