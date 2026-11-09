# Journal contention and maintenance

The schema-1 journal uses bbolt. A writable open holds an exclusive file lock;
a read-only open holds a shared lock, both until close. There is no SQLite WAL
or busy_timeout. Transactions are short atomic mutations; shortening a transaction
alone does not shorten an open handle's file lock. Supervisor adapter I/O and
await sleeps happen outside that lock.

Recovery filters the stored state before loading full immutable bindings and
validating active envelopes. Label-scoped await-any filters labels before that
work. A terminal *event* is not assumed to have updated its execution projection.
Read-only outbox scans use shared handles. Await polls back off unchanged
snapshots with jitter to at most four seconds, resetting on revision changes
(for await-any, changes in watched count). Deadlines and cancellation still bound
waits. Interactive launch/wait commands already have a jittered 120s contention
budget; the supervisor now uses 10s, bounded by its probe/cycle context. Other
errors still fail immediately; journal_busy retains exit 6 and retry accounting.

## Automatic retention

Only an explicitly running supervisor performs maintenance. After one minute,
and every five minutes thereafter, it plans cleanup of terminal executions older
than 30 days. `supervisor run --no-journal-retention` or the service environment
`AGENTCTL_JOURNAL_RETENTION=off` opts out. No read-only command, open, or installer
starts cleanup. `--once` does not bypass the startup grace.

New journals enforce after the startup grace. Existing journals enter a
persistent seven-day plan-only window on the first writable open by this
version; enforcement begins automatically at its stored deadline. Each preview
pass writes `<journal path>.retention-report.json` atomically with owner-only
permissions: counts by record category, diagnostic counts/bytes, oldest/newest
eligible terminal timestamps (null if empty), and protection counts by reason.
Its JSON shape is defined by [the retention report schema](../schemas/journal-retention-report.schema.json).
It contains no execution IDs or content and previews all eligible records,
without deleting records or diagnostics. Restarts do not reset the window and
read-only opens never start it. `--journal-retention-immediate` or
`AGENTCTL_JOURNAL_RETENTION=immediate` bypasses this window only. Back up the
binary and journal before installation; see the [rollout guide](journal-busy-rollout.md).

Batches preserve uncollected results regardless of acknowledgement epoch,
nonterminal or partly retained lineages, promotions, active exact
subscriptions/cursors and every retained delivery/receipt. Missing collection
evidence protects pre-epoch results; explicit `result`/`await` or operator
acknowledgement can make them eligible. `anx.card.*` labels or durable ANX run
links protect execution/outcome/event history even after collection, including
with explicit `--include-unreconciled` cleanup. Run-link registration stamps this
protection before spooling or delivery; legacy pending/dead/delivered spool
references are read locally during cleanup, including when registration is off.
Spool read failures defer cleanup. Automatic maintenance never acknowledges
unread results to make them eligible. Native and Multica histories use the same
protection graph; retention
never changes the Multica issue/run or the native CLI's conversation store.

Each batch contains whole components, at most 32 executions and 4096 events.
Oversized components are deferred to explicit digest-bound `data cleanup`.
Planning takes a read-only handle and is cancellable (30s total deadline).
After closing it, maintenance gets a writable handle and checks the exact bbolt
transaction generation before touching records or diagnostics. Any intervening
commit defers the batch without deletion. The write transaction deletes only
planned keys; it does not scan history again. Lock acquisition gets at most
250ms per phase; maintenance does not consume the supervisor recovery budget.
Unexpected maintenance failures are logged on stderr; busy/generation conflicts
simply defer until the next scheduled pass. The normal recovery pass still runs.
Cleanup is atomic for database records; a failed diagnostic removal preserves
them. Cancellation can remove a diagnostic cache file before a database rollback,
as with explicit cleanup, but cannot leave private output without its record.

Cleanup removes owned events, indexes, dedupe, results, collection stamps,
status contacts and idempotency keys together. Request keys no longer deduplicate
once their execution graph is expired. All changes use the existing schema-1
format and lock domain; older binaries remain compatible. Explicit cleanup keeps
its digest-bound CLI and JSON shape; its idempotency join is now one scan per
plan instead of one full scan per selected execution.

Deletion is **online logical compaction**: freed pages can be reused by later
commits. It does not shrink the physical file. Physical bbolt compaction cannot
safely replace the inode while old processes or waiting openers hold it; that
would create two independent lock domains and risk lost writes. There is no
online file-replacement command. Operators needing disk shrink must first
quiesce every journal owner and use a separately reviewed offline procedure.

## Stale native admissions

The supervisor already reconciles a stale established launch only after kernel
proof that both its recorded child and collector identities are gone, with a
revision/host/lease recheck before commit. A starting admission with no child
binding can now use its recorded collector identity alone. An active lease,
a live or uncertain identity, a missing collector identity, a foreign journal
host, a parked recovery or a recent observation never authorizes orphaning.
Orphaned means outcome unknown. No process is killed and no success/failure is
inferred. Legacy ownership remains unproven; Multica requires its own authority.

A stale native `starting` row without a child launch identity or source binding
remains unknown even when its collector is gone. Owner absence cannot prove an
independent detached child is absent. `doctor` reports these execution IDs under
`journal.needs_reconcile` and the problem `journal_native_child_unknown`; the
supervisor leaves their durable state unchanged. Recorded child and collector
absence are both required for automatic orphaning.
