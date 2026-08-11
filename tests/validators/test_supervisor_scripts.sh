#!/usr/bin/env bash
# Disposable launchd distribution acceptance tests. A fake agentctl emits a
# reviewed supervisor plan and a fake launchctl records load/unload calls; no
# real launchd service is touched.

set -euo pipefail
umask 077

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
INSTALL="$ROOT/scripts/install-supervisor.sh"
UNINSTALL="$ROOT/scripts/uninstall-supervisor.sh"
TMP=$(mktemp -d "${TMPDIR:-/tmp}/agentctl-supervisor-test.XXXXXX")
trap 'rm -rf "$TMP"' EXIT HUP INT TERM

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
if [ "$(uname -s)" != Darwin ]; then
  printf 'skip: launchd supervisor tests require macOS\n'
  exit 0
fi
if [ "$(id -u)" -eq 0 ]; then
  printf 'skip: launchd supervisor tests require a non-root user\n'
  exit 0
fi

export HOME="$TMP/home"
export PATH="$TMP/bin:$PATH"
mkdir -p "$HOME" "$TMP/bin"
AGENTCTL="$TMP/bin/fake-agentctl"
LAUNCHCTL="$TMP/bin/launchctl"
ARGS_LOG="$TMP/agentctl-args.log"
LAUNCH_LOG="$TMP/launchctl.log"
LOADED="$TMP/loaded"
export AGENTCTL_ARGS_LOG="$ARGS_LOG" LAUNCHCTL_LOG="$LAUNCH_LOG" FAKE_LOADED="$LOADED"

cat >"$AGENTCTL" <<'SH'
#!/bin/sh
set -eu
printf '%s\n' "$*" >>"$AGENTCTL_ARGS_LOG"
state=
exe=
prev=
for arg in "$@"; do
  if [ "$prev" = --state-dir ]; then state=$arg; fi
  if [ "$prev" = --executable ]; then exe=$arg; fi
  prev=$arg
done
[ -n "$state" ] && [ -n "$exe" ] || exit 2
python3 - "$HOME" "$state" "$exe" <<'PY'
import base64, json, os, plistlib, sys
home, state, exe = sys.argv[1:]
label = 'io.agentctl.supervisor'
plist_path = os.path.join(home, 'Library', 'LaunchAgents', label + '.plist')
if os.environ.get('PLAN_BAD') == 'path':
    plist_path = os.path.join(home, 'wrong.plist')
args = [exe, 'supervisor', 'run', '--socket', os.path.join(state, 'supervisor.sock'), '--state-dir', state]
plist = plistlib.dumps({'Label': label, 'ProgramArguments': args, 'RunAtLoad': True, 'KeepAlive': True}, fmt=plistlib.FMT_XML, sort_keys=False)
print(json.dumps({'ok': True, 'schema_version': 1, 'result': {'Path': plist_path, 'Contents': base64.b64encode(plist).decode(), 'Service': {'Label': label, 'ProgramArguments': args, 'Environment': None, 'RunAtLoad': True, 'KeepAlive': True}}, 'warnings': [], 'next_actions': []}, separators=(',', ':')))
PY
SH
chmod 0755 "$AGENTCTL"

cat >"$LAUNCHCTL" <<'SH'
#!/bin/sh
set -eu
printf '%s\n' "$*" >>"$LAUNCHCTL_LOG"
if [ "${FAIL_ACTION:-}" = "$1" ]; then exit 97; fi
case "$1" in
  print)
    if [ -e "$FAKE_LOADED" ] && [ -n "${HIDE_LOADED_PRINT_COUNT_FILE:-}" ] && [ -f "$HIDE_LOADED_PRINT_COUNT_FILE" ]; then
      remaining=$(cat "$HIDE_LOADED_PRINT_COUNT_FILE")
      if [ "$remaining" -gt 0 ]; then
        printf '%s\n' "$((remaining - 1))" >"$HIDE_LOADED_PRINT_COUNT_FILE"
        exit 113
      fi
    fi
    if [ -n "${DELAYED_BOOTOUT_COUNT_FILE:-}" ] && [ -f "$DELAYED_BOOTOUT_COUNT_FILE" ]; then
      remaining=$(cat "$DELAYED_BOOTOUT_COUNT_FILE")
      if [ "$remaining" -gt 0 ]; then
        printf '%s\n' "$((remaining - 1))" >"$DELAYED_BOOTOUT_COUNT_FILE"
      else
        rm -f "$FAKE_LOADED" "$DELAYED_BOOTOUT_COUNT_FILE"
      fi
    fi
    [ -e "$FAKE_LOADED" ] || exit 113
    printf 'path = %s\n' "$HOME/Library/LaunchAgents/io.agentctl.supervisor.plist"
    ;;
  bootout)
    if [ -n "${DELAYED_BOOTOUT_COUNT_FILE:-}" ]; then
      printf '%s\n' "${DELAYED_BOOTOUT_PRINTS:-3}" >"$DELAYED_BOOTOUT_COUNT_FILE"
    else
      rm -f "$FAKE_LOADED"
    fi
    ;;
  bootstrap)
    if [ -n "${FAIL_BOOTSTRAP_ONCE_FILE:-}" ] && [ ! -e "$FAIL_BOOTSTRAP_ONCE_FILE" ]; then
      : >"$FAIL_BOOTSTRAP_ONCE_FILE"
      exit 97
    fi
    : >"$FAKE_LOADED"
    ;;
  kickstart) : ;;
  *) exit 2 ;;
esac
SH
chmod 0755 "$LAUNCHCTL"

plist="$HOME/Library/LaunchAgents/io.agentctl.supervisor.plist"
manifest="$HOME/Library/LaunchAgents/io.agentctl.supervisor.agentctl-manifest"
state="$HOME/.local/state/agentctl"

if "$INSTALL" --state-dir "$state" >/dev/null 2>&1; then fail 'installer accepted missing --agentctl'; fi
if "$INSTALL" --agentctl relative-agentctl >/dev/null 2>&1; then fail 'installer accepted relative --agentctl'; fi
if "$INSTALL" --agentctl "$AGENTCTL" --state-dir relative-state >/dev/null 2>&1; then fail 'installer accepted relative state dir'; fi

"$INSTALL" --agentctl "$AGENTCTL" --state-dir "$state" --dry-run >/dev/null
[ ! -e "$plist" ] || fail 'dry-run wrote plist'
[ ! -e "$manifest" ] || fail 'dry-run wrote manifest'
[ ! -e "$LAUNCH_LOG" ] || fail 'dry-run touched launchctl'

export PLAN_BAD=path
if "$INSTALL" --agentctl "$AGENTCTL" --state-dir "$state" >/dev/null 2>&1; then fail 'installer accepted a plan with an unexpected path'; fi
unset PLAN_BAD
[ ! -e "$plist" ] && [ ! -e "$manifest" ] || fail 'invalid plan wrote managed files'

"$INSTALL" --agentctl "$AGENTCTL" --state-dir "$state" --output json | grep -q '"state":"installed"'
[ -f "$plist" ] || fail 'install did not write plist'
[ -f "$manifest" ] || fail 'install did not write manifest'
[ "$(stat -f '%Lp' "$plist")" = 600 ] || fail 'plist is not owner-only'
[ "$(stat -f '%Lp' "$manifest")" = 600 ] || fail 'manifest is not owner-only'
python3 - "$plist" <<'PY'
import plistlib, sys
with open(sys.argv[1], 'rb') as fh: data = plistlib.load(fh)
assert data['Label'] == 'io.agentctl.supervisor'
assert data['ProgramArguments'][1:3] == ['supervisor', 'run']
assert data['RunAtLoad'] is True and data['KeepAlive'] is True
PY
grep -q 'bootstrap gui/' "$LAUNCH_LOG" || fail 'install did not bootstrap service'
grep -q 'kickstart -k gui/' "$LAUNCH_LOG" || fail 'install did not kickstart service'

first_hash=$(shasum -a 256 "$plist" | cut -d ' ' -f 1)
"$INSTALL" --agentctl "$AGENTCTL" --state-dir "$state" >/dev/null
second_hash=$(shasum -a 256 "$plist" | cut -d ' ' -f 1)
[ "$first_hash" = "$second_hash" ] || fail 'idempotent install changed plist bytes'

export FAIL_BOOTSTRAP_ONCE_FILE="$TMP/bootstrap-failed-once"
"$INSTALL" --agentctl "$AGENTCTL" --state-dir "$state" >/dev/null
[ -e "$FAIL_BOOTSTRAP_ONCE_FILE" ] && [ -e "$LOADED" ] || fail 'installer did not recover from a transient launchd bootstrap race'
unset FAIL_BOOTSTRAP_ONCE_FILE

export DELAYED_BOOTOUT_COUNT_FILE="$TMP/delayed-bootout-count" DELAYED_BOOTOUT_PRINTS=3
"$INSTALL" --agentctl "$AGENTCTL" --state-dir "$state" >/dev/null
[ -e "$LOADED" ] || fail 'installer did not wait for delayed launchd bootout completion'
unset DELAYED_BOOTOUT_COUNT_FILE DELAYED_BOOTOUT_PRINTS

rm -f "$manifest"
if "$INSTALL" --agentctl "$AGENTCTL" --state-dir "$state" >/dev/null 2>&1; then fail 'installer overwrote unmanaged plist'; fi
[ -f "$plist" ] || fail 'conflict refusal removed plist'
"$INSTALL" --agentctl "$AGENTCTL" --state-dir "$state" --force >/dev/null

printf '%s\n' modified >>"$plist"
if "$INSTALL" --agentctl "$AGENTCTL" --state-dir "$state" >/dev/null 2>&1; then fail 'installer accepted modified managed plist'; fi
"$INSTALL" --agentctl "$AGENTCTL" --state-dir "$state" --force >/dev/null

# A launchd kickstart failure restores both managed files and the previously
# loaded service instead of leaving a new plist with an old manifest.
before_plist=$(shasum -a 256 "$plist" | cut -d ' ' -f 1)
before_manifest=$(shasum -a 256 "$manifest" | cut -d ' ' -f 1)
export FAIL_ACTION=kickstart
if "$INSTALL" --agentctl "$AGENTCTL" --state-dir "$state" --force >/dev/null 2>&1; then fail 'installer hid launchctl kickstart failure'; fi
unset FAIL_ACTION
[ "$(shasum -a 256 "$plist" | cut -d ' ' -f 1)" = "$before_plist" ] || fail 'kickstart rollback did not restore plist'
[ "$(shasum -a 256 "$manifest" | cut -d ' ' -f 1)" = "$before_manifest" ] || fail 'kickstart rollback did not restore manifest'
[ -e "$LOADED" ] || fail 'kickstart rollback did not restore loaded service'

# With no previous installation, a bootstrap failure removes the new plist and
# manifest rather than leaving a half-installed service definition.
"$UNINSTALL" --agentctl "$AGENTCTL" >/dev/null
[ ! -e "$plist" ] && [ ! -e "$manifest" ] || fail 'setup uninstall did not remove managed files'
export HIDE_LOADED_PRINT_COUNT_FILE="$TMP/hide-loaded-print-count"
printf '%s\n' 10 >"$HIDE_LOADED_PRINT_COUNT_FILE"
if "$INSTALL" --agentctl "$AGENTCTL" --state-dir "$state" >/dev/null 2>&1; then fail 'installer accepted an unverified loaded service'; fi
unset HIDE_LOADED_PRINT_COUNT_FILE
[ ! -e "$plist" ] && [ ! -e "$manifest" ] || fail 'unverified-load rollback left new artifacts'
[ ! -e "$LOADED" ] || fail 'unverified-load rollback left service loaded'
export FAIL_ACTION=bootstrap
if "$INSTALL" --agentctl "$AGENTCTL" --state-dir "$state" >/dev/null 2>&1; then fail 'installer hid launchctl bootstrap failure'; fi
unset FAIL_ACTION
[ ! -e "$plist" ] && [ ! -e "$manifest" ] || fail 'bootstrap rollback left new artifacts'
[ ! -e "$LOADED" ] || fail 'bootstrap rollback left service loaded'
"$INSTALL" --agentctl "$AGENTCTL" --state-dir "$state" >/dev/null

# If the previous managed files exist but the service was not loaded, a
# kickstart failure restores files without unexpectedly loading the service.
"$LAUNCHCTL" bootout "gui/$(id -u)/io.agentctl.supervisor"
before_plist=$(shasum -a 256 "$plist" | cut -d ' ' -f 1)
before_manifest=$(shasum -a 256 "$manifest" | cut -d ' ' -f 1)
export FAIL_ACTION=kickstart
if "$INSTALL" --agentctl "$AGENTCTL" --state-dir "$state" --force >/dev/null 2>&1; then fail 'installer hid unloaded kickstart failure'; fi
unset FAIL_ACTION
[ "$(shasum -a 256 "$plist" | cut -d ' ' -f 1)" = "$before_plist" ] || fail 'unloaded rollback did not restore plist'
[ "$(shasum -a 256 "$manifest" | cut -d ' ' -f 1)" = "$before_manifest" ] || fail 'unloaded rollback did not restore manifest'
[ ! -e "$LOADED" ] || fail 'unloaded rollback unexpectedly loaded service'
"$INSTALL" --agentctl "$AGENTCTL" --state-dir "$state" >/dev/null

mkdir -p "$state"
printf '%s\n' config >"$HOME/.agentctl-config"
printf '%s\n' journal >"$state/journal.db"
printf '%s\n' supervisor >"$state/supervisor-state.json"
"$UNINSTALL" --agentctl "$AGENTCTL" --dry-run >/dev/null
[ -f "$plist" ] && [ -f "$manifest" ] || fail 'uninstall dry-run changed managed files'
[ -e "$LOADED" ] || fail 'uninstall dry-run unloaded service'

printf '%s\n' modified >>"$plist"
if "$UNINSTALL" --agentctl "$AGENTCTL" >/dev/null 2>&1; then fail 'uninstall accepted modified plist'; fi
[ -f "$plist" ] && [ -f "$manifest" ] || fail 'failed uninstall removed managed files'
"$INSTALL" --agentctl "$AGENTCTL" --state-dir "$state" --force >/dev/null
"$UNINSTALL" --agentctl "$AGENTCTL" --output json | grep -q '"state":"uninstalled"'
[ ! -e "$plist" ] && [ ! -e "$manifest" ] || fail 'uninstall left managed files'
[ ! -e "$LOADED" ] || fail 'uninstall left launchd service loaded'
[ "$(cat "$HOME/.agentctl-config")" = config ] || fail 'uninstall removed config'
[ "$(cat "$state/journal.db")" = journal ] || fail 'uninstall removed journal'
[ "$(cat "$state/supervisor-state.json")" = supervisor ] || fail 'uninstall removed supervisor state'

printf 'ok: supervisor installer is plan-derived, owner-only, conflict-safe, and state-preserving\n'
