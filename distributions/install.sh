#!/bin/sh
# Install only the named portable assets. This command intentionally requires
# --target-dir: it never guesses a home, profile, or harness state directory.

set -eu
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
. "$SCRIPT_DIR/lib.sh"

parse_common_args "$@"
validate_sources
ensure_target_dir
ensure_managed_dir

managed=$(managed_dir)
revision=$(sed -n 's/.*"revision": "\([^"]*\)".*/\1/p' "$MANIFEST" | head -n 1)

install_asset() {
  id=$1
  source=$2
  destination=$3
  if [ -e "$destination" ] || [ -L "$destination" ]; then
    if [ -L "$destination" ]; then
      resolved=$(readlink "$destination" 2>/dev/null || true)
      candidate=$resolved
      case "$candidate" in /*) ;; *) candidate="$(dirname -- "$destination")/$candidate" ;; esac
      if [ -n "$resolved" ] && [ -f "$candidate" ]; then
        resolved_abs=$(canonical_file "$candidate")
        source_abs=$(canonical_file "$source")
        if [ "$resolved_abs" = "$source_abs" ]; then
          [ "$JSON" -eq 1 ] || printf '%s\n' "reused $id mode=link"
          return 0
        fi
      fi
      die "managed asset conflicts with an existing symlink: $destination"
    fi
    [ -f "$destination" ] || die "managed asset conflicts with an existing path: $destination"
    [ "$(sha256_file "$destination")" = "$(sha256_file "$source")" ] || die "managed asset differs from source: $destination"
    [ "$JSON" -eq 1 ] || printf '%s\n' "reused $id mode=copy"
    return 0
  fi
  if [ "$DRY_RUN" -eq 1 ]; then
    [ "$JSON" -eq 1 ] || printf '%s\n' "would-install $id mode=$MODE destination=$destination"
    return 0
  fi
  copy_or_link "$source" "$destination"
  [ "$JSON" -eq 1 ] || printf '%s\n' "installed $id mode=$MODE destination=$destination"
}

install_asset portable-skill "$SKILL_SOURCE" "$managed/SKILL.md"
install_asset revision-manifest "$MANIFEST" "$managed/revision-manifest.json"
if [ "$HARNESS" = multica ]; then
  install_asset multica-runtime-bundle "$BUNDLE_SOURCE" "$managed/agentctl-portable.bundle.json"
fi

if [ "$DRY_RUN" -eq 1 ]; then result_state=planned; else result_state=installed; fi
if [ "$JSON" -eq 1 ]; then
  printf '%s\n' "{\"ok\":true,\"state\":\"$result_state\",\"harness\":\"$HARNESS\",\"target_dir\":\"$TARGET_DIR\",\"managed_dir\":\"$managed\",\"revision\":\"$revision\",\"mode\":\"$MODE\"}"
else
  printf '%s\n' "status=$result_state harness=$HARNESS target_dir=$TARGET_DIR revision=$revision"
fi
