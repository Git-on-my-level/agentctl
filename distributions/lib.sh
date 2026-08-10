#!/bin/sh
# Shared, deliberately small distribution helpers. This file never discovers
# or copies harness state; callers pass an explicit target directory.

set -eu

DIST_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_DIR=$(CDPATH= cd -- "$DIST_DIR/.." && pwd)
SKILL_SOURCE="$REPO_DIR/skills/agentctl-portable/SKILL.md"
BUNDLE_SOURCE="$DIST_DIR/multica/agentctl-portable.bundle.json"
ALLOWLIST="$DIST_DIR/allowlist.json"
MANIFEST="$DIST_DIR/revision-manifest.json"

die() {
  printf '%s\n' "error: $*" >&2
  exit 2
}

usage() {
  cat >&2 <<'EOF'
usage: distribution command --harness <hermes|codex|claude|cursor|omp|multica> --target-dir <directory> [options]
EOF
}

sha256_file() {
  file=$1
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$file" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$file" | awk '{print $1}'
  else
    die "sha256sum or shasum is required"
  fi
}

canonical_file() {
  path=$1
  directory=$(dirname -- "$path")
  basename=$(basename -- "$path")
  printf '%s/%s' "$(CDPATH= cd -- "$directory" && pwd -P)" "$basename"
}

reject_symlink_components() {
  path=$1
  case "$path" in
    /*) ;;
    *) return 1 ;;
  esac
  case "$path" in
    *//*|*'/./'*|*'/../'*|*'/.'|*'/..') return 1 ;;
  esac
  current=/
  remainder=${path#/}
  while [ -n "$remainder" ]; do
    component=${remainder%%/*}
    if [ "$remainder" = "$component" ]; then remainder=; else remainder=${remainder#*/}; fi
    [ -n "$component" ] || continue
    current=${current%/}/$component
    [ ! -L "$current" ] || return 1
  done
}

is_harness() {
  case "$1" in
    hermes|codex|claude|cursor|omp|multica) return 0 ;;
    *) return 1 ;;
  esac
}

parse_common_args() {
  HARNESS=
  TARGET_DIR=
  MODE=copy
  JSON=0
  DRY_RUN=0

  while [ "$#" -gt 0 ]; do
    case "$1" in
      --harness)
        [ "$#" -ge 2 ] || die "--harness needs a value"
        HARNESS=$2; shift 2
        ;;
      --target-dir)
        [ "$#" -ge 2 ] || die "--target-dir needs a value"
        TARGET_DIR=$2; shift 2
        ;;
      --mode)
        [ "$#" -ge 2 ] || die "--mode needs copy or link"
        MODE=$2; shift 2
        ;;
      --json|--output=json)
        JSON=1; shift
        ;;
      --output)
        [ "$#" -ge 2 ] || die "--output needs text or json"
        case "$2" in
          json) JSON=1 ;;
          text) JSON=0 ;;
          *) die "--output must be text or json" ;;
        esac
        shift 2
        ;;
      --dry-run)
        DRY_RUN=1; shift
        ;;
      --help|-h)
        usage; exit 0
        ;;
      *)
        die "unknown option: $1"
        ;;
    esac
  done

  [ -n "$HARNESS" ] || die "--harness is required"
  is_harness "$HARNESS" || die "unsupported harness: $HARNESS"
  [ -n "$TARGET_DIR" ] || die "--target-dir is required; no home directory is inferred"
  reject_symlink_components "$TARGET_DIR" || die "target directory must be an absolute clean path without symlink components: $TARGET_DIR"
  case "$MODE" in copy|link) ;; *) die "--mode must be copy or link" ;; esac

  # Do not follow a symlink supplied as the target root. It could escape the
  # caller's intended temporary/home-specific target.
  if [ -e "$TARGET_DIR" ] && [ -L "$TARGET_DIR" ]; then
    die "target directory must not be a symlink: $TARGET_DIR"
  fi
  case "$TARGET_DIR" in
    *"/auth"|*"/sessions"|*"/memories"|*"/settings"|*"/plugins"|*"/caches")
      die "target directory names a forbidden harness state class: $TARGET_DIR" ;;
  esac
}

managed_dir() {
  printf '%s/agentctl-portable' "$TARGET_DIR"
}

ensure_target_dir() {
  if [ "$DRY_RUN" -eq 1 ]; then return 0; fi
  if [ -e "$TARGET_DIR" ] && [ ! -d "$TARGET_DIR" ]; then
    die "target is not a directory: $TARGET_DIR"
  fi
  if [ ! -e "$TARGET_DIR" ]; then
    umask 077
    mkdir -p "$TARGET_DIR"
  fi
  [ ! -L "$TARGET_DIR" ] || die "target directory became a symlink: $TARGET_DIR"
}

ensure_managed_dir() {
  dir=$(managed_dir)
  if [ -e "$dir" ] && [ ! -d "$dir" ]; then
    die "managed path is not a directory: $dir"
  fi
  if [ -e "$dir" ] && [ -L "$dir" ]; then
    die "managed directory must not be a symlink: $dir"
  fi
  if [ "$DRY_RUN" -eq 0 ] && [ ! -e "$dir" ]; then
    umask 077
    mkdir "$dir"
  fi
}

source_hashes_match_manifest() {
  skill_hash=$(sha256_file "$SKILL_SOURCE")
  bundle_hash=$(sha256_file "$BUNDLE_SOURCE")
  allowlist_hash=$(sha256_file "$ALLOWLIST")
  grep -q "\"sha256\": \"$skill_hash\"" "$MANIFEST" || return 1
  grep -q "\"sha256\": \"$bundle_hash\"" "$MANIFEST" || return 1
  grep -q "\"sha256\": \"$allowlist_hash\"" "$MANIFEST" || return 1
}

manifest_revision() {
  sed -n 's/.*"revision": "\([^"]*\)".*/\1/p' "$1" | head -n 1
}

distribution_metadata_matches() {
  allowlist_revision=$(manifest_revision "$ALLOWLIST")
  manifest_revision_value=$(manifest_revision "$MANIFEST")
  bundle_revision=$(manifest_revision "$BUNDLE_SOURCE")
  [ -n "$allowlist_revision" ] || return 1
  [ "$allowlist_revision" = "$manifest_revision_value" ] || return 1
  [ "$allowlist_revision" = "$bundle_revision" ] || return 1
  skill_hash=$(sha256_file "$SKILL_SOURCE")
  grep -q "\"sha256\": \"$skill_hash\"" "$BUNDLE_SOURCE" || return 1
}

validate_sources() {
  [ -f "$SKILL_SOURCE" ] || die "allowlisted skill source is missing"
  [ -f "$BUNDLE_SOURCE" ] || die "allowlisted Multica bundle is missing"
  [ -f "$ALLOWLIST" ] || die "allowlist manifest is missing"
  [ -f "$MANIFEST" ] || die "revision manifest is missing"
  grep -q '"skills/agentctl-portable/SKILL.md"' "$ALLOWLIST" || die "skill is not allowlisted"
  grep -q '"distributions/multica/agentctl-portable.bundle.json"' "$ALLOWLIST" || die "Multica bundle is not allowlisted"
  grep -q '"distributions/revision-manifest.json"' "$ALLOWLIST" || die "revision manifest is not allowlisted"
  source_hashes_match_manifest || die "source hash does not match revision manifest"
  distribution_metadata_matches || die "distribution revision or embedded skill hash does not match"
}

copy_or_link() {
  source=$1
  destination=$2
  if [ "$MODE" = link ]; then
    # Use the canonical source path. Relative links are unsafe when a system
    # path such as /tmp is itself a symlink (as on macOS).
    ln -s "$(canonical_file "$source")" "$destination"
  else
    umask 077
    tmp="$destination.tmp.$$"
    rm -f "$tmp"
    cp "$source" "$tmp"
    chmod 0600 "$tmp"
    mv -f "$tmp" "$destination"
  fi
}

json_bool() { [ "$1" -eq 1 ] && printf true || printf false; }
