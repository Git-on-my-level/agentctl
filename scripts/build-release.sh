#!/usr/bin/env bash
# Build deterministic, credential-free release archives for supported targets.

set -euo pipefail

ROOT="$(cd -- "$(dirname -- "$0")/.." && pwd)"
OUT_DIR=${OUT_DIR:-"$ROOT/dist"}
MAIN_PKG=${MAIN_PKG:-./cmd/agentctl}
VERSION=${VERSION:-}
TARGETS=${TARGETS:-}
PYTHON=${PYTHON:-python3}

die() { printf 'error: %s\n' "$*" >&2; exit 2; }
usage() {
  cat <<'EOF'
usage: scripts/build-release.sh [--output-dir DIR] [--version VERSION]
                           [--targets 'GOOS/GOARCH ...']

No network access or credentials are used. MAIN_PKG, VERSION, TARGETS, and
SOURCE_DATE_EPOCH may also be supplied through the environment.
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
    --output-dir)
      [ "$#" -ge 2 ] || die '--output-dir needs a value'
      OUT_DIR=$2; shift 2
      ;;
    --version)
      [ "$#" -ge 2 ] || die '--version needs a value'
      VERSION=$2; shift 2
      ;;
    --targets)
      [ "$#" -ge 2 ] || die '--targets needs a value'
      TARGETS=$2; shift 2
      ;;
    --help|-h) usage; exit 0 ;;
    *) die "unknown option: $1" ;;
  esac
done

command -v go >/dev/null 2>&1 || die 'Go is required'
command -v "$PYTHON" >/dev/null 2>&1 || die "Python is required to create deterministic archives: $PYTHON"
[ -f "$ROOT/go.mod" ] || die 'go.mod is required for a release build'
if [ -z "${TARGETS:-}" ] && [ -f "$ROOT/packaging/release-targets.txt" ]; then
  TARGETS=$(sed -e 's/#.*//' -e '/^[[:space:]]*$/d' "$ROOT/packaging/release-targets.txt" | tr '\n' ' ')
fi
TARGETS=${TARGETS:-"darwin/amd64 darwin/arm64 linux/amd64 linux/arm64"}
if [ -z "$VERSION" ]; then
  VERSION=$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || printf '%s' dev)
fi
case "$VERSION" in
  ''|*[!A-Za-z0-9._+-]*) die "version contains unsupported filename characters: $VERSION" ;;
esac
case "${BINARY_NAME:-agentctl}" in
  ''|*/*|*[!A-Za-z0-9._+-]*) die 'BINARY_NAME must be a simple filename' ;;
esac

if [ -z "${SOURCE_DATE_EPOCH:-}" ]; then
  SOURCE_DATE_EPOCH=$(git -C "$ROOT" show -s --format=%ct HEAD 2>/dev/null || printf '%s' 0)
fi
export SOURCE_DATE_EPOCH

mkdir -p "$OUT_DIR"
OUT_DIR="$(cd -- "$OUT_DIR" && pwd)"
staging=$(mktemp -d "$OUT_DIR/.agentctl-release.XXXXXX")
trap 'rm -rf "$staging"' EXIT

if [ ! -f "$ROOT/LICENSE" ] && [ ! -f "$ROOT/NOTICE" ]; then
  printf '%s\n' 'warning: no LICENSE or NOTICE is present; review licensing before publishing' >&2
fi

archive_files=
for target in $TARGETS; do
  case "$target" in
    */*) ;;
    *) die "target must be GOOS/GOARCH: $target" ;;
  esac
  goos=${target%%/*}
  goarch=${target#*/}
  case "$goos/$goarch" in
    darwin/amd64|darwin/arm64|linux/amd64|linux/arm64) ;;
    *) die "unsupported release target: $target" ;;
  esac

  binary_name=${BINARY_NAME:-agentctl}
  package_name="${binary_name}_${VERSION}_${goos}_${goarch}"
  package_dir="$staging/$package_name"
  mkdir -p "$package_dir"
  GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 \
    go build -C "$ROOT" -trimpath -buildvcs=false -ldflags '-s -w' \
      -o "$package_dir/$binary_name" "$MAIN_PKG"

  cp "$ROOT/README.md" "$package_dir/README.md"
  if [ -d "$ROOT/docs" ]; then cp -R "$ROOT/docs" "$package_dir/docs"; fi
  if [ -d "$ROOT/schemas" ]; then cp -R "$ROOT/schemas" "$package_dir/schemas"; fi
  for metadata in LICENSE NOTICE; do
    if [ -f "$ROOT/$metadata" ]; then cp "$ROOT/$metadata" "$package_dir/$metadata"; fi
  done

  archive="$OUT_DIR/$package_name"
  "$PYTHON" "$ROOT/scripts/make-archive.py" \
    --source "$package_dir" --output "$archive.tar.gz" \
    --name "$package_name" --epoch "$SOURCE_DATE_EPOCH"
  archive_files="$archive_files $archive.tar.gz"
done

checksum_tmp=$(mktemp "$OUT_DIR/.checksums.XXXXXX")
trap 'rm -rf "$staging" "$checksum_tmp"' EXIT
for artifact in $archive_files; do
  printf '%s  %s\n' "$(sha256_file "$artifact")" "$(basename "$artifact")"
done | sort >"$checksum_tmp"
mv -f "$checksum_tmp" "$OUT_DIR/SHA256SUMS"
printf 'release artifacts written to %s\n' "$OUT_DIR"
