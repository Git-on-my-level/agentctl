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

verify_managed_asset() {
  id=$1
  destination=$2
  old_manifest=$3
  expected=$(manifest_asset_hash "$old_manifest" "$id")
  [ -n "$expected" ] || die "installed manifest does not bind $id"
  [ -e "$destination" ] || [ -L "$destination" ] || die "managed asset is missing: $destination"
  [ -f "$destination" ] || die "managed asset is not a regular file: $destination"
  [ "$(sha256_file "$destination")" = "$expected" ] || die "managed asset was modified locally: $destination"
}

upgrade_installation() {
  [ "$MODE" = copy ] || die "--upgrade requires --mode copy"
  old_manifest="$managed/revision-manifest.json"
  [ -f "$old_manifest" ] && [ ! -L "$old_manifest" ] || die "--upgrade requires a managed revision manifest"
  grep -q '"distribution_id": "agentctl-portable"' "$old_manifest" || die "installed manifest belongs to another distribution"
  verify_managed_asset portable-skill "$managed/SKILL.md" "$old_manifest"
  if [ "$HARNESS" = multica ]; then
    verify_managed_asset multica-runtime-bundle "$managed/agentctl-portable.bundle.json" "$old_manifest"
  fi
  if [ "$DRY_RUN" -eq 1 ]; then
    if [ "$JSON" -eq 1 ]; then
      printf '%s\n' "{\"ok\":true,\"state\":\"planned\",\"operation\":\"upgrade\",\"harness\":\"$HARNESS\",\"target_dir\":\"$TARGET_DIR\",\"managed_dir\":\"$managed\",\"revision\":\"$revision\",\"mode\":\"copy\"}"
    else
      printf '%s\n' "status=planned operation=upgrade harness=$HARNESS target_dir=$TARGET_DIR revision=$revision"
    fi
    return 0
  fi
  umask 077
  stage="$managed/.agentctl-upgrade.$$"
  backup="$managed/.agentctl-backup.$$"
  [ ! -e "$stage" ] && [ ! -e "$backup" ] || die "upgrade staging path already exists"
  mkdir "$stage" "$backup"
  cp "$SKILL_SOURCE" "$stage/SKILL.md"
  cp "$MANIFEST" "$stage/revision-manifest.json"
  chmod 0600 "$stage/SKILL.md" "$stage/revision-manifest.json"
  if [ "$HARNESS" = multica ]; then
    cp "$BUNDLE_SOURCE" "$stage/agentctl-portable.bundle.json"
    chmod 0600 "$stage/agentctl-portable.bundle.json"
  fi
  rollback_upgrade() {
    for name in SKILL.md revision-manifest.json agentctl-portable.bundle.json; do
      [ ! -e "$backup/$name" ] || { rm -f "$managed/$name"; mv "$backup/$name" "$managed/$name"; }
    done
    rm -rf "$stage" "$backup"
  }
  trap 'rollback_upgrade; exit 2' HUP INT TERM
  for name in SKILL.md revision-manifest.json; do mv "$managed/$name" "$backup/$name" || { rollback_upgrade; die "upgrade failed while backing up $name"; }; done
  if [ "$HARNESS" = multica ]; then mv "$managed/agentctl-portable.bundle.json" "$backup/agentctl-portable.bundle.json" || { rollback_upgrade; die "upgrade failed while backing up Multica bundle"; }; fi
  for name in SKILL.md revision-manifest.json; do mv "$stage/$name" "$managed/$name" || { rollback_upgrade; die "upgrade failed while installing $name"; }; done
  if [ "$HARNESS" = multica ]; then mv "$stage/agentctl-portable.bundle.json" "$managed/agentctl-portable.bundle.json" || { rollback_upgrade; die "upgrade failed while installing Multica bundle"; }; fi
  rm -rf "$stage" "$backup"
  trap - HUP INT TERM
  if [ "$JSON" -eq 1 ]; then
    printf '%s\n' "{\"ok\":true,\"state\":\"installed\",\"operation\":\"upgrade\",\"harness\":\"$HARNESS\",\"target_dir\":\"$TARGET_DIR\",\"managed_dir\":\"$managed\",\"revision\":\"$revision\",\"mode\":\"copy\"}"
  else
    printf '%s\n' "status=installed operation=upgrade harness=$HARNESS target_dir=$TARGET_DIR revision=$revision"
  fi
}

if [ "$UPGRADE" -eq 1 ]; then
  upgrade_installation
  exit 0
fi

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
