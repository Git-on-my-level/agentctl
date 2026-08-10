#!/usr/bin/env bash
# Install an explicitly supplied agentctl binary into a user-owned prefix.
# This script never downloads code and never reads or writes agentctl config,
# journal, credentials, caches, or harness state.

set -euo pipefail

PREFIX=${PREFIX:-"${HOME}/.local"}
BINARY_NAME=${BINARY_NAME:-agentctl}
SOURCE=
FORCE=0
DRY_RUN=0

die() { printf 'error: %s\n' "$*" >&2; exit 2; }
usage() {
  cat <<'EOF'
usage: scripts/install.sh --binary PATH [--prefix DIR] [--force] [--dry-run]

The default prefix is ~/.local. The destination is PREFIX/bin/agentctl;
only the managed install manifest under PREFIX/share/agentctl is created.
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
    --help|-h) usage; exit 0 ;;
    *) die "unknown option: $1" ;;
  esac
done

case "$(uname -s)" in
  Darwin|Linux) ;;
  *) die 'only macOS and Linux are supported' ;;
esac
[ "$(id -u)" -ne 0 ] || die 'refusing to run as root; choose a user-owned --prefix'
[ -n "$SOURCE" ] || die '--binary is required; install never downloads an artifact'
[ -f "$SOURCE" ] || die "binary does not exist: $SOURCE"
[ -x "$SOURCE" ] || die "binary is not executable: $SOURCE"
case "$BINARY_NAME" in
  ''|*/*) die '--name must be a simple executable name' ;;
esac
[ -n "$PREFIX" ] || die '--prefix cannot be empty'
[ ! -L "$PREFIX" ] || die "refusing symlinked prefix: $PREFIX"

bindir=$PREFIX/bin
sharedir=$PREFIX/share/agentctl
target=$bindir/$BINARY_NAME
manifest=$sharedir/install-manifest

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

if [ "$DRY_RUN" -eq 1 ]; then
  printf 'would install %s -> %s\n' "$SOURCE" "$target"
  exit 0
fi

umask 077
mkdir -p "$bindir" "$sharedir"
[ ! -L "$bindir" ] || die "refusing symlinked bin directory: $bindir"
[ ! -L "$sharedir" ] || die "refusing symlinked share directory: $sharedir"

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
