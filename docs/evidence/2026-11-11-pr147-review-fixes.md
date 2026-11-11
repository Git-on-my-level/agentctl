# PR 147 independent-review fixes — 2026-11-11

Reviewed base: `85c10f686667ad70ecd83c8ea0f498ebd871e935` on
`feat/uncollected-children-at-exit`. Scope: PR 147 only. The separately owned
stacked exit-guard worktree and branch were not edited.

## Verified defects and fixes

- An attention child matched a synthetic terminal event, so a terminal-only
  subscription could hide blocked permission/input. Queries now always preserve attention
  children: a subscription added after the event receives no attention backfill. Exact-ID await processes supply live lock evidence for other
  active local states, with cancellation, timeout, exit and crash release.
- Automatic retention mistook execution-owned attribution snapshots/indexes for
  request-key replay. Selection now excludes those reserved metadata scopes from
  replay protection while preserving real replay and graph protection.
- The remaining Bugbot contention report is already fixed in the reviewed base:
  managed attribution fails closed on unavailable evidence, native admission
  borrows its acquired journal handle, and implicit children returns retryable
  incomplete coverage. Both existing contention regressions were rerun.

## Regression evidence

New tests exercise attention versus terminal/progress/all-kind subscriptions,
expiry, multiple waiters, helper-process SIGKILL, concurrent shared probes,
read-only query behavior, metadata bounds, CLI summary and waiter lifecycle,
and automatic cleanup versus keyed replay.

A Go overlay restoring the original `children.go` and `retention.go` makes
`TestChildrenAttentionRemainsActionableWithLateSubscription` and
`TestAutomaticCleanupRemovesUnkeyedCallerAttribution` fail. The original attention
case incorrectly returns an empty result for a terminal subscription; automatic
cleanup incorrectly removes no executions. Both pass with the final fixes.

## Checks

Result: all listed checks passed. Final local CI completed at 2026-11-11T05:45:40Z.

The test PATH directory contains a `git` symlink to `/usr/bin/git`, and blocking
stubs for notification commands. Task git operations use the managed wrapper.
Tests isolate HOME/XDG state through `go-hermetic.sh`; callback fixtures use
local disposable destinations. No real page, Telegram, paid native call, operator
journal write, host installation, merge, tag, release or GCP tier was used.

`PLAIN_GIT_DIR` and `NOTIFY_BLOCK_DIR` below name the recorded disposable
plain-git and notification-stub directories using portable variables.

```sh
PYTHONDONTWRITEBYTECODE=1 PATH="$PLAIN_GIT_DIR:$NOTIFY_BLOCK_DIR:$PATH" make ci
PYTHONDONTWRITEBYTECODE=1 PATH="$PLAIN_GIT_DIR:$NOTIFY_BLOCK_DIR:$PATH" scripts/go-hermetic.sh go test -race ./cmd/agentctl ./internal/store ./internal/runtime -count=1
PYTHONDONTWRITEBYTECODE=1 PATH="$PLAIN_GIT_DIR:$NOTIFY_BLOCK_DIR:$PATH" scripts/go-hermetic.sh go test -race ./cmd/agentctl ./internal/store -run 'TestChildren|TestAwaitContinuesWhenWaiterRegistrationFails|TestWaiter|TestAutomaticCleanupRemovesUnkeyedCallerAttribution|TestManagedChildrenAndLaunchAttributionFailClosedOnContention|TestNativeAdmissionPreservesParentAfterContendedJournal' -count=3
git diff --check
```

The first CI attempt caught a stale portable-skill hash in the Multica bundle.
The bundle, embedded copy and both revision manifests were synchronized before
the final CI run. Python bytecode writes are disabled for that run to avoid
modifying tracked generated cache files.

## Follow-up automated review

Automated review of `fe03d80` identified two further verified edge cases:
late attention subscriptions do not backfill their existing event, and sidecar
registration failure must not break primary await polling. Attention now always
remains actionable, regardless of matching filters. Waiter registration is a
single best-effort attempt; unavailable sidecars leave coverage unknown while
normal polling, timeout and cancellation continue.

`TestChildrenAttentionRemainsActionableWithLateSubscription` appends the attention
event before subscription creation and confirms there is no pending delivery.
`TestAwaitContinuesWhenWaiterRegistrationFails` makes sidecar directory creation
impossible while leaving the journal readable, and verifies timeout/cancellation.
An overlay of the `fe03d80` implementations makes both follow-up regressions
fail: the late attention subscription hides the child, and sidecar creation
failure returns an internal error instead of timeout/cancellation.

Follow-up validation: all listed checks passed again at 2026-11-11T06:06:53Z.

## Limits

Exact-ID await evidence is supported on Linux/macOS. Older awaits, await-any and
remote forwarding rows stay conservative. Evidence is live per page and may
change immediately after observation. Normal waiter exit removes its unique
file; crashes may leave empty files and directories. Unlocked residue does not
prove coverage, and excessive per-child metadata fails closed. Callback health,
historical coverage, existing graph-retention policy and parent-exit enforcement
remain outside this fix.
