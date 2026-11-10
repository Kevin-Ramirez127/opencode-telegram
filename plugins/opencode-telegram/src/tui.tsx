import { Plugin } from "@opencode/plugin/tui"
import type { Context } from "@opencode/plugin/tui/context"
import { createSignal } from "solid-js"

const DEFAULT_DAEMON = "http://127.0.0.1:8787"
const TELEGRAM_ICON = "✈"
const STATUS_POLL_MS = 10_000
const START_TIMEOUT_MS = 15_000
const START_POLL_MS = 400
const START_COMMAND_TIMEOUT_MS = 20_000
const HEALTH_TIMEOUT_MS = 1_500

// Tried in order when the daemon is unreachable. The installer puts the
// command in ~/.local/bin; the direct paths cover setups where PATH does not
// include it yet, and systemd covers persistent user-service installs.
const DEFAULT_START_COMMAND = [
  'if command -v opencode-telegram >/dev/null 2>&1; then exec opencode-telegram start; fi',
  'if [ -x "$HOME/.local/bin/opencode-telegram" ]; then exec "$HOME/.local/bin/opencode-telegram" start; fi',
  'if [ -x "$HOME/.local/share/opencode-telegram/scripts/daemon.sh" ]; then exec "$HOME/.local/share/opencode-telegram/scripts/daemon.sh" start; fi',
  'if command -v systemctl >/dev/null 2>&1; then exec systemctl --user start opencode-telegram; fi',
  'echo "opencode-telegram is not installed; run ./install.sh or set the plugin startCommand option." >&2',
  'exit 127',
].join("; ")

type LongMessageMode = "split" | "summary"

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
  long_message_mode: LongMessageMode
  show_summarize_thinking: boolean
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

function pluginOption(context: Context, name: string): unknown {
  return (context.options as Record<string, unknown> | undefined)?.[name]
}

function startCommand(context: Context): string {
  const option = pluginOption(context, "startCommand")
  if (typeof option === "string" && option.trim()) return option.trim()
  const fallback = env("OPENCODE_TELEGRAM_START_COMMAND")
  if (fallback?.trim()) return fallback.trim()
  return DEFAULT_START_COMMAND
}

function autoStart(context: Context): boolean {
  const option = pluginOption(context, "autoStart")
  if (typeof option === "boolean") return option
  const fallback = env("OPENCODE_TELEGRAM_AUTOSTART")
  return fallback ? /^(1|true|yes|on)$/i.test(fallback.trim()) : false
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms))
}

// daemonAlive only checks that something answers on the plugin API. A 401
// still means the daemon is running; only a connection failure means it has
// to be started.
async function daemonAlive(context: Context): Promise<boolean> {
  const fetchFn = (globalThis as { fetch?: (input: string, init?: unknown) => Promise<unknown> }).fetch
  if (typeof fetchFn !== "function") return false

  const controller = typeof AbortController === "function" ? new AbortController() : undefined
  const timer = setTimeout(() => controller?.abort(), HEALTH_TIMEOUT_MS)
  try {
    await fetchFn(daemonUrl(context) + "/health", {
      method: "GET",
      headers: { accept: "application/json", ...authHeaders(context) },
      signal: controller?.signal,
    })
    return true
  } catch {
    return false
  } finally {
    clearTimeout(timer)
  }
}

type StartResult = {
  code: number | null
  output: string
}

async function runShell(command: string, timeoutMs: number): Promise<StartResult> {
  const { spawn } = (await import("node:child_process")) as {
    spawn: (
      command: string,
      args: string[],
      options: Record<string, unknown>,
    ) => {
      stdout?: { on: (event: string, listener: (chunk: unknown) => void) => void }
      stderr?: { on: (event: string, listener: (chunk: unknown) => void) => void }
      on: (event: string, listener: (value: unknown) => void) => void
      kill: (signal?: string) => void
    }
  }

  return await new Promise<StartResult>((resolve) => {
    let output = ""
    let settled = false
    let child: ReturnType<typeof spawn> | undefined
    const finish = (code: number | null) => {
      if (settled) return
      settled = true
      clearTimeout(timer)
      resolve({ code, output: output.trim() })
    }
    const timer = setTimeout(() => {
      output += "\n(start command timed out)"
      child?.kill("SIGTERM")
      finish(null)
    }, timeoutMs)

    try {
      child = spawn("sh", ["-c", command], {
        stdio: ["ignore", "pipe", "pipe"],
        windowsHide: true,
      })
    } catch (error) {
      output = errText(error)
      finish(null)
      return
    }

    child.stdout?.on("data", (chunk) => {
      output += String(chunk)
    })
    child.stderr?.on("data", (chunk) => {
      output += String(chunk)
    })
    child.on("error", (error) => {
      output += `\n${errText(error)}`
      finish(null)
    })
    // "close" fires after stdout/stderr are drained, unlike "exit".
    child.on("close", (code) => finish(typeof code === "number" ? code : null))
  })
}

function trimOutput(value: string, limit = 600): string {
  const text = value.trim()
  return text.length > limit ? `${text.slice(0, limit)}…` : text
}

// ensureDaemon starts the installed service (or the command configured by the
// startCommand plugin option) and waits for its loopback API to answer.
async function ensureDaemon(context: Context): Promise<void> {
  if (await daemonAlive(context)) return

  const result = await runShell(startCommand(context), START_COMMAND_TIMEOUT_MS)
  if (await daemonAlive(context)) return

  if (result.code !== 0 && result.code !== null) {
    const detail = result.output ? `\n\n${trimOutput(result.output)}` : ""
    throw new Error(`Could not start the Telegram service (exit code ${result.code}).${detail}`)
  }

  const deadline = Date.now() + START_TIMEOUT_MS
  while (Date.now() < deadline) {
    await sleep(START_POLL_MS)
    if (await daemonAlive(context)) return
  }

  const detail = result.output ? `\n\n${trimOutput(result.output)}` : ""
  throw new Error(
    `Started the Telegram service, but ${daemonUrl(context)} did not respond within ` +
      `${Math.round(START_TIMEOUT_MS / 1000)}s.${detail}\n\n` +
      `Check the daemon log with "opencode-telegram logs".`,
  )
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
    const message = typeof data.error === "string" ? data.error : `HTTP ${response.status}`
    if (message.includes("unknown field")) {
      throw new Error(
        `${message}. The running daemon is older than this plugin; rebuild and restart it ` +
          `(make build && opencode-telegram restart).`,
      )
    }
    throw new Error(message)
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

function createNotificationStatus(context: Context) {
  const [enabled, setEnabled] = createSignal<boolean>()
  const [busy, setBusy] = createSignal(false)
  const [error, setError] = createSignal<string>()
  const [hovered, setHovered] = createSignal(false)
  let disposed = false
  let refreshing = false
  let pending = false
  let version = 0

  const refresh = async () => {
    if (disposed) return
    if (refreshing) {
      pending = true
      return
    }
    refreshing = true
    const stamp = version
    try {
      const data = await load(context)
      if (disposed || stamp !== version) return
      setEnabled(data.settings.notifications.enabled)
      setError(undefined)
    } catch (cause) {
      if (disposed) return
      // A stopped daemon is the normal "off" state, not an error state the
      // footer needs to advertise.
      setEnabled(false)
      setError(errText(cause))
    } finally {
      refreshing = false
      if (pending && !disposed) {
        pending = false
        void refresh()
      }
    }
  }

  const toggle = async () => {
    if (busy()) return
    setBusy(true)
    try {
      let data: SettingsResponse | undefined
      let started = false
      try {
        data = await load(context)
      } catch (cause) {
        // If the daemon is simply not running, turning the icon on starts it
        // and then enables notifications. Any other failure (bad token, old
        // daemon, ...) is surfaced instead of launching a duplicate.
        if (await daemonAlive(context)) throw cause
        context.ui.toast.show({
          title: "OpenCode Telegram",
          message: "Starting the Telegram service…",
          variant: "info",
          duration: 2000,
        })
        await ensureDaemon(context)
        data = await load(context)
        started = true
      }

      if (!data) return
      const next = started ? true : !data.settings.notifications.enabled
      await patchNotifications(context, { enabled: next })
      version += 1
      setEnabled(next)
      setError(undefined)
      context.ui.toast.show({
        title: "OpenCode Telegram",
        message: started
          ? "Telegram service started and notifications enabled."
          : next
            ? "Notifications enabled."
            : "Notifications disabled.",
        variant: next ? "success" : "info",
        duration: started ? 3000 : 2500,
      })
    } catch (cause) {
      setError(errText(cause))
      await toastError(context, cause)
    } finally {
      setBusy(false)
    }
  }

  const timer = setInterval(() => void refresh(), STATUS_POLL_MS)
  void refresh()

  const stop = () => {
    disposed = true
    clearInterval(timer)
  }

  return { enabled, busy, error, hovered, setHovered, toggle, refresh, stop }
}

type NotificationStatus = ReturnType<typeof createNotificationStatus>

function TelegramStatus(props: { context: Context; status: NotificationStatus }) {
  const { context, status } = props

  const color = () => {
    if (status.enabled() === true) return context.theme.text.feedback.success.base
    return context.theme.text.muted
  }

  const label = () => {
    if (status.busy()) return "…"
    if (status.enabled() === true) return "on"
    return "off"
  }

  return (
    <text
      fg={color()}
      bg={status.hovered() ? context.theme.background.raised.base : undefined}
      selectable={false}
      onMouseOver={() => status.setHovered(true)}
      onMouseOut={() => status.setHovered(false)}
      onMouseUp={() => void status.toggle()}
    >
      {`${TELEGRAM_ICON} Telegram ${label()}`}
    </text>
  )
}

async function toggleSecurity(
  context: Context,
  key: keyof Security,
  enable: boolean,
): Promise<boolean> {
  if (enable) {
    let title = "Allow custom answers?"
    let message =
      "This adds a \"Type your own response\" option to OpenCode questions, allowing arbitrary text to be sent back into a form. Only enable it if you trust every account and device that can reach this bot."
    if (key === "allow_new_messages") {
      title = "Allow new messages?"
      message =
        "This lets any allowlisted Telegram user send arbitrary prompts to the OpenCode session bound to their chat. Only enable it if you trust every account and device that can reach this bot."
    }
    const confirm = await context.ui.dialog.confirm({
      title,
      message,
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
      title: `Long messages: ${n.long_message_mode === "summary" ? "preview + summarize" : "split into blocks"}`,
      value: "toggle_long_message_mode",
      description:
        n.long_message_mode === "summary"
          ? "Oversized messages send a preview instead of many blocks; Summarize output is always available"
          : "Send one Telegram message per assistant text block (never truncate)",
    },
    {
      title: `Summarize thinking button ${stateLabel(n.show_summarize_thinking)}`,
      value: "toggle_show_summarize_thinking",
      description: "Show \"Summarize thinking\" under raw thinking; on by default",
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
    case "toggle_include_directory":
    case "toggle_show_summarize_thinking": {
      const key = value.replace("toggle_", "") as keyof Notifications
      const data = await load(context)
      const current = data.settings.notifications[key]
      if (typeof current !== "boolean") return true
      await patchNotifications(context, { [key]: !current } as Partial<Notifications>)
      return true
    }
    case "toggle_long_message_mode": {
      const data = await load(context)
      const next: LongMessageMode =
        data.settings.notifications.long_message_mode === "summary" ? "split" : "summary"
      await patchNotifications(context, { long_message_mode: next })
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

async function openSettings(context: Context, onChange?: () => void) {
  for (;;) {
    let data: SettingsResponse | undefined
    try {
      data = await load(context)
    } catch (error) {
      if (await daemonAlive(context)) {
        await context.ui.dialog.alert({
          title: "OpenCode Telegram",
          message: `The daemon at ${daemonUrl(context)} did not answer correctly.\n\n${errText(error)}`,
        })
        return
      }

      // Opening the settings is an explicit "turn it on" action: bring the
      // service up instead of asking the user to leave OpenCode.
      try {
        await ensureDaemon(context)
        data = await load(context)
      } catch (startError) {
        await context.ui.dialog.alert({
          title: "OpenCode Telegram",
          message:
            `Could not reach the daemon at ${daemonUrl(context)} and could not start it.\n\n` +
            `${errText(startError)}\n\n` +
            `Run "opencode-telegram start" manually to see the full log.`,
        })
        return
      }
    }

    if (!data) return
    const status = data.status
    const subtitle = status?.opencode_ok ? "OpenCode connected" : "OpenCode disconnected"
    const choice = await context.ui.dialog.select({
      title: `OpenCode Telegram — ${subtitle}`,
      options: buildOptions(data.settings, status),
    })
    if (!choice) return

    try {
      const again = await handleChoice(context, choice)
      onChange?.()
      if (!again) return
    } catch (error) {
      await toastError(context, error)
    }
  }
}

export default Plugin.define({
  id: "opencode-telegram",
  setup(context) {
    const status = createNotificationStatus(context)
    const release = [
      context.ui.slot({
        append: "prompt.footer.status",
        render: () => <TelegramStatus context={context} status={status} />,
      }),
    ]

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
          run: () => openSettings(context, () => void status.refresh()),
        },
      ],
    }))

    // Optional configuration path: start the service as soon as the plugin
    // loads. The footer still reflects the daemon's notification setting.
    if (autoStart(context)) {
      void ensureDaemon(context)
        .then(() => status.refresh())
        .catch((error) => toastError(context, error))
    }

    return () => {
      status.stop()
      for (const off of release) off()
    }
  },
})
