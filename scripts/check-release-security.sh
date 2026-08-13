#!/usr/bin/env bash
# Verify checksums and scan the exact compiled binaries in release archives.

set -euo pipefail

ROOT="$(cd -- "$(dirname -- "$0")/.." && pwd)"
DIST_DIR=${1:-"$ROOT/dist"}
GOVULNCHECK=${GOVULNCHECK:-govulncheck}

die() { printf 'error: %s\n' "$*" >&2; exit 2; }

command -v "$GOVULNCHECK" >/dev/null 2>&1 || die "govulncheck is required: $GOVULNCHECK"
[ -d "$DIST_DIR" ] || die "release directory does not exist: $DIST_DIR"
[ -s "$DIST_DIR/SHA256SUMS" ] || die "missing release checksum manifest: $DIST_DIR/SHA256SUMS"

dist_abs="$(cd -- "$DIST_DIR" && pwd)"
work=$(mktemp -d "${TMPDIR:-/tmp}/agentctl-release-security.XXXXXX")
trap 'rm -rf "$work"' EXIT

(
  cd "$dist_abs"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum --check SHA256SUMS
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 --check SHA256SUMS
  else
    die 'sha256sum or shasum is required'
  fi
)

found=0
while read -r _ archive_name; do
	[ -n "$archive_name" ] || continue
	case "$archive_name" in
		agentctl_*.tar.gz) ;;
		*) die "unexpected release artifact in SHA256SUMS: $archive_name" ;;
	esac
	[ "$(basename "$archive_name")" = "$archive_name" ] || die "release artifact must be a basename: $archive_name"
	archive="$dist_abs/$archive_name"
	[ -f "$archive" ] || die "release archive is missing: $archive_name"
	found=1
	name=$(basename "$archive" .tar.gz)
  target="$work/$name"
  mkdir -p "$target"
  tar -xzf "$archive" -C "$target"
  for required in \
    LICENSE NOTICE README.md \
    scripts/install.sh scripts/uninstall.sh \
    scripts/install-supervisor.sh scripts/uninstall-supervisor.sh \
    scripts/install-systemd-supervisor.sh scripts/uninstall-systemd-supervisor.sh \
    skills/agentctl-portable/SKILL.md; do
    [ -f "$target/$name/$required" ] || die "archive is missing $required: $archive_name"
  done
  binary="$target/$name/agentctl"
  [ -f "$binary" ] || die "archive does not contain the expected agentctl binary: $archive_name"
	printf 'scanning %s\n' "$archive_name"
	"$GOVULNCHECK" -mode=binary "$binary"
done < "$dist_abs/SHA256SUMS"

[ "$found" -eq 1 ] || die "no release archives found in $dist_abs"
