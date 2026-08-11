#!/usr/bin/env bash
# Remove only the launchd supervisor installed by install-supervisor.sh.
# Configuration, journal, credentials, sessions, and the state directory are
# deliberately outside this script's deletion set.

set -euo pipefail

AGENTCTL=
FORCE=0
DRY_RUN=0
OUTPUT=text
LABEL=io.agentctl.supervisor

die() { printf 'error: %s\n' "$*" >&2; exit 2; }

usage() {
  cat <<'EOF'
usage: scripts/uninstall-supervisor.sh [--agentctl PATH] [--force]
       [--dry-run] [--output text|json]

Remove the user launchd supervisor only when its owner-only manifest matches
the canonical plist. This never removes agentctl configuration, journal,
credentials, sessions, or the supervisor state directory.
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
  *) die 'launchd supervisor uninstallation is only supported on macOS' ;;
esac
[ "$(id -u)" -ne 0 ] || die 'refusing to uninstall a user launchd service as root'
case "$OUTPUT" in text|json) ;; *) die '--output must be text or json' ;; esac

HOME_DIR=${HOME:-}
[ -n "$HOME_DIR" ] || die 'HOME is required'
case "$HOME_DIR" in
  /*) ;;
  *) die 'HOME must be an absolute path' ;;
esac
if [ -n "$AGENTCTL" ]; then
  case "$AGENTCTL" in /*) ;; *) die '--agentctl must be an absolute path' ;; esac
  [ -f "$AGENTCTL" ] || die "agentctl does not exist: $AGENTCTL"
  [ -x "$AGENTCTL" ] || die "agentctl is not executable: $AGENTCTL"
fi

launch_agents="$HOME_DIR/Library/LaunchAgents"
plist="$launch_agents/$LABEL.plist"
manifest="$launch_agents/$LABEL.agentctl-manifest"
domain="gui/$(id -u)"
[ ! -L "$launch_agents" ] || die "refusing symlinked LaunchAgents directory: $launch_agents"
[ -f "$manifest" ] || die "managed supervisor manifest not found: $manifest"
[ ! -L "$manifest" ] || die "refusing symlinked supervisor manifest: $manifest"
[ -f "$plist" ] || die "managed supervisor plist not found: $plist"
[ ! -L "$plist" ] || die "refusing symlinked supervisor plist: $plist"
[ "$(stat -f '%Lp' "$manifest")" = 600 ] || die "refusing non-owner-only supervisor manifest: $manifest"
[ "$(stat -f '%Lp' "$plist")" = 600 ] || die "refusing non-owner-only supervisor plist: $plist"

manifest_version=$(sed -n 's/^manifest_version=//p' "$manifest")
managed_by=$(sed -n 's/^managed_by=//p' "$manifest")
manifest_label=$(sed -n 's/^label=//p' "$manifest")
manifest_plist=$(sed -n 's/^plist=//p' "$manifest")
manifest_agentctl=$(sed -n 's/^agentctl=//p' "$manifest")
manifest_hash=$(sed -n 's/^plist_sha256=//p' "$manifest")
[ "$manifest_version" = 1 ] || die 'unrecognized supervisor manifest version'
[ "$managed_by" = agentctl-supervisor ] || die 'supervisor manifest is not agentctl-managed'
[ "$manifest_label" = "$LABEL" ] || die 'supervisor manifest label does not match'
[ "$manifest_plist" = "$plist" ] || die 'supervisor manifest plist path does not match'
[ -n "$manifest_agentctl" ] || die 'supervisor manifest has no agentctl path'
[ -n "$manifest_hash" ] || die 'supervisor manifest has no plist checksum'
if [ -n "$AGENTCTL" ] && [ "$manifest_agentctl" != "$AGENTCTL" ]; then
  die 'requested agentctl does not match the managed supervisor manifest'
fi
actual_hash=$(sha256_file "$plist")
if [ "$actual_hash" != "$manifest_hash" ] && [ "$FORCE" -ne 1 ]; then
  die "refusing to remove modified supervisor plist: $plist (use --force after inspection)"
fi

if [ "$DRY_RUN" -eq 1 ]; then
  json_result planned "$plist" "$manifest"
  exit 0
fi

launchctl_bin=$(command -v launchctl 2>/dev/null || true)
[ -n "$launchctl_bin" ] || die 'launchctl is required to unload the supervisor'
if "$launchctl_bin" print "$domain/$LABEL" >/dev/null 2>&1; then
  "$launchctl_bin" bootout "$domain/$LABEL" >/dev/null 2>&1 || die "failed to unload supervisor service: $LABEL"
fi

rm -f "$plist" "$manifest"
json_result uninstalled "$plist" "$manifest"
