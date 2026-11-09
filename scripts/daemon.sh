#!/usr/bin/env bash
#
# daemon.sh — start / stop / restart the opencode-telegram daemon.
#
# Installed as `opencode-telegram` in ~/.local/bin by install.sh: the installer
# copies the app to ~/.local/share/opencode-telegram and generates a wrapper,
# so `opencode-telegram ...` runs the installed copy from anywhere.
#
# Usage:
#   ./scripts/daemon.sh start [--foreground]   start the daemon
#   ./scripts/daemon.sh stop                   stop the daemon
#   ./scripts/daemon.sh restart                restart the daemon
#   ./scripts/daemon.sh status                 show whether it is running
#   ./scripts/daemon.sh logs [-f]              show the log (follow with -f)
#   ./scripts/daemon.sh reload                 reload allowlist + settings (SIGHUP)
#   ./scripts/daemon.sh run                    run in the foreground
#   ./scripts/daemon.sh check                  validate config and connectivity
#   ./scripts/daemon.sh config [options]       manage the user/chat allowlist
#   ./scripts/daemon.sh settings [show|edit]   show or edit settings.json
#
# config options:
#   --adduser <id>    allowlist a user AND the same id as a chat (covers DMs)
#   --deluser <id>    remove a user from both lists
#   --addchat <id>    allowlist a group/channel chat id
#   --delchat <id>    remove a chat id
#   --list            show the allowlist
#
# Details:
#   - Runs bin/opencode-telegram from its own directory (the Go app reads .env
#     itself). When installed, that is ~/.local/share/opencode-telegram.
#   - PID file: .run/opencode-telegram.pid ; log: opencode-telegram.log.
#   - Refuses to start without TELEGRAM_BOT_TOKEN (env or .env).
#   - config edits data/allowlist.json atomically (backup .bak, mode 600) and
#     reloads the running daemon with SIGHUP.
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$(readlink -f "${BASH_SOURCE[0]}" 2>/dev/null || echo "${BASH_SOURCE[0]}")")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
BIN="$ROOT/bin/opencode-telegram"
PIDFILE="$ROOT/.run/opencode-telegram.pid"
LOGFILE="$ROOT/opencode-telegram.log"

fail() { echo "error: $*" >&2; exit 1; }

pid_alive() { [[ -n "${1:-}" ]] && kill -0 "$1" 2>/dev/null; }

current_pid() {
  [[ -f "$PIDFILE" ]] || return 1
  local pid
  pid="$(tr -d '[:space:]' < "$PIDFILE" 2>/dev/null || true)"
  [[ "$pid" =~ ^[0-9]+$ ]] || return 1
  pid_alive "$pid" || return 1
  printf '%s' "$pid"
}

has_token() {
  [[ -n "${TELEGRAM_BOT_TOKEN:-}" ]] && return 0
  [[ -f "$ROOT/.env" ]] || return 1
  grep -Eq '^[[:space:]]*TELEGRAM_BOT_TOKEN=[^[:space:]#]+' "$ROOT/.env"
}

ensure_built() {
  if [[ ! -x "$BIN" ]]; then
    [[ -f "$ROOT/go.mod" ]] || fail "binary not found at $BIN (reinstall with ./install.sh)"
    echo "binary not found, building..."
    (cd "$ROOT" && go build -o bin/opencode-telegram .) \
      || fail "build failed (run ./install.sh first)"
  fi
}

cmd_start() {
  local foreground=false
  [[ "${1:-}" == "--foreground" ]] && foreground=true

  if pid=$(current_pid); then
    echo "already running (pid $pid)"
    return 0
  fi
  [[ -f "$PIDFILE" ]] && rm -f "$PIDFILE"

  ensure_built
  has_token || fail "TELEGRAM_BOT_TOKEN is not set — export it or put it in $ROOT/.env (see .env.example)"

  if $foreground; then
    echo "running in foreground (Ctrl+C to stop)..."
    cd "$ROOT" && exec "$BIN"
  fi

  mkdir -p "$(dirname "$PIDFILE")" "$(dirname "$LOGFILE")"
  touch "$LOGFILE"
  cd "$ROOT" && nohup "$BIN" >>"$LOGFILE" 2>&1 &
  echo "$!" > "$PIDFILE"
  sleep 1
  if pid=$(current_pid); then
    echo "started (pid $pid, log: $LOGFILE)"
  else
    rm -f "$PIDFILE"
    fail "failed to start — check $LOGFILE"
  fi
}

cmd_stop() {
  local pid
  if ! pid=$(current_pid); then
    [[ -f "$PIDFILE" ]] && rm -f "$PIDFILE"
    echo "not running"
    return 0
  fi
  echo -n "stopping (pid $pid)..."
  kill "$pid" 2>/dev/null || true
  for _ in $(seq 1 50); do
    pid_alive "$pid" || break
    sleep 0.2
    echo -n "."
  done
  if pid_alive "$pid"; then
    echo -n " (forcing)"
    kill -9 "$pid" 2>/dev/null || true
    sleep 0.5
  fi
  rm -f "$PIDFILE"
  pid_alive "$pid" && fail "could not stop pid $pid"
  echo " stopped"
}

cmd_status() {
  local pid
  if pid=$(current_pid); then
    echo "running (pid $pid, log: $LOGFILE)"
  else
    echo "not running"
    return 1
  fi
}

cmd_logs() {
  [[ -f "$LOGFILE" ]] || fail "no log file yet ($LOGFILE)"
  if [[ "${1:-}" == "-f" ]]; then
    tail -f "$LOGFILE"
  else
    tail -n 80 "$LOGFILE"
  fi
}

cmd_reload() {
  local pid
  pid=$(current_pid) || fail "not running — nothing to reload"
  kill -HUP "$pid"
  echo "reloaded (SIGHUP sent to pid $pid)"
}

# --- allowlist ---------------------------------------------------------------
wl_file() {
  local d="${DATA_DIR:-data}"
  [[ "$d" = /* ]] || d="$ROOT/$d"
  printf '%s/allowlist.json' "$d"
}

wl_validate() {
  jq -e '(.users | type == "array") and (.chats | type == "array")
    and (all(.users[]; type == "number" and . != 0 and floor == .))
    and (all(.chats[]; type == "number" and . != 0 and floor == .))' "$1" >/dev/null
}

wl_ensure() {
  local f
  f="$(wl_file)"
  if [[ ! -f "$f" ]]; then
    mkdir -p "$(dirname "$f")"
    cat > "$f" <<'EOF'
{
  "note": "Allowlist. Both the sending user AND the chat must be listed.",
  "users": [],
  "chats": []
}
EOF
    chmod 600 "$f"
    echo "note: created empty $f"
  fi
  wl_validate "$f" || fail "allowlist file is not valid: $f"
}

wl_commit() {
  local f tmp
  f="$(wl_file)"
  tmp="$(mktemp "$(dirname "$f")/.allowlist.XXXXXX.json")"
  trap 'rm -f "$tmp"' EXIT
  jq "$@" "$f" > "$tmp" || fail "jq could not apply the change (file left untouched)"
  wl_validate "$tmp" || fail "the resulting file failed validation (file left untouched)"
  chmod 600 "$tmp"
  cp -p "$f" "$f.bak"
  chmod 600 "$f.bak"
  mv -f "$tmp" "$f"
  trap - EXIT
}

wl_valid_id() { [[ "${1:-}" =~ ^-?[1-9][0-9]*$ ]]; }

wl_present() {
  local f
  f="$(wl_file)"
  jq -e --argjson id "$1" --arg list "$2" '(.[$list] // []) | index($id) != null' "$f" >/dev/null
}

wl_adduser() {
  wl_valid_id "$1" || fail "user id must be a non-zero integer (got '${1:-}')"
  wl_ensure
  if wl_present "$1" users && wl_present "$1" chats; then
    echo "note: user $1 is already allowlisted"
    return 0
  fi
  wl_commit --argjson id "$1" '
    .users |= ((. // []) + [$id] | unique)
    | .chats |= ((. // []) + [$id] | unique)'
  echo "ok: user $1 allowlisted (users + chats)"
}

wl_deluser() {
  wl_valid_id "$1" || fail "user id must be a non-zero integer (got '${1:-}')"
  wl_ensure
  wl_commit --argjson id "$1" '
    .users |= ((. // []) | map(select(. != $id)))
    | .chats |= ((. // []) | map(select(. != $id)))'
  echo "ok: user $1 removed from the allowlist (users + chats)"
}

wl_addchat() {
  wl_valid_id "$1" || fail "chat id must be a non-zero integer (got '${1:-}')"
  wl_ensure
  if wl_present "$1" chats; then
    echo "note: chat $1 is already allowlisted"
    return 0
  fi
  wl_commit --argjson id "$1" '.chats |= ((. // []) + [$id] | unique)'
  echo "ok: chat $1 allowlisted"
}

wl_delchat() {
  wl_valid_id "$1" || fail "chat id must be a non-zero integer (got '${1:-}')"
  wl_ensure
  wl_commit --argjson id "$1" '.chats |= ((. // []) | map(select(. != $id)))'
  echo "ok: chat $1 removed from the allowlist"
}

wl_list() {
  local f
  f="$(wl_file)"
  [[ -f "$f" ]] || fail "allowlist file not found: $f (run ./install.sh first)"
  wl_validate "$f" || fail "allowlist file is not valid: $f"
  jq -r '"users (\(.users | length)): \([.users[] | tostring] | join(", "))",
          "chats (\(.chats | length)): \([.chats[] | tostring] | join(", "))"' "$f"
}

cmd_config() {
  command -v jq >/dev/null 2>&1 || fail "jq is required (install with: sudo apt install jq | brew install jq | sudo dnf install jq)"
  local dirty=false
  if [[ $# -eq 0 ]]; then
    wl_list
    return 0
  fi
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --adduser) [[ $# -ge 2 ]] || fail "config --adduser requires an ID"; wl_adduser "$2"; dirty=true; shift 2 ;;
      --deluser) [[ $# -ge 2 ]] || fail "config --deluser requires an ID"; wl_deluser "$2"; dirty=true; shift 2 ;;
      --addchat) [[ $# -ge 2 ]] || fail "config --addchat requires an ID"; wl_addchat "$2"; dirty=true; shift 2 ;;
      --delchat) [[ $# -ge 2 ]] || fail "config --delchat requires an ID"; wl_delchat "$2"; dirty=true; shift 2 ;;
      --list) wl_list; shift ;;
      -h|--help) wl_list; echo "usage: config [--adduser ID] [--deluser ID] [--addchat ID] [--delchat ID] [--list]"; return 0 ;;
      *) fail "unknown config option '$1' (try: --adduser|--deluser|--addchat|--delchat|--list)" ;;
    esac
  done
  if $dirty; then
    local pid
    if pid=$(current_pid); then
      kill -HUP "$pid" 2>/dev/null && echo "note: running daemon (pid $pid) reloaded"
    else
      echo "note: daemon not running — changes apply on next start"
    fi
  fi
}

settings_file() {
  local d="${DATA_DIR:-data}"
  [[ "$d" = /* ]] || d="$ROOT/$d"
  printf '%s/settings.json' "$d"
}

cmd_settings() {
  local f
  f="$(settings_file)"
  [[ -f "$f" ]] || fail "settings file not found: $f (run ./install.sh first)"
  case "${1:-show}" in
    show|"") jq . "$f" ;;
    edit)
      "${EDITOR:-vi}" "$f"
      local pid
      if pid=$(current_pid); then kill -HUP "$pid" 2>/dev/null || true; fi
      ;;
    *) fail "usage: settings [show|edit]" ;;
  esac
}

usage() {
  sed -n '2,/^$/p' "$0" | sed 's/^# \?//'
}

cmd="${1:-}"
shift || true
case "$cmd" in
  start)   cmd_start "${1:-}" ;;
  run)     cmd_start --foreground ;;
  stop)    cmd_stop ;;
  restart) cmd_stop; cmd_start "${1:-}" ;;
  status)  cmd_status ;;
  logs)    cmd_logs "${1:-}" ;;
  reload)  cmd_reload ;;
  check)   ensure_built; has_token || fail "TELEGRAM_BOT_TOKEN is not set"; cd "$ROOT" && "$BIN" --check ;;
  config)  cmd_config "$@" ;;
  settings) cmd_settings "${1:-show}" ;;
  help|-h|--help|"") usage ;;
  *) fail "unknown command '$cmd' (try: start|stop|restart|status|logs|reload|run|check|config|settings)" ;;
esac
