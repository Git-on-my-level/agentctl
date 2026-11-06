#!/usr/bin/env bash
# Checks that scripts/go-hermetic.sh hides operator state from the wrapped command,
# removes its scratch tree, and passes the command's exit status through.
set -euo pipefail

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
WRAP="$ROOT/scripts/go-hermetic.sh"
TMP=$(mktemp -d "${TMPDIR:-/tmp}/agentctl-hermetic-test.XXXXXX")
trap 'rm -rf "$TMP"' EXIT HUP INT TERM

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# Run from a directory without go.mod so the Go toolchain never downloads a
# different version while the wrapper queries its cache locations.
cd "$TMP"
operator_home="$TMP/operator-home"
mkdir -p "$operator_home/.config/agentctl"
printf '{}\n' >"$operator_home/.config/agentctl/config.json"
export GOTOOLCHAIN=local

seen=$(HOME="$operator_home" AGENTCTL_CONFIG="$operator_home/.config/agentctl/config.json" \
  XDG_CONFIG_HOME="$operator_home/.config" CLAUDECODE=1 \
  "$WRAP" sh -c 'printf "%s|%s|%s|%s" "$HOME" "${AGENTCTL_CONFIG-unset}" "${XDG_CONFIG_HOME-unset}" "${CLAUDECODE-unset}"')

IFS='|' read -r child_home child_config child_xdg child_marker <<<"$seen"
[ "$child_home" != "$operator_home" ] || fail "child inherited operator HOME"
[ "$child_config" = unset ] || fail "AGENTCTL_CONFIG leaked into the child"
[ "$child_xdg" != "$operator_home/.config" ] || fail "XDG_CONFIG_HOME leaked into the child"
[ "$child_marker" = unset ] || fail "agent-session marker leaked into the child"
[ -e "$child_home" ] && fail "scratch HOME still exists after the command finished"

set +e
"$WRAP" sh -c 'exit 7'
status=$?
set -e
[ "$status" -eq 7 ] || fail "exit status not propagated: want 7, got $status"

printf 'go-hermetic checks passed\n'
