# OpenCode Telegram

A TUI plugin that controls the `opencode-telegram` notification daemon from
inside OpenCode.

The daemon is the Go application at the root of this repository. This plugin
adds a `/telegram` slash command and a palette command,
**OpenCode Telegram notifications**, which opens a settings picker:

- master notification switch (off by default);
- which events to deliver: session finished, failed, interrupted, questions,
  permissions, retry status;
- whether to include child/subagent sessions, the last assistant message, and
  the working directory;
- how oversized responses are delivered: split into one message per text block,
  or a preview with **Summarize output**;
- whether raw thinking ends with a **Summarize thinking** button (on by
  default);
- security switches for **custom answers** and **new messages** (both off by
  default);
- send a test notification.

Every change is applied immediately through the daemon's loopback API.

## Footer icon

The plugin also adds a **✈ Telegram** item to the TUI footer status row (under
the prompt, also visible on the home screen). It shows the master notification
state at a glance:

- **on** — the daemon is running and notifications are enabled (green);
- **off** — notifications are disabled, or the daemon is not running (muted);
- **…** — the plugin is starting the daemon or applying a change.

Click or tap the icon to toggle notifications without opening the full
settings picker. If the daemon is not running, turning the icon on starts the
service first and then enables notifications. The icon refreshes every few
seconds, so changes made with `/telegram` are reflected immediately.

Opening `/telegram` also starts a stopped daemon automatically, so the
settings picker works without leaving OpenCode. If the service cannot be
started, the dialog shows the command output to help diagnose it.

## Security switches

OpenCode Telegram deliberately separates *notifications* from *actions*:

| Setting | Default | What it enables |
|---|---|---|
| `allow_custom_form_answers` | off | Adds **“Type your own response”** to OpenCode questions, so arbitrary text can be sent back into a form. |
| `allow_new_messages` | off | Lets free-text Telegram messages become prompts for the session bound to that chat. |

These are the only ways Telegram can inject content into OpenCode. If you do
not fully trust every Telegram account and device with access to the bot, leave
them off and answer questions inside OpenCode. The switches are intentionally
not exposed through the Telegram bot itself.

## Loading

Add the plugin to `opencode.json(c)` (project or global):

```jsonc
{
  "$schema": "https://opencode.ai/config.json",
  "plugins": ["/home/<you>/opencode-telegram/plugins/opencode-telegram"]
}
```

Or use a package name once published. The package has a no-op server part so
OpenCode loads the TUI part. Run `/reload` in the TUI after changing plugin
configuration.

The `opencode-tools` repository exposes this directory as a symlink at
`plugins/opencode-telegram`, so a config that points there also works.

## Daemon URL

The plugin talks to the daemon at `http://127.0.0.1:8787` by default. Override
it with plugin options or environment variables:

```jsonc
{
  "plugins": [
    {
      "package": "/home/<you>/opencode-telegram/plugins/opencode-telegram",
      "options": {
        "daemonUrl": "http://127.0.0.1:8787",
        "token": "optional-bearer-token",
        "autoStart": false,
        "startCommand": "opencode-telegram start"
      }
    }
  ]
}
```

Environment fallbacks: `OPENCODE_TELEGRAM_URL`,
`OPENCODE_TELEGRAM_TOKEN`, `OPENCODE_TELEGRAM_AUTOSTART`,
`OPENCODE_TELEGRAM_START_COMMAND`.

`autoStart` (off by default) starts the service when the plugin loads, without
changing the notification setting. `startCommand` overrides how the plugin
launches the service; the default tries `opencode-telegram start`, the
installed copy in `~/.local/bin`, `~/.local/share/opencode-telegram/scripts`,
then the `opencode-telegram` systemd user unit.

If the daemon cannot be started, the plugin reports the command output and
suggests running `opencode-telegram start` manually to see the full log.

## Requirements

- OpenCode v2.2 or newer.
- The `opencode-telegram` daemon running on the same machine (or reachable over
  a private network) with its loopback settings API enabled.
