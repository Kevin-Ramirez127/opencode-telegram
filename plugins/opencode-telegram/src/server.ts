import { Plugin } from "@opencode/plugin"

/**
 * Server part.
 *
 * The daemon (the opencode-telegram Go program) owns the OpenCode event
 * subscription. This part exists so the package can be loaded from
 * opencode.json project/global configuration; all user-facing controls live
 * in the TUI part.
 */
export default Plugin.define({
  id: "opencode-telegram",
  setup() {},
})
