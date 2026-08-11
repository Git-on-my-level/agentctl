#!/usr/bin/env bash
# Install the host-local agentctl supervisor as a user launchd service.
#
# The launchd plist is never authored by this script. It is obtained from the
# exact agentctl binary supplied by the caller via `supervisor plan`, decoded,
# and validated before it is written.

set -euo pipefail

AGENTCTL=
STATE_DIR=
FORCE=0
DRY_RUN=0
OUTPUT=text
LABEL=io.agentctl.supervisor

die() { printf 'error: %s\n' "$*" >&2; exit 2; }

usage() {
  cat <<'EOF'
usage: scripts/install-supervisor.sh --agentctl PATH [--state-dir DIR]
       [--force] [--dry-run] [--output text|json]

Install the owner-only launchd supervisor for the current user. The supplied
agentctl path must be absolute and executable. No config, journal,
credential, or session files are read or removed.
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
    python3 - "$state" "$plist" "$manifest" <<'PY'
import json, sys
print(json.dumps({"ok": True, "state": sys.argv[1], "plist": sys.argv[2], "manifest": sys.argv[3]}, separators=(",", ":")))
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

"$AGENTCTL" --output json supervisor plan --platform darwin --executable "$AGENTCTL" --state-dir "$STATE_DIR" >"$plan_json" || die 'agentctl supervisor plan failed'

# Validate every field used by installation and decode the plist bytes. The
# validator rejects plans outside this user's LaunchAgents path or with a
# changed supervisor argv shape.
python3 - "$plan_json" "$plan_plist" "$plist" "$AGENTCTL" "$STATE_DIR" "$LABEL" <<'PY'
import base64, json, os, plistlib, sys

plan_path, output_path, expected_path, executable, state_dir, label = sys.argv[1:]
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
    data = base64.b64decode(encoded, validate=True)
    parsed = plistlib.loads(data)
    if parsed.get('Label') != label or parsed.get('ProgramArguments') != args:
        raise ValueError('decoded plist does not match the plan Service projection')
    if parsed.get('RunAtLoad') is not True or parsed.get('KeepAlive') is not True:
        raise ValueError('supervisor plist must run at load and keep alive')
    if set(parsed) - {'Label', 'ProgramArguments', 'RunAtLoad', 'KeepAlive'}:
        raise ValueError('supervisor plist contains unreviewed keys')
    with open(output_path, 'wb') as fh:
        fh.write(data)
except Exception as exc:
    print(f'error: invalid agentctl supervisor plan: {exc}', file=sys.stderr)
    raise SystemExit(2)
PY

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
      if [ "$FORCE" -ne 1 ]; then
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
[ ! -e "$manifest" ] || [ -f "$manifest" ] || die "refusing non-file supervisor manifest: $manifest"

if [ -e "$plist" ] && [ "$managed" -ne 1 ] && [ "$FORCE" -ne 1 ]; then
  die "refusing to overwrite unmanaged supervisor plist: $plist (use --force after inspection)"
fi
[ ! -e "$plist" ] || [ -f "$plist" ] || die "refusing non-file supervisor plist: $plist"
if [ "$managed" -eq 1 ] && [ "$FORCE" -ne 1 ]; then
  [ "$(stat -f '%Lp' "$manifest")" = 600 ] || die "refusing non-owner-only managed supervisor manifest: $manifest (use --force after inspection)"
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
if "$launchctl_bin" print "$domain/$LABEL" >/dev/null 2>&1; then
  loaded=1
  if [ "$managed" -ne 1 ] && [ "$FORCE" -ne 1 ]; then
    die "refusing to replace an unmanaged loaded supervisor service: $LABEL (use --force after inspection)"
  fi
fi

umask 077
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
cleanup() {
  rm -f "$plan_json" "$plan_plist" "$plist_tmp" "$manifest_tmp" || :
  [ -z "$old_plist_tmp" ] || rm -f "$old_plist_tmp"
  [ -z "$old_manifest_tmp" ] || rm -f "$old_manifest_tmp"
}
trap cleanup EXIT HUP INT TERM
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
} >"$manifest_tmp"
chmod 0600 "$manifest_tmp"
mv -f "$plist_tmp" "$plist"
mv -f "$manifest_tmp" "$manifest"

rollback() {
  # Ensure a failed bootstrap/kickstart cannot leave a service running from a
  # plist whose bytes no longer match its manifest.
  if [ "$loaded" -eq 1 ] || [ "$service_loaded" -eq 1 ]; then
    "$launchctl_bin" bootout "$domain/$LABEL" >/dev/null 2>&1 || :
  fi
  rm -f "$plist" "$manifest"
  if [ "$old_plist_exists" -eq 1 ]; then
    mv -f "$old_plist_tmp" "$plist"
    old_plist_tmp=
  fi
  if [ "$old_manifest_exists" -eq 1 ]; then
    mv -f "$old_manifest_tmp" "$manifest"
    old_manifest_tmp=
  fi
  if [ "$loaded" -eq 1 ] && [ "$old_plist_exists" -eq 1 ]; then
    "$launchctl_bin" bootstrap "$domain" "$plist" >/dev/null 2>&1 || :
  fi
}

service_loaded=0
if [ "$loaded" -eq 1 ]; then
  if ! "$launchctl_bin" bootout "$domain/$LABEL" >/dev/null 2>&1; then
    rollback
    die "failed to unload existing supervisor service: $LABEL"
  fi
	# launchctl may acknowledge bootout before the label has completely left the
	# domain. Wait briefly so the following bootstrap does not race the old job.
	unload_attempt=0
	while "$launchctl_bin" print "$domain/$LABEL" >/dev/null 2>&1; do
		unload_attempt=$((unload_attempt + 1))
		if [ "$unload_attempt" -ge 40 ]; then
			rollback
			die "supervisor service did not finish unloading: $LABEL"
		fi
		sleep 0.05
	done
fi
bootstrap_attempt=0
while ! "$launchctl_bin" bootstrap "$domain" "$plist" >/dev/null 2>&1; do
	# A just-unloaded launchd label can transiently reject bootstrap even after
	# print stops finding it. Bound retries keep real plist errors fail-closed.
	bootstrap_attempt=$((bootstrap_attempt + 1))
	if [ "$bootstrap_attempt" -ge 10 ]; then
		break
	fi
	sleep 0.1
done
if ! "$launchctl_bin" print "$domain/$LABEL" >/dev/null 2>&1; then
  rollback
  die "failed to load supervisor plist: $plist"
fi
service_loaded=1
if ! "$launchctl_bin" kickstart -k "$domain/$LABEL" >/dev/null 2>&1; then
  rollback
  die "failed to start supervisor service: $LABEL"
fi

json_result installed "$plist" "$manifest"
