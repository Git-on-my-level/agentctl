#!/bin/sh
# Validate distribution sources and one explicit installation target. Doctor is
# read-only: it does not refresh, rewrite, delete, or inspect private harness
# databases.

set -eu
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
. "$SCRIPT_DIR/lib.sh"

parse_common_args "$@"
[ "$UPGRADE" -eq 0 ] || die "--upgrade is only valid for install.sh"
validate_sources

errors=0
check_file() {
  path=$1
  label=$2
  if [ ! -f "$path" ]; then
    printf '%s\n' "error=$label missing path=$path" >&2
    errors=$((errors + 1))
  fi
}

check_file "$SKILL_SOURCE" skill_source
check_file "$BUNDLE_SOURCE" multica_bundle_source
check_file "$ALLOWLIST" allowlist
check_file "$MANIFEST" revision_manifest

if [ -e "$TARGET_DIR" ] && [ -L "$TARGET_DIR" ]; then
  printf '%s\n' "error=target_symlink path=$TARGET_DIR" >&2
  errors=$((errors + 1))
elif [ -e "$TARGET_DIR" ] && [ ! -d "$TARGET_DIR" ]; then
  printf '%s\n' "error=target_not_directory path=$TARGET_DIR" >&2
  errors=$((errors + 1))
fi

managed=$(managed_dir)
if [ -e "$managed" ] && [ -L "$managed" ]; then
  printf '%s\n' "error=managed_directory_symlink path=$managed" >&2
  errors=$((errors + 1))
fi

revision=$(sed -n 's/.*"revision": "\([^"]*\)".*/\1/p' "$MANIFEST" | head -n 1)
if [ "$JSON" -eq 1 ]; then
  if [ "$errors" -eq 0 ]; then
    printf '%s\n' "{\"ok\":true,\"state\":\"healthy\",\"harness\":\"$HARNESS\",\"target_dir\":\"$TARGET_DIR\",\"revision\":\"$revision\"}"
  else
    printf '%s\n' "{\"ok\":false,\"state\":\"invalid\",\"harness\":\"$HARNESS\",\"target_dir\":\"$TARGET_DIR\",\"revision\":\"$revision\"}"
  fi
else
  if [ "$errors" -eq 0 ]; then
    printf '%s\n' "state=healthy harness=$HARNESS target_dir=$TARGET_DIR revision=$revision"
  else
    printf '%s\n' "state=invalid harness=$HARNESS target_dir=$TARGET_DIR revision=$revision errors=$errors"
  fi
fi

[ "$errors" -eq 0 ]
