#!/usr/bin/env bash
# Install the host-local agentctl supervisor as a user launchd service.
#
# The launchd plist is never authored by this script. It is obtained from the
# exact agentctl binary supplied by the caller via `supervisor plan`, decoded,
# and validated before it is written.

set -euo pipefail

AGENTCTL=
PLAN_WITH=
STATE_DIR=
FORCE=0
DRY_RUN=0
OUTPUT=text
LABEL=io.agentctl.supervisor
REGISTER_WRAPPER=
WRAPPER_INTERPRETER=
wrapper=
wrapper_hash=
interpreter_hash=

die() { printf 'error: %s\n' "$*" >&2; exit 2; }

usage() {
  cat <<'EOF'
usage: scripts/install-supervisor.sh --agentctl PATH [--plan-with PATH]
       [--state-dir DIR] [--force] [--dry-run] [--output text|json]
       [--register-wrapper PATH [--wrapper-interpreter /bin/bash|/bin/sh]]

Install the owner-only launchd supervisor for the current user. The supplied
agentctl path must be absolute and executable. --plan-with names the
executable that renders the plan when it differs from the reviewed service
executable, which is how an upgrade preflights the plan the replacement
binary will install; it defaults to --agentctl. No config, journal,
credential, or session files are read or removed.
Wrapper registration is explicit; plan it with --dry-run first. Subsequent
upgrades preserve the registered launcher and refuse script/interpreter drift.
EOF
}

sha256_file() {
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | cut -d ' ' -f 1
  elif command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d ' ' -f 1
  else
    die 'shasum or sha256sum is required'
  fi
}

json_result() {
  state=$1
  plist=$2
  manifest=$3
  if [ "$OUTPUT" = json ]; then
    python3 - "$state" "$plist" "$manifest" "$wrapper" "$wrapper_hash" "$WRAPPER_INTERPRETER" <<'PY'
import json, sys
print(json.dumps({"ok": True, "state": sys.argv[1], "plist": sys.argv[2], "manifest": sys.argv[3], "wrapper": sys.argv[4], "wrapper_sha256": sys.argv[5], "wrapper_interpreter": sys.argv[6]}, separators=(",", ":")))
PY
  else
    printf 'state=%s plist=%s manifest=%s\n' "$state" "$plist" "$manifest"
  fi
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --agentctl)
      [ "$#" -ge 2 ] || die '--agentctl requires a value'
      AGENTCTL=$2; shift 2 ;;
    --plan-with)
      [ "$#" -ge 2 ] || die '--plan-with requires a value'
      PLAN_WITH=$2; shift 2 ;;
    --state-dir)
      [ "$#" -ge 2 ] || die '--state-dir requires a value'
      STATE_DIR=$2; shift 2 ;;
    --register-wrapper)
      [ "$#" -ge 2 ] || die '--register-wrapper requires an absolute script path'
      REGISTER_WRAPPER=$2; shift 2 ;;
    --wrapper-interpreter)
      [ "$#" -ge 2 ] || die '--wrapper-interpreter requires /bin/bash or /bin/sh'
      WRAPPER_INTERPRETER=$2; shift 2 ;;
    --force) FORCE=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    --output)
      [ "$#" -ge 2 ] || die '--output requires text or json'
      OUTPUT=$2; shift 2 ;;
    --help|-h) usage; exit 0 ;;
    *) die "unknown option: $1" ;;
  esac
done

case "$(uname -s)" in
  Darwin) ;;
  *) die 'launchd supervisor installation is only supported on macOS' ;;
esac
[ "$(id -u)" -ne 0 ] || die 'refusing to install a user launchd service as root'
[ -n "$AGENTCTL" ] || die '--agentctl is required; installation never discovers an executable'
case "$AGENTCTL" in
  /*) ;;
  *) die '--agentctl must be an absolute path' ;;
esac
[ -f "$AGENTCTL" ] || die "agentctl does not exist: $AGENTCTL"
[ -x "$AGENTCTL" ] || die "agentctl is not executable: $AGENTCTL"
if [ -z "$PLAN_WITH" ]; then
  PLAN_WITH=$AGENTCTL
fi
case "$PLAN_WITH" in
  /*) ;;
  *) die '--plan-with must be an absolute path' ;;
esac
[ -f "$PLAN_WITH" ] || die "plan executable does not exist: $PLAN_WITH"
[ -x "$PLAN_WITH" ] || die "plan executable is not executable: $PLAN_WITH"
case "$OUTPUT" in text|json) ;; *) die '--output must be text or json' ;; esac

HOME_DIR=${HOME:-}
[ -n "$HOME_DIR" ] || die 'HOME is required'
case "$HOME_DIR" in
  /*) ;;
  *) die 'HOME must be an absolute path' ;;
esac

if [ -z "$STATE_DIR" ]; then
  STATE_DIR="$HOME_DIR/.local/state/agentctl"
fi
case "$STATE_DIR" in
  /*) ;;
  *) die '--state-dir must be an absolute path' ;;
esac

log_dir="$HOME_DIR/Library/Logs/agentctl"
launch_agents="$HOME_DIR/Library/LaunchAgents"
plist="$launch_agents/$LABEL.plist"
manifest="$launch_agents/$LABEL.agentctl-manifest"
domain="gui/$(id -u)"

[ ! -L "$launch_agents" ] || die "refusing symlinked LaunchAgents directory: $launch_agents"
[ ! -L "$plist" ] || die "refusing symlinked plist: $plist"
[ ! -L "$manifest" ] || die "refusing symlinked manifest: $manifest"

plan_json=$(mktemp "${TMPDIR:-/tmp}/agentctl-supervisor-plan.XXXXXX")
plan_plist=$(mktemp "${TMPDIR:-/tmp}/agentctl-supervisor-plist.XXXXXX")
trap 'rm -f "$plan_json" "$plan_plist"' EXIT HUP INT TERM

"$PLAN_WITH" --output json supervisor plan --platform darwin --executable "$AGENTCTL" --state-dir "$STATE_DIR" >"$plan_json" || die 'agentctl supervisor plan failed'

# Validate every field used by installation and decode the plist bytes. The
# validator rejects plans outside this user's LaunchAgents path or log
# directory, or with a changed supervisor argv shape.
python3 - "$plan_json" "$plan_plist" "$plist" "$AGENTCTL" "$STATE_DIR" "$LABEL" "$log_dir" <<'PY'
import base64, json, os, plistlib, sys

plan_path, output_path, expected_path, executable, state_dir, label, log_dir = sys.argv[1:]
try:
    with open(plan_path, 'rb') as fh:
        doc = json.load(fh)
    result = doc.get('result')
    if not isinstance(result, dict):
        raise ValueError('plan result is not an object')
    path = result.get('Path')
    encoded = result.get('Contents')
    service = result.get('Service')
    if path != expected_path:
        raise ValueError('plan path does not match the current user LaunchAgents path')
    if not isinstance(encoded, str) or not encoded:
        raise ValueError('plan Contents is missing')
    if not isinstance(service, dict):
        raise ValueError('plan Service is missing')
    if service.get('Label') != label:
        raise ValueError('plan label is not the reviewed supervisor label')
    args = service.get('ProgramArguments')
    expected_socket = os.path.join(state_dir, 'supervisor.sock')
    if not isinstance(args, list) or args != [executable, 'supervisor', 'run', '--socket', expected_socket, '--state-dir', state_dir]:
        raise ValueError('plan ProgramArguments do not match the requested executable/state directory')
    if service.get('Environment') not in (None, {}):
        raise ValueError('supervisor plan must not introduce environment credentials')
    stdout_path = os.path.join(log_dir, 'supervisor.out.log')
    stderr_path = os.path.join(log_dir, 'supervisor.err.log')
    if service.get('StandardOutPath') != stdout_path or service.get('StandardErrorPath') != stderr_path:
        raise ValueError('plan log paths are not this user reviewed agentctl log directory')
    data = base64.b64decode(encoded, validate=True)
    parsed = plistlib.loads(data)
    if parsed.get('Label') != label or parsed.get('ProgramArguments') != args:
        raise ValueError('decoded plist does not match the plan Service projection')
    if parsed.get('RunAtLoad') is not True or parsed.get('KeepAlive') is not True:
        raise ValueError('supervisor plist must run at load and keep alive')
    if parsed.get('StandardOutPath') != stdout_path or parsed.get('StandardErrorPath') != stderr_path:
        raise ValueError('supervisor plist must declare both reviewed log paths')
    if parsed.get('ThrottleInterval') != service.get('ThrottleInterval'):
        raise ValueError('decoded plist throttle does not match the plan Service projection')
    if not isinstance(parsed.get('ThrottleInterval'), int) or parsed['ThrottleInterval'] < 1:
        raise ValueError('supervisor plist must bound respawn with ThrottleInterval')
    if set(parsed) - {'Label', 'ProgramArguments', 'RunAtLoad', 'KeepAlive', 'StandardOutPath', 'StandardErrorPath', 'ThrottleInterval'}:
        raise ValueError('supervisor plist contains unreviewed keys')
    with open(output_path, 'wb') as fh:
        fh.write(data)
except Exception as exc:
    print(f'error: invalid agentctl supervisor plan: {exc}', file=sys.stderr)
    raise SystemExit(2)
PY

# Wrapper ownership is explicit and hash-bound. Planning reads but never runs
# the launcher. Normal upgrades reuse its registered argv prefix.
if [ -n "$REGISTER_WRAPPER" ]; then
  wrapper=$REGISTER_WRAPPER
elif [ -f "$manifest" ]; then
  wrapper=$(sed -n 's/^wrapper=//p' "$manifest")
  WRAPPER_INTERPRETER=$(sed -n 's/^wrapper_interpreter=//p' "$manifest")
fi
if [ -n "$wrapper" ]; then
  python3 - "$wrapper" "$WRAPPER_INTERPRETER" "$AGENTCTL" "$STATE_DIR" "$plan_plist" "$plist" "$REGISTER_WRAPPER" <<'WRAPPER_PY'
import os, pathlib, plistlib, stat, sys
wrapper, interpreter, executable, state, plan, existing, registering = sys.argv[1:]
p = pathlib.Path(wrapper)
if not p.is_absolute() or str(p) != wrapper or '..' in p.parts or any(c in wrapper for c in '\n\r'):
    raise SystemExit('error: wrapper must be an absolute clean path')
if any(part.is_symlink() for part in [p, *p.parents]):
    raise SystemExit('error: wrapper path contains a symlink')
st = p.stat()
if not stat.S_ISREG(st.st_mode) or st.st_uid != os.getuid() or st.st_mode & 0o022:
    raise SystemExit('error: wrapper must be an owner-controlled regular file')
if interpreter not in ('', '/bin/bash', '/bin/sh'):
    raise SystemExit('error: wrapper interpreter must be /bin/bash or /bin/sh')
if not interpreter and not os.access(wrapper, os.X_OK):
    raise SystemExit('error: direct wrapper must be executable')
prefix = [interpreter, wrapper] if interpreter else [wrapper]
args = prefix + ['supervisor', 'run', '--socket', state + '/supervisor.sock', '--state-dir', state]
with open(plan, 'rb') as f: proposed = plistlib.load(f)
if registering and os.path.exists(existing):
    st = os.stat(existing)
    if st.st_uid != os.getuid() or st.st_mode & 0o022:
        raise SystemExit('error: existing plist must be owner controlled')
    with open(existing, 'rb') as f: old = plistlib.load(f)
    allowed = set(proposed)
    if set(old) - allowed or old.get('ProgramArguments') != args:
        raise SystemExit('error: existing plist does not match the explicit wrapper contract')
    for key, value in old.items():
        if key != 'ProgramArguments' and value != proposed.get(key):
            raise SystemExit('error: existing plist contains an unreviewed setting')
proposed['ProgramArguments'] = args
with open(plan, 'wb') as f: plistlib.dump(proposed, f, sort_keys=False)
WRAPPER_PY
  wrapper_hash=$(sha256_file "$wrapper")
  [ -z "$WRAPPER_INTERPRETER" ] || interpreter_hash=$(sha256_file "$WRAPPER_INTERPRETER")
  if [ -z "$REGISTER_WRAPPER" ]; then
    [ "$(sed -n 's/^wrapper_sha256=//p' "$manifest")" = "$wrapper_hash" ] || die 'registered wrapper changed; explicitly review and register again'
    [ "$(sed -n 's/^wrapper_interpreter_sha256=//p' "$manifest")" = "$interpreter_hash" ] || die 'registered wrapper interpreter changed; explicitly review and register again'
  fi
elif [ -n "$WRAPPER_INTERPRETER" ]; then
  die '--wrapper-interpreter requires --register-wrapper'
fi

plist_hash=$(sha256_file "$plan_plist")
agentctl_hash=$(sha256_file "$AGENTCTL")

managed=0
if [ -f "$manifest" ]; then
  if grep -Fqx 'manifest_version=1' "$manifest" && \
     grep -Fqx 'managed_by=agentctl-supervisor' "$manifest" && \
     grep -Fqx "label=$LABEL" "$manifest" && \
     grep -Fqx "plist=$plist" "$manifest" && \
     grep -Fqx "agentctl=$AGENTCTL" "$manifest" && \
     grep -Fqx "state_dir=$STATE_DIR" "$manifest"; then
    managed=1
    recorded_hash=$(sed -n 's/^plist_sha256=//p' "$manifest")
    if [ -f "$plist" ] && [ -n "$recorded_hash" ] && [ "$(sha256_file "$plist")" != "$recorded_hash" ]; then
      if [ "$FORCE" -ne 1 ] && [ -z "$REGISTER_WRAPPER" ]; then
        die "refusing to overwrite modified managed supervisor plist: $plist (use --force after inspection)"
      fi
      managed=0
    fi
  elif [ "$FORCE" -ne 1 ]; then
    die "refusing to overwrite unmanaged supervisor manifest: $manifest (use --force after inspection)"
  fi
elif [ -e "$manifest" ] && [ "$FORCE" -ne 1 ]; then
  die "refusing to overwrite non-file supervisor manifest: $manifest (use --force after inspection)"
fi
[ -z "$REGISTER_WRAPPER" ] || managed=1
[ ! -e "$manifest" ] || [ -f "$manifest" ] || die "refusing non-file supervisor manifest: $manifest"

if [ -e "$plist" ] && [ "$managed" -ne 1 ] && [ "$FORCE" -ne 1 ]; then
  die "refusing to overwrite unmanaged supervisor plist: $plist (use --force after inspection)"
fi
[ ! -e "$plist" ] || [ -f "$plist" ] || die "refusing non-file supervisor plist: $plist"
if [ "$managed" -eq 1 ] && [ "$FORCE" -ne 1 ]; then
  [ ! -e "$manifest" ] || [ "$(stat -f '%Lp' "$manifest")" = 600 ] || die "refusing non-owner-only managed supervisor manifest: $manifest (use --force after inspection)"
  [ ! -e "$plist" ] || [ "$(stat -f '%Lp' "$plist")" = 600 ] || die "refusing non-owner-only managed supervisor plist: $plist (use --force after inspection)"
fi

if [ "$DRY_RUN" -eq 1 ]; then
  json_result planned "$plist" "$manifest"
  exit 0
fi

launchctl_bin=$(command -v launchctl 2>/dev/null || true)
[ -n "$launchctl_bin" ] || die 'launchctl is required to load the supervisor'

# Inspect the loaded label before replacing either file. A loaded service with
# no matching managed manifest is a separate conflict from a caller-owned
# plist and must be explicitly forced.
loaded=0
old_pid=
old_identity=
if "$launchctl_bin" print "$domain/$LABEL" >/dev/null 2>&1; then
  loaded=1
  old_pid=$("$launchctl_bin" print "$domain/$LABEL" | sed -n 's/^[[:space:]]*pid = \([0-9][0-9]*\).*$/\1/p')
  if [ -n "$old_pid" ]; then old_identity=$(ps -p "$old_pid" -o lstart= 2>/dev/null || true); fi
  if [ "$managed" -ne 1 ] && [ "$FORCE" -ne 1 ]; then
    die "refusing to replace an unmanaged loaded supervisor service: $LABEL (use --force after inspection)"
  fi
fi

umask 077
# launchd fails to spawn a job whose StandardOutPath directory does not exist.
mkdir -p "$log_dir"
mkdir -p "$launch_agents"
[ ! -L "$launch_agents" ] || die "LaunchAgents directory became a symlink: $launch_agents"

plist_tmp=$(mktemp "$launch_agents/.$LABEL.plist.XXXXXX")
manifest_tmp=$(mktemp "$launch_agents/.$LABEL.agentctl-manifest.XXXXXX")
old_plist_tmp=
old_manifest_tmp=
old_plist_exists=0
old_manifest_exists=0
if [ -f "$plist" ]; then
  old_plist_tmp=$(mktemp "$launch_agents/.$LABEL.previous-plist.XXXXXX")
  cp "$plist" "$old_plist_tmp"
  chmod 0600 "$old_plist_tmp"
  old_plist_exists=1
fi
if [ -f "$manifest" ]; then
  old_manifest_tmp=$(mktemp "$launch_agents/.$LABEL.previous-manifest.XXXXXX")
  cp "$manifest" "$old_manifest_tmp"
  chmod 0600 "$old_manifest_tmp"
  old_manifest_exists=1
fi
service_transaction=0
rollback_failed=0
cleanup() {
  rc=$?
  trap - EXIT
  if [ "$service_transaction" -eq 1 ]; then
    rollback || rollback_failed=1
  fi
  rm -f "$plan_json" "$plan_plist" "$plist_tmp" "$manifest_tmp" || :
  if [ "$rollback_failed" -eq 0 ]; then
    [ -z "$old_plist_tmp" ] || rm -f "$old_plist_tmp"
    [ -z "$old_manifest_tmp" ] || rm -f "$old_manifest_tmp"
  else
    printf 'AGENTCTL_SUPERVISOR_ROLLBACK=incomplete\n' >&2
    printf 'supervisor recovery backups: %s %s\n' "$old_plist_tmp" "$old_manifest_tmp" >&2
  fi
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' HUP TERM
cp "$plan_plist" "$plist_tmp"
chmod 0600 "$plist_tmp"
{
  printf '%s\n' 'manifest_version=1'
  printf 'managed_by=agentctl-supervisor\n'
  printf 'label=%s\n' "$LABEL"
  printf 'plist=%s\n' "$plist"
  printf 'agentctl=%s\n' "$AGENTCTL"
  printf 'agentctl_sha256=%s\n' "$agentctl_hash"
  printf 'state_dir=%s\n' "$STATE_DIR"
  printf 'plist_sha256=%s\n' "$plist_hash"
  if [ -n "$wrapper" ]; then
    printf 'wrapper=%s\nwrapper_sha256=%s\n' "$wrapper" "$wrapper_hash"
    printf 'wrapper_interpreter=%s\nwrapper_interpreter_sha256=%s\n' "$WRAPPER_INTERPRETER" "$interpreter_hash"
  fi
} >"$manifest_tmp"
chmod 0600 "$manifest_tmp"

wait_for_unloaded() {
  wait_attempt=0
  while :; do
    label_present=0
    "$launchctl_bin" print "$domain/$LABEL" >/dev/null 2>&1 && label_present=1
    process_present=0
    if [ -n "$old_pid" ] && [ -n "$old_identity" ]; then
      identity=$(ps -p "$old_pid" -o lstart= 2>/dev/null || true)
      [ "$identity" != "$old_identity" ] || process_present=1
    fi
    if [ "$label_present" -eq 0 ] && [ "$process_present" -eq 0 ]; then return 0; fi
    wait_attempt=$((wait_attempt + 1))
    [ "$wait_attempt" -lt 150 ] || return 1
    sleep 0.2
  done
}

verify_replacement() {
  python3 - "$AGENTCTL" "$STATE_DIR" "$agentctl_hash" "$launchctl_bin" "$domain/$LABEL" <<'VERIFY_PY'
import json, os, re, subprocess, sys, time
binary, state, digest, launchctl, label = sys.argv[1:]
digest = 'sha256:' + digest
deadline = time.monotonic() + 120
env = dict(os.environ, AGENTCTL_UPDATE_MODE='off')
while time.monotonic() < deadline:
    try:
        loaded = subprocess.run([launchctl, 'print', label], capture_output=True, text=True, timeout=3)
        match = re.search(r'^\s*pid = (\d+)\s*$', loaded.stdout, re.M)
        if loaded.returncode == 0 and match:
            status = subprocess.run([binary, '--output', 'json', 'supervisor', 'status', '--socket', state + '/supervisor.sock'], capture_output=True, text=True, timeout=2, env=env)
            response = json.loads(status.stdout)
            value = response.get('result', {})
            # Legacy releases did not expose PID; retain their hash verification
            # for rollback compatibility. Current releases bind both identities.
            if status.returncode == 0 and response.get('ok') and value.get('running'):
                if value.get('state_dir') != state or value.get('executable_sha256') != digest or value.get('pid', int(match[1])) != int(match[1]):
                    raise SystemExit('error: replacement supervisor identity mismatch')
                raise SystemExit(0)
    except (ValueError, OSError, subprocess.TimeoutExpired):
        pass
    time.sleep(0.25)
raise SystemExit('error: replacement supervisor identity was not verified within 120 seconds')
VERIFY_PY
}

bootstrap_with_retry() {
  bootstrap_path=$1
  bootstrap_attempt=0
  while [ "$bootstrap_attempt" -lt 10 ]; do
    "$launchctl_bin" bootstrap "$domain" "$bootstrap_path" >/dev/null 2>&1 || :
    if "$launchctl_bin" print "$domain/$LABEL" >/dev/null 2>&1; then
      return 0
    fi
    bootstrap_attempt=$((bootstrap_attempt + 1))
    [ "$bootstrap_attempt" -lt 10 ] || break
    sleep 0.1
  done
  return 1
}

rollback() {
  # Ensure a failed bootstrap/kickstart cannot leave a service running from a
  # plist whose bytes no longer match its manifest.
  if [ "$loaded" -eq 1 ] || [ "$service_loaded" -eq 1 ] || "$launchctl_bin" print "$domain/$LABEL" >/dev/null 2>&1; then
    rollback_pid=$("$launchctl_bin" print "$domain/$LABEL" 2>/dev/null | sed -n 's/^[[:space:]]*pid = \([0-9][0-9]*\).*$/\1/p' || true)
    if [ -n "$rollback_pid" ]; then
      old_pid=$rollback_pid
      old_identity=$(ps -p "$old_pid" -o lstart= 2>/dev/null || true)
    fi
    "$launchctl_bin" bootout "$domain/$LABEL" >/dev/null 2>&1 || :
    if ! wait_for_unloaded; then
      printf 'AGENTCTL_SUPERVISOR_ROLLBACK=incomplete\n' >&2
      return 1
    fi
  fi
  rm -f "$plist" "$manifest" || return 1
  if [ "$old_plist_exists" -eq 1 ]; then
    mv -f "$old_plist_tmp" "$plist" || return 1
    old_plist_tmp=
  fi
  if [ "$old_manifest_exists" -eq 1 ]; then
    mv -f "$old_manifest_tmp" "$manifest" || return 1
    old_manifest_tmp=
  fi
  if [ "$loaded" -eq 1 ] && [ "$old_plist_exists" -eq 1 ]; then
    if ! bootstrap_with_retry "$plist"; then
      printf 'AGENTCTL_SUPERVISOR_ROLLBACK=incomplete\n' >&2
      return 1
    fi
  fi
}

service_loaded=0
service_transaction=1
if [ "$loaded" -eq 1 ]; then
  if ! "$launchctl_bin" bootout "$domain/$LABEL" >/dev/null 2>&1; then
    die "failed to unload existing supervisor service: $LABEL"
  fi
  # launchctl may acknowledge bootout before the label has completely left the
  # domain. Wait briefly so the following bootstrap does not race the old job.
  if ! wait_for_unloaded; then
    die "supervisor service did not finish unloading: $LABEL"
  fi
fi
mv -f "$plist_tmp" "$plist"
mv -f "$manifest_tmp" "$manifest"
# A just-unloaded launchd label can transiently reject bootstrap even after
# print stops finding it. Bound retries keep real plist errors fail-closed.
if ! bootstrap_with_retry "$plist"; then
  die "failed to load supervisor plist: $plist"
fi
service_loaded=1
if ! "$launchctl_bin" kickstart -k "$domain/$LABEL" >/dev/null 2>&1; then
  die "failed to start supervisor service: $LABEL"
fi

if ! verify_replacement; then
  die 'replacement supervisor failed identity verification'
fi
service_transaction=0
json_result installed "$plist" "$manifest"
