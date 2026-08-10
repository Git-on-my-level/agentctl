#!/usr/bin/env bash
# Remove only an install previously recorded by scripts/install.sh.

set -euo pipefail

PREFIX=${PREFIX:-"${HOME}/.local"}
BINARY_NAME=${BINARY_NAME:-agentctl}
FORCE=0
DRY_RUN=0

die() { printf 'error: %s\n' "$*" >&2; exit 2; }
usage() {
  cat <<'EOF'
usage: scripts/uninstall.sh [--prefix DIR] [--force] [--dry-run]

Uninstall never removes configuration, journal, credentials, caches, or
harness state. It removes a binary only when the install manifest matches.
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

while [ "$#" -gt 0 ]; do
  case "$1" in
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
    --help|-h) usage; exit 0 ;;
    *) die "unknown option: $1" ;;
  esac
done

case "$(uname -s)" in
  Darwin|Linux) ;;
  *) die 'only macOS and Linux are supported' ;;
esac
[ "$(id -u)" -ne 0 ] || die 'refusing to run as root; choose a user-owned --prefix'
case "$BINARY_NAME" in
  ''|*/*) die '--name must be a simple executable name' ;;
esac
[ -n "$PREFIX" ] || die '--prefix cannot be empty'
[ ! -L "$PREFIX" ] || die "refusing symlinked prefix: $PREFIX"

target=$PREFIX/bin/$BINARY_NAME
manifest=$PREFIX/share/agentctl/install-manifest
[ -f "$manifest" ] || die "no agentctl install manifest found: $manifest"
grep -Fqx 'manifest_version=1' "$manifest" || die "unrecognized install manifest: $manifest"
manifest_target=$(sed -n 's/^target=//p' "$manifest")
manifest_hash=$(sed -n 's/^sha256=//p' "$manifest")
[ "$manifest_target" = "$target" ] || die 'manifest target does not match requested prefix/name'
[ -n "$manifest_hash" ] || die 'install manifest has no checksum'
[ -f "$target" ] || die "managed executable is missing: $target"
actual_hash=$(sha256_file "$target")
if [ "$actual_hash" != "$manifest_hash" ] && [ "$FORCE" -ne 1 ]; then
  die "refusing to remove modified executable: $target (use --force after inspection)"
fi

if [ "$DRY_RUN" -eq 1 ]; then
  printf 'would uninstall %s\n' "$target"
  exit 0
fi

rm -f "$target" "$manifest"
rmdir "$PREFIX/share/agentctl" 2>/dev/null || :
printf 'uninstalled %s\n' "$target"
