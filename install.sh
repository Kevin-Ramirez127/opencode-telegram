#!/usr/bin/env bash
#
# install.sh — one-shot installer for opencode-telegram.
#
# What it does (idempotent, safe to re-run):
#   1. Checks for required tools: go (>= 1.23) and jq.
#   2. Builds the daemon to bin/opencode-telegram.
#   3. Installs the application to $PREFIX/share/opencode-telegram
#      (default PREFIX ~/.local): binary, control script, .env, data/.
#      User content (.env token, data/*.json) is seeded on first install and
#      never overwritten afterwards.
#   4. Generates the `opencode-telegram` command in $PREFIX/bin.
#   5. Asks for the Telegram bot token when needed.
#   6. Optionally installs a systemd user unit with --systemd.
#
# Usage:
#   ./install.sh                          # full install
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

CHECK=false
NO_PROMPT=false
SYSTEMD=false
fail() { echo "error: $*" >&2; exit 1; }
info() { echo "==> $*"; }
for arg in "$@"; do
  case "$arg" in
    --check) CHECK=true ;;
    --systemd) SYSTEMD=true ;;
    --no-prompt|--non-interactive) NO_PROMPT=true ;;
    -h|--help) sed -n '2,/^$/p' "$0" | sed 's/^# \?//'; exit 0 ;;
    *) fail "unknown option '$arg' (try --help)" ;;
  esac
done

# --- 1. prerequisites ---------------------------------------------------------
info "checking prerequisites..."
command -v go >/dev/null 2>&1 || fail "go is not installed (see https://go.dev/dl/ — need go >= 1.23)"
command -v jq >/dev/null 2>&1 || fail "jq is required (install with: sudo apt install jq | brew install jq | sudo dnf install jq)"
info "found $(go version)"

# --- 2. build -----------------------------------------------------------------
VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
info "building bin/opencode-telegram ($VERSION)..."
mkdir -p bin
go mod download
go build -ldflags "-X main.version=$VERSION" -o bin/opencode-telegram . || fail "build failed"
chmod +x bin/opencode-telegram
info "build ok"

# --- 3. repo .env -------------------------------------------------------------
if [[ ! -f .env ]]; then
  cp .env.example .env
  chmod 600 .env
  info "created .env from .env.example (chmod 600)"
else
  chmod 600 .env
  info ".env already exists (kept, chmod 600 enforced)"
fi

# --- 4. token helpers ---------------------------------------------------------
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

# --- 5. install application ---------------------------------------------------
info "installing application to $APP_DIR..."
mkdir -p "$APP_DIR/bin" "$APP_DIR/scripts" "$APP_DIR/data" "$APP_DIR/packaging"
install -m755 bin/opencode-telegram "$APP_DIR/bin/opencode-telegram"
install -m755 scripts/daemon.sh "$APP_DIR/scripts/daemon.sh"
install -m644 packaging/opencode-telegram.service "$APP_DIR/packaging/opencode-telegram.service" 2>/dev/null || true
info "installed binary + control script"

seed_json() {
  local src="$1" dst="$2"
  if [[ ! -f "$APP_DIR/data/$dst" ]]; then
    cp -p "data/$src" "$APP_DIR/data/$dst"
    info "installed data/$dst (new file)"
  fi
  chmod 600 "$APP_DIR/data/$dst"
}
[[ -f data/allowlist.json ]] || fail "data/allowlist.json is missing"
[[ -f data/settings.json ]] || fail "data/settings.json is missing"
seed_json allowlist.json allowlist.json
seed_json settings.json settings.json

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

# --- 6. token ----------------------------------------------------------------
if [[ -n "${TELEGRAM_BOT_TOKEN:-}" ]]; then
  set_env_file_token "$APP_DIR/.env" "$TELEGRAM_BOT_TOKEN"
  info "bot token taken from environment and saved to installed .env (chmod 600)"
elif env_file_token "$APP_DIR/.env" >/dev/null; then
  info "bot token already configured in installed .env (kept)"
elif [[ "$NO_PROMPT" == "true" ]]; then
  info "no token configured — set TELEGRAM_BOT_TOKEN in $APP_DIR/.env later (--no-prompt given)"
elif [[ ! -t 0 ]]; then
  info "no token configured — set TELEGRAM_BOT_TOKEN in $APP_DIR/.env later"
else
  printf 'Enter your Telegram bot token from @BotFather (empty to skip): ' >&2
  token=""
  read -rs token || true
  echo >&2
  token="$(printf '%s' "$token" | tr -d '[:space:]')"
  if [[ -z "$token" ]]; then
    info "skipped — set TELEGRAM_BOT_TOKEN in $APP_DIR/.env later"
  else
    [[ "$token" =~ ^[0-9]+:[A-Za-z0-9_-]+$ ]] || echo "warning: that does not look like a Telegram token" >&2
    set_env_file_token "$APP_DIR/.env" "$token"
    info "bot token saved to installed .env (chmod 600)"
  fi
  unset token
fi

# --- 7. wrapper ---------------------------------------------------------------
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

# --- 8. systemd user unit (optional) ------------------------------------------
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

# --- 9. optional verification -------------------------------------------------
if $CHECK; then
  info "running go vet..."
  go vet ./...
  info "running go test ./..."
  go test ./...
  info "checks passed"
fi

# --- next steps ---------------------------------------------------------------
cat <<'EOF'

install done. Next steps:

  Installed layout (self-contained, repo not needed at runtime):
    ~/.local/bin/opencode-telegram              # the command
    ~/.local/share/opencode-telegram/           # app root: bin/, scripts/, .env,
                                                #   data/allowlist.json, data/settings.json
    ~/.local/share/opencode-telegram/opencode-telegram.log

  1. Allowlist yourself (numeric IDs from @userinfobot):
       opencode-telegram config --adduser <your-user-id>

  2. Start the daemon:
       opencode-telegram start
       opencode-telegram logs -f

  3. In OpenCode, load the companion plugin and run /telegram to enable
     notifications. Notifications are OFF by default.

  4. If you want the daemon to survive logout, prefer systemd:
       ./install.sh --systemd
     or copy packaging/opencode-telegram.service manually.

EOF
