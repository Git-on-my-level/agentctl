#!/usr/bin/env bash
# Disposable systemd-user installer checks. A fake agentctl emits the reviewed
# plan and a fake systemctl records state; no real user service is touched.

set -euo pipefail
umask 077

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
INSTALL="$ROOT/scripts/install-systemd-supervisor.sh"
UNINSTALL="$ROOT/scripts/uninstall-systemd-supervisor.sh"
TMP=$(mktemp -d "${TMPDIR:-/tmp}/agentctl-systemd-test.XXXXXX")
trap 'rm -rf "$TMP"' EXIT HUP INT TERM

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
if [ "$(uname -s)" != Linux ]; then
  printf 'skip: systemd supervisor tests require Linux\n'
  exit 0
fi
if [ "$(id -u)" -eq 0 ]; then
  printf 'skip: systemd supervisor tests require a non-root user\n'
  exit 0
fi

export HOME="$TMP/home"
export XDG_CONFIG_HOME="$HOME/custom-config"
export XDG_STATE_HOME="$HOME/custom-state"
export PATH="$TMP/bin:$PATH"
mkdir -p "$HOME" "$TMP/bin"
AGENTCTL="$TMP/bin/fake-agentctl"
SYSTEMCTL="$TMP/bin/systemctl"
SYSTEMCTL_LOG="$TMP/systemctl.log"
LOADED="$TMP/loaded"
ENABLED="$TMP/enabled"
ACTIVE="$TMP/active"
export SYSTEMCTL_LOG FAKE_LOADED="$LOADED" FAKE_ENABLED="$ENABLED" FAKE_ACTIVE="$ACTIVE"

cat >"$AGENTCTL" <<'SH'
#!/bin/sh
set -eu
state=
exe=
prev=
for arg in "$@"; do
  if [ "$prev" = --state-dir ]; then state=$arg; fi
  if [ "$prev" = --executable ]; then exe=$arg; fi
  prev=$arg
done
[ -n "$state" ] && [ -n "$exe" ] || exit 2
python3 - "$XDG_CONFIG_HOME" "$state" "$exe" <<'PY'
import base64, json, os, sys
config, state, exe = sys.argv[1:]
def quote(value):
    escaped = (value.replace('\\', '\\\\').replace('"', '\\"')
                    .replace('\n', '\\n').replace('\r', '\\r').replace('\t', '\\t')
                    .replace('%', '%%'))
    if value and not any(ch in value for ch in " \t\n\r\\\"'"):
        return escaped
    return '"' + escaped + '"'
name = 'io.agentctl.supervisor'
path = os.path.join(config, 'systemd', 'user', name + '.service')
if os.environ.get('PLAN_BAD') == 'path': path = os.path.join(config, 'wrong.service')
argv = [exe, 'supervisor', 'run', '--socket', os.path.join(state, 'supervisor.sock'), '--state-dir', state]
exec_start = ' '.join(quote(value) for value in argv)
service = {'UnitName': name, 'Description': 'agentctl host-local supervisor', 'ExecStart': exec_start, 'Environment': None, 'Restart': 'on-failure', 'WantedBy': 'default.target'}
unit = (f'[Unit]\nDescription=agentctl host-local supervisor\n\n[Service]\nType=simple\n'
        f'ExecStart={exec_start}\nRestart=on-failure\n\n[Install]\nWantedBy=default.target\n').encode()
print(json.dumps({'ok': True, 'result': {'Path': path, 'Contents': base64.b64encode(unit).decode(), 'Service': service}}, separators=(',', ':')))
PY
SH
chmod 0755 "$AGENTCTL"

cat >"$SYSTEMCTL" <<'SH'
#!/bin/sh
set -eu
printf '%s\n' "$*" >>"$SYSTEMCTL_LOG"
[ "$1" = --user ] || exit 2
shift
action=$1
shift
if [ -n "${FAIL_ENABLE_ONCE_FILE:-}" ] && [ "$action" = enable ] && [ ! -e "$FAIL_ENABLE_ONCE_FILE" ]; then
  : >"$FAIL_ENABLE_ONCE_FILE"
  exit 97
fi
if [ -n "${FAIL_RELOAD_ONCE_FILE:-}" ] && [ "$action" = daemon-reload ] && [ ! -e "$FAIL_RELOAD_ONCE_FILE" ]; then
  : >"$FAIL_RELOAD_ONCE_FILE"
  exit 97
fi
case "$action" in
  show)
    if [ -e "$FAKE_LOADED" ]; then printf '%s\n' loaded; else printf '%s\n' not-found; fi
    ;;
  daemon-reload)
    if [ -f "$XDG_CONFIG_HOME/systemd/user/io.agentctl.supervisor.service" ]; then : >"$FAKE_LOADED"; else rm -f "$FAKE_LOADED"; fi
    ;;
  enable)
    : >"$FAKE_ENABLED"; : >"$FAKE_LOADED"
    [ "${1:-}" != --now ] || : >"$FAKE_ACTIVE"
    ;;
  disable)
    rm -f "$FAKE_ENABLED"
    [ "${1:-}" != --now ] || rm -f "$FAKE_ACTIVE"
    ;;
  start) : >"$FAKE_ACTIVE"; : >"$FAKE_LOADED" ;;
  is-enabled) [ -e "$FAKE_ENABLED" ] ;;
  is-active) [ -e "$FAKE_ACTIVE" ] ;;
  *) exit 2 ;;
esac
SH
chmod 0755 "$SYSTEMCTL"

unit="$XDG_CONFIG_HOME/systemd/user/io.agentctl.supervisor.service"
manifest="$unit.agentctl-manifest"
state="$XDG_STATE_HOME/agentctl"

if "$INSTALL" --state-dir "$state" >/dev/null 2>&1; then fail 'installer accepted missing --agentctl'; fi
if "$INSTALL" --agentctl relative >/dev/null 2>&1; then fail 'installer accepted relative --agentctl'; fi
if XDG_CONFIG_HOME=relative "$INSTALL" --agentctl "$AGENTCTL" >/dev/null 2>&1; then fail 'installer accepted relative XDG_CONFIG_HOME'; fi
ln -s "$HOME" "$TMP/linked-home"
if HOME="$TMP/linked-home" "$INSTALL" --agentctl "$AGENTCTL" >/dev/null 2>&1; then fail 'installer accepted a symlinked parent path'; fi
if "$INSTALL" --agentctl "$AGENTCTL" --state-dir "$state"$'\nforged=1' >/dev/null 2>&1; then fail 'installer accepted a manifest-line injection'; fi

"$INSTALL" --agentctl "$AGENTCTL" --dry-run >/dev/null
[ ! -e "$unit" ] && [ ! -e "$manifest" ] || fail 'dry-run wrote managed files'
[ ! -e "$SYSTEMCTL_LOG" ] || fail 'dry-run touched systemctl'

export PLAN_BAD=path
if "$INSTALL" --agentctl "$AGENTCTL" >/dev/null 2>&1; then fail 'installer accepted plan outside XDG systemd path'; fi
unset PLAN_BAD
[ ! -e "$unit" ] && [ ! -e "$manifest" ] || fail 'invalid plan wrote managed files'

"$INSTALL" --agentctl "$AGENTCTL" --output json | grep -q '"state":"installed"'
[ -f "$unit" ] && [ -f "$manifest" ] || fail 'install did not write managed files'
[ "$(stat -c '%a' "$unit")" = 600 ] || fail 'unit is not owner-only'
[ "$(stat -c '%a' "$manifest")" = 600 ] || fail 'manifest is not owner-only'
grep -Fq "ExecStart=$AGENTCTL supervisor run --socket $state/supervisor.sock --state-dir $state" "$unit" || fail 'unit argv does not match requested paths'
[ -e "$LOADED" ] && [ -e "$ENABLED" ] && [ -e "$ACTIVE" ] || fail 'install did not enable and start service'
grep -q '^--user daemon-reload$' "$SYSTEMCTL_LOG" || fail 'install did not reload systemd'
grep -q '^--user enable --now io.agentctl.supervisor.service$' "$SYSTEMCTL_LOG" || fail 'install did not enable service'

first_hash=$(sha256sum "$unit" | cut -d ' ' -f 1)
"$INSTALL" --agentctl "$AGENTCTL" >/dev/null
[ "$(sha256sum "$unit" | cut -d ' ' -f 1)" = "$first_hash" ] || fail 'idempotent install changed unit bytes'

rm -f "$manifest"
if "$INSTALL" --agentctl "$AGENTCTL" >/dev/null 2>&1; then fail 'installer overwrote unmanaged unit'; fi
"$INSTALL" --agentctl "$AGENTCTL" --force >/dev/null
printf '%s\n' modified >>"$unit"
if "$INSTALL" --agentctl "$AGENTCTL" >/dev/null 2>&1; then fail 'installer accepted modified managed unit'; fi
"$INSTALL" --agentctl "$AGENTCTL" --force >/dev/null

before_unit=$(sha256sum "$unit" | cut -d ' ' -f 1)
before_manifest=$(sha256sum "$manifest" | cut -d ' ' -f 1)
export FAIL_ENABLE_ONCE_FILE="$TMP/enable-failed-once"
if "$INSTALL" --agentctl "$AGENTCTL" --force >/dev/null 2>&1; then fail 'installer hid enable failure'; fi
unset FAIL_ENABLE_ONCE_FILE
[ "$(sha256sum "$unit" | cut -d ' ' -f 1)" = "$before_unit" ] || fail 'activation rollback did not restore unit'
[ "$(sha256sum "$manifest" | cut -d ' ' -f 1)" = "$before_manifest" ] || fail 'activation rollback did not restore manifest'
[ -e "$ENABLED" ] && [ -e "$ACTIVE" ] || fail 'activation rollback did not restore service state'

# Fail the second managed-file replacement after the old service was stopped.
# The armed EXIT transaction must restore both old files and service state.
REAL_MV=$(command -v mv)
export REAL_MV FAIL_MV_DEST="$manifest" FAIL_MV_ONCE="$TMP/mv-failed-once"
cat >"$TMP/bin/mv" <<'SH'
#!/bin/sh
set -eu
eval "last=\${$#}"
if [ "$last" = "$FAIL_MV_DEST" ] && [ ! -e "$FAIL_MV_ONCE" ]; then
  : >"$FAIL_MV_ONCE"
  exit 98
fi
exec "$REAL_MV" "$@"
SH
chmod 0755 "$TMP/bin/mv"
if "$INSTALL" --agentctl "$AGENTCTL" --force >/dev/null 2>&1; then fail 'installer hid mid-mutation failure'; fi
unset FAIL_MV_DEST FAIL_MV_ONCE
rm -f "$TMP/bin/mv"
[ "$(sha256sum "$unit" | cut -d ' ' -f 1)" = "$before_unit" ] || fail 'mutation rollback did not restore unit'
[ "$(sha256sum "$manifest" | cut -d ' ' -f 1)" = "$before_manifest" ] || fail 'mutation rollback did not restore manifest'
[ -e "$ENABLED" ] && [ -e "$ACTIVE" ] || fail 'mutation rollback did not restore service state'

mkdir -p "$state"
printf '%s\n' config >"$HOME/config-sentinel"
printf '%s\n' journal >"$state/journal.db"
"$UNINSTALL" --agentctl "$AGENTCTL" --dry-run >/dev/null
[ -f "$unit" ] && [ -f "$manifest" ] && [ -e "$ACTIVE" ] || fail 'uninstall dry-run mutated service'

export FAIL_RELOAD_ONCE_FILE="$TMP/reload-failed-once"
if "$UNINSTALL" --agentctl "$AGENTCTL" >/dev/null 2>&1; then fail 'uninstaller hid daemon-reload failure'; fi
unset FAIL_RELOAD_ONCE_FILE
[ -f "$unit" ] && [ -f "$manifest" ] || fail 'uninstall rollback did not restore files'
[ -e "$ENABLED" ] && [ -e "$ACTIVE" ] || fail 'uninstall rollback did not restore service state'

printf '%s\n' modified >>"$unit"
if "$UNINSTALL" --agentctl "$AGENTCTL" >/dev/null 2>&1; then fail 'uninstaller accepted modified unit'; fi
"$INSTALL" --agentctl "$AGENTCTL" --force >/dev/null
"$UNINSTALL" --agentctl "$AGENTCTL" --output json | grep -q '"state":"uninstalled"'
[ ! -e "$unit" ] && [ ! -e "$manifest" ] || fail 'uninstall left managed files'
[ ! -e "$LOADED" ] && [ ! -e "$ENABLED" ] && [ ! -e "$ACTIVE" ] || fail 'uninstall left service state'
[ "$(cat "$HOME/config-sentinel")" = config ] || fail 'uninstall removed unrelated config'
[ "$(cat "$state/journal.db")" = journal ] || fail 'uninstall removed journal'

printf 'ok: systemd installer is plan-derived, owner-only, rollback-safe, and state-preserving\n'
