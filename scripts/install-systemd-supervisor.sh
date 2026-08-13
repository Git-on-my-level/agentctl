#!/usr/bin/env bash
# Install the host-local agentctl supervisor as a systemd user service.
#
# The unit is obtained from the exact supplied agentctl binary's read-only
# supervisor plan. This script validates that plan before writing owner-only,
# manifest-bound files and rolls back service state on activation failure.

set -euo pipefail

AGENTCTL=
STATE_DIR=
FORCE=0
DRY_RUN=0
OUTPUT=text
UNIT_NAME=io.agentctl.supervisor.service

die() { printf 'error: %s\n' "$*" >&2; exit 2; }

usage() {
  cat <<'EOF'
usage: scripts/install-systemd-supervisor.sh --agentctl PATH [--state-dir DIR]
       [--force] [--dry-run] [--output text|json]

Install the owner-only systemd user supervisor for the current user. The
supplied agentctl path must be absolute and executable. No config, journal,
credential, or session files are read or removed.
EOF
}

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d ' ' -f 1
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | cut -d ' ' -f 1
  else
    die 'sha256sum or shasum is required'
  fi
}

json_result() {
  state=$1
  unit=$2
  manifest=$3
  if [ "$OUTPUT" = json ]; then
    python3 - "$state" "$unit" "$manifest" <<'PY'
import json, sys
print(json.dumps({"ok": True, "state": sys.argv[1], "unit": sys.argv[2], "manifest": sys.argv[3]}, separators=(",", ":")))
PY
  else
    printf 'state=%s unit=%s manifest=%s\n' "$state" "$unit" "$manifest"
  fi
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --agentctl)
      [ "$#" -ge 2 ] || die '--agentctl requires a value'
      AGENTCTL=$2; shift 2 ;;
    --state-dir)
      [ "$#" -ge 2 ] || die '--state-dir requires a value'
      STATE_DIR=$2; shift 2 ;;
    --force) FORCE=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    --output)
      [ "$#" -ge 2 ] || die '--output requires text or json'
      OUTPUT=$2; shift 2 ;;
    --help|-h) usage; exit 0 ;;
    *) die "unknown option: $1" ;;
  esac
done

[ "$(uname -s)" = Linux ] || die 'systemd supervisor installation is only supported on Linux'
[ "$(id -u)" -ne 0 ] || die 'refusing to install a user systemd service as root'
[ -n "$AGENTCTL" ] || die '--agentctl is required; installation never discovers an executable'
case "$AGENTCTL" in /*) ;; *) die '--agentctl must be an absolute path' ;; esac
[ -f "$AGENTCTL" ] || die "agentctl does not exist: $AGENTCTL"
[ -x "$AGENTCTL" ] || die "agentctl is not executable: $AGENTCTL"
case "$OUTPUT" in text|json) ;; *) die '--output must be text or json' ;; esac

HOME_DIR=${HOME:-}
[ -n "$HOME_DIR" ] || die 'HOME is required'
case "$HOME_DIR" in /*) ;; *) die 'HOME must be an absolute path' ;; esac
config_home=${XDG_CONFIG_HOME:-"$HOME_DIR/.config"}
state_home=${XDG_STATE_HOME:-"$HOME_DIR/.local/state"}
case "$config_home" in /*) ;; *) die 'XDG_CONFIG_HOME must be an absolute path' ;; esac
case "$state_home" in /*) ;; *) die 'XDG_STATE_HOME must be an absolute path' ;; esac
if [ -z "$STATE_DIR" ]; then STATE_DIR="$state_home/agentctl"; fi
case "$STATE_DIR" in /*) ;; *) die '--state-dir must be an absolute path' ;; esac

# These values are persisted one-per-line in the ownership manifest. Reject
# control characters and every existing symlink component before any plan or
# service-manager interaction can redirect a write or forge a manifest field.
python3 - "$HOME_DIR" "$config_home" "$state_home" "$STATE_DIR" "$AGENTCTL" <<'PY'
import os, stat, sys
for value in sys.argv[1:]:
    if not os.path.isabs(value) or os.path.normpath(value) != value:
        raise SystemExit(f'error: path must be absolute and clean: {value!r}')
    if any(ord(ch) < 32 or ord(ch) == 127 for ch in value):
        raise SystemExit('error: paths must not contain control characters')
    current = os.path.sep
    for part in value.split(os.path.sep)[1:]:
        current = os.path.join(current, part)
        try:
            mode = os.lstat(current).st_mode
        except FileNotFoundError:
            continue
        if stat.S_ISLNK(mode):
            raise SystemExit(f'error: refusing symlinked path component: {current}')
PY

unit_dir="$config_home/systemd/user"
unit="$unit_dir/$UNIT_NAME"
manifest="$unit_dir/$UNIT_NAME.agentctl-manifest"
[ ! -L "$unit_dir" ] || die "refusing symlinked systemd user directory: $unit_dir"
[ ! -L "$unit" ] || die "refusing symlinked unit: $unit"
[ ! -L "$manifest" ] || die "refusing symlinked manifest: $manifest"

plan_json=$(mktemp "${TMPDIR:-/tmp}/agentctl-systemd-plan.XXXXXX")
plan_unit=$(mktemp "${TMPDIR:-/tmp}/agentctl-systemd-unit.XXXXXX")
trap 'rm -f "$plan_json" "$plan_unit"' EXIT HUP INT TERM

"$AGENTCTL" --output json supervisor plan --platform linux --executable "$AGENTCTL" --state-dir "$STATE_DIR" >"$plan_json" || die 'agentctl supervisor plan failed'

python3 - "$plan_json" "$plan_unit" "$unit" "$AGENTCTL" "$STATE_DIR" <<'PY'
import base64, json, os, sys

plan_path, output_path, expected_path, executable, state_dir = sys.argv[1:]

def quote(value):
    escaped = (value.replace('\\', '\\\\').replace('"', '\\"')
                    .replace('\n', '\\n').replace('\r', '\\r').replace('\t', '\\t')
                    .replace('%', '%%'))
    if value and not any(ch in value for ch in " \t\n\r\\\"'"):
        return escaped
    return '"' + escaped + '"'

try:
    with open(plan_path, 'rb') as fh:
        doc = json.load(fh)
    result = doc.get('result')
    if not isinstance(result, dict):
        raise ValueError('plan result is not an object')
    if result.get('Path') != expected_path:
        raise ValueError('plan path does not match XDG systemd user path')
    encoded = result.get('Contents')
    service = result.get('Service')
    if not isinstance(encoded, str) or not encoded:
        raise ValueError('plan Contents is missing')
    if not isinstance(service, dict):
        raise ValueError('plan Service is missing')
    socket = os.path.join(state_dir, 'supervisor.sock')
    argv = [executable, 'supervisor', 'run', '--socket', socket, '--state-dir', state_dir]
    exec_start = ' '.join(quote(value) for value in argv)
    expected_service = {
        'UnitName': 'io.agentctl.supervisor',
        'Description': 'agentctl host-local supervisor',
        'ExecStart': exec_start,
        'Environment': None,
        'Restart': 'on-failure',
        'WantedBy': 'default.target',
    }
    if service != expected_service:
        raise ValueError('plan service does not match requested executable/state directory')
    data = base64.b64decode(encoded, validate=True)
    expected = (f'[Unit]\nDescription=agentctl host-local supervisor\n\n'
                f'[Service]\nType=simple\nExecStart={exec_start}\nRestart=on-failure\n\n'
                f'[Install]\nWantedBy=default.target\n').encode()
    if data != expected:
        raise ValueError('decoded unit does not match the reviewed systemd projection')
    with open(output_path, 'wb') as fh:
        fh.write(data)
except Exception as exc:
    print(f'error: invalid agentctl supervisor plan: {exc}', file=sys.stderr)
    raise SystemExit(2)
PY

unit_hash=$(sha256_file "$plan_unit")
agentctl_hash=$(sha256_file "$AGENTCTL")
managed=0
if [ -f "$manifest" ]; then
  if grep -Fqx 'manifest_version=1' "$manifest" && \
     grep -Fqx 'managed_by=agentctl-supervisor' "$manifest" && \
     grep -Fqx 'platform=systemd-user' "$manifest" && \
     grep -Fqx "unit_name=$UNIT_NAME" "$manifest" && \
     grep -Fqx "unit=$unit" "$manifest" && \
     grep -Fqx "agentctl=$AGENTCTL" "$manifest" && \
     grep -Fqx "state_dir=$STATE_DIR" "$manifest"; then
    managed=1
    recorded_hash=$(sed -n 's/^unit_sha256=//p' "$manifest")
    if [ -f "$unit" ] && [ -n "$recorded_hash" ] && [ "$(sha256_file "$unit")" != "$recorded_hash" ]; then
      [ "$FORCE" -eq 1 ] || die "refusing to overwrite modified managed supervisor unit: $unit (use --force after inspection)"
      managed=0
    fi
  elif [ "$FORCE" -ne 1 ]; then
    die "refusing to overwrite unmanaged supervisor manifest: $manifest (use --force after inspection)"
  fi
elif [ -e "$manifest" ] && [ "$FORCE" -ne 1 ]; then
  die "refusing to overwrite non-file supervisor manifest: $manifest (use --force after inspection)"
fi
[ ! -e "$manifest" ] || [ -f "$manifest" ] || die "refusing non-file supervisor manifest: $manifest"
if [ -e "$unit" ] && [ "$managed" -ne 1 ] && [ "$FORCE" -ne 1 ]; then
  die "refusing to overwrite unmanaged supervisor unit: $unit (use --force after inspection)"
fi
[ ! -e "$unit" ] || [ -f "$unit" ] || die "refusing non-file supervisor unit: $unit"
if [ "$managed" -eq 1 ] && [ "$FORCE" -ne 1 ]; then
  [ "$(stat -c '%a' "$manifest")" = 600 ] || die "refusing non-owner-only managed supervisor manifest: $manifest (use --force after inspection)"
  [ ! -e "$unit" ] || [ "$(stat -c '%a' "$unit")" = 600 ] || die "refusing non-owner-only managed supervisor unit: $unit (use --force after inspection)"
fi

if [ "$DRY_RUN" -eq 1 ]; then
  json_result planned "$unit" "$manifest"
  exit 0
fi

systemctl_bin=$(command -v systemctl 2>/dev/null || true)
[ -n "$systemctl_bin" ] || die 'systemctl is required to load the supervisor'
load_state=$("$systemctl_bin" --user show "$UNIT_NAME" --property=LoadState --value 2>/dev/null || true)
loaded=0
case "$load_state" in ''|not-found) ;; *) loaded=1 ;; esac
if [ "$loaded" -eq 1 ] && [ "$managed" -ne 1 ] && [ "$FORCE" -ne 1 ]; then
  die "refusing to replace an unmanaged loaded supervisor service: $UNIT_NAME (use --force after inspection)"
fi
enabled=0
active=0
"$systemctl_bin" --user is-enabled --quiet "$UNIT_NAME" >/dev/null 2>&1 && enabled=1
"$systemctl_bin" --user is-active --quiet "$UNIT_NAME" >/dev/null 2>&1 && active=1

umask 077
mkdir -p "$unit_dir"
[ ! -L "$unit_dir" ] || die "systemd user directory became a symlink: $unit_dir"
unit_tmp=$(mktemp "$unit_dir/.$UNIT_NAME.XXXXXX")
manifest_tmp=$(mktemp "$unit_dir/.$UNIT_NAME.agentctl-manifest.XXXXXX")
old_unit_tmp=
old_manifest_tmp=
old_unit_exists=0
old_manifest_exists=0
if [ -f "$unit" ]; then old_unit_tmp=$(mktemp "$unit_dir/.$UNIT_NAME.previous-unit.XXXXXX"); cp "$unit" "$old_unit_tmp"; chmod 0600 "$old_unit_tmp"; old_unit_exists=1; fi
if [ -f "$manifest" ]; then old_manifest_tmp=$(mktemp "$unit_dir/.$UNIT_NAME.previous-manifest.XXXXXX"); cp "$manifest" "$old_manifest_tmp"; chmod 0600 "$old_manifest_tmp"; old_manifest_exists=1; fi
cleanup() {
  rm -f "$plan_json" "$plan_unit" "$unit_tmp" "$manifest_tmp" || :
  [ -z "$old_unit_tmp" ] || rm -f "$old_unit_tmp"
  [ -z "$old_manifest_tmp" ] || rm -f "$old_manifest_tmp"
}
trap cleanup EXIT HUP INT TERM
cp "$plan_unit" "$unit_tmp"
chmod 0600 "$unit_tmp"
{
  printf '%s\n' 'manifest_version=1' 'managed_by=agentctl-supervisor' 'platform=systemd-user'
  printf 'unit_name=%s\nunit=%s\nagentctl=%s\nagentctl_sha256=%s\nstate_dir=%s\nunit_sha256=%s\n' \
    "$UNIT_NAME" "$unit" "$AGENTCTL" "$agentctl_hash" "$STATE_DIR" "$unit_hash"
} >"$manifest_tmp"
chmod 0600 "$manifest_tmp"

restore_previous() {
  "$systemctl_bin" --user disable --now "$UNIT_NAME" >/dev/null 2>&1 || :
  rm -f "$unit" "$manifest"
  if [ "$old_unit_exists" -eq 1 ]; then mv -f "$old_unit_tmp" "$unit"; old_unit_tmp=; fi
  if [ "$old_manifest_exists" -eq 1 ]; then mv -f "$old_manifest_tmp" "$manifest"; old_manifest_tmp=; fi
  "$systemctl_bin" --user daemon-reload >/dev/null 2>&1 || :
  [ "$enabled" -eq 0 ] || "$systemctl_bin" --user enable "$UNIT_NAME" >/dev/null 2>&1 || :
  [ "$active" -eq 0 ] || "$systemctl_bin" --user start "$UNIT_NAME" >/dev/null 2>&1 || :
}

rollback_armed=0
finish() {
  status=$?
  trap - EXIT HUP INT TERM
  if [ "$rollback_armed" -eq 1 ]; then restore_previous; fi
  cleanup
  exit "$status"
}
trap finish EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

rollback_armed=1
if [ "$loaded" -eq 1 ]; then
  if ! "$systemctl_bin" --user disable --now "$UNIT_NAME" >/dev/null 2>&1; then
    die "failed to stop existing supervisor service: $UNIT_NAME"
  fi
fi
mv -f "$unit_tmp" "$unit"
mv -f "$manifest_tmp" "$manifest"
if ! "$systemctl_bin" --user daemon-reload >/dev/null 2>&1 || \
   ! "$systemctl_bin" --user enable --now "$UNIT_NAME" >/dev/null 2>&1 || \
   ! "$systemctl_bin" --user is-enabled --quiet "$UNIT_NAME" >/dev/null 2>&1 || \
   ! "$systemctl_bin" --user is-active --quiet "$UNIT_NAME" >/dev/null 2>&1; then
  die "failed to enable and start supervisor service: $UNIT_NAME"
fi
rollback_armed=0

json_result installed "$unit" "$manifest"
