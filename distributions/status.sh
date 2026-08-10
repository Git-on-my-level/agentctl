#!/bin/sh
# Read-only status for a single explicit target. It never scans a home or
# attempts repair.

set -eu
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
. "$SCRIPT_DIR/lib.sh"

parse_common_args "$@"
validate_sources

managed=$(managed_dir)
[ ! -L "$managed" ] || die "managed directory must not be a symlink: $managed"
skill_dest="$managed/SKILL.md"
manifest_dest="$managed/revision-manifest.json"
bundle_dest="$managed/agentctl-portable.bundle.json"
state=installed
detail=

check_asset() {
  name=$1
  source=$2
  destination=$3
  if [ ! -e "$destination" ] && [ ! -L "$destination" ]; then
    state=missing
    detail="$detail $name=missing"
    return 0
  fi
  if [ -L "$destination" ]; then
    source_abs=$(canonical_file "$source")
    resolved=$(readlink "$destination" 2>/dev/null || true)
    candidate=$resolved
    case "$candidate" in /*) ;; *) candidate="$(dirname -- "$destination")/$candidate" ;; esac
    if [ -z "$resolved" ] || [ ! -f "$candidate" ]; then
      state=drifted; detail="$detail $name=drifted"; return 0
    fi
    resolved_abs=$(canonical_file "$candidate")
    if [ "$resolved_abs" != "$source_abs" ]; then
      state=drifted; detail="$detail $name=drifted"; return 0
    fi
    detail="$detail $name=linked"
    return 0
  fi
  if [ ! -f "$destination" ] || [ "$(sha256_file "$destination")" != "$(sha256_file "$source")" ]; then
    state=drifted
    detail="$detail $name=drifted"
  else
    detail="$detail $name=copied"
  fi
}

check_asset skill "$SKILL_SOURCE" "$skill_dest"
check_asset manifest "$MANIFEST" "$manifest_dest"
if [ "$HARNESS" = multica ]; then
  check_asset multica_bundle "$BUNDLE_SOURCE" "$bundle_dest"
fi

revision=$(sed -n 's/.*"revision": "\([^"]*\)".*/\1/p' "$MANIFEST" | head -n 1)
ok=false
if [ "$state" = installed ]; then ok=true; fi
if [ "$JSON" -eq 1 ]; then
  printf '%s\n' "{\"ok\":$ok,\"state\":\"$state\",\"harness\":\"$HARNESS\",\"target_dir\":\"$TARGET_DIR\",\"managed_dir\":\"$managed\",\"revision\":\"$revision\"}"
else
  printf '%s\n' "state=$state harness=$HARNESS target_dir=$TARGET_DIR revision=$revision$detail"
fi

case "$state" in
  installed) exit 0 ;;
  missing|drifted) exit 1 ;;
esac
