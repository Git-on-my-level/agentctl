# Journal contention implementation evidence (2026-11-10)

This change was developed and tested with disposable journals. No production
journal cleanup, compaction, binary installation, service restart, or release
was performed.

## Verified mechanisms

- `journal_busy.go` previously gave only run/delegate/dispatch/continue/await/
  await-any a default retry budget. Result collection, reconciliation and
  steering now receive that same bounded 120-second budget. Bulk collection and
  reconciliation write opens also respect the caller's cancellation context.
- Previous cleanup decoded the entire idempotency bucket once per eligible
  execution and inspected all selected event histories under a single open
  journal. Apply did that work inside a write transaction.
- Cleanup now copies metadata pages, computes protections outside transactions,
  and selects at most eight executions with 128 events each. The idempotency
  bucket is indexed once per plan. CLI handles close after every page/commit and retain the normal command open
  budget between pages, verified against a held writer lock.
  Event-heavy executions drain through reviewed partial batches. Event IDs
  participate in the digest so equal-sized prefixes cannot reuse an old digest.
  The latest event stays until the final commit so older writers cannot reuse
  a deleted sequence; this edge case was reproduced before the fix. Replayable
  event kinds retain their event/dedupe records until final removal; ingestion
  and cleanup share the terminal rejection predicate.
- Each deletion requires a consistent protection snapshot and an unchanged
  bbolt transaction generation. Late subscriptions, lineage links, events,
  acknowledgements, outcomes and diagnostic files fail closed. Legacy rows are
  compared against their original stored bytes, including unknown extensions. This works with
  older writers without adding a journal schema or a second write authority.
- All lineage-linked executions are retained conservatively, including fully
  terminal components. This is stricter than the previous manual policy and
  prevents intermediate dangling references across bounded commits.
- Default-on supervisor retention uses the same protections, additionally
  requires an explicit acknowledgement for pre-tracking records, and processes
  at most one event batch/execution every five minutes. The default cutoff is 14 days.
  `journal.retention.enabled=false` disables it; policy reloads each maintenance pass.
  Native request-key/immutable bindings, source bindings and ANX links are exempt.
  Paused subscription cursors remain protected. Manual plans disclose
  `pre_tracking_unreconciled` and inventory/recovery use paged, cancellable reads.
- Current main already classified a missing detached worker receipt as
  `admitted`, not `launched`. Added coverage verifies worker startup failures
  return errors, including under journal contention, and a lost receipt remains
  an admission requiring status/await evidence.

## Validation

Tests cover page-level file-lock release, read-only plans, bounded selection,
large-history partial draining with bbolt integrity checks, committed-prefix
receipts on cancellation, stale digest rejection, late reference/event/cache
changes, automatic acknowledgement and lineage protection, default/cutoff/kill
switch policy, native and Multica local retention, acknowledgement contention
and cancellation, and detached failure receipts.

The PR records terminal outcomes for full `make ci`, focused race tests, and
hosted checks. No provider-backed native or Multica operation was invoked to
validate retention; those authorities remain unchanged.

## Limits and follow-ups

- Concurrent commits can defer a stable protection snapshot, especially on a
  continuously busy journal. Maintenance has a 30-second pass deadline and
  preserves completed scan pages on timeout. Intervening writes invalidate the
  proof; deferrals are logged and remain visible in supervisor health. The
  stable proof is reused for selection and the commit, avoiding duplicate scans.
  Dedicated protection indexes or a reviewed single-writer design would reduce
  the metadata scan and improve progress under constant writes.
- An execution whose metadata or replayable event set exceeds the final batch
  is reported protected as `cleanup_final_batch_too_large`. Planning skips it
  and continues to later eligible candidates. Supporting arbitrarily large
  replayable sets needs a separately reviewed staged-GC/tombstone contract.
- Freed bbolt pages are reusable; physical file size does not shrink.
- Progress event sampling is left as a separate contract change; see the design
  in the PR body. Immutable events and subscription delivery are not rewritten.
- Open PR [#137](https://github.com/Git-on-my-level/agentctl/pull/137) overlaps
  this area. Its ANX stamp/legacy spool protection, await jitter/backoff,
  filter-before-hydrate queries and read-only outbox scans are ported here.
  This paged engine and host-local config policy supersede its duplicate engine
  and environment switch. A seven-day rollout preview/report is deferred: it
  needs a durable upgrade epoch and report contract; paged manual plans provide
  the preview boundary without delaying the default-on fix.

## Final review corrections

Manual cleanup preserves compact durable request-key refusal tombstones while
reclaiming plans, snapshots, events and answers. Same-key retries cannot launch
new work; changed inputs still conflict. Await-any prefix scans and their
acknowledgements are paged with writer-acquisition coverage. Cleanup plans check
for a selectable tail before setting `has_more`; data opens keep their contention
allowance while polling cancellation. Unchanged completed maintenance generations
skip rescanning, with cutoff-ageing and external-protection invalidation tests.
Health timestamps the actual failed maintenance pass rather than cadence ticks.

Deferred: convergence under sustained writes, the diagnostic-file removal versus
transaction-commit window, and repeated manual-apply protection scans. Older ANX
binaries that do not stamp links cannot participate atomically in the new marker
protocol; current registration paths stamp before spooling/sending.
