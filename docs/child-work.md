# Child work attribution and read-only discovery

Launch admission for `run`, `delegate`, `continue`, and `dispatch` records the
launching caller separately from the execution's own native session:

- `parent_execution_id` reuses the execution envelope's existing field. It is
  populated only from journal-observed managed execution evidence, using the
  supervisor's existing `AGENTCTL_EXECUTION_ID` environment. Conflicting inherited
  native markers leave attribution unknown, following `identity` rules.
- Optional `caller_identity` contains `provider`, `native_session_id` (the
  provider-scoped hash returned by `identity`), and `provenance`. A session hash
  correlates a conversation; it is neither authentication nor a resume handle.
  The existing `caller.harness` declaration remains independent.

Native and Multica admission capture managed caller evidence while holding their
existing journal admission lock, after checking keyed replay. A detached worker
uses its reserved attribution without re-resolving its parent's identity.
Remote admission requires available managed evidence before transport; contention
returns a retryable `caller_identity_unavailable` instead of launching untracked
work. Standalone `identity` retains its best-effort observation behavior. A genuinely
absent execution (for example inherited context pointing into a different
journal) remains unknown, distinct from a failed read of available evidence.

These values are immutable admission metadata, excluded from semantic request
keys. Replaying a request retains its original attribution. Detached workers use
that original admission. Immutable snapshots in the execution-owned idempotency
graph restore metadata dropped by an older supervisor. Cleanup removes snapshots
and caller index references with their execution. No historical attribution is
inferred or backfilled.

## Unmanaged caller declaration

Unmanaged Codex callers can use the existing unambiguous `CODEX_THREAD_ID` hint.
Claude Code and other harness integrations must explicitly provide their current
native session rather than infer one from an inherited parent, cwd, or harness.
For an invocation from an interactive Claude Code session:

```sh
AGENTCTL_CALLER_PROVIDER=claude \
AGENTCTL_CALLER_NATIVE_SESSION_ID="$session_id" \
agentctl delegate --request-file request.json --prompt-file task.md --detach
```

The provider and native session form one explicit assertion, with the same
validation and hashing as `identity --provider ... --native-session-id ...`.
Both environment entries are stripped before native child launch, along with
`AGENTCTL_CALLER_HARNESS`; the child receives its own managed execution context.
The raw session is never journaled. Do not persist the declaration globally or
reuse it in another conversation. An explicit declaration takes precedence over
ambient identity; it does not borrow a managed parent's execution ID.
`agentctl identity --json` reports the same explicit assertion when these
variables are present. Unknown attribution stays absent.

## Query contract

```sh
agentctl children
agentctl children --execution "$parent_execution_id"
agentctl children --provider claude --native-session-id "$session_id"
agentctl children --provider claude --native-session-id "$session_id" --summary
```

The default uses the same identity resolver as `identity`. Journal-observed
execution identity selects that exact parent; otherwise a known provider/session
selects that conversation. Explicit execution and provider/session scopes are
mutually exclusive. Unknown identity fails with `children_identity_unknown`
(exit 8). An explicit native session is hashed using the identity v1 domain;
it is never echoed raw. Explicit execution lookup requires a retained journal
record, independent of ambient conflicts.

Success follows [children-result](../schemas/children-result.schema.json) and
includes `complete: true`, counts, scope, and at most 200 `executions`. Each item
contains a typed `id`, `created_at`, `state`, authority, adapter, `reason`,
`waiter_status`, and `collect_argv`. Remote items include the frozen remote
provenance, but collection commands always use the local forwarding ID and
retain explicit journal/config/profile options. `--summary` omits items while
preserving the full counts.

Refused continuation reservations (`admission_refused`) are excluded because
no native work launched. Ordinary canceled executions remain eligible.
Terminal executions without a collection acknowledgement have reason
`terminal_uncollected`, regardless of terminal state or a subscription. A query
never acknowledges collection or reads a result. These newly attributed rows
do not depend on the legacy acknowledgement epoch.

Nonterminal children have reason `running_without_known_waiter` and
`waiter_status: unknown` when no supported waiter evidence exists. Active native
and remote forwarding executions are excluded when an exact-ID `await` holds
its local process lock. Supported, unexpired terminal subscriptions additionally
cover local executions; they never prove a remote completion wake. A child in
`attention` always remains actionable, even with an attention subscription or a
live await (including `--ignore-attention`). Subscription creation only backfills
terminal events, so a matching attention filter may have missed the event that
blocked this child.
Terminal uncollected children are never suppressed by waiters.

On Linux and macOS, `await <id>` holds an exclusive lock in its unique private
`<journal>.waiters/<execution-id>/await-*` sidecar. Multiple awaits can coexist;
exit, timeout, cancellation, and process death release their locks. Registration
is best effort and attempted once: sidecar creation/locking failure leaves
coverage unknown while the original await polling and cancellation continue.
Normal exit removes its file; a crash can leave an empty file. Queries use
shared probes that cannot mistake concurrent queries for waiters, open only
selected files read-only, never create a waiter directory or file, and ignore
unlocked crash residue. More than 128 sidecars for one child fails closed.
Sidecar errors fail the query; unsupported
platforms, older await binaries, and `await-any` remain unknown. Evidence is live
per page: a waiter can exit immediately after observation. The query cannot
prove a supervisor is healthy or that a callback destination consumes a result.
Remote forwarding rows use the same exact-ID waiter locks, released when the
local remote collector exits or is cancelled. Their cached state can still be
stale; local subscriptions do not prove collection because local terminal event
fanout does not observe remote completion. No SSH or Multica API calls occur. A remote launch whose
receipt was lost before local forwarding admission remains subject to the
existing identical-key recovery contract; it is not discoverable here until
that local row exists.

## Bounds and failure behavior

Admission atomically writes parent and provider/session indexes into the existing
execution-owned idempotency graph. Attribution metadata does not confer request-key
replay retention: an acknowledged, unkeyed caller-attributed execution without
other references remains eligible for automatic cleanup. Discovery seeks only
the selected caller's index and reads the referenced projections and acknowledgements. It never walks
unrelated executions, event history, results, or all acknowledgement history.
There is no new background index migration and old binaries need no schema
upgrade to coexist with these records. Retention hydrates the immutable
attribution snapshot before evaluating graph protection, so older supervisor
projection rewrites cannot sever a live child from its retained parent. Corrupt
or conflicting snapshots fail cleanup closed without rewriting projections.

The query has a 1.5-second total processing budget. Every journal open is
read-only with a 50 ms lock allowance; handles close between pages of at most
128 index records or 256 KiB. Additional bounds are 100,000 index entries,
64 MiB index bytes, 1 MiB per execution projection or selected acknowledgement,
200 actionable children,
and 1,024 subscription records / 4 MiB subscription metadata. It never takes
the write lock or triggers updates, bootstrap, ANX flushing, or other maintenance.
Normal OS scheduling and output delivery are outside the processing budget.

Contention, deadline, a bound, or membership mutation between pages returns
exit 9, `ok: false`, diagnostic
`children_query_incomplete`, `complete: false`, and `retryable: true`; no partial
success is emitted. Other journal/identity errors also return nonzero structured
errors. A consumer must not interpret any error as an empty child set.
Selected acknowledgements must decode to the matching execution ID, a nonzero
collection timestamp and an allowed collection source. Legacy expired indexes
are skipped only for a matching typed ID whose execution is absent. Invalid
records, retained targets behind tombstones, and missing required index or
execution buckets fail with journal corruption; read-only queries never repair
or migrate them. Optional historical metadata still requires no upgrade write.
Membership is stable from the first index page through the final page check.
Each page reads the selected scope's transaction stamp, an empty-scope pruning
fence, and a mixed-version writer fence. Admission and index removal update
stamps atomically with the index. Current journal transactions certify their
transaction sequence: outcome updates and admissions in other scopes do not
invalidate membership. An uncertified transaction from an older writer fails
closed, even if a current writer commits afterward. Older outcome-only writes
also cause this conservative failure; empty-scope stamp pruning can invalidate
unrelated queries. A stable retry works with or without the optional metadata,
without a write or backfill. Empty-scope stamps are reclaimed by current cleanup;
legacy cleanup can leave a small stamp until that scope is next mutated.

Coverage is `retained_attributed_executions_only`, with live projections,
acknowledgements and waiter evidence per page, not a frozen lifecycle snapshot.
A new admission after the final checked page is outside that observation.
Launches from binaries predating attribution and pre-upgrade launches are
outside coverage. Hook integrations should retain
existing explicit-label coverage for that history during rollout.

## Scope of this change

This is the first stage of the parent-exit safeguard, providing attribution and
a fast query for coordkit's Stop hook. It does not alter native exit behavior,
automatically continue a parent, or add inbox warnings. The automatic single
continuation, non-resumable harness warning path,
and default-on kill switch remain follow-up work. Native CLIs and Multica retain
execution authority; dispatch only attributes its existing local execution row.
Structured Multica delegate remains unavailable under its existing contract.

## Coverage and retention boundaries

`complete: true` means the selected retained-attributed index was completely
read with stable membership within the safety bounds. It is not proof that every attempted launch has
a local execution row. In particular, a lost SSH receipt remains uncertain:
retry the identical request key to recover remote admission before relying on
its local forwarding row. Never convert that transport error plus an empty
retained index into a claim that no remote work launched. Adding a durable local
pre-receipt intent requires a separate pending/uncertain-admission contract;
this query does not invent remote IDs or own remote execution lifecycle.

Parent links retain the existing execution-graph deletion guard, even after all
linked rows are terminal and acknowledged. This protects lineage, replay and
external references; it is not a new lifecycle-aware graph collection policy.
Changing that policy requires a separate design and deletion safety review.
See the [round2 review disposition](evidence/2026-11-11-pr147-round2.md) for
published contract and base-branch code evidence.
