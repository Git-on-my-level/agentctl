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
`waiter_status: unknown`. On macOS and Linux, an active `agentctl await`, or a
terminal `await-any` (without `--since`), or structured `delegate/continue --wait`, excludes its exact child while that
collector holds a kernel waiter lock. This includes an await of a local remote
forwarding ID. Ordinary native owner/runner heartbeats do **not** count: a
detached owner is running the work, not collecting it for a caller.

Each collector publishes a unique, exclusively locked file under
`<journal>.waiters/<execution-id>/`. It acquires the lock before publishing the
file, holds the descriptor through waiting and delivery, and closes/unlinks it
on every normal exit (including timeout, attention, cancellation and errors).
Process death releases the kernel lock, so stale files are not evidence.
Read-only queries use nonblocking shared probes; concurrent queries cannot
mistake one another for waiters. They create no files and never take the
journal write lock. Terminal uncollected children remain actionable even while
a collector is delivering their outcome: only successful acknowledgement clears
them. The waiter is evidence of a live agentctl collector, not a guarantee that
its stdout destination or an outer harness will consume the answer. Arbitrary
shell waits, older binaries and platforms without this lock implementation
remain unknown. `await-any` with a timestamp exclusion or without terminal
watching also remains unknown; it cannot prove a wake for every child terminal.

Active local executions with a supported, unexpired durable terminal
subscription are also excluded. This does not prove supervisor health or
callback consumption. Local subscriptions do not exclude remote forwarding rows:
local terminal event fanout does not observe remote completion. A live local
`await` of that forwarding row does count because it observes the remote
execution through the established forwarding contract. No SSH or Multica API calls occur. A remote launch whose
receipt was lost before local forwarding admission remains subject to the
existing identical-key recovery contract; it is not discoverable here until
that local row exists.

## Bounds and failure behavior

Admission atomically writes parent and provider/session indexes into the existing
execution-owned idempotency graph. Discovery seeks only the selected caller's
index and reads the referenced projections and acknowledgements. It never walks
unrelated executions, event history, results, or all acknowledgement history.
There is no new background index migration and old binaries need no schema
upgrade to coexist with these records.

The query has a 1.5-second total processing budget. Every journal open is
read-only with a 50 ms lock allowance; handles close between pages of at most
128 index records or 256 KiB. Additional bounds are 100,000 index entries,
64 MiB index bytes, 1 MiB per execution projection, 200 actionable children,
1,024 subscription records / 4 MiB subscription metadata, and 128 waiter entries
per running child. Unlocked crash files can be removed by an operator after
confirming no collector owns them; normal collector exits remove their file. It never takes
the write lock or triggers updates, bootstrap, ANX flushing, or other maintenance.
Normal OS scheduling and output delivery are outside the processing budget.

Contention, deadline, or a bound returns exit 9, `ok: false`, diagnostic
`children_query_incomplete`, `complete: false`, and `retryable: true`; no partial
success is emitted. Other journal/identity errors also return nonzero structured
errors. A consumer must not interpret any error as an empty child set.
Coverage is `retained_attributed_executions_only`, with live state per page,
not a transactionally frozen snapshot. New launches from an older binary and
pre-upgrade launches are outside coverage. Hook integrations should retain
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
| OpenCode-backed Morph models, OpenClaw, generic process | Flag and inbox actions; no verified native continuation route. |
| Hermes | Flag and inbox actions; provider-backed continuation remains unverified. |
| Multica | Multica retains task/run authority. Local native parents can track Multica children; agentctl does not introduce Multica auto-continuation. |

Disable automatic follow-ups per profile by setting
`profiles.<name>.delegation.child_exit_auto_continue` to `false` in live config or
the config bundle. Omitted/true enables the safeguard. The switch suppresses
continuation, **not** exit snapshots, warnings or inbox actions; it does not stop
a follow-up that has already launched. Policy is read when the parent exits.

`inbox` includes a parent with unresolved child work even after that parent's own
result was acknowledged. It rechecks live child metadata (no result reads, SSH,
maintenance or writes) and offers each child's collect command. A shared 1.5s
budget bounds these child rechecks; incomplete coverage stays actionable.
Collected children and running children with new waiter evidence disappear from
the live inbox projection; the stored historical exit snapshot is retained.
Remote forwarding parents remain excluded from host-local inbox as before; the
native exit check runs at the actual owner host, and remote child collection uses
the origin parent's local forwarding ID. Lost receipts before local admission
retain the existing replay recovery limit described above.
