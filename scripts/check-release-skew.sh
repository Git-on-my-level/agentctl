#!/usr/bin/env bash
# Proves an update can install under live work: execution owners and a supervisor
# from the previous release keep working while the executable is replaced, the
# supervisor restarts, and the new release writes to the same journal.
#
# usage: check-release-skew.sh [PREVIOUS_BINARY CURRENT_BINARY]
# Without arguments it builds the newest earlier release tag and the working tree.
# Releases in different store.CompatEpoch values are not expected to share a
# journal, so the check does not apply to them; the updater waits for owners then.
set -u

fail() {
  printf 'release skew: %s\n' "$1" >&2
  for log in "${logs[@]:-}"; do
    [ -s "$log" ] && { printf '%s:\n' "$log" >&2; cut -c1-600 "$log" >&2; }
  done
  exit 1
}

logs=()
supervisor=
scratch=$(mktemp -d "${TMPDIR:-/tmp}/agentctl-skew.XXXXXX")
scratch=$(cd "$scratch" && pwd -P)
stop_supervisor() {
  if [ -n "$supervisor" ]; then
    kill -TERM "$supervisor" 2>/dev/null
    wait "$supervisor" 2>/dev/null
    supervisor=
  fi
}
cleanup() {
  [ -z "${scratch:-}" ] || touch "$scratch/release"
  stop_supervisor
  rm -rf "$scratch"
}
trap cleanup EXIT

if [ $# -eq 2 ]; then
  previous=$1
  current=$2
elif [ $# -eq 0 ]; then
  head_commit=$(git rev-parse HEAD)
  tag=
  while IFS= read -r candidate; do
    if [ "$(git rev-list -n 1 "$candidate")" != "$head_commit" ]; then
      tag=$candidate
      break
    fi
  done < <(git tag --merged HEAD --sort=-v:refname --list 'v[0-9]*.[0-9]*.[0-9]*')
  [ -n "$tag" ] || fail 'no earlier release tag; fetch tags and full history'
  mkdir -p "$scratch/source"
  git archive "$tag" | tar -x -C "$scratch/source" || fail "cannot export $tag"
  (cd "$scratch/source" && go build -ldflags "-X main.version=$tag" -o "$scratch/previous" ./cmd/agentctl) || fail "cannot build $tag"
  go build -o "$scratch/current" ./cmd/agentctl || fail 'cannot build the working tree'
  previous=$scratch/previous
  current=$scratch/current
else
  fail 'usage: check-release-skew.sh [PREVIOUS_BINARY CURRENT_BINARY]'
fi

epoch_of() {
  # v0.18.0 predates the probe and defines epoch 1.
  "$1" _journal-compat 2>/dev/null | sed -n 's/.*"journal_compat_epoch":\([0-9]*\).*/\1/p' | grep . || echo 1
}
previous_epoch=$(epoch_of "$previous")
current_epoch=$(epoch_of "$current")
if [ "$previous_epoch" != "$current_epoch" ]; then
  printf 'release skew: epoch %s to %s; updates wait for owners, nothing to check\n' "$previous_epoch" "$current_epoch"
  exit 0
fi

state=$scratch/state
bin=$scratch/bin/agentctl
mkdir -p "$scratch/home" "$scratch/bin" "$scratch/events" "$scratch/work" "$state"
chmod 700 "$state" "$scratch/events" "$scratch/bin"
cp "$previous" "$bin"
export HOME=$scratch/home AGENTCTL_UPDATE_MODE=off

cat >"$scratch/cursor-agent" <<EOF
#!/bin/sh
if [ "\$1" = --version ]; then echo 2026.09.10; exit 0; fi
input="\$* \$(cat)"
session="skew-\$\$"
echo '{"type":"system","subtype":"init","session_id":"'"\$session"'"}'
case "\$input" in *hold*)
  touch "$scratch/started.\$\$"
  count=0
  while [ ! -e "$scratch/release" ] && [ "\$count" -lt 400 ]; do /bin/sleep 0.2; count=\$((count + 1)); done
  echo '{"type":"assistant","session_id":"'"\$session"'","message":{"content":[{"type":"text","text":"working"}]}}'
  ;;
esac
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"'"\$session"'","result":"done"}'
EOF
chmod 700 "$scratch/cursor-agent"
cat >"$scratch/config.json" <<EOF
{"schema_version":1,"default_profile":"test","profiles":{"test":{
 "adapters":{"cursor":{"executable":"$scratch/cursor-agent"}},
 "route":{"this_host":"workstation","hosts":{"remote":"other"}},
 "agent_preferences":{"mode":"advisory","preferred":[{"agent":"cursor","family":"grok","version":"4.6","model":"cursor-grok-4.6-high","speed":"regular","default":true}]}}}}
EOF
chmod 600 "$scratch/config.json"

agentctl() {
  "$bin" --config "$scratch/config.json" --journal "$state/journal.db" "$@"
}
request() {
  printf '{"schema_version":1,"request_key":"%s","selector":{"family":"grok"}}' "$1" >"$scratch/work/$1.json"
  printf '%s' "$2" >"$scratch/work/$1.prompt"
}
start_supervisor() {
  "$bin" --config "$scratch/config.json" --journal "$state/journal.db" supervisor run --state-dir "$state" --socket "$state/s.sock" --interval 1s >>"$scratch/supervisor.log" 2>&1 &
  supervisor=$!
}
field() {
  python3 -c 'import json,sys
value = json.load(sys.stdin)
for key in sys.argv[1].split("."):
    value = value.get(key) if isinstance(value, dict) else None
print("" if value is None else value)' "$1"
}
logs=("$scratch/supervisor.log" "$scratch/foreground.out" "$scratch/foreground.err" "$scratch/detached.out" "$scratch/writes.log")
cd "$scratch/work" || fail 'no work directory'

# Live work on the previous release.
start_supervisor
request foreground 'hold'
request detached 'hold'
agentctl delegate --request-file foreground.json --prompt-file foreground.prompt --wait --content --timeout 110s --label skew.foreground >"$scratch/foreground.out" 2>"$scratch/foreground.err" &
foreground=$!
agentctl --output json delegate --request-file detached.json --prompt-file detached.prompt --detach --timeout 110s --label skew.detached >"$scratch/detached.out" 2>&1 || fail 'detached launch failed on the previous release'
detached=$(field result.id <"$scratch/detached.out")
[ -n "$detached" ] || fail 'detached receipt has no execution id'
count=0
while [ "$(find "$scratch" -maxdepth 1 -name 'started.*' | wc -l)" -lt 2 ]; do
  count=$((count + 1))
  [ "$count" -lt 150 ] || fail 'owners did not start'
  /bin/sleep 0.2
done

# The update: replace the executable, then restart the supervisor from it.
{ cp "$current" "$bin.new" && mv -f "$bin.new" "$bin"; } || fail 'cannot replace the executable'
stop_supervisor
start_supervisor

# The new release writes everything a previous owner reads while appending.
{
  agentctl subscribe create --label-prefix skew. --kind terminal,attention --ttl 1h --destination file --target "$scratch/events/prefix.ndjson" &&
    agentctl subscribe create --execution "$detached" --ttl 1h --destination file --target "$scratch/events/exact.ndjson" &&
    agentctl subscribe create --authority direct --ttl 1h --destination file --target "$scratch/events/authority.ndjson" &&
    request quick 'finish' &&
    agentctl delegate --request-file quick.json --prompt-file quick.prompt --wait --content --timeout 60s --label skew.quick
} >"$scratch/writes.log" 2>&1 || fail 'the current release could not write beside live owners'

/bin/sleep 3
kill -0 "$supervisor" 2>/dev/null || fail 'the current supervisor exited beside live owners'
touch "$scratch/release"

wait "$foreground"
foreground_exit=$?
[ "$foreground_exit" -eq 0 ] || fail "previous-release foreground owner exited $foreground_exit"
[ "$(cat "$scratch/foreground.out")" = "done" ] || fail 'previous-release foreground owner lost its result'
content=$(agentctl await "$detached" --content --timeout 60s 2>>"$scratch/writes.log") || fail 'current release could not collect the detached owner'
[ "$content" = "done" ] || fail "detached owner returned: $content"
agentctl --output json supervisor status --socket "$state/s.sock" >/dev/null 2>>"$scratch/writes.log" || fail 'current supervisor stopped answering'

# Nothing may have been orphaned or left running by the swap.
agentctl --output json recent >"$scratch/recent.json" 2>>"$scratch/writes.log" || fail 'current release could not list executions'
python3 - "$scratch/recent.json" <<'EOF' || fail 'an execution did not complete'
import json, sys
executions = json.load(open(sys.argv[1]))["result"]["executions"]
if len(executions) != 3:
    sys.exit(f"expected 3 executions, got {len(executions)}")
for execution in executions:
    if execution["state"] != "completed":
        sys.exit(f"{execution['labels']} ended {execution['state']}")
EOF
printf 'release skew: ok (epoch %s)\n' "$current_epoch"
