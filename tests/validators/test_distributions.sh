#!/bin/sh
# Distribution acceptance checks. Every write is confined to a disposable
# temporary home and every target is passed explicitly to the scripts.

set -eu
umask 077
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
DIST="$ROOT/distributions"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/agentctl-distribution-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
export HOME="$tmp/home"
mkdir -p "$HOME"

fail() { printf '%s\n' "FAIL: $*" >&2; exit 1; }

[ ! -e "$HOME/.codex" ] || fail "test home unexpectedly populated before install"
if "$DIST/install.sh" --harness codex >/dev/null 2>&1; then
  fail "installer accepted an implicit home"
fi

for harness in hermes codex claude cursor omp; do
  target="$HOME/targets/$harness"
  mkdir -p "$target"
  printf '%s\n' "keep-$harness" >"$target/unmanaged.txt"
  mkdir -p "$target/agentctl-portable"
  printf '%s\n' "keep-managed-$harness" >"$target/agentctl-portable/unmanaged.txt"
  "$DIST/doctor.sh" --harness "$harness" --target-dir "$target" --output=json >/dev/null
  "$DIST/install.sh" --harness "$harness" --target-dir "$target" --output=json >/dev/null
  "$DIST/status.sh" --harness "$harness" --target-dir "$target" --output=json | grep -q '"ok":true,"state":"installed"'
  [ "$(cat "$target/unmanaged.txt")" = "keep-$harness" ] || fail "unmanaged file changed for $harness"
  [ "$(cat "$target/agentctl-portable/unmanaged.txt")" = "keep-managed-$harness" ] || fail "managed-directory unmanaged file changed for $harness"

  # A second install is idempotent and does not need a repair/write pass.
  first_hash=$(shasum -a 256 "$target/agentctl-portable/SKILL.md" | awk '{print $1}')
  "$DIST/install.sh" --harness "$harness" --target-dir "$target" --output=json >/dev/null
  second_hash=$(shasum -a 256 "$target/agentctl-portable/SKILL.md" | awk '{print $1}')
  [ "$first_hash" = "$second_hash" ] || fail "idempotent install changed $harness"
done

multica="$HOME/targets/multica"
mkdir -p "$multica"
printf '%s\n' keep-multica >"$multica/unmanaged.txt"
"$DIST/install.sh" --harness multica --target-dir "$multica" --mode link --output=json >/dev/null
"$DIST/status.sh" --harness multica --target-dir "$multica" --output=json | grep -q '"state":"installed"'
[ -f "$multica/agentctl-portable/agentctl-portable.bundle.json" ] || fail "Multica runtime bundle was not installed"
[ -L "$multica/agentctl-portable/SKILL.md" ] || fail "link mode did not create a managed link"
[ "$(cat "$multica/unmanaged.txt")" = keep-multica ] || fail "Multica unmanaged file changed"

# Existing unmanaged content at a managed path is never overwritten.
conflict="$HOME/targets/conflict"
mkdir -p "$conflict/agentctl-portable"
printf '%s\n' caller-owned >"$conflict/agentctl-portable/SKILL.md"
if "$DIST/install.sh" --harness codex --target-dir "$conflict" >/dev/null 2>&1; then
  fail "installer overwrote a conflicting managed path"
fi
[ "$(cat "$conflict/agentctl-portable/SKILL.md")" = caller-owned ] || fail "conflict content changed"

missing="$HOME/targets/missing"
missing_status="$tmp/missing-status.json"
if "$DIST/status.sh" --harness codex --target-dir "$missing" --output=json >"$missing_status" 2>/dev/null; then
  fail "status reported a missing target as installed"
fi
grep -q '"ok":false,"state":"missing"' "$missing_status" || fail "missing status JSON claimed success"

drifted="$HOME/targets/drifted"
mkdir -p "$drifted"
"$DIST/install.sh" --harness codex --target-dir "$drifted" --output=json >/dev/null
printf '%s\n' modified >>"$drifted/agentctl-portable/SKILL.md"
drifted_status="$tmp/drifted-status.json"
if "$DIST/status.sh" --harness codex --target-dir "$drifted" --output=json >"$drifted_status" 2>/dev/null; then
  fail "status reported a drifted target as installed"
fi
grep -q '"ok":false,"state":"drifted"' "$drifted_status" || fail "drifted status JSON claimed success"

symlink_target="$HOME/targets/symlink-target"
symlink_external="$HOME/targets/symlink-external"
mkdir -p "$symlink_target" "$symlink_external"
"$DIST/install.sh" --harness codex --target-dir "$symlink_external" --output=json >/dev/null
ln -s "$symlink_external/agentctl-portable" "$symlink_target/agentctl-portable"
if "$DIST/status.sh" --harness codex --target-dir "$symlink_target" >/dev/null 2>&1; then
  fail "status followed a symlinked managed directory"
fi

forbidden="$HOME/targets/auth"
if "$DIST/install.sh" --harness codex --target-dir "$forbidden" >/dev/null 2>&1; then
  fail "installer accepted a forbidden state target"
fi
[ ! -e "$forbidden" ] || fail "forbidden target was mutated"

# The real home remains untouched; all files created by this test are below tmp.
[ ! -e "$HOME/.claude" ] || fail "unexpected harness state was created"
printf '%s\n' "ok: distribution installers/status/doctor are deterministic and scoped"
