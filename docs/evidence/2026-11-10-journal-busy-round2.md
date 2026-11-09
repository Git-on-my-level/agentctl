# Journal busy review fixes, round 2

Source follow-up to [PR #137](https://github.com/Git-on-my-level/agentctl/pull/137),
based on `e434e034f42c96dfbfb61c5ff5404efb251a28ad`. Recorded on m5-mbp on
2026-11-10 (Asia/Ho_Chi_Minh). No installation, merge, live-journal change or
supervisor restart was performed. GCP tier: none; GCP was not used.

## Contracts

Retention remains on by default. Unacknowledged terminal results are protected
regardless of the acknowledgement epoch. ANX card labels and run-link stamps
protect executions, outcomes and events even after collection or an explicit
unread-result cleanup override. New links persist protection before registration;
cleanup also reads legacy pending, dead and delivered spool evidence locally.
Spool read failures defer cleanup.

Existing journals store a seven-day preview deadline on their first writable
open by this version; read-only opens never initialize it. Preview passes write
only an aggregate report, and enforcement starts automatically at that deadline.
New journals enforce after startup grace. Opt-out and immediate-enforcement
options are documented in [the rollout guide](../journal-busy-rollout.md), along
with the mandatory operator backup step and rollback limits.

Owner absence cannot prove an unrecorded child absent. Unbound native starting
rows remain unknown and appear in doctor as needing reconciliation. Established
launches still require recorded child and collector absence with revision,
host and lease rechecks. Interactive authority reprobes use a jittered two-second
journal budget; only background supervisor cycles select ten seconds.

## Local validation

The final focused race command passed all three touched stateful packages:

```sh
GOMAXPROCS=4 nice -n 10 scripts/go-hermetic.sh go test -race \
  ./internal/store ./internal/anxrun ./cmd/agentctl \
  -run 'TestCleanup|TestRetentionPolicy|TestLinkedExecutions|TestJournalBusy|TestScopedWriterProgress|TestAwait.*Multica|TestAwait.*Poll|TestAwait.*Backoff|TestSupervisorOrphans|TestSupervisorStarting|TestSupervisorRetention|TestSupervisorPendingOutbox|TestLegacyRetention|TestANXUnlabeledRunProtection' \
  -count=1 -timeout=240s
```

Coverage includes pre-epoch and missing-epoch unread protection, collected
pre-epoch eligibility, ANX labels/stamps/legacy references in native and Multica
histories, protection after spool removal, delivered-link discovery, preview
reports/permissions/content exclusion, persisted deadlines and enforcement at
the exact boundary, immediate mode, opt-outs, live child with gone owner,
doctor reporting and the interactive await reprobe's typed contention budget.
Schema, link and whitespace checks passed.

An additional CLI run that included the pre-existing
`TestANXSecurityDetachedCommandLatency` failed its 1.5-second wall-clock bound
under local host load. The new requested regressions passed in that run and in
the final race run. The detachment test now blocks ANX until the parent command
returns, with the command deadline shorter than ANX's timeout. This directly
checks independence from transport completion instead of an absolute 1.5-second
host-speed assertion.

The initial full local CI attempt failed: a portable-skill manifest hash was
stale (both manifest copies have now been regenerated), the managed Git wrapper
rejected synthetic config-fixture identities in the hermetic HOME, and the CLI
suite reached its ten-minute limit after a native-version probe timeout. This
is not recorded as a green full suite. A subsequent full hook uses stock Git
only for fixture subprocesses, with native agent binaries excluded from test
PATH (the timeout trace showed doctor hashing a real installed agent). The
publishing command still uses managed Git and the unchanged pre-push hook; no
host configuration or guard is changed. Go, gofmt, Python and stock Git are real
binaries in a task-local tool directory; system utilities remain on PATH and
TMPDIR/GOTMPDIR point to Ephemeral. The Homebrew `bin/git` alias is itself managed,
so the fixture symlink targets the actual `opt/git/bin/git` binary.
Full local CI, the normal pre-push hook result and exact-head hosted checks are reported on
[the PR](https://github.com/Git-on-my-level/agentctl/pull/137); hosted CI remains
the full-suite acceptance gate.

The isolated full suite completed with only the pre-existing early-result
fixture's startup deadline failing; the normal hook also exposed a descendant
cleanup assertion charging Python startup to the pipe-close deadline. The
fixtures now use an explicit child-release barrier and a parent-exit marker,
respectively. The six-second lease observation and two-second cleanup assertion
remain; startup has its own bounded allowance and all fixture processes are
released or cancelled during cleanup. These changes preserve the lifecycle
assertions while removing dependence on launch speed under host load.

The normal pre-push hook passed in full and published the source fix. The
remaining distribution stage then caught the Multica runtime bundle's internal
skill-hash reference. Both bundle copies and their manifest entries were
regenerated. The embedded-distribution unit contract now checks the bundle's
skill hash and revision as well as byte equality, so the ordinary pre-push gate
catches this cross-asset mismatch before publication.

Devin subsequently identified a legacy custom-spool migration gap: after a root
change, current configuration cannot discover all historical unlabeled links.
Retention now protects admissions without a valid creation-time tracking receipt
as `legacy_anx_history_unknown`. Only genuinely new executions get that receipt,
atomically with admission; collection, updates and idempotent reuse never grant
it retroactively. This preserves unknown legacy history regardless of current
spool roots, collection, preview expiry or unread-result overrides. Retention
remains enabled for eligible tracked history. Regression tests cover the moved
custom spool, retained outcomes/events, a collected tracked control that expires,
and collection/reuse that cannot reclassify unknown legacy history. Tracking
records are counted and removed with eligible execution graphs.

A later final-head hook attempt exhausted Go's aggregate ten-minute package
budget under host load while another test was running, without an assertion
failure. `TEST_TIMEOUT` now exposes the same ten-minute default for full tests
and races; a bounded `20m` override is used only by the loaded-host publication
command. The unchanged pre-push hook still runs every check, and hosted CI keeps
its ten-minute default. The early-result release fixture also has its own
independent deadline so an aborted Go test process cannot leave it running.

The parallel final push run exposed another existing fixture race: its two-second
context deadline could expire before Python reported readiness. The full local
`make ci` unit suite passed that same package. The timeout fixture now allows
startup separately and measures the unchanged pipe/lock cleanup budget from
actual deadline expiry, still requiring `context.DeadlineExceeded`. Cancellation
and timeout paths both retain their cleanup and descendant assertions.
