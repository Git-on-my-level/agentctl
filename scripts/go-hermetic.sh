#!/bin/sh
# Runs a Go command with HOME, XDG roots, and agentctl overrides isolated under a
# fresh temporary directory. Without this, tests that reach os.UserHomeDir or
# XDG paths read the operator's live agentctl config, config-source state, and
# git rules, so a local `make test` can fail on machine state that CI never sees.
#
# Go's module and build caches stay where the caller's toolchain keeps them, so
# isolation does not force a re-download or a cold build.
#
# usage: [GO=go] go-hermetic.sh go test ./...
set -eu

if [ "$#" -eq 0 ]; then
  printf 'usage: go-hermetic.sh <command> [args...]\n' >&2
  exit 2
fi

# Resolve the cache and config locations before HOME moves. GO names the Go
# binary used for these queries; the wrapped command itself is unrestricted.
go_bin=${GO:-go}
GOCACHE=$("$go_bin" env GOCACHE)
GOMODCACHE=$("$go_bin" env GOMODCACHE)
GOPATH=$("$go_bin" env GOPATH)
GOENV=$("$go_bin" env GOENV)
export GOCACHE GOMODCACHE GOPATH GOENV

scratch=$(mktemp -d "${TMPDIR:-/tmp}/agentctl-hermetic.XXXXXX")
trap 'rm -rf "$scratch"' EXIT HUP INT TERM

HOME="$scratch/home"
XDG_CONFIG_HOME="$scratch/config"
XDG_DATA_HOME="$scratch/data"
XDG_STATE_HOME="$scratch/state"
XDG_CACHE_HOME="$scratch/cache"
mkdir -p "$HOME" "$XDG_CONFIG_HOME" "$XDG_DATA_HOME" "$XDG_STATE_HOME" "$XDG_CACHE_HOME"
export HOME XDG_CONFIG_HOME XDG_DATA_HOME XDG_STATE_HOME XDG_CACHE_HOME

# Drop operator overrides for agentctl, the ZCode provider files, and the
# agent-session markers that change behaviour when a test runs inside an agent.
for name in $(env | sed -n 's/^\(AGENTCTL_[A-Za-z0-9_]*\)=.*/\1/p') \
  $(env | sed -n 's/^\(ZCODE_[A-Za-z0-9_]*\)=.*/\1/p') \
  CLAUDECODE CODEX_THREAD_ID CURSOR_AGENT_COMPLETED_PATH; do
  unset "$name"
done

# Not exec: the EXIT trap must still run to remove the scratch tree.
status=0
"$@" || status=$?
exit "$status"
