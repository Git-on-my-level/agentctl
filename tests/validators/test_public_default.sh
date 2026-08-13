#!/usr/bin/env bash
# End-to-end acceptance for a public user with an empty home, no private
# configuration, no Multica/Tailnet, and no supported-agent installation.

set -euo pipefail
umask 077

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
TMP=$(mktemp -d "${TMPDIR:-/tmp}/agentctl-public-default.XXXXXX")
trap 'rm -rf "$TMP"' EXIT HUP INT TERM
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
if [ "$(id -u)" -eq 0 ]; then
  printf 'skip: public-default acceptance requires a non-root user\n'
  exit 0
fi

SOURCE=${AGENTCTL_TEST_BINARY:-}
if [ -z "$SOURCE" ]; then
  SOURCE="$TMP/source-agentctl"
  go build -trimpath -buildvcs=false -ldflags '-X main.version=v0.2.0-test' -o "$SOURCE" "$ROOT/cmd/agentctl"
fi
case "$SOURCE" in /*) ;; *) SOURCE="$(CDPATH='' cd -- "$(dirname -- "$SOURCE")" && pwd)/$(basename -- "$SOURCE")" ;; esac
[ -x "$SOURCE" ] || fail "test binary is not executable: $SOURCE"

export HOME="$TMP/home"
export XDG_CONFIG_HOME="$HOME/xdg/config"
export XDG_STATE_HOME="$HOME/xdg/state"
export XDG_DATA_HOME="$HOME/xdg/data"
export XDG_CACHE_HOME="$HOME/xdg/cache"
mkdir -p "$HOME"
# Exclude the operator's Homebrew/npm bins so detection cannot inherit Codex,
# Cursor, Claude, OMP, Multica, or private fleet tools from the host.
export PATH="$HOME/.local/bin:/usr/bin:/bin"
PREFIX="$HOME/.local"

printf '%s\n' preserve >"$HOME/unrelated-sentinel"
"$ROOT/scripts/install.sh" --binary "$SOURCE" --prefix "$PREFIX" >/dev/null
AGENTCTL="$PREFIX/bin/agentctl"
[ -x "$AGENTCTL" ] || fail 'binary installer did not create agentctl'
[ -f "$PREFIX/share/agentctl/install-manifest" ] || fail 'binary installer did not create its ownership manifest'
for root in .codex .cursor .claude .omp .hermes .multica; do
  [ ! -e "$HOME/$root" ] || fail "empty-home install unexpectedly created $root"
done

"$AGENTCTL" help >"$TMP/help.json"
python3 - "$TMP/help.json" <<'PY'
import json, sys
doc = json.load(open(sys.argv[1]))
assert doc['ok'] is True
commands = doc['result']['commands']
names = {command['name'] for command in commands}
assert {'doctor', 'run', 'result'} <= names
PY

"$AGENTCTL" doctor --static >"$TMP/doctor.json"
python3 - "$TMP/doctor.json" "$XDG_STATE_HOME/agentctl/journal.db" "$XDG_CONFIG_HOME/agentctl/config.json" <<'PY'
import json, sys
doc = json.load(open(sys.argv[1]))['result']
assert doc['healthy'] is True, doc
assert doc['bootstrap']['harnesses'] == [], doc['bootstrap']
assert doc['journal']['path'] == sys.argv[2] and doc['journal']['status'] == 'absent_ready_to_create'
assert doc['config']['path'] == sys.argv[3] and doc['config']['status'] == 'absent_optional'
assert doc['adapters'] == []
PY

"$AGENTCTL" bootstrap status >"$TMP/bootstrap.json"
python3 - "$TMP/bootstrap.json" <<'PY'
import json, sys
doc = json.load(open(sys.argv[1]))['result']
assert doc['healthy'] is True and doc['harnesses'] == [], doc
PY

"$AGENTCTL" run --adapter generic-process -- /bin/sh -c \
  'printf '\''%s\n'\'' '\''{"type":"result","status":"completed","result":{"summary":"public-default-ok"}}'\''' >"$TMP/run.json"
execution_id=$(python3 - "$TMP/run.json" <<'PY'
import json, sys
doc = json.load(open(sys.argv[1]))
result = doc['result']
assert result['state'] == 'completed', result
print(result['id'])
PY
)
case "$execution_id" in exec-*) ;; *) fail "run returned invalid execution ID: $execution_id" ;; esac

"$AGENTCTL" result "$execution_id" >"$TMP/result.json"
python3 - "$TMP/result.json" <<'PY'
import json, sys
outcome = json.load(open(sys.argv[1]))['result']['outcome']
assert outcome['availability'] == 'stored', outcome
assert outcome['content']['text'] == 'public-default-ok', outcome
assert outcome['result_ref'].startswith('agentctl://host-'), outcome
PY

journal="$XDG_STATE_HOME/agentctl/journal.db"
[ -f "$journal" ] || fail 'generic run did not create the XDG journal'
"$ROOT/scripts/uninstall.sh" --prefix "$PREFIX" >/dev/null
[ ! -e "$AGENTCTL" ] && [ ! -e "$PREFIX/share/agentctl/install-manifest" ] || fail 'binary uninstall left managed artifacts'
[ -f "$journal" ] || fail 'binary uninstall removed the execution journal'
[ "$(cat "$HOME/unrelated-sentinel")" = preserve ] || fail 'binary uninstall touched an unrelated file'

printf 'ok: empty-home public install, discovery, generic execution, result retrieval, and uninstall are self-contained\n'
