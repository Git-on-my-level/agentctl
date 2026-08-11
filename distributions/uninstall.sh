#!/bin/sh
# Remove only files bound by an installed agentctl-portable revision manifest.
# Unmanaged files and the harness target directory are always preserved.

set -eu
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
. "$SCRIPT_DIR/lib.sh"

parse_common_args "$@"
[ "$UPGRADE" -eq 0 ] || die "--upgrade is only valid for install.sh"
validate_sources
managed=$(managed_dir)
old_manifest="$managed/revision-manifest.json"
[ -d "$managed" ] && [ ! -L "$managed" ] || die "managed installation is missing or unsafe: $managed"
[ -f "$old_manifest" ] && [ ! -L "$old_manifest" ] || die "managed revision manifest is missing or unsafe"
grep -q '"distribution_id": "agentctl-portable"' "$old_manifest" || die "installed manifest belongs to another distribution"

verify_removal_asset() {
  id=$1
  destination=$2
  expected=$(manifest_asset_hash "$old_manifest" "$id")
  [ -n "$expected" ] || die "installed manifest does not bind $id"
  [ -e "$destination" ] || [ -L "$destination" ] || die "managed asset is missing: $destination"
  [ -f "$destination" ] || die "managed asset is not a regular file: $destination"
  [ "$(sha256_file "$destination")" = "$expected" ] || die "managed asset was modified locally: $destination"
}

verify_removal_asset portable-skill "$managed/SKILL.md"
if [ "$HARNESS" = multica ]; then
  verify_removal_asset multica-runtime-bundle "$managed/agentctl-portable.bundle.json"
fi

revision=$(manifest_revision "$old_manifest")
if [ "$DRY_RUN" -eq 0 ]; then
  rm "$managed/SKILL.md"
  if [ "$HARNESS" = multica ]; then rm "$managed/agentctl-portable.bundle.json"; fi
  rm "$old_manifest"
  rmdir "$managed" 2>/dev/null || true
fi
state=removed
[ "$DRY_RUN" -eq 0 ] || state=planned
if [ "$JSON" -eq 1 ]; then
  printf '%s\n' "{\"ok\":true,\"state\":\"$state\",\"operation\":\"uninstall\",\"harness\":\"$HARNESS\",\"target_dir\":\"$TARGET_DIR\",\"managed_dir\":\"$managed\",\"revision\":\"$revision\"}"
else
  printf '%s\n' "status=$state operation=uninstall harness=$HARNESS target_dir=$TARGET_DIR revision=$revision"
fi
