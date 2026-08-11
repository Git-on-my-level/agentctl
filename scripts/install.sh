#!/usr/bin/env bash
# Install an explicitly supplied agentctl binary into a user-owned prefix.
# This script never downloads code and never reads or writes agentctl config,
# journal, credentials, or caches. The default path delegates portable-skill
# reconciliation to the exact installed binary.

set -euo pipefail

PREFIX=${PREFIX:-}
BINARY_NAME=${BINARY_NAME:-agentctl}
SOURCE=
FORCE=0
DRY_RUN=0
BINARY_ONLY=0

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
SUPERVISOR_INSTALLER="$SCRIPT_DIR/install-supervisor.sh"

die() { printf 'error: %s\n' "$*" >&2; exit 2; }
usage() {
  cat <<'EOF'
usage: scripts/install.sh --binary PATH [--prefix DIR] [--force] [--dry-run]
       [--binary-only]

The default prefix is ~/.local. The destination is PREFIX/bin/agentctl;
only the managed install manifest under PREFIX/share/agentctl is created.
By default, the installed binary reconciles the portable skill in every
detected harness root. Use --binary-only to skip that reconciliation.
EOF
}

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    die 'sha256sum or shasum is required'
  fi
}

safe_absolute_path() {
  local path=$1 current=/ remainder component
  case "$path" in /*) ;; *) return 1 ;; esac
  case "$path" in *//*|*'/./'*|*'/../'*|*'/.'|*'/..') return 1 ;; esac
  remainder=${path#/}
  while [ -n "$remainder" ]; do
    component=${remainder%%/*}
    if [ "$remainder" = "$component" ]; then remainder=; else remainder=${remainder#*/}; fi
    [ -n "$component" ] || continue
    current=${current%/}/$component
    [ ! -L "$current" ] || return 1
  done
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --binary)
      [ "$#" -ge 2 ] || die '--binary needs a value'
      SOURCE=$2; shift 2
      ;;
    --prefix)
      [ "$#" -ge 2 ] || die '--prefix needs a value'
      PREFIX=$2; shift 2
      ;;
    --name)
      [ "$#" -ge 2 ] || die '--name needs a value'
      BINARY_NAME=$2; shift 2
      ;;
    --force) FORCE=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    --binary-only) BINARY_ONLY=1; shift ;;
    --help|-h) usage; exit 0 ;;
    *) die "unknown option: $1" ;;
  esac
done

if [ -z "$PREFIX" ]; then
  [ -n "${HOME:-}" ] || die 'HOME is required unless --prefix is supplied'
  PREFIX="$HOME/.local"
fi

case "$(uname -s)" in
  Darwin|Linux) ;;
  *) die 'only macOS and Linux are supported' ;;
esac
[ "$(id -u)" -ne 0 ] || die 'refusing to run as root; choose a user-owned --prefix'
[ -n "$SOURCE" ] || die '--binary is required; install never downloads an artifact'
[ -f "$SOURCE" ] || die "binary does not exist: $SOURCE"
[ -x "$SOURCE" ] || die "binary is not executable: $SOURCE"
source_absolute=$SOURCE
case "$source_absolute" in
  /*) ;;
  *) source_absolute="$(CDPATH='' cd -- "$(dirname -- "$source_absolute")" && pwd)/$(basename -- "$source_absolute")" || die "cannot resolve binary path: $SOURCE" ;;
esac
case "$BINARY_NAME" in
  ''|*/*) die '--name must be a simple executable name' ;;
esac
[ -n "$PREFIX" ] || die '--prefix cannot be empty'
safe_absolute_path "$PREFIX" || die "prefix must be an absolute clean path without symlink components: $PREFIX"

bindir=$PREFIX/bin
sharedir=$PREFIX/share/agentctl
target=$bindir/$BINARY_NAME
manifest=$sharedir/install-manifest

# Refuse symlinked installation directories before mkdir/copy. Checking after
# mkdir would allow a symlinked bindir/share directory to redirect writes.
[ ! -L "$bindir" ] || die "refusing symlinked bin directory: $bindir"
[ ! -L "$sharedir" ] || die "refusing symlinked share directory: $sharedir"

managed=0
if [ -e "$target" ] || [ -L "$target" ]; then
  if [ -f "$manifest" ] && grep -Fqx "target=$target" "$manifest" 2>/dev/null; then
    managed=1
  fi
  if [ "$managed" -eq 0 ] && [ "$FORCE" -ne 1 ]; then
    die "refusing to overwrite unmanaged executable: $target (use --force only after inspection)"
  fi
fi
if [ -e "$manifest" ] && ! grep -Fqx 'manifest_version=1' "$manifest" 2>/dev/null && [ "$FORCE" -ne 1 ]; then
  die "refusing to overwrite unmanaged manifest: $manifest"
fi

supervisor_required=0
supervisor_manifest=
supervisor_state_dir=
supervisor_launch_agents=

# A managed launchd supervisor refers to the binary by absolute path. When
# that path is the binary being replaced, reconcile it with the sibling
# installer. Inspect and validate the manifest before changing the binary so
# a missing helper or a conflicting supervisor cannot leave a stale service.
inspect_supervisor() {
  [ "$BINARY_ONLY" -eq 0 ] || return 0
  [ -n "${HOME:-}" ] || return 0
  case "$HOME" in /*) ;; *) return 0 ;; esac
  supervisor_launch_agents="$HOME/Library/LaunchAgents"
  supervisor_manifest="$supervisor_launch_agents/io.agentctl.supervisor.agentctl-manifest"
  if [ ! -e "$supervisor_manifest" ] && [ ! -L "$supervisor_manifest" ]; then
    return 0
  fi
  [ ! -L "$supervisor_manifest" ] || die "refusing symlinked supervisor manifest: $supervisor_manifest"
  [ -f "$supervisor_manifest" ] || die "refusing non-file supervisor manifest: $supervisor_manifest"
  manifest_agentctl=$(sed -n 's/^agentctl=//p' "$supervisor_manifest")
  [ "$manifest_agentctl" = "$target" ] || return 0
  grep -Fqx 'manifest_version=1' "$supervisor_manifest" || die "refusing conflicting supervisor manifest: $supervisor_manifest"
  grep -Fqx 'managed_by=agentctl-supervisor' "$supervisor_manifest" || die "refusing conflicting supervisor manifest: $supervisor_manifest"
  grep -Fqx 'label=io.agentctl.supervisor' "$supervisor_manifest" || die "refusing conflicting supervisor manifest: $supervisor_manifest"
  expected_plist="$supervisor_launch_agents/io.agentctl.supervisor.plist"
  manifest_plist=$(sed -n 's/^plist=//p' "$supervisor_manifest")
  [ "$manifest_plist" = "$expected_plist" ] || die "refusing conflicting supervisor manifest: $supervisor_manifest"
  supervisor_state_dir=$(sed -n 's/^state_dir=//p' "$supervisor_manifest")
  [ -n "$supervisor_state_dir" ] || die "refusing supervisor manifest without state_dir: $supervisor_manifest"
  case "$supervisor_state_dir" in /*) ;; *) die "refusing relative supervisor state_dir: $supervisor_state_dir" ;; esac
  [ -x "$SUPERVISOR_INSTALLER" ] || die "supervisor reconciliation helper unavailable: $SUPERVISOR_INSTALLER"
  supervisor_required=1
}

reconcile_supervisor() {
  [ "$supervisor_required" -eq 1 ] || return 0
  local agentctl_path=$1 dry_flag=${2:-}
  local args=(--agentctl "$agentctl_path" --state-dir "$supervisor_state_dir" --output json)
  [ -n "$dry_flag" ] && args+=("$dry_flag")
  "$SUPERVISOR_INSTALLER" "${args[@]}" >/dev/null || die "supervisor reconciliation failed for $agentctl_path"
}

if [ "$BINARY_ONLY" -eq 0 ]; then
  # The source is the only executable available before the replacement. Its
  # dry-run is the transaction preflight for both normal installs and --dry-run
  # invocations; it must not write harness state.
  "$source_absolute" bootstrap update --dry-run >/dev/null || die 'bootstrap update preflight failed; refusing to mutate the binary'
fi
inspect_supervisor
# A managed supervisor normally points at the currently installed target. Use
# that matching executable for the ownership/plan dry-run; the source path is
# intentionally different and would correctly fail manifest binding. If the
# target was deleted, restoring it is the recovery prerequisite, so defer the
# helper invocation until the replacement exists.
if [ -x "$target" ]; then
  reconcile_supervisor "$target" --dry-run
fi

if [ "$DRY_RUN" -eq 1 ]; then
  printf 'would install %s -> %s\n' "$SOURCE" "$target"
  exit 0
fi

umask 077
mkdir -p "$bindir" "$sharedir"

tmp=$(mktemp "$bindir/.agentctl-install.XXXXXX")
trap 'rm -f "$tmp"' EXIT
cp "$SOURCE" "$tmp"
chmod 0755 "$tmp"
mv -f "$tmp" "$target"
trap - EXIT

hash=$(sha256_file "$target")
manifest_tmp=$(mktemp "$sharedir/.install-manifest.XXXXXX")
trap 'rm -f "$manifest_tmp"' EXIT
{
  printf '%s\n' 'manifest_version=1'
  printf 'target=%s\n' "$target"
  printf 'sha256=%s\n' "$hash"
} >"$manifest_tmp"
chmod 0600 "$manifest_tmp"
mv -f "$manifest_tmp" "$manifest"
trap - EXIT
printf 'installed %s\n' "$target"

if [ "$BINARY_ONLY" -eq 0 ]; then
  # Run the exact binary now installed, not the caller-supplied source path.
  "$target" bootstrap update || die 'bootstrap update failed after binary installation'
  reconcile_supervisor "$target"
fi
