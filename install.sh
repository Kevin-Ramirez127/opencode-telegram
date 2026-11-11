#!/usr/bin/env bash
#
# install.sh — one-shot installer for opencode-telegram.
#
# What it does (idempotent, safe to re-run):
#   By default it installs both the daemon and the OpenCode plugin.
#   Pass --daemon or --plugin to install only one of them.
#   1. Checks for required tools (go >= 1.23 is only needed for the daemon).
#   2. Builds the daemon to bin/opencode-telegram.
#   3. Installs the application to $PREFIX/share/opencode-telegram
#      (default PREFIX ~/.local): binary, control script, .env, data/.
#      User content (.env token, data/*.json) is seeded on first install and
#      never overwritten afterwards.
#   4. Generates the `opencode-telegram` command in $PREFIX/bin.
#   5. Asks for the Telegram bot token when needed.
#   6. Installs the OpenCode plugin into the app directory and registers it in
#      the global opencode.json(c).
#   7. Optionally installs a systemd user unit with --systemd.
#
# Usage:
#   ./install.sh                          # daemon + plugin (default)
#   ./install.sh --daemon                 # daemon only
#   ./install.sh --plugin                 # plugin only
#   ./install.sh --check                  # also run go vet + go test
#   ./install.sh --systemd                # also install + enable the user unit
#   ./install.sh --no-prompt              # never prompt
#   TELEGRAM_BOT_TOKEN=... ./install.sh   # provide the token non-interactively
#   PREFIX=/opt/otg ./install.sh          # custom install prefix
#

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT_DIR"

PREFIX="${PREFIX:-$HOME/.local}"
INSTALL_DIR="${INSTALL_DIR:-$PREFIX/bin}"
APP_DIR="${APP_DIR:-$PREFIX/share/opencode-telegram}"
INSTALL_NAME="opencode-telegram"
PLUGIN_SUBDIR="plugins/opencode-telegram"
PLUGIN_SRC="$ROOT_DIR/$PLUGIN_SUBDIR"
PLUGIN_DIR="$APP_DIR/$PLUGIN_SUBDIR"
OPENCODE_CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/opencode"

CHECK=false
NO_PROMPT=false
SYSTEMD=false
DAEMON_REQUESTED=false
PLUGIN_REQUESTED=false
fail() { echo "error: $*" >&2; exit 1; }
info() { echo "==> $*"; }
for arg in "$@"; do
  case "$arg" in
    --daemon) DAEMON_REQUESTED=true ;;
    --plugin) PLUGIN_REQUESTED=true ;;
    --check) CHECK=true ;;
    --systemd) SYSTEMD=true ;;
    --no-prompt|--non-interactive) NO_PROMPT=true ;;
    -h|--help) sed -n '2,/^$/p' "$0" | sed 's/^# \?//'; exit 0 ;;
    *) fail "unknown option '$arg' (try --help)" ;;
  esac
done

if $DAEMON_REQUESTED || $PLUGIN_REQUESTED; then
  DO_DAEMON=$DAEMON_REQUESTED
  DO_PLUGIN=$PLUGIN_REQUESTED
else
  DO_DAEMON=true
  DO_PLUGIN=true
fi
if $SYSTEMD && ! $DO_DAEMON; then
  echo "note: --systemd ignored because the daemon was not selected" >&2
  SYSTEMD=false
fi

# --- 1. prerequisites ---------------------------------------------------------
info "checking prerequisites..."
command -v jq >/dev/null 2>&1 || fail "jq is required (install with: sudo apt install jq | brew install jq | sudo dnf install jq)"
if $DO_DAEMON; then
  command -v go >/dev/null 2>&1 || fail "go is not installed (see https://go.dev/dl/ — need go >= 1.23)"
  info "found $(go version)"
fi

# --- 2. build -----------------------------------------------------------------
if $DO_DAEMON; then
  VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
  info "building bin/opencode-telegram ($VERSION)..."
  mkdir -p bin
  go mod download
  go build -ldflags "-X main.version=$VERSION" -o bin/opencode-telegram . || fail "build failed"
  chmod +x bin/opencode-telegram
  info "build ok"
fi

# --- 3. repo .env -------------------------------------------------------------
if $DO_DAEMON; then
  if [[ ! -f .env ]]; then
    cp .env.example .env
    chmod 600 .env
    info "created .env from .env.example (chmod 600)"
  else
    chmod 600 .env
    info ".env already exists (kept, chmod 600 enforced)"
  fi
fi

# --- 4. helpers ---------------------------------------------------------------
env_file_token() {
  [[ -f "$1" ]] || return 1
  local v
  v="$(sed -n 's/^[[:space:]]*TELEGRAM_BOT_TOKEN=[[:space:]]*//p' "$1" | head -n 1)"
  v="$(printf '%s' "$v" | tr -d '[:space:]' | tr -d "\"'")"
  [[ -n "$v" ]] && printf '%s' "$v"
}

set_env_file_token() {
  local f="$1" t="$2" t_esc tmp
  t_esc="$(printf '%s' "$t" | sed 's/[&\\/]/\\&/g')"
  tmp="$(mktemp "$(dirname "$f")/.env.XXXXXX")"
  trap 'rm -f "$tmp"' EXIT
  if grep -Eq '^[[:space:]]*TELEGRAM_BOT_TOKEN=' "$f" 2>/dev/null; then
    sed -E "s|^[[:space:]]*TELEGRAM_BOT_TOKEN=.*|TELEGRAM_BOT_TOKEN=${t_esc}|" "$f" > "$tmp"
  else
    cat "$f" > "$tmp" 2>/dev/null || true
    [[ -s "$tmp" && -n "$(tail -c 1 "$tmp")" ]] && printf '\n' >> "$tmp"
    printf 'TELEGRAM_BOT_TOKEN=%s\n' "$t" >> "$tmp"
  fi
  chmod 600 "$tmp"
  mv -f "$tmp" "$f"
  trap - EXIT
}

seed_json() {
  local src="$1" dst="$2"
  if [[ ! -f "$APP_DIR/data/$dst" ]]; then
    cp -p "data/$src" "$APP_DIR/data/$dst"
    info "installed data/$dst (new file)"
  fi
  chmod 600 "$APP_DIR/data/$dst"
}

canonical_path() {
  local p="$1"
  if [[ -d "$p" ]]; then
    (cd "$p" 2>/dev/null && pwd -P)
  elif [[ -e "$p" ]]; then
    printf '%s/%s' "$(cd "$(dirname "$p")" 2>/dev/null && pwd -P)" "$(basename "$p")"
  else
    printf '%s' "$p"
  fi
}

tty_available() { { : </dev/tty; } 2>/dev/null; }

# Ask for the token on the controlling terminal. Input is visible on purpose:
# a silent prompt (read -s) looks frozen while pasting and hides typos, and
# reading /dev/tty keeps it working when the script itself is piped into bash.
prompt_token() {
  local attempt=0 answer
  TOKEN=""
  while (( attempt < 3 )); do
    attempt=$((attempt + 1))
    printf 'Enter your Telegram bot token from @BotFather\n(paste with Ctrl+Shift+V or your terminal paste menu; visible input, empty to skip): ' >&2
    if ! IFS= read -r answer < /dev/tty; then
      echo >&2
      return 0
    fi
    answer="$(printf '%s' "$answer" | tr -d '[:space:][:cntrl:]')"
    if [[ -z "$answer" ]]; then
      return 0
    fi
    if [[ "$answer" =~ ^[0-9]+:[A-Za-z0-9_-]+$ ]]; then
      TOKEN="$answer"
      return 0
    fi
    echo "warning: that does not look like a Telegram bot token (expected 123456789:ABC...); try again" >&2
  done
  echo "note: no valid token entered — skipping (re-run the installer or edit the .env)" >&2
  return 0
}

register_plugin() {
  local cfg tmp entry src_entry resolved_src
  if [[ -f "$OPENCODE_CONFIG_DIR/opencode.jsonc" ]]; then
    cfg="$OPENCODE_CONFIG_DIR/opencode.jsonc"
  elif [[ -f "$OPENCODE_CONFIG_DIR/opencode.json" ]]; then
    cfg="$OPENCODE_CONFIG_DIR/opencode.json"
  else
    cfg="$OPENCODE_CONFIG_DIR/opencode.json"
    mkdir -p "$OPENCODE_CONFIG_DIR"
    printf '{\n  "$schema": "https://opencode.ai/config.json",\n  "plugins": []\n}\n' > "$cfg"
    info "created $cfg"
  fi

  # A config may already load this plugin from the repo checkout or through a
  # symlink. Find that entry so a reinstall replaces it instead of adding a
  # second copy of the same plugin.
  src_entry=""
  resolved_src="$(canonical_path "$PLUGIN_SRC")"
  while IFS= read -r entry; do
    [[ -n "$entry" && "$entry" != "$PLUGIN_DIR" ]] || continue
    if [[ "$(canonical_path "$entry")" == "$resolved_src" ]]; then
      src_entry="$entry"
      break
    fi
  done < <(jq -r '(.plugins // [])[] | if type == "string" then . else empty end' "$cfg" 2>/dev/null || true)

  tmp="$(mktemp "$OPENCODE_CONFIG_DIR/.opencode.json.XXXXXX")"
  if jq --arg path "$PLUGIN_DIR" --arg src "$src_entry" '
      (.plugins // []) as $plugins
      | ([$plugins[] | if type == "string" then . else (.package // "") end]) as $entries
      | if ($entries | index($path)) != null then .
        elif ($src != "" and ($entries | index($src)) != null)
          then .plugins = ($plugins | map(if type == "string" and . == $src then $path else . end))
        else .plugins = ($plugins + [$path])
        end
    ' "$cfg" > "$tmp" 2>/dev/null; then
    if cmp -s "$cfg" "$tmp"; then
      rm -f "$tmp"
      info "OpenCode plugin already registered in $cfg"
    else
      cat "$tmp" > "$cfg"
      rm -f "$tmp"
      info "registered OpenCode plugin in $cfg"
    fi
  else
    rm -f "$tmp"
    echo "note: could not update $cfg automatically (it may contain comments)." >&2
    echo "      Add this entry to \"plugins\" by hand:" >&2
    echo "        \"$PLUGIN_DIR\"" >&2
  fi
  REGISTERED_CONFIG="$cfg"
}

# --- 5. daemon ----------------------------------------------------------------
if $DO_DAEMON; then
  info "installing application to $APP_DIR..."
  mkdir -p "$APP_DIR/bin" "$APP_DIR/scripts" "$APP_DIR/data" "$APP_DIR/packaging"
  install -m755 bin/opencode-telegram "$APP_DIR/bin/opencode-telegram"
  install -m755 scripts/daemon.sh "$APP_DIR/scripts/daemon.sh"
  install -m755 scripts/simulate.sh "$APP_DIR/scripts/simulate.sh"
  install -m644 packaging/opencode-telegram.service "$APP_DIR/packaging/opencode-telegram.service" 2>/dev/null || true
  info "installed binary + control script"

  [[ -f data/allowlist.example.json ]] || fail "data/allowlist.example.json is missing"
  [[ -f data/settings.example.json ]] || fail "data/settings.example.json is missing"
  seed_json allowlist.example.json allowlist.json
  seed_json settings.example.json settings.json

  if [[ ! -f "$APP_DIR/.env" ]]; then
    if t="$(env_file_token .env)"; then
      printf '# Installed by opencode-telegram/install.sh. Never commit this file.\nTELEGRAM_BOT_TOKEN=%s\n' "$t" > "$APP_DIR/.env"
      info "installed .env (token taken from repo .env)"
    else
      cp .env.example "$APP_DIR/.env"
      info "installed .env from .env.example (no token yet)"
    fi
    chmod 600 "$APP_DIR/.env"
    unset t
  fi

  # --- 6. token ---------------------------------------------------------------
  if [[ -n "${TELEGRAM_BOT_TOKEN:-}" ]]; then
    set_env_file_token "$APP_DIR/.env" "$TELEGRAM_BOT_TOKEN"
    info "bot token taken from environment and saved to installed .env (chmod 600)"
  elif env_file_token "$APP_DIR/.env" >/dev/null; then
    info "bot token already configured in installed .env (kept)"
  elif [[ "$NO_PROMPT" == "true" ]]; then
    info "no token configured — set TELEGRAM_BOT_TOKEN in $APP_DIR/.env later (--no-prompt given)"
  elif ! tty_available; then
    info "no token configured — set TELEGRAM_BOT_TOKEN in $APP_DIR/.env later"
  else
    prompt_token
    if [[ -n "$TOKEN" ]]; then
      set_env_file_token "$APP_DIR/.env" "$TOKEN"
      info "bot token saved to installed .env (chmod 600)"
    else
      info "skipped — set TELEGRAM_BOT_TOKEN in $APP_DIR/.env later"
    fi
    unset TOKEN
  fi

  # --- 7. wrapper -------------------------------------------------------------
  info "installing $INSTALL_NAME into $INSTALL_DIR..."
  mkdir -p "$INSTALL_DIR"
  rm -f "$INSTALL_DIR/$INSTALL_NAME"
  cat > "$INSTALL_DIR/$INSTALL_NAME" <<EOF
#!/usr/bin/env bash
# Generated by opencode-telegram/install.sh — do not edit (reinstall to update).
exec "$APP_DIR/scripts/daemon.sh" "\$@"
EOF
  chmod +x "$INSTALL_DIR/$INSTALL_NAME"
  info "installed $INSTALL_DIR/$INSTALL_NAME (runs $APP_DIR)"
  case ":${PATH:-}:" in
    *":$INSTALL_DIR:"*) ;;
    *) echo "note: $INSTALL_DIR is not on your PATH — add 'export PATH=\"\$HOME/.local/bin:\$PATH\"' to your shell rc" >&2 ;;
  esac
fi

# --- 8. OpenCode plugin -------------------------------------------------------
if $DO_PLUGIN; then
  [[ -f "$PLUGIN_SRC/package.json" ]] || fail "plugin sources are missing at $PLUGIN_SRC"
  info "installing OpenCode plugin to $PLUGIN_DIR..."
  rm -rf "$PLUGIN_DIR"
  mkdir -p "$PLUGIN_DIR"
  cp -R "$PLUGIN_SRC/src" "$PLUGIN_DIR/src"
  install -m644 "$PLUGIN_SRC/package.json" "$PLUGIN_DIR/package.json"
  if [[ -f "$PLUGIN_SRC/README.md" ]]; then
    install -m644 "$PLUGIN_SRC/README.md" "$PLUGIN_DIR/README.md"
  fi
  info "installed plugin files"
  register_plugin
fi

# --- 9. systemd user unit (optional) ------------------------------------------
if $SYSTEMD; then
  UNIT_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
  mkdir -p "$UNIT_DIR"
  sed "s|%h/.local/share/opencode-telegram|$APP_DIR|g" \
    packaging/opencode-telegram.service > "$UNIT_DIR/opencode-telegram.service"
  info "installed $UNIT_DIR/opencode-telegram.service"
  if command -v systemctl >/dev/null 2>&1; then
    systemctl --user daemon-reload || true
    systemctl --user enable --now opencode-telegram.service || \
      echo "note: could not enable the unit automatically; run: systemctl --user enable --now opencode-telegram" >&2
  fi
fi

# --- 10. optional verification ------------------------------------------------
if $CHECK && $DO_DAEMON; then
  info "running go vet..."
  go vet ./...
  info "running go test ./..."
  go test ./...
  info "checks passed"
elif $CHECK; then
  echo "note: --check ignored because the daemon was not selected" >&2
fi

# --- next steps ---------------------------------------------------------------
STEP=0
step() { STEP=$((STEP + 1)); echo "  $STEP. $*"; }

echo
echo "install done. Next steps:"
echo
if $DO_DAEMON; then
  echo "  Installed daemon layout (self-contained, repo not needed at runtime):"
  echo "    $INSTALL_DIR/$INSTALL_NAME              # the command"
  echo "    $APP_DIR/           # app root: bin/, scripts/, .env,"
  echo "                                                #   data/allowlist.json, data/settings.json"
  echo "    $APP_DIR/opencode-telegram.log"
  echo
  step "Allowlist yourself (numeric IDs from @userinfobot):"
  echo "       opencode-telegram config --adduser <your-user-id>"
  echo
  step "Start the daemon:"
  echo "       opencode-telegram start"
  echo "       opencode-telegram logs -f"
  echo
fi
if $DO_PLUGIN; then
  echo "  Installed OpenCode plugin:"
  echo "    files:  $PLUGIN_DIR"
  echo "    config: ${REGISTERED_CONFIG:-$OPENCODE_CONFIG_DIR/opencode.json}"
  echo
  step "Reload OpenCode (/reload) and run /telegram to enable notifications."
  echo "     Notifications are OFF by default."
  echo
  if ! $DO_DAEMON; then
    echo "  The daemon is not installed. Install it with:"
    echo "       ./install.sh --daemon"
    echo
  fi
fi
if $SYSTEMD; then
  step "The systemd unit is enabled, so the daemon survives logout. Control it with:"
  echo "       systemctl --user status opencode-telegram"
  echo
fi
