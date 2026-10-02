#!/usr/bin/env bash
# Publish only verified release artifacts. Existing release state and assets
# are preserved; reruns upload missing assets and refuse conflicting bytes.
set -euo pipefail

ROOT="$(cd -- "$(dirname -- "$0")/.." && pwd)"
TAG=${1:-}
DIST_DIR=${2:-"$ROOT/dist"}
die() { printf 'error: %s\n' "$*" >&2; exit 1; }
[[ "$TAG" == v* && "$TAG" != *[!A-Za-z0-9._+-]* ]] || die 'an exact safe version tag is required'
[[ -d "$DIST_DIR" ]] || die 'release artifact directory is missing'
DIST_DIR="$(cd -- "$DIST_DIR" && pwd)"
command -v gh >/dev/null 2>&1 || die 'GitHub CLI is required'

# Validate the complete local archive/checksum set again after the workflow's
# artifact download. No unchecked or extra archive may enter publication.
python3 - "$DIST_DIR" "$TAG" <<'PY'
import hashlib
import re
import sys
from pathlib import Path

root, tag = Path(sys.argv[1]), sys.argv[2]
manifest = root / "SHA256SUMS"
if not manifest.is_file() or manifest.is_symlink():
    raise SystemExit("release checksum manifest is missing or unsafe")
expected = {}
for line in manifest.read_text().splitlines():
    match = re.fullmatch(r"([0-9a-f]{64})  ([A-Za-z0-9._+-]+\.tar\.gz)", line)
    if not match:
        raise SystemExit("invalid release checksum entry")
    digest, name = match.groups()
    if not name.startswith("agentctl_" + tag + "_") or name in expected:
        raise SystemExit("release checksum tag mismatch or duplicate")
    expected[name] = digest
archives = {path.name: path for path in root.glob("*.tar.gz")}
if not expected or set(archives) != set(expected):
    raise SystemExit("release archives do not match the checksum manifest")
for name, path in archives.items():
    if not path.is_file() or path.is_symlink() or hashlib.sha256(path.read_bytes()).hexdigest() != expected[name]:
        raise SystemExit("release archive checksum mismatch or unsafe file")
PY

shopt -s nullglob
assets=("$DIST_DIR"/*.tar.gz "$DIST_DIR/SHA256SUMS")
if gh release view "$TAG" >/dev/null 2>&1; then
  existing_assets="$(gh release view "$TAG" --json assets --jq '.assets[].name')"
  verify_dir=$(mktemp -d)
  trap 'rm -rf "$verify_dir"' EXIT
  upload_assets=()
  for asset in "${assets[@]}"; do
    name=$(basename "$asset")
    if grep -Fxq "$name" <<< "$existing_assets"; then
      gh release download "$TAG" --pattern "$name" --dir "$verify_dir"
      cmp -s "$asset" "$verify_dir/$name" || die "existing release asset differs; refusing to overwrite: $name"
    else
      upload_assets+=("$asset")
    fi
  done
  if [[ ${#upload_assets[@]} -eq 0 ]]; then
    printf 'All verified release assets already exist for %s\n' "$TAG"
    exit 0
  fi
  # No clobber, edit, delete, or draft/prerelease/publication-state mutation.
  gh release upload "$TAG" "${upload_assets[@]}"
else
  notes="$ROOT/docs/release-${TAG}.md"
  # Keep the array nonempty: Bash 3.2 treats empty arrays as unset under -u.
  create_args=(--verify-tag --generate-notes --title "$TAG")
  if [[ -f "$notes" ]]; then
    create_args+=(--notes-file "$notes")
  fi
  gh release create "$TAG" "${assets[@]}" "${create_args[@]}"
fi
