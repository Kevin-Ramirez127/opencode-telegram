#!/usr/bin/env bash
#
# simulate.sh — send premade OpenCode conversations to Telegram for manual QA.
#
# Usage:
#   ./scripts/simulate.sh                # all scenarios
#   ./scripts/simulate.sh list           # list scenario names
#   ./scripts/simulate.sh done           # one scenario
#   ./scripts/simulate.sh question1      # form with one field
#   ./scripts/simulate.sh question2      # form with two fields
#   ./scripts/simulate.sh question3      # form with three fields
#   ./scripts/simulate.sh text,done      # comma-separated scenarios
#
# It never creates a real OpenCode session. Stop the running daemon first:
# Telegram delivers each update to only one poller, so a live daemon would
# consume the simulated button presses.
set -euo pipefail

ROOT="$(cd "$(dirname "$(readlink -f "${BASH_SOURCE[0]}" 2>/dev/null || echo "${BASH_SOURCE[0]}")")/.." && pwd)"
BIN="$ROOT/bin/opencode-telegram"

scenario="${1:-all}"
shift || true

needs_build() {
  [[ ! -x "$BIN" ]] && return 0
  [[ -n "$(find "$ROOT" -maxdepth 3 -name '*.go' -newer "$BIN" -print -quit 2>/dev/null)" ]]
}

if needs_build; then
  echo "building $BIN..."
  (cd "$ROOT" && go build -o bin/opencode-telegram .)
fi

if [[ "$scenario" == "list" ]]; then
  exec "$BIN" --simulate list
fi

if command -v opencode-telegram >/dev/null 2>&1; then
  if opencode-telegram status >/dev/null 2>&1; then
    echo "warning: the opencode-telegram daemon is running; stop it before using buttons:" >&2
    echo "         opencode-telegram stop" >&2
  fi
fi

echo "simulating: $scenario"
cd "$ROOT"
exec "$BIN" --simulate "$scenario"
