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
  or a preview with an **Ask for resume** button;
- whether to show the **Show raw thinking** button, and whether **Resume
  thinking** should summarize the reasoning (default) or continue the run;
- security switches for **custom answers**, **new messages**, and **resume
  thinking** (all off by default);
- send a test notification.

Every change is applied immediately through the daemon's loopback API.

## Security switches

OpenCode Telegram deliberately separates *notifications* from *actions*:

| Setting | Default | What it enables |
|---|---|---|
| `allow_custom_form_answers` | off | Adds **“Type your own response”** to OpenCode questions, so arbitrary text can be sent back into a form. |
| `allow_new_messages` | off | Lets free-text Telegram messages become prompts for the session bound to that chat. |
| `allow_resume_thinking` | off | Shows the **“Resume thinking”** button. In `continue` mode it starts a fixed OpenCode turn; in `summary` mode it only generates a summary. |

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
        "token": "optional-bearer-token"
      }
    }
  ]
}
```

Environment fallbacks: `OPENCODE_TELEGRAM_URL`,
`OPENCODE_TELEGRAM_TOKEN`.

If the daemon is not reachable, the plugin shows the URL it tried and suggests
`opencode-telegram start`.

## Requirements

- OpenCode v2.2 or newer.
- The `opencode-telegram` daemon running on the same machine (or reachable over
  a private network) with its loopback settings API enabled.
