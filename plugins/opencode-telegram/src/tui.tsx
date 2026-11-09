import { Plugin } from "@opencode/plugin/tui"
import type { Context } from "@opencode/plugin/tui/context"

const DEFAULT_DAEMON = "http://127.0.0.1:8787"

type Notifications = {
  enabled: boolean
  session_done: boolean
  session_failed: boolean
  session_interrupted: boolean
  question_asked: boolean
  permission_asked: boolean
  retry: boolean
  include_child_sessions: boolean
  include_last_message: boolean
  include_directory: boolean
  notify_chats: number[]
}

type Security = {
  allow_custom_form_answers: boolean
  allow_new_messages: boolean
}

type Settings = {
  notifications: Notifications
  security: Security
  updated_at?: number
}

type Status = {
  ok: boolean
  opencode_ok: boolean
  opencode_url?: string
  opencode_version?: string
  active_sessions: number
  pending_forms: number
  pending_permissions: number
  last_error?: string
}

type SettingsResponse = {
  settings: Settings
  status: Status
}

type Choice = {
  title: string
  value: string
  description?: string
}

function env(name: string): string | undefined {
  const proc = (globalThis as { process?: { env?: Record<string, string | undefined> } }).process
  return proc?.env?.[name]
}

function daemonUrl(context: Context): string {
  const option = (context.options as Record<string, unknown> | undefined)?.daemonUrl
  const value = typeof option === "string" && option.trim() ? option : env("OPENCODE_TELEGRAM_URL") ?? DEFAULT_DAEMON
  return value.replace(/\/+$/, "")
}

function authHeaders(context: Context): Record<string, string> {
  const option = (context.options as Record<string, unknown> | undefined)?.token
  const token = typeof option === "string" && option.trim() ? option : env("OPENCODE_TELEGRAM_TOKEN")
  return token ? { authorization: `Bearer ${token}` } : {}
}

async function request(
  context: Context,
  method: string,
  path: string,
  body?: unknown,
): Promise<Record<string, unknown>> {
  const fetchFn = (globalThis as { fetch?: (input: string, init?: unknown) => Promise<unknown> }).fetch
  if (typeof fetchFn !== "function") throw new Error("fetch is not available in this runtime")

  const response = (await fetchFn(daemonUrl(context) + path, {
    method,
    headers: {
      accept: "application/json",
      ...(body === undefined ? {} : { "content-type": "application/json" }),
      ...authHeaders(context),
    },
    body: body === undefined ? undefined : JSON.stringify(body),
  })) as {
    ok: boolean
    status: number
    text: () => Promise<string>
  }

  const text = await response.text()
  let data: Record<string, unknown> = {}
  if (text) {
    try {
      data = JSON.parse(text) as Record<string, unknown>
    } catch {
      data = { error: text }
    }
  }
  if (!response.ok) {
    throw new Error(typeof data.error === "string" ? data.error : `HTTP ${response.status}`)
  }
  return data
}

async function load(context: Context): Promise<SettingsResponse> {
  return (await request(context, "GET", "/api/plugin/settings")) as unknown as SettingsResponse
}

function patchNotifications(context: Context, patch: Partial<Notifications>) {
  return request(context, "PATCH", "/api/plugin/settings", { notifications: patch })
}

function patchSecurity(context: Context, patch: Partial<Security>) {
  return request(context, "PATCH", "/api/plugin/settings", { security: patch })
}

function stateLabel(value: boolean): string {
  return value ? "[on]" : "[off]"
}

function errText(error: unknown): string {
  return error instanceof Error ? error.message : String(error)
}

async function toastError(context: Context, error: unknown) {
  context.ui.toast.show({
    title: "OpenCode Telegram",
    message: errText(error),
    variant: "error",
    duration: 6000,
  })
}

async function toggleSecurity(
  context: Context,
  key: keyof Security,
  enable: boolean,
): Promise<boolean> {
  if (enable) {
    const confirm = await context.ui.dialog.confirm({
      title: key === "allow_new_messages" ? "Allow new messages?" : "Allow custom answers?",
      message:
        key === "allow_new_messages"
          ? "This lets any allowlisted Telegram user send arbitrary prompts to the OpenCode session bound to their chat. Only enable it if you trust every account and device that can reach this bot."
          : "This adds a \"Type your own response\" option to OpenCode questions, allowing arbitrary text to be sent back into a form. Only enable it if you trust every account and device that can reach this bot.",
      label: { confirm: "Enable", cancel: "Cancel" },
    })
    if (!confirm) return false
  }
  await patchSecurity(context, { [key]: enable } as Partial<Security>)
  return true
}

function buildOptions(settings: Settings, status: Status | undefined): Choice[] {
  const n = settings.notifications
  const s = settings.security
  const statusLabel = status?.opencode_ok ? "OpenCode connected" : "OpenCode disconnected"
  return [
    {
      title: `Notifications ${stateLabel(n.enabled)}`,
      value: "toggle_enabled",
      description: `${statusLabel} — master switch; everything is off by default`,
    },
    {
      title: `Session finished ${stateLabel(n.session_done)}`,
      value: "toggle_session_done",
      description: "A normal session run completed",
    },
    {
      title: `Session failed ${stateLabel(n.session_failed)}`,
      value: "toggle_session_failed",
      description: "The agent loop ended with an error",
    },
    {
      title: `Session interrupted ${stateLabel(n.session_interrupted)}`,
      value: "toggle_session_interrupted",
      description: "A run was interrupted or superseded",
    },
    {
      title: `Questions ${stateLabel(n.question_asked)}`,
      value: "toggle_question_asked",
      description: "The agent created a form/question that needs a human answer",
    },
    {
      title: `Permissions ${stateLabel(n.permission_asked)}`,
      value: "toggle_permission_asked",
      description: "The agent is waiting for a permission decision",
    },
    {
      title: `Retry status ${stateLabel(n.retry)}`,
      value: "toggle_retry",
      description: "Provider retry notifications (usually noisy)",
    },
    {
      title: `Include child sessions ${stateLabel(n.include_child_sessions)}`,
      value: "toggle_include_child_sessions",
      description: "Also notify for subagent/child sessions (off by default)",
    },
    {
      title: `Include last message ${stateLabel(n.include_last_message)}`,
      value: "toggle_include_last_message",
      description: "Attach the last assistant text to finished-session notifications",
    },
    {
      title: `Include directory ${stateLabel(n.include_directory)}`,
      value: "toggle_include_directory",
      description: "Show the session working directory",
    },
    {
      title: `Security: custom answers ${stateLabel(s.allow_custom_form_answers)}`,
      value: "toggle_custom_answers",
      description: "Adds \"Type your own response\" to questions; off by default",
    },
    {
      title: `Security: new messages ${stateLabel(s.allow_new_messages)}`,
      value: "toggle_new_messages",
      description: "Allow free-text Telegram messages to become OpenCode prompts; off by default",
    },
    {
      title: "Send a test notification",
      value: "test",
      description: "Ask the daemon to message every allowlisted chat",
    },
    {
      title: "Close",
      value: "close",
    },
  ]
}

async function handleChoice(context: Context, value: string): Promise<boolean> {
  switch (value) {
    case "close":
      return false
    case "toggle_enabled": {
      const data = await load(context)
      await patchNotifications(context, { enabled: !data.settings.notifications.enabled })
      return true
    }
    case "toggle_session_done":
    case "toggle_session_failed":
    case "toggle_session_interrupted":
    case "toggle_question_asked":
    case "toggle_permission_asked":
    case "toggle_retry":
    case "toggle_include_child_sessions":
    case "toggle_include_last_message":
    case "toggle_include_directory": {
      const key = value.replace("toggle_", "") as keyof Notifications
      const data = await load(context)
      const current = data.settings.notifications[key]
      if (typeof current !== "boolean") return true
      await patchNotifications(context, { [key]: !current } as Partial<Notifications>)
      return true
    }
    case "toggle_custom_answers": {
      const data = await load(context)
      const current = data.settings.security.allow_custom_form_answers
      await toggleSecurity(context, "allow_custom_form_answers", !current)
      return true
    }
    case "toggle_new_messages": {
      const data = await load(context)
      const current = data.settings.security.allow_new_messages
      await toggleSecurity(context, "allow_new_messages", !current)
      return true
    }
    case "test": {
      const result = await request(context, "POST", "/api/plugin/test-notification")
      context.ui.toast.show({
        title: "OpenCode Telegram",
        message: `Test notification sent to ${String(result.sent ?? 0)} chat(s).`,
        variant: "success",
      })
      return true
    }
    default:
      return false
  }
}

async function openSettings(context: Context) {
  for (;;) {
    let data: SettingsResponse
    try {
      data = await load(context)
    } catch (error) {
      await context.ui.dialog.alert({
        title: "OpenCode Telegram",
        message:
          `Could not reach the daemon at ${daemonUrl(context)}.\n\n` +
          `Start it with "opencode-telegram start" and try again.\n\n${errText(error)}`,
      })
      return
    }

    const status = data.status
    const subtitle = status?.opencode_ok ? "OpenCode connected" : "OpenCode disconnected"
    const choice = await context.ui.dialog.select({
      title: `OpenCode Telegram — ${subtitle}`,
      options: buildOptions(data.settings, status),
    })
    if (!choice) return

    try {
      const again = await handleChoice(context, choice)
      if (!again) return
    } catch (error) {
      await toastError(context, error)
    }
  }
}

export default Plugin.define({
  id: "opencode-telegram",
  setup(context) {
    context.keymap.layer(() => ({
      mode: "global",
      priority: 10,
      commands: [
        {
          id: "opencode-telegram.settings",
          title: "OpenCode Telegram notifications",
          group: "Notifications",
          palette: true,
          slash: { name: "telegram", aliases: ["tg"] },
          suggested: true,
          run: () => openSettings(context),
        },
      ],
    }))
  },
})
