#!/usr/bin/env bash
# Disposable acceptance checks for the binary install manifest lifecycle.

set -euo pipefail
umask 077

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
INSTALL="$ROOT/scripts/install.sh"
UNINSTALL="$ROOT/scripts/uninstall.sh"
TMP=$(mktemp -d "${TMPDIR:-/tmp}/agentctl-install-test.XXXXXX")
trap 'rm -rf "$TMP"' EXIT HUP INT TERM

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
if [ "$(id -u)" -eq 0 ]; then
  printf 'skip: binary installer tests require a non-root user\n'
  exit 0
fi

SOURCE="$TMP/source-agentctl"
FAKE_LOG="$TMP/fake-agentctl.log"
FAKE_DRY="$TMP/fake-bootstrap-dry-run"
FAKE_RUN="$TMP/fake-bootstrap-run"
export FAKE_LOG FAKE_DRY FAKE_RUN
cat >"$SOURCE" <<'SH'
#!/bin/sh
set -eu
printf '%s\n' "$*" >>"$FAKE_LOG"
if [ "${1:-}" = bootstrap ] && [ "${2:-}" = update ]; then
  if [ "${3:-}" = --dry-run ]; then
    : >"$FAKE_DRY"
  else
    : >"$FAKE_RUN"
  fi
  exit 0
fi
exit 0
SH
chmod 0755 "$SOURCE"
PREFIX="$TMP/prefix"

if env -u HOME -u PREFIX "$INSTALL" --binary "$SOURCE" --dry-run >/dev/null 2>"$TMP/no-home.err"; then
  fail 'installer accepted missing HOME without explicit prefix'
fi
grep -q 'HOME is required unless --prefix is supplied' "$TMP/no-home.err" || fail 'missing HOME did not produce stable error'

if "$INSTALL" --binary "$SOURCE" --prefix relative-prefix --dry-run >/dev/null 2>&1; then
  fail 'installer accepted a relative prefix'
fi
[ ! -e relative-prefix ] || fail 'relative-prefix refusal mutated cwd'

outside="$TMP/outside"
mkdir -p "$outside" "$TMP/links"
ln -s "$outside" "$TMP/links/redirect"
if "$INSTALL" --binary "$SOURCE" --prefix "$TMP/links/redirect/prefix" >/dev/null 2>&1; then
  fail 'installer followed a symlinked prefix component'
fi
[ ! -e "$outside/prefix" ] || fail 'symlink refusal mutated external target'

export HOME="$TMP/home"
mkdir -p "$HOME"
"$INSTALL" --binary "$SOURCE" --prefix "$PREFIX" >/dev/null
target="$PREFIX/bin/agentctl"
manifest="$PREFIX/share/agentctl/install-manifest"
[ -x "$target" ] && [ -f "$manifest" ] || fail 'managed install is incomplete'
[ -e "$FAKE_DRY" ] || fail 'default install did not preflight bootstrap update'
[ -e "$FAKE_RUN" ] || fail 'default install did not run installed bootstrap update'
grep -q '^bootstrap update --dry-run$' "$FAKE_LOG" || fail 'default install used the wrong bootstrap preflight argv'
grep -q '^bootstrap update$' "$FAKE_LOG" || fail 'default install did not use the installed binary for bootstrap update'
first_hash=$(shasum -a 256 "$target" | cut -d ' ' -f 1)
rm -f "$FAKE_DRY" "$FAKE_RUN" "$FAKE_LOG"
"$INSTALL" --binary "$SOURCE" --prefix "$PREFIX" --binary-only >/dev/null
[ ! -e "$FAKE_DRY" ] && [ ! -e "$FAKE_RUN" ] && [ ! -e "$FAKE_LOG" ] || fail '--binary-only unexpectedly reconciled bootstrap skills'
[ "$(shasum -a 256 "$target" | cut -d ' ' -f 1)" = "$first_hash" ] || fail 'idempotent install changed binary'

dry_prefix="$TMP/dry-prefix"
rm -f "$FAKE_DRY" "$FAKE_RUN" "$FAKE_LOG"
"$INSTALL" --binary "$SOURCE" --prefix "$dry_prefix" --dry-run >/dev/null
[ ! -e "$dry_prefix/bin/agentctl" ] && [ ! -e "$dry_prefix/share/agentctl/install-manifest" ] || fail 'dry-run wrote binary install artifacts'
[ -e "$FAKE_DRY" ] || fail 'dry-run did not preflight bootstrap update'
[ ! -e "$FAKE_RUN" ] || fail 'dry-run ran mutating bootstrap update'
[ "$(wc -l <"$FAKE_LOG")" -eq 1 ] || fail 'dry-run invoked more than the bootstrap preflight'

absent_supervisor_prefix="$TMP/absent-supervisor-prefix"
"$INSTALL" --binary "$SOURCE" --prefix "$absent_supervisor_prefix" --binary-only >/dev/null
[ ! -e "$HOME/Library/LaunchAgents/io.agentctl.supervisor.agentctl-manifest" ] || fail 'installer created a supervisor when no managed supervisor existed'

recovery_home="$TMP/recovery-home"
recovery_prefix="$TMP/recovery-prefix"
recovery_package="$TMP/recovery-package"
recovery_log="$TMP/recovery-supervisor.log"
mkdir -p "$recovery_home/Library/LaunchAgents" "$recovery_package"
cp "$INSTALL" "$recovery_package/install.sh"
cat >"$recovery_package/install-supervisor.sh" <<'SH'
#!/bin/bash
set -euo pipefail
agentctl_path=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --agentctl) agentctl_path=$2; shift 2 ;;
    *) shift ;;
  esac
done
[ -x "$agentctl_path" ]
printf '%s\n' "$agentctl_path" >>"$RECOVERY_LOG"
SH
chmod 0755 "$recovery_package/install.sh" "$recovery_package/install-supervisor.sh"
cat >"$recovery_home/Library/LaunchAgents/io.agentctl.supervisor.agentctl-manifest" <<EOF
manifest_version=1
managed_by=agentctl-supervisor
label=io.agentctl.supervisor
agentctl=$recovery_prefix/bin/agentctl
plist=$recovery_home/Library/LaunchAgents/io.agentctl.supervisor.plist
state_dir=$recovery_home/state
EOF
HOME="$recovery_home" RECOVERY_LOG="$recovery_log" "$recovery_package/install.sh" --binary "$SOURCE" --prefix "$recovery_prefix" >/dev/null
[ -x "$recovery_prefix/bin/agentctl" ] || fail 'installer did not restore a missing managed supervisor binary'
[ "$(wc -l <"$recovery_log")" -eq 1 ] || fail 'missing-target recovery did not defer supervisor reconciliation until after install'

printf '\0' >>"$target"
if "$UNINSTALL" --prefix "$PREFIX" >/dev/null 2>&1; then
  fail 'uninstaller removed a modified managed binary without force'
fi
"$INSTALL" --binary "$SOURCE" --prefix "$PREFIX" --force >/dev/null
"$UNINSTALL" --prefix "$PREFIX" >/dev/null
[ ! -e "$target" ] && [ ! -e "$manifest" ] || fail 'uninstall left managed files'

printf 'ok: binary install lifecycle is explicit, path-safe, and manifest-bound\n'
