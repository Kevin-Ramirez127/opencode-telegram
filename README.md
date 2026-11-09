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
│  service (v2 API)    │                              │  Go daemon             │
│                      │  REST: sessions, forms,      │                        │
│  sessions / forms /  │  permissions, prompts        │  state machine         │
│  permissions         │ ────────────────────────────► │  allowlist + settings  │
└──────────────────────┘                              └───────────┬────────────┘
                                                                  │ Telegram Bot API
                                                                  ▼
                                                      ┌────────────────────────┐
                                                      │  allowlisted chats     │
                                                      └────────────────────────┘
                                                                  ▲
                                                                  │ PATCH settings
                                                      ┌───────────┴────────────┐
                                                      │ OpenCode plugin (TUI)   │
                                                      │ /telegram               │
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
    "/home/<you>/telegram-pc-remote/plugins/opencode-telegram"
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
    "notify_chats": []
  },
  "security": {
    "allow_custom_form_answers": false,
    "allow_new_messages": false
  }
}
```

`notify_chats` empty means “every allowlisted chat”. Entries must themselves be
allowlisted; the API rejects anything else.

`include_child_sessions` is off by default: subagent/child sessions finish
often and usually should not page you.

### Security switches

| Setting | Default | Effect |
|---|---|---|
| `allow_custom_form_answers` | `false` | Adds **“Type your own response”** to questions. Without it, only the options offered by the form (or answering inside OpenCode) are possible. |
| `allow_new_messages` | `false` | Lets free-text Telegram messages become prompts for the chat's bound session. |

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
| `/interrupt` | Interrupt the bound session's current run |
| `/notify on\|off` | Toggle notification delivery |
| `/settings` | Show current notification/security settings |
| `/test` | Send a test notification to the allowed chats |
| `/id` | Show your Telegram user/chat ids |

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
```

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
scripts/daemon.sh           start/stop/logs/config/settings
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
