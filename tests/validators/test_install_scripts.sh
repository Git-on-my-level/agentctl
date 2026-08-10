#!/usr/bin/env bash
# Disposable acceptance checks for the binary install manifest lifecycle.

set -euo pipefail
umask 077

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
INSTALL="$ROOT/scripts/install.sh"
UNINSTALL="$ROOT/scripts/uninstall.sh"
TMP=$(mktemp -d "${TMPDIR:-/tmp}/agentctl-install-test.XXXXXX")
trap 'rm -rf "$TMP"' EXIT HUP INT TERM

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
if [ "$(id -u)" -eq 0 ]; then
  printf 'skip: binary installer tests require a non-root user\n'
  exit 0
fi

SOURCE="$TMP/source-agentctl"
cp /bin/echo "$SOURCE"
chmod 0755 "$SOURCE"
PREFIX="$TMP/prefix"

if env -u HOME -u PREFIX "$INSTALL" --binary "$SOURCE" --dry-run >/dev/null 2>"$TMP/no-home.err"; then
  fail 'installer accepted missing HOME without explicit prefix'
fi
grep -q 'HOME is required unless --prefix is supplied' "$TMP/no-home.err" || fail 'missing HOME did not produce stable error'

if "$INSTALL" --binary "$SOURCE" --prefix relative-prefix --dry-run >/dev/null 2>&1; then
  fail 'installer accepted a relative prefix'
fi
[ ! -e relative-prefix ] || fail 'relative-prefix refusal mutated cwd'

outside="$TMP/outside"
mkdir -p "$outside" "$TMP/links"
ln -s "$outside" "$TMP/links/redirect"
if "$INSTALL" --binary "$SOURCE" --prefix "$TMP/links/redirect/prefix" >/dev/null 2>&1; then
  fail 'installer followed a symlinked prefix component'
fi
[ ! -e "$outside/prefix" ] || fail 'symlink refusal mutated external target'

"$INSTALL" --binary "$SOURCE" --prefix "$PREFIX" >/dev/null
target="$PREFIX/bin/agentctl"
manifest="$PREFIX/share/agentctl/install-manifest"
[ -x "$target" ] && [ -f "$manifest" ] || fail 'managed install is incomplete'
first_hash=$(shasum -a 256 "$target" | cut -d ' ' -f 1)
"$INSTALL" --binary "$SOURCE" --prefix "$PREFIX" >/dev/null
[ "$(shasum -a 256 "$target" | cut -d ' ' -f 1)" = "$first_hash" ] || fail 'idempotent install changed binary'

printf '\0' >>"$target"
if "$UNINSTALL" --prefix "$PREFIX" >/dev/null 2>&1; then
  fail 'uninstaller removed a modified managed binary without force'
fi
"$INSTALL" --binary "$SOURCE" --prefix "$PREFIX" --force >/dev/null
"$UNINSTALL" --prefix "$PREFIX" >/dev/null
[ ! -e "$target" ] && [ ! -e "$manifest" ] || fail 'uninstall left managed files'

printf 'ok: binary install lifecycle is explicit, path-safe, and manifest-bound\n'
