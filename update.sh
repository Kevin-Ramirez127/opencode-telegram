#!/usr/bin/env bash
#
# update.sh — safe updater for an existing opencode-telegram install.
#
# Typical use: pull the repository, then run the updater:
#
#   git pull
#   ./update.sh
#
# The installed command runs the same updater from the checkout recorded by
# install.sh:
#
#   opencode-telegram update
#   opencode-telegram update --pull --dry-run
#
# It rebuilds the daemon and refreshes the installed copy + OpenCode plugin
# while leaving your state untouched:
#
#   - .env, data/allowlist.json, data/settings.json, logs and .run/ are kept;
#   - new settings keys from data/settings.example.json are added to
#     settings.json without overwriting existing values;
#   - the previous binary, plugin, config and data are backed up first;
#   - a failure (build, install, verification) rolls the backup back;
#   - a running daemon is stopped and restarted in the same mode.
#
# Usage:
#   ./update.sh                      # update the existing install
#   ./update.sh --pull               # git pull --ff-only first
#   ./update.sh --dry-run            # show what would happen, change nothing
#   ./update.sh -y                   # no confirmation prompt
#   ./update.sh --daemon             # update the daemon only
#   ./update.sh --plugin             # update the OpenCode plugin only
#   ./update.sh --no-restart         # update but leave the daemon stopped
#   ./update.sh --keep 3             # keep the last 3 backups (default 5)
#   ./update.sh --rollback latest    # restore the newest backup
#   ./update.sh --rollback <dir>     # restore a specific backup directory
#   ./update.sh --list-backups       # show available backups
#   ./update.sh --check              # also run go vet + go test
#   ./update.sh --keep-unit          # do not refresh the systemd unit
#   ./update.sh --force              # allow updating with local git changes
#
# Environment (same as install.sh):
#   PREFIX=~/.local       install prefix
#   INSTALL_DIR=...       where the `opencode-telegram` command lives
#   APP_DIR=...           installed app directory

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$(readlink -f "${BASH_SOURCE[0]}" 2>/dev/null || echo "${BASH_SOURCE[0]}")")" && pwd)"
cd "$ROOT_DIR"

PREFIX="${PREFIX:-$HOME/.local}"
INSTALL_DIR="${INSTALL_DIR:-$PREFIX/bin}"
DEFAULT_APP_DIR="$PREFIX/share/opencode-telegram"
APP_DIR="${APP_DIR:-}"
APP_DIR_SOURCE="env"
OPENCODE_CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/opencode"
UNIT_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
UNIT_FILE="$UNIT_DIR/opencode-telegram.service"

PULL=false
DRY_RUN=false
ASSUME_YES=false
FORCE_DAEMON=false
FORCE_PLUGIN=false
NO_RESTART=false
CHECK=false
FORCE=false
KEEP_UNIT=false
KEEP=5
ROLLBACK=""
LIST_BACKUPS=false

fail() { echo "error: $*" >&2; exit 1; }
info() { echo "==> $*"; }
warn() { echo "warning: $*" >&2; }
note() { echo "note: $*" >&2; }
tty_available() { { : </dev/tty; } 2>/dev/null; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --pull) PULL=true ;;
    --dry-run) DRY_RUN=true ;;
    -y|--yes) ASSUME_YES=true ;;
    --daemon) FORCE_DAEMON=true ;;
    --plugin) FORCE_PLUGIN=true ;;
    --no-restart) NO_RESTART=true ;;
    --check) CHECK=true ;;
    --force) FORCE=true ;;
    --keep-unit) KEEP_UNIT=true ;;
    --keep|--keep=*)
      if [[ "$1" == --keep ]]; then
        [[ $# -ge 2 && "$2" =~ ^[0-9]+$ ]] || fail "--keep requires a number"
        KEEP="$2"
        shift
      else
        KEEP="${1#*=}"
        [[ "$KEEP" =~ ^[0-9]+$ ]] || fail "--keep requires a number"
      fi
      ;;
    --rollback|--rollback=*)
      if [[ "$1" == --rollback ]]; then
        if [[ $# -ge 2 && "$2" != -* ]]; then
          ROLLBACK="$2"
          shift
        else
          ROLLBACK=latest
        fi
      else
        ROLLBACK="${1#*=}"
      fi
      ;;
    --list-backups) LIST_BACKUPS=true ;;
    -h|--help) sed -n '2,/^$/p' "$0" | sed 's/^# \?//'; exit 0 ;;
    *) fail "unknown option '$1' (try --help)" ;;
  esac
  shift
done

(( KEEP >= 1 )) || fail "--keep must be at least 1"

# --- resolve the installed layout --------------------------------------------
# If APP_DIR was not given, read it from the generated command wrapper so a
# custom PREFIX/INSTALL_DIR install keeps working.
if [[ -z "$APP_DIR" ]]; then
  WRAPPER="$INSTALL_DIR/opencode-telegram"
  if [[ -x "$WRAPPER" ]]; then
    parsed="$(sed -n 's|^exec "\(.*\)/scripts/daemon\.sh".*|\1|p' "$WRAPPER" | head -n1)"
    if [[ -n "$parsed" ]]; then
      APP_DIR="$parsed"
      APP_DIR_SOURCE="wrapper"
    fi
  fi
fi
if [[ -z "$APP_DIR" ]]; then
  APP_DIR="$DEFAULT_APP_DIR"
  APP_DIR_SOURCE="default"
fi

BACKUP_ROOT="$APP_DIR/backups"
MARKER="$APP_DIR/.installed-version"
LOCK_FILE="$APP_DIR/.update.lock"

DAEMON_INSTALLED=false
[[ -x "$APP_DIR/bin/opencode-telegram" ]] && DAEMON_INSTALLED=true
PLUGIN_INSTALLED=false
[[ -f "$APP_DIR/plugins/opencode-telegram/package.json" ]] && PLUGIN_INSTALLED=true

resolve_data_dir() {
  local d="${DATA_DIR:-}"
  if [[ -z "$d" && -f "$APP_DIR/.env" ]]; then
    d="$(sed -n 's/^[[:space:]]*DATA_DIR[[:space:]]*=[[:space:]]*//p' "$APP_DIR/.env" | tail -n1)"
    d="${d%%#*}"
    d="$(printf '%s' "$d" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' \
      -e 's/^"//' -e 's/"$//' -e "s/^'//" -e "s/'$//")"
  fi
  d="${d:-data}"
  [[ "$d" = /* ]] || d="$APP_DIR/$d"
  printf '%s' "$d"
}
DATA_DIR_RESOLVED="$(resolve_data_dir)"

CONFIG_FILE=""
if [[ -f "$OPENCODE_CONFIG_DIR/opencode.jsonc" ]]; then
  CONFIG_FILE="$OPENCODE_CONFIG_DIR/opencode.jsonc"
elif [[ -f "$OPENCODE_CONFIG_DIR/opencode.json" ]]; then
  CONFIG_FILE="$OPENCODE_CONFIG_DIR/opencode.json"
fi

# --- helpers -----------------------------------------------------------------
sha256_sum() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  elif command -v openssl >/dev/null 2>&1; then
    openssl dgst -sha256 "$1" | awk '{print $NF}'
  else
    cksum "$1" | awk '{print $1 "-" $2}'
  fi
}

hash_file() {
  if [[ -f "$1" ]]; then
    sha256_sum "$1"
  else
    printf 'missing'
  fi
}

# hash_tree lists every file under a directory except settings.json (which the
# update may extend). Used to prove that user data was not rewritten.
hash_tree() {
  local dir="$1" f
  [[ -d "$dir" ]] || return 0
  ( cd "$dir" && find . -type f ! -name 'settings.json' -print 2>/dev/null \
      | LC_ALL=C sort \
      | while IFS= read -r f; do
          printf '%s  %s\n' "$(sha256_sum "$f")" "$f"
        done )
}

daemon_command() {
  if [[ -x "$INSTALL_DIR/opencode-telegram" ]]; then
    printf '%s' "$INSTALL_DIR/opencode-telegram"
  elif [[ -x "$APP_DIR/scripts/daemon.sh" ]]; then
    printf '%s' "$APP_DIR/scripts/daemon.sh"
  else
    return 1
  fi
}

daemon_is_running() {
  local cmd
  cmd="$(daemon_command)" || return 1
  "$cmd" status >/dev/null 2>&1
}

systemd_is_active() {
  command -v systemctl >/dev/null 2>&1 && \
    systemctl --user is-active --quiet opencode-telegram 2>/dev/null
}

old_version() {
  local v
  if [[ -f "$MARKER" ]]; then
    v="$(sed -n 's/^version=//p' "$MARKER" | head -n1)"
  fi
  if [[ -z "${v:-}" && -x "$APP_DIR/bin/opencode-telegram" ]]; then
    v="$("$APP_DIR/bin/opencode-telegram" --version 2>/dev/null | awk '{print $2}' || true)"
  fi
  printf '%s' "${v:-unknown}"
}

current_commit() {
  git rev-parse --short HEAD 2>/dev/null || echo unknown
}

confirm() {
  $ASSUME_YES && return 0
  tty_available || return 0
  local answer=""
  printf '%s [Y/n] ' "$1" >&2
  IFS= read -r answer < /dev/tty || return 0
  case "$answer" in
    ""|y|Y|yes|YES) return 0 ;;
    *) return 1 ;;
  esac
}

is_git_repo() { git rev-parse --is-inside-work-tree >/dev/null 2>&1; }

join_lines() { paste -sd, - 2>/dev/null | sed 's/,/, /g'; }

git_dirty() {
  is_git_repo || return 1
  [[ -n "$(git status --porcelain --untracked-files=no 2>/dev/null)" ]]
}

# --- backups -----------------------------------------------------------------
write_backup_info() {
  local file="$1"
  {
    printf 'BK_APP_DIR=%q\n' "$APP_DIR"
    printf 'BK_DATA_DIR=%q\n' "$DATA_DIR_RESOLVED"
    printf 'BK_VERSION=%q\n' "$OLD_VERSION"
    printf 'BK_COMMIT=%q\n' "$OLD_COMMIT"
    printf 'BK_DATE=%q\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf 'BK_WAS_RUNNING=%q\n' "$WAS_RUNNING"
    printf 'BK_RUN_MODE=%q\n' "$RUN_MODE"
    printf 'BK_INSTALL_MODE=%q\n' "$INSTALL_MODE"
    printf 'BK_CONFIG_BASENAME=%q\n' "$(basename "$CONFIG_FILE" 2>/dev/null || true)"
    printf 'BK_UNIT_PRESENT=%q\n' "$UNIT_PRESENT"
  } > "$file"
  chmod 600 "$file"
}

make_backup() {
  local stamp safe
  stamp="$(date +%Y%m%d-%H%M%S)"
  safe="$(printf '%s' "$OLD_VERSION" | tr -c 'A-Za-z0-9._-' '_' | cut -c1-40)"
  BACKUP_DIR="$BACKUP_ROOT/${stamp}${safe:+-$safe}"

  mkdir -p "$BACKUP_DIR"
  chmod 700 "$BACKUP_DIR"

  if [[ -x "$APP_DIR/bin/opencode-telegram" ]]; then
    mkdir -p "$BACKUP_DIR/bin"
    cp -p "$APP_DIR/bin/opencode-telegram" "$BACKUP_DIR/bin/opencode-telegram"
  fi
  if [[ -d "$APP_DIR/plugins/opencode-telegram" ]]; then
    mkdir -p "$BACKUP_DIR/plugins"
    cp -a "$APP_DIR/plugins/opencode-telegram" "$BACKUP_DIR/plugins/opencode-telegram"
  fi
  if [[ -f "$APP_DIR/.env" ]]; then
    cp -p "$APP_DIR/.env" "$BACKUP_DIR/.env"
  fi
  if [[ -d "$DATA_DIR_RESOLVED" ]]; then
    mkdir -p "$BACKUP_DIR/data"
    cp -a "$DATA_DIR_RESOLVED/." "$BACKUP_DIR/data/"
  fi
  if [[ -f "$MARKER" ]]; then
    cp -p "$MARKER" "$BACKUP_DIR/.installed-version"
  fi
  if [[ -n "$CONFIG_FILE" && -f "$CONFIG_FILE" ]]; then
    cp -p "$CONFIG_FILE" "$BACKUP_DIR/opencode-config.$(basename "$CONFIG_FILE")"
  fi
  if [[ "$UNIT_PRESENT" == "true" && -f "$UNIT_FILE" ]]; then
    cp -p "$UNIT_FILE" "$BACKUP_DIR/opencode-telegram.service"
  fi

  write_backup_info "$BACKUP_DIR/info.txt"
  chmod -R go-rwx "$BACKUP_DIR" 2>/dev/null || true

  prune_backups
  info "backup saved to $BACKUP_DIR"
}

prune_backups() {
  local count=0 path
  [[ -d "$BACKUP_ROOT" ]] || return 0
  while IFS= read -r path; do
    [[ -n "$path" ]] || continue
    count=$((count + 1))
    if (( count > KEEP )); then
      rm -rf "$path"
    fi
  done < <(ls -1dt "$BACKUP_ROOT"/*/ 2>/dev/null | sed 's:/$::')
}

latest_backup() {
  local path
  path="$(ls -1dt "$BACKUP_ROOT"/*/ 2>/dev/null | head -n1 | sed 's:/$::' || true)"
  [[ -n "$path" ]] || return 1
  printf '%s' "$path"
}

find_backup() {
  local want="$1"
  if [[ "$want" == "latest" ]]; then
    latest_backup || fail "no backups found in $BACKUP_ROOT"
  elif [[ -d "$want" ]]; then
    printf '%s' "$want"
  elif [[ -d "$BACKUP_ROOT/$want" ]]; then
    printf '%s' "$BACKUP_ROOT/$want"
  else
    fail "backup not found: $want (see --list-backups)"
  fi
}

list_backups() {
  local path
  [[ -d "$BACKUP_ROOT" ]] || { echo "no backups in $BACKUP_ROOT"; return 0; }
  while IFS= read -r path; do
    [[ -n "$path" ]] || continue
    local ver=""
    [[ -f "$path/info.txt" ]] && ver="$(sed -n 's/^BK_VERSION=//p' "$path/info.txt" | head -n1)"
    printf '%s  %s\n' "$(basename "$path")" "${ver//\'/}"
  done < <(ls -1dt "$BACKUP_ROOT"/*/ 2>/dev/null | sed 's:/$::')
}

restart_installed_daemon() {
  local mode="$1"
  if $NO_RESTART; then
    note "daemon left stopped (--no-restart)"
    return 0
  fi
  case "$mode" in
    systemd)
      if command -v systemctl >/dev/null 2>&1; then
        systemctl --user start opencode-telegram 2>/dev/null || \
          warn "could not start the systemd unit; run: systemctl --user start opencode-telegram"
        echo "restarted daemon (systemd user unit)"
      else
        warn "systemd unit was active but systemctl is unavailable"
      fi
      ;;
    wrapper|*)
      local cmd
      if cmd="$(daemon_command)"; then
        if "$cmd" start >/dev/null 2>&1; then
          echo "restarted daemon"
        else
          warn "could not start the daemon; run: $cmd start"
        fi
      fi
      ;;
  esac
}

restore_backup() {
  local dir="$1" quiet="${2:-}"
  local info="$dir/info.txt"
  local -a problems=()
  local was_running run_mode target_app target_data config_base unit_present

  [[ -f "$info" ]] || { warn "not a valid backup (missing info.txt): $dir"; return 1; }

  # shellcheck disable=SC1090
  source "$info"
  target_app="${BK_APP_DIR:-$APP_DIR}"
  target_data="${BK_DATA_DIR:-$target_app/data}"
  was_running="${BK_WAS_RUNNING:-0}"
  run_mode="${BK_RUN_MODE:-wrapper}"
  config_base="${BK_CONFIG_BASENAME:-}"
  unit_present="${BK_UNIT_PRESENT:-false}"

  stop_installed_daemon "quiet" || true

  if [[ -x "$dir/bin/opencode-telegram" ]]; then
    mkdir -p "$target_app/bin"
    cp -p "$dir/bin/opencode-telegram" "$target_app/bin/opencode-telegram" || problems+=("binary")
  fi
  if [[ -d "$dir/plugins/opencode-telegram" ]]; then
    rm -rf "$target_app/plugins/opencode-telegram"
    mkdir -p "$target_app/plugins"
    cp -a "$dir/plugins/opencode-telegram" "$target_app/plugins/opencode-telegram" || problems+=("plugin")
  fi
  if [[ -f "$dir/.env" ]]; then
    cp -p "$dir/.env" "$target_app/.env" || problems+=("env")
    chmod 600 "$target_app/.env" 2>/dev/null || true
  fi
  if [[ -d "$dir/data" ]]; then
    mkdir -p "$target_data"
    cp -a "$dir/data/." "$target_data/" || problems+=("data")
  fi
  if [[ -f "$dir/.installed-version" ]]; then
    cp -p "$dir/.installed-version" "$target_app/.installed-version" || problems+=("version marker")
  fi
  if [[ -n "$config_base" && -f "$dir/opencode-config.$config_base" ]]; then
    cp -p "$dir/opencode-config.$config_base" "$OPENCODE_CONFIG_DIR/$config_base" || problems+=("opencode config")
  fi
  if [[ "$unit_present" == "true" && -f "$dir/opencode-telegram.service" ]]; then
    mkdir -p "$UNIT_DIR"
    cp -p "$dir/opencode-telegram.service" "$UNIT_FILE" || problems+=("systemd unit")
    systemctl --user daemon-reload 2>/dev/null || true
  fi

  RESTORED=true
  if (( ${#problems[@]} > 0 )); then
    warn "rollback restored most files but failed for: ${problems[*]}"
  fi
  if [[ "$was_running" == "1" ]]; then
    restart_installed_daemon "$run_mode"
  fi
  if [[ -z "$quiet" ]]; then
    echo "restored from $dir"
  fi
}

# --- stop / start ------------------------------------------------------------
stop_installed_daemon() {
  local quiet="${1:-}"
  if systemd_is_active; then
    RUN_MODE="systemd"
    if command -v systemctl >/dev/null 2>&1; then
      systemctl --user stop opencode-telegram 2>/dev/null || true
    fi
    [[ -z "$quiet" ]] && echo "stopped daemon (systemd user unit)"
    return 0
  fi
  if daemon_is_running; then
    RUN_MODE="wrapper"
    local cmd
    cmd="$(daemon_command)"
    "$cmd" stop >/dev/null 2>&1 || true
    [[ -z "$quiet" ]] && echo "stopped daemon"
    return 0
  fi
  return 0
}

# --- settings migration ------------------------------------------------------
leaf_paths() {
  jq -r '[paths(scalars)] | .[] | map(tostring) | join(".")' "$1" 2>/dev/null | sort -u
}

merge_settings() {
  local example="$ROOT_DIR/data/settings.example.json"
  local file="$DATA_DIR_RESOLVED/settings.json"
  local merged tmp new_paths=""

  [[ -f "$example" ]] || return 0
  if [[ ! -f "$file" ]]; then
    return 0
  fi
  if ! jq -e . "$file" >/dev/null 2>&1; then
    warn "$file is not valid JSON — leaving it untouched (the daemon will keep refusing to load it)"
    return 0
  fi
  if ! jq -e . "$example" >/dev/null 2>&1; then
    warn "data/settings.example.json is not valid JSON — skipping settings migration"
    return 0
  fi

  # Right-biased recursive merge: the example fills in missing keys and the
  # user's existing values always win.
  merged="$(jq -s '.[0] * .[1]' "$example" "$file")" || {
    warn "could not merge new settings keys — leaving $file untouched"
    return 0
  }
  if [[ "$(jq -S . "$file")" == "$(jq -S . <<<"$merged")" ]]; then
    return 0
  fi

  new_paths="$(comm -13 \
    <(leaf_paths "$file") \
    <(jq -S . <<<"$merged" | jq -r '[paths(scalars)] | .[] | map(tostring) | join(".")' | sort -u) \
    | join_lines || true)"

  tmp="$(mktemp "$(dirname "$file")/.settings.XXXXXX.json")"
  printf '%s\n' "$merged" > "$tmp"
  chmod 600 "$tmp"
  mv -f "$tmp" "$file"

  if [[ -n "$new_paths" ]]; then
    info "settings.json: added new keys (existing values kept): $new_paths"
  else
    info "settings.json: normalized (existing values kept)"
  fi
}

validate_allowlist() {
  local file="$DATA_DIR_RESOLVED/allowlist.json"
  [[ -f "$file" ]] || { warn "allowlist missing: $file"; return 0; }
  if ! jq -e '(.users | type == "array") and (.chats | type == "array")' "$file" >/dev/null 2>&1; then
    warn "allowlist is not valid JSON or has the wrong shape: $file (the daemon denies everyone until it is fixed)"
  fi
}

verify_preserved() {
  local before_env="$1" before_tree="$2" after_env after_tree changed
  after_env="$(hash_file "$APP_DIR/.env")"
  after_tree="$(hash_tree "$DATA_DIR_RESOLVED")"

  if [[ "$before_env" != "missing" && "$before_env" != "$after_env" ]]; then
    fail "installed .env changed unexpectedly during the update"
  fi

  # Only files that existed before must still exist unchanged; newly seeded
  # example files are allowed.
  changed="$(comm -23 <(printf '%s\n' "$before_tree") <(printf '%s\n' "$after_tree") || true)"
  if [[ -n "$changed" ]]; then
    printf '%s\n' "$changed" >&2
    fail "user data changed unexpectedly during the update"
  fi
}

# --- pull --------------------------------------------------------------------
pull_changes() {
  is_git_repo || fail "--pull needs a git checkout (this directory is not a git repository)"
  local branch
  branch="$(git symbolic-ref --short HEAD 2>/dev/null || echo detached)"
  if [[ "$branch" != "main" && "$branch" != "master" ]]; then
    warn "current branch is '$branch', not main; pulling origin/main into it may not be what you want"
  fi
  if git_dirty && ! $FORCE; then
    fail "tracked files have local changes; commit/stash them or pass --force"
  fi

  info "fetching origin..."
  git fetch --prune origin || fail "git fetch failed"

  local target="${branch}"
  git rev-parse --verify --quiet "origin/$target" >/dev/null || target="main"
  local incoming
  incoming="$(git log --oneline "HEAD..origin/$target" 2>/dev/null | head -n 20 || true)"
  if [[ -z "$incoming" ]]; then
    info "already up to date with origin/$target"
  else
    info "incoming changes from origin/$target:"
    printf '%s\n' "$incoming" | sed 's/^/    /'
  fi

  if $DRY_RUN; then
    [[ -n "$incoming" ]] && info "dry run: would run 'git pull --ff-only'"
    return 0
  fi
  if [[ -n "$incoming" ]]; then
    info "pulling (fast-forward only)..."
    git pull --ff-only origin "$target" || fail "git pull was not a fast-forward; resolve the branch manually and re-run"
  fi
}

# --- version marker ----------------------------------------------------------
write_marker() {
  local version="$1"
  {
    printf 'version=%s\n' "$version"
    printf 'commit=%s\n' "$(current_commit)"
    printf 'date=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf 'mode=%s\n' "$INSTALL_MODE"
  } > "$MARKER"
  chmod 600 "$MARKER"
}

refresh_systemd_unit() {
  local template="$ROOT_DIR/packaging/opencode-telegram.service"
  [[ "$UNIT_PRESENT" == "true" ]] || return 0
  [[ -f "$template" ]] || return 0
  if $KEEP_UNIT; then
    note "systemd unit not refreshed (--keep-unit)"
    return 0
  fi

  local tmp
  tmp="$(mktemp "$UNIT_DIR/.opencode-telegram.service.XXXXXX")"
  sed "s|%h/.local/share/opencode-telegram|$APP_DIR|g" "$template" > "$tmp"
  if cmp -s "$tmp" "$UNIT_FILE"; then
    rm -f "$tmp"
    return 0
  fi
  cp -p "$UNIT_FILE" "$UNIT_FILE.bak"
  cat "$tmp" > "$UNIT_FILE"
  rm -f "$tmp"
  chmod 644 "$UNIT_FILE"
  systemctl --user daemon-reload 2>/dev/null || true
  info "refreshed systemd unit (previous: $UNIT_FILE.bak)"
}

# --- rollback path -----------------------------------------------------------
if [[ -n "$ROLLBACK" ]]; then
  chosen="$(find_backup "$ROLLBACK")" || exit 1
  if $DRY_RUN; then
    echo "dry run: would restore $chosen"
    exit 0
  fi
  info "rolling back from $chosen"
  stop_installed_daemon
  RESTORED=false
  restore_backup "$chosen"
  exit 0
fi

if $LIST_BACKUPS; then
  list_backups
  exit 0
fi

# --- selection ---------------------------------------------------------------
if [[ ! -f "$ROOT_DIR/install.sh" ]]; then
  fail "install.sh is missing from $ROOT_DIR — run the updater from the repository checkout"
fi

DO_DAEMON=$DAEMON_INSTALLED
DO_PLUGIN=$PLUGIN_INSTALLED
$FORCE_DAEMON && DO_DAEMON=true
$FORCE_PLUGIN && DO_PLUGIN=true

if ! $DO_DAEMON && ! $DO_PLUGIN; then
  fail "no installed opencode-telegram found at $APP_DIR — run ./install.sh first (or set APP_DIR)"
fi

INSTALL_MODE=""
if $DO_DAEMON && $DO_PLUGIN; then INSTALL_MODE="daemon+plugin"
elif $DO_DAEMON; then INSTALL_MODE="daemon"
else INSTALL_MODE="plugin"; fi

UNIT_PRESENT=false
[[ -f "$UNIT_FILE" ]] && UNIT_PRESENT=true

if [[ ! -d "$APP_DIR" ]]; then
  fail "installed app directory not found: $APP_DIR"
fi

# --- report ------------------------------------------------------------------
info "opencode-telegram update"
echo "    app:     $APP_DIR ($APP_DIR_SOURCE)"
echo "    mode:    $INSTALL_MODE"
echo "    data:    $DATA_DIR_RESOLVED"
if is_git_repo; then
  echo "    repo:    $ROOT_DIR ($(current_commit) on $(git symbolic-ref --short HEAD 2>/dev/null || echo detached))"
else
  echo "    repo:    $ROOT_DIR (not a git checkout)"
fi

OLD_VERSION="$(old_version)"
OLD_COMMIT="$(current_commit)"
echo "    version: $OLD_VERSION"
if [[ "$UNIT_PRESENT" == "true" ]]; then
  echo "    systemd: $UNIT_FILE"
fi
echo

if git_dirty; then
  warn "tracked files have local changes (they will be included in the build)"
  if $PULL && ! $FORCE; then
    note "pass --force to allow --pull with a dirty working tree"
  fi
fi

if $PULL; then
  pull_changes
fi

# --- pre-flight snapshot -----------------------------------------------------
WAS_RUNNING=0
RUN_MODE="wrapper"
if systemd_is_active; then
  WAS_RUNNING=1
  RUN_MODE="systemd"
elif daemon_is_running; then
  WAS_RUNNING=1
  RUN_MODE="wrapper"
fi

BEFORE_ENV="$(hash_file "$APP_DIR/.env")"
BEFORE_TREE="$(hash_tree "$DATA_DIR_RESOLVED")"

if $DRY_RUN; then
  if [[ -f "$DATA_DIR_RESOLVED/settings.json" && -f "$ROOT_DIR/data/settings.example.json" ]]; then
    added="$(comm -13 \
      <(leaf_paths "$DATA_DIR_RESOLVED/settings.json") \
      <(jq -s '.[0] * .[1]' "$ROOT_DIR/data/settings.example.json" "$DATA_DIR_RESOLVED/settings.json" \
        | jq -r '[paths(scalars)] | .[] | map(tostring) | join(".")' | sort -u) \
      | join_lines || true)"
    [[ -n "$added" ]] && echo "    settings: would add missing keys: $added"
  fi
  echo
  info "dry run: would back up, stop the daemon, install, migrate settings, and restart"
  if [[ "$WAS_RUNNING" == "1" ]]; then
    echo "    daemon:  running ($RUN_MODE) — would be restarted"
  else
    echo "    daemon:  not running"
  fi
  exit 0
fi

if ! confirm "Update to the current checkout of $ROOT_DIR?"; then
  echo "nothing changed"
  exit 0
fi

# --- lock --------------------------------------------------------------------
if command -v flock >/dev/null 2>&1; then
  exec 9>"$LOCK_FILE"
  if ! flock -n 9; then
    fail "another update is already running (lock: $LOCK_FILE)"
  fi
fi

BACKUP_DIR=""
RESTORED=false
TRAP_ARMED=false
cleanup_on_exit() {
  local code=$?
  rm -f "$LOCK_FILE" 2>/dev/null || true
  if $TRAP_ARMED && ! $RESTORED && [[ -n "$BACKUP_DIR" && -d "$BACKUP_DIR" ]]; then
    trap - EXIT
    set +e
    echo >&2
    warn "update failed (exit $code); rolling back from $BACKUP_DIR"
    restore_backup "$BACKUP_DIR" quiet
    RESTORED=true
  fi
  return 0
}
trap cleanup_on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# --- backup, stop, install ---------------------------------------------------
make_backup

if [[ "$WAS_RUNNING" == "1" ]]; then
  stop_installed_daemon
else
  echo "daemon is not running"
fi
TRAP_ARMED=true

INSTALL_ARGS=()
if $DO_DAEMON && ! $DO_PLUGIN; then
  INSTALL_ARGS+=(--daemon)
elif $DO_PLUGIN && ! $DO_DAEMON; then
  INSTALL_ARGS+=(--plugin)
fi
$CHECK && INSTALL_ARGS+=(--check)

info "installing ($INSTALL_MODE) from $ROOT_DIR..."
( cd "$ROOT_DIR" && PREFIX="$PREFIX" INSTALL_DIR="$INSTALL_DIR" APP_DIR="$APP_DIR" \
    ./install.sh "${INSTALL_ARGS[@]}" --no-prompt )

# --- migrate settings, refresh unit, verify ----------------------------------
merge_settings
if $DO_DAEMON; then
  validate_allowlist
fi
refresh_systemd_unit
verify_preserved "$BEFORE_ENV" "$BEFORE_TREE"

NEW_VERSION="$OLD_VERSION"
if [[ -x "$APP_DIR/bin/opencode-telegram" ]]; then
  NEW_VERSION="$("$APP_DIR/bin/opencode-telegram" --version 2>/dev/null | awk '{print $2}' || true)"
  [[ -n "$NEW_VERSION" ]] || NEW_VERSION="$OLD_VERSION"
fi
write_marker "$NEW_VERSION"

# --- restart + summary -------------------------------------------------------
if [[ "$WAS_RUNNING" == "1" ]]; then
  restart_installed_daemon "$RUN_MODE"
else
  echo "daemon was not running before the update; leaving it stopped"
fi

TRAP_ARMED=false
echo
info "update complete: $OLD_VERSION -> $NEW_VERSION"
echo "    commit:  $(current_commit)"
echo "    backup:  ${BACKUP_DIR:-none}"
echo "    data:    preserved (allowlist, .env and user files untouched)"
echo
echo "    roll back with: ./update.sh --rollback latest"
if [[ -f "$APP_DIR/.env" && -f "$ROOT_DIR/.env.example" ]]; then
  missing_keys="$(comm -23 \
    <(sed -n 's/^#\{0,1\}[[:space:]]*\([A-Z][A-Z0-9_]*\)=.*/\1/p' "$ROOT_DIR/.env.example" | sort -u) \
    <(sed -n 's/^#\{0,1\}[[:space:]]*\([A-Z][A-Z0-9_]*\)=.*/\1/p' "$APP_DIR/.env" | sort -u) || true)"
  if [[ -n "$missing_keys" ]]; then
    echo
    echo "    new optional variables in .env.example (not added automatically):"
    printf '%s\n' "$missing_keys" | sed 's/^/      /'
  fi
fi
