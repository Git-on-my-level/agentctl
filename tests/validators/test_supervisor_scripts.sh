#!/usr/bin/env bash
# Disposable launchd distribution acceptance tests. A fake agentctl emits a
# reviewed supervisor plan and a fake launchctl records load/unload calls; no
# real launchd service is touched.

set -euo pipefail
umask 077

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
INSTALL="$ROOT/scripts/install-supervisor.sh"
BINARY_INSTALL="$ROOT/scripts/install.sh"
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
if [ "${1:-}" = bootstrap ]; then exit 0; fi
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
log_dir = os.path.join(home, 'Library', 'Logs', 'agentctl')
stdout_path = os.path.join(log_dir, 'supervisor.out.log')
stderr_path = os.path.join(log_dir, 'supervisor.err.log')
if os.environ.get('PLAN_BAD') == 'logs':
    stdout_path = os.path.join(home, 'unreviewed.out.log')
keys = {'Label': label, 'ProgramArguments': args, 'RunAtLoad': True, 'KeepAlive': True,
        'StandardOutPath': stdout_path, 'StandardErrorPath': stderr_path, 'ThrottleInterval': 10}
if os.environ.get('PLAN_LEGACY'):
    # A binary released before the reviewed log contract omits the keys entirely.
    for key in ('StandardOutPath', 'StandardErrorPath', 'ThrottleInterval'):
        keys.pop(key)
plist = plistlib.dumps(keys, fmt=plistlib.FMT_XML, sort_keys=False)
service = dict(keys)
service['Environment'] = None
print(json.dumps({'ok': True, 'schema_version': 1, 'result': {'Path': plist_path, 'Contents': base64.b64encode(plist).decode(), 'Service': service}, 'warnings': [], 'next_actions': []}, separators=(',', ':')))
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
export PLAN_BAD=logs
if "$INSTALL" --agentctl "$AGENTCTL" --state-dir "$state" >/dev/null 2>&1; then fail 'installer accepted a plan with an unreviewed log path'; fi
unset PLAN_BAD
[ ! -e "$plist" ] && [ ! -e "$manifest" ] || fail 'invalid plan wrote managed files'

# A binary released before the reviewed log contract renders a plan without the
# log keys. It must never install such a plan, and it must never be the binary
# whose plan an upgrade is judged by.
LEGACY="$TMP/bin/legacy-agentctl"
cat >"$LEGACY" <<'SH'
#!/bin/sh
set -eu
PLAN_LEGACY=1 export PLAN_LEGACY
exec "$FAKE_CURRENT_AGENTCTL" "$@"
SH
chmod 0755 "$LEGACY"
export FAKE_CURRENT_AGENTCTL="$AGENTCTL"

if "$INSTALL" --agentctl "$LEGACY" --state-dir "$state" >/dev/null 2>&1; then
  fail 'installer accepted a plan that predates the reviewed log contract'
fi
[ ! -e "$plist" ] && [ ! -e "$manifest" ] || fail 'legacy plan wrote managed files'

if "$INSTALL" --agentctl "$AGENTCTL" --plan-with relative-planner --state-dir "$state" >/dev/null 2>&1; then
  fail 'installer accepted a relative --plan-with'
fi
if "$INSTALL" --agentctl "$AGENTCTL" --plan-with "$TMP/bin/absent-planner" --state-dir "$state" >/dev/null 2>&1; then
  fail 'installer accepted a --plan-with that does not exist'
fi

# --plan-with renders the plan with the replacement while the reviewed service
# executable stays the installed target: the upgrade preflight shape.
"$INSTALL" --agentctl "$LEGACY" --plan-with "$AGENTCTL" --state-dir "$state" --dry-run >/dev/null \
  || fail 'upgrade preflight rejected a replacement-rendered plan for the installed target'
[ ! -e "$plist" ] || fail 'upgrade preflight wrote plist'
[ ! -e "$manifest" ] || fail 'upgrade preflight wrote manifest'

"$INSTALL" --agentctl "$LEGACY" --plan-with "$AGENTCTL" --state-dir "$state" --output json >/dev/null \
  || fail 'replacement-rendered plan did not install'
python3 - "$plist" "$LEGACY" <<'UPGRADEPY'
import plistlib, sys
with open(sys.argv[1], 'rb') as fh: data = plistlib.load(fh)
assert data['ProgramArguments'][0] == sys.argv[2], data['ProgramArguments']
assert data['StandardOutPath'].endswith('/Library/Logs/agentctl/supervisor.out.log')
assert data['ThrottleInterval'] == 10
UPGRADEPY
grep -Fqx "agentctl=$LEGACY" "$manifest" || fail 'manifest did not bind the reviewed service executable'
rm -f "$plist" "$manifest" "$LAUNCH_LOG" "$LOADED"

# End to end: upgrading a managed installation whose live supervisor still
# points at a binary that predates the reviewed log contract must succeed.
UPGRADE_PREFIX="$TMP/upgrade-prefix"
mkdir -p "$UPGRADE_PREFIX/bin" "$UPGRADE_PREFIX/share/agentctl"
UPGRADE_TARGET="$UPGRADE_PREFIX/bin/agentctl"
cp "$LEGACY" "$UPGRADE_TARGET"
chmod 0755 "$UPGRADE_TARGET"
{
  printf '%s\n' 'manifest_version=1'
  printf 'target=%s\n' "$UPGRADE_TARGET"
  printf 'sha256=%s\n' "$(shasum -a 256 "$UPGRADE_TARGET" | cut -d ' ' -f 1)"
} >"$UPGRADE_PREFIX/share/agentctl/install-manifest"
chmod 0600 "$UPGRADE_PREFIX/share/agentctl/install-manifest"

"$INSTALL" --agentctl "$UPGRADE_TARGET" --plan-with "$AGENTCTL" --state-dir "$state" >/dev/null \
  || fail 'could not stage a managed supervisor for the upgrade test'
[ -f "$manifest" ] || fail 'upgrade staging did not write a supervisor manifest'

"$BINARY_INSTALL" --binary "$AGENTCTL" --prefix "$UPGRADE_PREFIX" --name agentctl >/dev/null \
  || fail 'upgrading a managed install with a live supervisor was refused'
cmp -s "$AGENTCTL" "$UPGRADE_TARGET" || fail 'upgrade did not replace the managed binary'
python3 - "$plist" "$UPGRADE_TARGET" <<'UPGRADEDPY'
import plistlib, sys
with open(sys.argv[1], 'rb') as fh: data = plistlib.load(fh)
assert data['ProgramArguments'][0] == sys.argv[2], data['ProgramArguments']
assert data['StandardOutPath'].endswith('/Library/Logs/agentctl/supervisor.out.log')
assert data['StandardErrorPath'].endswith('/Library/Logs/agentctl/supervisor.err.log')
assert data['ThrottleInterval'] == 10
UPGRADEDPY
rm -f "$plist" "$manifest" "$LAUNCH_LOG" "$LOADED"
unset FAKE_CURRENT_AGENTCTL


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
assert data['StandardOutPath'].endswith('/Library/Logs/agentctl/supervisor.out.log')
assert data['StandardErrorPath'].endswith('/Library/Logs/agentctl/supervisor.err.log')
assert data['ThrottleInterval'] == 10
PY
[ -d "$HOME/Library/Logs/agentctl" ] || fail 'install did not create the supervisor log directory'
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
