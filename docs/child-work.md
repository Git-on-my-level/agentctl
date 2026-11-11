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
`waiter_status: unknown`. On macOS and Linux, an active `agentctl await` or
structured `delegate/continue --wait` excludes its exact child while that
collector holds a kernel waiter lock. This includes an await of a local remote
forwarding ID. Ordinary native owner/runner heartbeats do **not** count: a
detached owner is running the work, not collecting it for a caller.

Each collector publishes a unique, exclusively locked file under
`<journal>.waiters/<execution-id>/`. Registration has a bounded 50 ms lock allowance; unlocked files do not prove
coverage. The collector holds the descriptor through waiting and delivery, and closes/unlinks it
on every normal exit (including timeout, attention, cancellation and errors).
Process death releases the kernel lock, so stale files are not evidence.
Read-only queries use nonblocking shared probes; concurrent queries cannot
mistake one another for waiters. They create no files and never take the
journal write lock. Terminal uncollected children remain actionable even while
a collector is delivering their outcome: only successful acknowledgement clears
them. The waiter is evidence of a live agentctl collector, not a guarantee that
its stdout destination or an outer harness will consume the answer. Arbitrary
shell waits, older binaries and platforms without this lock implementation
remain unknown. `await-any` also remains unknown: its first match releases the
fan-in waiter, which cannot guarantee collection of the other selected children.
A waiter on the parent itself does not cover its children.

Registration is best effort and attempted once. Sidecar creation/locking failure
leaves coverage unknown while original awaiting and foreground native execution
continue; this coordination metadata never becomes an admission prerequisite.

Active local executions with a supported, unexpired durable subscription
covering the relevant event are also excluded. Attention always remains actionable, including with an attention subscription:
late subscriptions do not backfill the already-emitted attention event.
Conflicted evidence is not covered by a live await. This does not prove supervisor health or
callback consumption. Local subscriptions do not exclude remote forwarding rows:
local terminal event fanout does not observe remote completion. A live local
`await` of that forwarding row does count because it observes the remote
execution through the established forwarding contract. No SSH or Multica API calls occur. A remote launch whose
receipt was lost before local forwarding admission remains subject to the
existing identical-key recovery contract; it is not discoverable here until
that local row exists.

## Bounds and failure behavior

Admission atomically writes parent and provider/session indexes into the existing
execution-owned idempotency graph. Attribution metadata does not create a
request-key replay promise: an acknowledged, unkeyed caller-attributed execution
without other references remains eligible for automatic cleanup. Discovery seeks
only the selected caller's
index and reads the referenced projections and acknowledgements. It never walks
unrelated executions, event history, results, or all acknowledgement history.
There is no new background index migration and old binaries need no schema
upgrade to coexist with these records. Retention hydrates the immutable
attribution snapshot before evaluating graph protection, so older supervisor
projection rewrites cannot sever a live child from its retained parent. Corrupt
or conflicting snapshots fail cleanup closed without rewriting projections.

The query has a 1.5-second total processing budget. Every journal open is
read-only with a 50 ms lock allowance; handles close between pages of at most
128 index records or 256 KiB. Additional bounds are 100,000 index entries,
64 MiB index bytes, 1 MiB per execution projection, 200 actionable children,
1,024 subscription records / 4 MiB subscription metadata, and 128 waiter entries
per running child. Unlocked crash files can be removed by an operator after
confirming no collector owns them; normal collector exits remove their file. It never takes
the write lock or triggers updates, bootstrap, ANX flushing, or other maintenance.
Normal OS scheduling and output delivery are outside the processing budget.

Contention, deadline, a bound, or membership mutation between pages returns
exit 9, `ok: false`, diagnostic
`children_query_incomplete`, `complete: false`, and `retryable: true`; no partial
success is emitted. Other journal/identity errors also return nonzero structured
errors. A consumer must not interpret any error as an empty child set.
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

## Native exit safeguard

When a native owner records a terminal result, it checks its attributed children
using the same bounded query and its already-held terminal journal handle. No
second writer or nested journal open is taken. The execution and terminal
outcome atomically retain `uncollected_children`, including reasons and exact
`collect_argv`, alongside the terminal event. Standard structured run, delegate,
status and result output warns with `uncollected_children`. Raw `--content`
stays raw text; inspect structured output or inbox for coordination metadata.
Childless runs have no extra result fields, warnings or continuation.

A separate `child_exit_recheck: true` records a historical obligation when the
bounded indexed check finds any attributed, unacknowledged children, including
children currently covered by an exact await or supported subscription. An
incomplete check also retains this obligation. This is **not** a claim that
those children are presently uncollected: covered-only exits have no
`uncollected_children`, warning, or automatic continuation. Childless and
fully acknowledged child sets do not create the marker.

The marker is committed atomically with the terminal execution/outcome and has
its own execution-owned immutable binding. New readers hydrate it after an older
supervisor drops the projection field; old hashed attribution shapes are not
extended. It is not a request-key replay promise or an additional retention
reference. Existing parent-graph retention remains in force, and cleanup removes
the marker with its execution; corrupt bindings fail reads and cleanup closed.
The field remains historical even after collection, so read it with live child
metadata rather than as an outstanding-work count.

`child_exit_guard` on the execution records `checked_at`, `auto_continue`, an
optional bounded diagnostic, and the admitted `continuation_id`. Outcomes retain
the exit-time snapshot; the execution carries later continuation status.
An incomplete check records `check_error: children_query_incomplete` and warns;
it never treats missing coverage as zero children or launches from a partial
list. A process killed before terminal commit cannot run this owner-exit check.

An eligible completed parent with a frozen delegated native recipe, recorded session and
verified continuation route receives **one** automatic native follow-up prompt.
It names every outstanding child and its exact scoped await command. The new
execution uses normal `continue` keyed replay, latest-turn admission and frozen
permissions/environment; it supersedes the original after completing. There is
no fresh-session fallback, provider switch, or raw prompt persistence.

The owner durably changes `pending` to `attempted` before admission. It never
retries this automatic attempt after an error or owner crash. The successor's
immutable `auto_continuation_of` prevents recursive automatic continuations,
including when that successor leaves new children behind. Verified failed turns can also resume when the existing adapter supports
failed-turn checkpoint recovery (Codex, Claude Code and eligible ZCode recipes).
Other failures remain flagged without an automatic turn. Cancelled or timed-out parents never receive an automatic turn.
An automatic turn runs under the original owner's cancellation/deadline, with
an additional 30-minute maximum. Its own execution can be cancelled normally.
The original parent stays terminal; await/result do not silently redirect to the
automatic successor. Read `child_exit_guard.continuation_id` to collect that turn.

| Harness / authority | Automatic action after an eligible exited turn with outstanding children |
| --- | --- |
| Codex, Cursor, Claude Code, OMP | One native continuation when the recorded recipe and session meet the existing exact-invocation route. Expert `run` without a frozen delegation recipe is flagged only. |
| ZCode | One native continuation for eligible coding recipes; the existing version check still requires CLI 0.17.0. Disposable read-only sessions cannot resume. |
| Devin / Devin cloud ACP | One conditional session-load attempt; normal ACP initialization must advertise `loadSession` and verify the frozen configuration. Missing support fails closed, leaving the flag. Print mode has no continuation route. |
| OpenCode-backed Morph models | One conditional continuation for the verified OpenCode 2.2.6 background-service recipe, preserving #150's version and session-existence checks. Other recipes/versions remain flagged. |
| OpenClaw, generic process | Flag and inbox actions; no verified native continuation route. |
| Hermes | Flag and inbox actions; provider-backed continuation remains unverified. |
| Multica | Multica retains task/run authority. Local native parents can track Multica children; agentctl does not introduce Multica auto-continuation. |

Disable automatic follow-ups per profile by setting
`profiles.<name>.delegation.child_exit_auto_continue` to `false` in live config or
the config bundle. Omitted/true enables the safeguard. The switch suppresses
continuation, **not** exit snapshots, warnings or inbox actions; it does not stop
a follow-up that has already launched. Policy is read when the parent exits.

`inbox` first uses the complete, hydrated execution and explicit acknowledgement
snapshots it already loads under its held read-only journal handle. Writers are
excluded for that handle's lifetime. It selects active parent scopes from every
retained child: nonterminal children remain obligations regardless of waiter
coverage, and terminal children settle only when their explicit collection stamp
exists and its stored key agrees with its execution identity, nonzero timestamp
and supported collection source. The selector and scoped child query share this
validation. Corrupt acknowledgement evidence fails inbox with exit 70,
`diagnostic_code: inbox_acknowledgement_corrupt` and `complete: false`; it never
retires a parent from a falsely clean snapshot. The legacy acknowledgement epoch
does not retire attributed children.
Before skipping a marked parent with no active execution rows, inbox also proves
its retained membership from that parent's attribution index. Every live index
must resolve to the same complete, hydrated snapshot; a missing execution row
requires a matching valid expiry tombstone. Missing rows (even with collection
stamps), malformed expiry evidence and incomplete proofs retain
`children_query_incomplete` with a read-only recheck action. The authoritative
query and this proof share the strict index/expiry decoder. Compact legacy expiry
tombstones remain supported only with both `scope` and `key` absent. A full
expiry must contain both matching the authoritative index scope/key. Partial,
null, empty or mismatched metadata is corrupt, not a compact expiry. Both forms
require the attribution producer's `input_digest` representation (`sha256:` plus
64 lowercase hex digits); absent/null/empty/invalid digests are corrupt. A
tombstone cannot hide a retained execution. Expiry JSON must also be unambiguous:
duplicate members in any object, including case-folded aliases of authoritative
top-level fields, are corrupt rather than last-value-wins evidence. Unique
extension fields remain compatible. Other journal record decoders are unchanged.

These scoped membership proofs share a separate 1.5s processing budget,
100,000 selected scopes/entries, 64 MiB index bytes and 1 MiB value bounds. They
use one stable read transaction on the same held read-only handle, validate scope
certification/generations, and read no subscription, waiter, event or result
history. There is no global index walk, schema change, mutable settlement or
read-side repair. The existing shared 1.5s live child-query budget starts after
this proof phase, so validated collected history spends none of it. The two
bounded phases can add up to 3s after the existing inbox snapshot loads.
A parent whose retained children are all collected or validly expired adds no
child-collection inbox reason. This changes only the read projection;
immutable execution markers, exit guards and outcomes are not rewritten. An
incomplete execution/acknowledgement listing fails the inbox read rather than
retiring obligations from a partial snapshot. There is no new mutable settlement
state, read-side write or additional global child scan.

`inbox` rechecks selected active parents with this marker or an existing exit guard, even when
all children were covered at exit. It includes a parent with unresolved child
work even after that parent's own
result was acknowledged. It rechecks live child metadata (no result reads, SSH,
maintenance or writes) and offers each child's collect command. A shared 1.5s
budget bounds these child rechecks; incomplete coverage stays actionable.
Covered running children disappear from the live inbox projection without
clearing the durable recheck obligation. Waiter cancellation/death or subscription
expiry/cancellation can therefore make them actionable again immediately on the
next inbox query, before the child's observation becomes stale. Terminal children
remain actionable until actual collection acknowledgement. Complete checks with
only covered or acknowledged children add no child-collection reason; incomplete
checks remain explicitly actionable without inventing uncollected children.
The shared query bounds apply even to covered children. Subscription metadata
is loaded lazily once per child query, only for an uncovered local nonterminal
child that can benefit from it. Collected children, terminal uncollected rows,
attention, remote rows and exact-await-covered children do not scan subscriptions.
When needed, the existing subscription count/byte/deadline limits still fail
closed, and the scan uses the same strict identity/key checks and compatibility
filtering as terminal event fanout, including unrelated active subscriptions. Retained historical markers therefore cannot repeatedly scan historical
subscription metadata, spend the shared recheck budget, or create incomplete
coverage items that crowd an active parent off the page. Rechecks use only each
parent's retained attribution index and cached metadata; there is no global child
scan, new remote observation, or inbox-triggered automatic continuation. The
historical exit snapshot and marker remain unchanged after collection.
Remote forwarding parents remain excluded from host-local inbox as before; the
native exit check runs at the actual owner host, and remote child collection uses
the origin parent's local forwarding ID. Lost receipts before local admission
retain the existing replay recovery limit described above.

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
