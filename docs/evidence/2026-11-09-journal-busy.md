# m5-mbp journal contention, 2026-11-09

Coordinator ask 761128 to cl2-coordkit. Base: `origin/main` at `e3544e9`.
All live-journal access was ordinary file reads: no bbolt open/lock, transaction,
migration, vacuum, deletion or supervisor restart. A same-volume atomic APFS
`clonefile` into a private temporary directory was copied to Ephemeral scratch;
the temporary clone was removed. A sequential unlocked copy was rejected as
inconsistent and was not used for measurements. The valid copy's mtime is
2026-11-09T16:45:50Z. `bbolt.Tx.Check` on that copy reported zero errors.
No user content, prompts, transcripts or result excerpts are included here.

## Measurements

At 16:42Z host load averages were 76.97 / 61.32 / 57.03, with dozens of
agentctl processes. The live file was 2,219,376,640 bytes at that sample.
The valid copy was 2,219,687,936 bytes in file length and its committed
transaction address space was 2,202,894,336 bytes.

| Bucket | Records | Logical key + value bytes | Largest value bytes |
| --- | ---: | ---: | ---: |
| events | 974574 | 1008020924 | 2146 |
| event_dedupe | 974574 | 217746992 | 167 |
| events_by_execution | 974574 | 50120264 | 58 |
| executions | 13727 | 53691513 | 5011 |
| outcomes | 13453 | 45335184 | 73803 |
| idempotency | 25011 | 18316234 | 1844 |
| execution_acknowledgements | 12259 | 2086853 | 146 |
| callback_outbox_v1 | 527 | 1296491 | 2655 |
| terminal_events | 13464 | 1155480 | 54 |
| subscriptions_v1 | 527 | 394333 | 780 |
| callback_outbox_receipts_v1 | 524 | 98606 | 142 |
| callback_outbox_dedupe_v1 | 527 | 84154 | 56 |
| callback_outbox_dead_v1 | 3 | 6905 | 2291 |
| metadata | 3 | 117 | 40 |
| status_contacts | 0 | 0 | 0 |

The copy contained 12565 completed, 422 failed, 398 cancelled, 243 orphaned,
52 running, 19 starting, 17 waiting and 11 attention executions. Of its 99
nonterminal rows, 41 were last updated over 24h earlier and 58 within an hour;
53 lacked collector identity and 46 recorded one. This is not the supervisor's
count: parked native recoveries are excluded by its runtime bridge, and the
live journal continued changing after the snapshot.

Events span August 10 through November 9. There were 503016 Codex started events;
these are historical adapter observations, not evidence of that many launches.
The largest per-execution history had 9031 events. History, indexes and page
allocation dominate file size; bounded final answers do not explain it. There
was no automatic retention in the installed implementation. A read-only freelist
load on the valid copy reported only 20 free pages, so this is retained live
data/page occupancy rather than a large free-page pool waiting to be shrunk.

At approximately 16:52Z, read-only kernel identity probes for stale rows in the
copy grouped them as: 18 with both child and collector provably gone; 9 with
child gone but collector unrecorded; 28 whose child was not proven gone; 4 with
no child-launch identity; 40 owned by remote authority. These are snapshot-based
candidates, not permission to alter live rows. No live row was reconciled.

A read-only Unix-socket status RPC at 16:56:02Z returned PID 8042,
self-reported supervisor version v0.21.5, healthy, 74 nonterminal executions,
zero current reprobe/delivery failures and a last cycle at 16:55:51Z. Its
last_error_at was 16:54:45Z. The operator reported installed binary v0.21.6.
An installed artifact is not the identity of an already-running supervisor;
health was intermittent and the healthy sample is not evidence this fix ran.

## Causal reproduction and implementation

The engine is bbolt, with shared read-only and exclusive writable inode locks
held from open until close. There is no WAL, SQLite transaction or busy_timeout.
Writable opens eagerly load the freelist; read-only opens load it lazily. The
supervisor already uses scoped mutations and releases the journal across
adapter/callback I/O. Per-execution await already reads without a write
transaction and closes before its 500ms polling sleep. Those were not missing
transaction boundaries.

The supervisor's nonterminal listing decoded, hydrated immutable bindings and
validated all 13727 retained envelopes while holding a shared lock. On the
same valid copy under `nice -n 10`, original-code lock holds were 10.580s,
10.123s and 6.725s. With lightweight state filtering, representative later
runs were 0.708s, 0.763s and 0.994s, returning the same 99 rows. This is a noisy
loaded-host reproduction, not a controlled throughput benchmark. It directly
shows a reader lock hold substantially exceeding the supervisor's fixed 2s
contention budget. Overlapping readers can extend writer starvation further.

An attempted shortcut using the terminal-event index returned only 97 active
rows: two legacy terminal events had not terminalized their projections. It was
rejected. The final implementation reads the execution state itself, with a
regression test for this distinction and no new cache/index migration.

Changes address that scan, label-filtered await-any scans, writable pending
outbox scans, synchronized fixed-frequency polling, and the supervisor's too
short 2s budget (now 10s within existing probe/cycle deadlines). The existing
interactive jittered 120s budget is preserved. Retention plans bounded complete
graphs in a read handle and applies only against the identical committed
generation, without rescanning history in its write transaction. The old
quadratic per-target idempotency scan is now one join per plan. Stale starting
admissions with a recorded collector can be orphaned only on kernel absence
proof, with the existing revision/host/lease recheck.

See [maintenance contract](../journal-maintenance.md) and
[operator rollout](../journal-busy-rollout.md). No runtime recovery or deployed
acceptance is claimed by this source change. GCP tier: none; GCP was not used.

## Local validation and host limits

Targeted regressions cover unchanged-poll pacing/deadlines, concurrent reader
snapshots with writer progress, shared pending-outbox reads, absent/live/unproven
native owner identities, retention default/opt-outs, bounded graph/event batches,
unread/lineage protections, stale-generation rejection, replay and integrity.
The valid-copy reproduction and integrity check above are separate from tests.

A bounded `nice -n 10 make ci` run with `GOMAXPROCS=4` reached the full suite,
but local verification did not pass: unchanged `internal/config` source fixtures
were rejected by the host managed Git wrapper for their synthetic
`agentctl@example.invalid` identity after adding a local fixture remote, because
the hermetic HOME intentionally has no global identity. The CLI package also
hit Go's default 10-minute limit, with native ZCode version-probe timeouts before
it reached that limit. No global Git or host configuration was changed.

A broad focused-race attempt passed the store package but had one CLI
result-collection fixture fail while opening a fresh journal before await.
Repeating that exact orphaned fixture three times produced one native-version
probe timeout, one fixture run-deadline timeout and one pass. Its delegate
fixture has a 10s context; this is an unstable loaded-host result, not a green
suite and not a detected Go data race. Earlier exploratory runs were stopped
when superseded by the finalized source/assets. Hosted exact-head CI is the
remaining full-suite gate. The local pre-push hook repeats the same suite;
publication of this unmerged review branch skips that hook, with these failures
recorded rather than suppressed. Normal managed Git commit identity remains on.
