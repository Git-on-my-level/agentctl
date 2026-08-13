#!/usr/bin/env bash
# Remove only the systemd user supervisor installed by the paired installer.

set -euo pipefail

AGENTCTL=
FORCE=0
DRY_RUN=0
OUTPUT=text
UNIT_NAME=io.agentctl.supervisor.service

die() { printf 'error: %s\n' "$*" >&2; exit 2; }
usage() {
  cat <<'EOF'
usage: scripts/uninstall-systemd-supervisor.sh [--agentctl PATH] [--force]
       [--dry-run] [--output text|json]

Remove the user systemd supervisor only when its owner-only manifest matches
the unit. Configuration, journal, credentials, sessions, and state remain.
EOF
}
sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d ' ' -f 1
  elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | cut -d ' ' -f 1
  else die 'sha256sum or shasum is required'; fi
}
json_result() {
  state=$1; unit=$2; manifest=$3
  if [ "$OUTPUT" = json ]; then
    python3 - "$state" "$unit" "$manifest" <<'PY'
import json, sys
print(json.dumps({"ok": True, "state": sys.argv[1], "unit": sys.argv[2], "manifest": sys.argv[3]}, separators=(",", ":")))
PY
  else printf 'state=%s unit=%s manifest=%s\n' "$state" "$unit" "$manifest"; fi
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --agentctl) [ "$#" -ge 2 ] || die '--agentctl requires a value'; AGENTCTL=$2; shift 2 ;;
    --force) FORCE=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    --output) [ "$#" -ge 2 ] || die '--output requires text or json'; OUTPUT=$2; shift 2 ;;
    --help|-h) usage; exit 0 ;;
    *) die "unknown option: $1" ;;
  esac
done

[ "$(uname -s)" = Linux ] || die 'systemd supervisor uninstallation is only supported on Linux'
[ "$(id -u)" -ne 0 ] || die 'refusing to uninstall a user systemd service as root'
case "$OUTPUT" in text|json) ;; *) die '--output must be text or json' ;; esac
HOME_DIR=${HOME:-}
[ -n "$HOME_DIR" ] || die 'HOME is required'
case "$HOME_DIR" in /*) ;; *) die 'HOME must be an absolute path' ;; esac
config_home=${XDG_CONFIG_HOME:-"$HOME_DIR/.config"}
case "$config_home" in /*) ;; *) die 'XDG_CONFIG_HOME must be an absolute path' ;; esac
if [ -n "$AGENTCTL" ]; then
  case "$AGENTCTL" in /*) ;; *) die '--agentctl must be an absolute path' ;; esac
  [ -f "$AGENTCTL" ] || die "agentctl does not exist: $AGENTCTL"
  [ -x "$AGENTCTL" ] || die "agentctl is not executable: $AGENTCTL"
fi
python3 - "$HOME_DIR" "$config_home" ${AGENTCTL:+"$AGENTCTL"} <<'PY'
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
[ -f "$manifest" ] || die "managed supervisor manifest not found: $manifest"
[ ! -L "$manifest" ] || die "refusing symlinked supervisor manifest: $manifest"
[ -f "$unit" ] || die "managed supervisor unit not found: $unit"
[ ! -L "$unit" ] || die "refusing symlinked supervisor unit: $unit"
[ "$(stat -c '%a' "$manifest")" = 600 ] || die "refusing non-owner-only supervisor manifest: $manifest"
[ "$(stat -c '%a' "$unit")" = 600 ] || die "refusing non-owner-only supervisor unit: $unit"

[ "$(sed -n 's/^manifest_version=//p' "$manifest")" = 1 ] || die 'unrecognized supervisor manifest version'
[ "$(sed -n 's/^managed_by=//p' "$manifest")" = agentctl-supervisor ] || die 'supervisor manifest is not agentctl-managed'
[ "$(sed -n 's/^platform=//p' "$manifest")" = systemd-user ] || die 'supervisor manifest platform does not match'
[ "$(sed -n 's/^unit_name=//p' "$manifest")" = "$UNIT_NAME" ] || die 'supervisor manifest unit name does not match'
[ "$(sed -n 's/^unit=//p' "$manifest")" = "$unit" ] || die 'supervisor manifest unit path does not match'
manifest_agentctl=$(sed -n 's/^agentctl=//p' "$manifest")
manifest_hash=$(sed -n 's/^unit_sha256=//p' "$manifest")
[ -n "$manifest_agentctl" ] || die 'supervisor manifest has no agentctl path'
[ -n "$manifest_hash" ] || die 'supervisor manifest has no unit checksum'
if [ -n "$AGENTCTL" ] && [ "$manifest_agentctl" != "$AGENTCTL" ]; then die 'requested agentctl does not match the managed supervisor manifest'; fi
if [ "$(sha256_file "$unit")" != "$manifest_hash" ] && [ "$FORCE" -ne 1 ]; then
  die "refusing to remove modified supervisor unit: $unit (use --force after inspection)"
fi
if [ "$DRY_RUN" -eq 1 ]; then json_result planned "$unit" "$manifest"; exit 0; fi

systemctl_bin=$(command -v systemctl 2>/dev/null || true)
[ -n "$systemctl_bin" ] || die 'systemctl is required to unload the supervisor'
enabled=0; active=0
"$systemctl_bin" --user is-enabled --quiet "$UNIT_NAME" >/dev/null 2>&1 && enabled=1
"$systemctl_bin" --user is-active --quiet "$UNIT_NAME" >/dev/null 2>&1 && active=1

backup_unit=$(mktemp "$unit_dir/.$UNIT_NAME.uninstall-unit.XXXXXX")
backup_manifest=$(mktemp "$unit_dir/.$UNIT_NAME.uninstall-manifest.XXXXXX")
cp "$unit" "$backup_unit"; chmod 0600 "$backup_unit"
cp "$manifest" "$backup_manifest"; chmod 0600 "$backup_manifest"
rollback_armed=1
restore_uninstall() {
  rm -f "$unit" "$manifest" || :
  if [ -n "$backup_unit" ] && [ -f "$backup_unit" ]; then mv -f "$backup_unit" "$unit"; backup_unit=; fi
  if [ -n "$backup_manifest" ] && [ -f "$backup_manifest" ]; then mv -f "$backup_manifest" "$manifest"; backup_manifest=; fi
  "$systemctl_bin" --user daemon-reload >/dev/null 2>&1 || :
  [ "$enabled" -eq 0 ] || "$systemctl_bin" --user enable "$UNIT_NAME" >/dev/null 2>&1 || :
  [ "$active" -eq 0 ] || "$systemctl_bin" --user start "$UNIT_NAME" >/dev/null 2>&1 || :
}
finish() {
  status=$?
  trap - EXIT HUP INT TERM
  [ "$rollback_armed" -eq 0 ] || restore_uninstall
  [ -z "$backup_unit" ] || rm -f "$backup_unit"
  [ -z "$backup_manifest" ] || rm -f "$backup_manifest"
  exit "$status"
}
trap finish EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
if { [ "$enabled" -eq 1 ] || [ "$active" -eq 1 ]; } && ! "$systemctl_bin" --user disable --now "$UNIT_NAME" >/dev/null 2>&1; then
  die "failed to stop supervisor service: $UNIT_NAME"
fi
rm -f "$unit" "$manifest"
if ! "$systemctl_bin" --user daemon-reload >/dev/null 2>&1; then
  die "failed to reload systemd after removing supervisor service: $UNIT_NAME"
fi
rollback_armed=0
json_result uninstalled "$unit" "$manifest"
