# Execution envelope and lifecycle

## Purpose

The execution envelope is the smallest common operational record needed to
observe one bounded attempt across a native agent CLI or Multica. It is not a
task, issue, conversation, or scheduling record. Native sessions remain
authoritative in direct mode; the exact Multica issue and run remain
authoritative in Multica mode.

## Core record

```json
{
  "schema_version": 1,
  "id": "exec-purple-monkey-dragon-river-candle-meadow",
  "origin_host_id": "host-amber-willow-orbit-tiger-harbor-gentle",
  "revision": 7,
  "authority": "native",
  "adapter": "codex",
  "mode": "direct",
  "acquisition": "launched",
  "state": "running",
  "liveness": "alive",
  "source_state": "turn.started",
  "source_bindings": [
    {
      "kind": "codex_thread",
      "alias_id": "source-velvet-comet-maple-badger-valley-sparrow",
      "fingerprint": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      "opaque_id": "redacted-in-normal-output"
    }
  ],
  "capabilities": {
    "negotiated_at": "2026-08-10T18:00:00Z",
    "adapter_version": "0.1.0",
    "backend_version": "verified-version-string",
    "items": [
      {"name": "events", "status": "supported", "source": "native_stream"},
      {"name": "resume", "status": "supported", "source": "native_cli"},
      {"name": "cancel", "status": "unavailable", "source": "manifest"}
    ]
  },
  "cwd": null,
  "repository": null,
  "parent_execution_id": null,
  "supersedes": [],
  "superseded_by": null,
  "promotion": null,
  "created_at": "2026-08-10T18:00:00Z",
  "started_at": "2026-08-10T18:00:01Z",
  "updated_at": "2026-08-10T18:00:10Z",
  "terminal_at": null,
  "observation": {
    "source": "native_stream",
    "integrity": "verified",
    "observed_at": "2026-08-10T18:00:10Z",
    "fresh_for_seconds": 15
  }
}
```

`mode` answers who owns the lifecycle (`direct` or `multica`). `acquisition`
answers how this envelope was obtained (`launched`, `attached`, or `promoted`).
They are independent; treating `attach` as a lifecycle mode would make an
attached Multica run contradictory.

`revision` is a host-local monotonically increasing optimistic-concurrency
value. It is not a source-authority revision. Paths and opaque source IDs are
operator-private and omitted or redacted in normal output. Alias IDs and source
fingerprints follow [Identifiers](identifiers.md).

## State and liveness

Normalized state is authority outcome, while liveness is observation evidence:

```text
state:     created starting running waiting attention completed failed cancelled orphaned
liveness: unknown alive blocked exited unreachable
```

Terminal states are `completed`, `failed`, `cancelled`, and `orphaned`.
`orphaned` means the adapter proved that the bound attempt cannot continue and
no authoritative terminal outcome can be recovered; temporary inability to
observe is `liveness: unreachable`, not orphaning.

`attention` means work may continue but an external decision or intervention is
currently required. `waiting` is non-actionable waiting on an external event.
Uncertainty does not overwrite the last known state: it changes liveness and
observation integrity to `unknown`.

## State invariants

- State transitions use compare-and-swap on `revision`; concurrent stale writes
  return `conflict`.
- A terminal state never transitions to nonterminal for the same execution ID.
- An adapter accepts terminal only after the strongest negotiated authoritative
  recheck. Contradictory later evidence follows the terminal-conflict rule in
  [Events](events-and-subscriptions.md), without rewriting history.
- A resumed turn stays in one execution only if the authority, exact source
  binding, and task contract all say it is the same attempt.
- A different native session, Multica run, owner reassignment, or retry attempt
  creates another execution. `supersedes` records directed continuation, while
  promotion records authority transfer; those are distinct relationships.
- Supersession links are acyclic. `superseded_by` is assigned once. A merge may
  supersede several attempts, but one attempt has at most one continuation.
- Observation freshness and liveness are independent of state.
- Completion is not acceptance evidence. The parent/coordinator verifies
  required artifacts and acceptance criteria through their actual authority.

## Terminal outcomes

Every newly terminalized direct execution records one versioned outcome in the
same journal transaction as terminal state, the terminal event, and callback
fan-out. `agentctl status` and events remain metadata-only; the explicit
`agentctl result <execution-id>` read returns the bounded final content and
normalized failure details. Stored content or a structured failure is required
by default so a successful retrieval cannot be mistaken for an empty outcome;
`--allow-empty` is the explicit metadata-only escape for omission tombstones or
legacy records. Its `result_ref` is the execution's portable
`agentctl://host-.../exec-...` URI, so callers never locate native session or
rollout files.

Availability is explicit: `stored`, `omitted_by_policy`,
`unavailable_at_source`, or `legacy_not_recorded`. `--no-store-result` writes
an omission tombstone. Final text is UTF-8, at most 1 MiB inline, and includes
a digest only when complete. The outcome never contains prompts, reasoning,
tool chatter, token streams, or arbitrary native stdout/stderr.

Stored text content may include a bounded `source` label such as
`terminal_result` or `assistant_message_fallback`. `agentctl result` can assert
that provenance and a caller-selected minimum byte count. Failed outcomes may
carry one bounded, secret-redacted native diagnostic when structured failure
data is absent; terminal events expose only its normalized failure code.

## Source bindings and authority

`source_bindings` retain exact local adapter bindings. A direct execution
normally has one native session binding. A Multica execution may have project,
issue, run, and native-worker bindings, but run state is primary liveness and
issue state is secondary workflow.

Bindings have a full fingerprint for reconciliation, a user-facing word alias,
and an optional opaque ID. Core code never dispatches by display label. Removing
the local journal may lose host-local aliases but does not delete or mutate the
native session or Multica records.

## Negotiated capabilities

Capabilities are an immutable snapshot for this envelope revision, not a guess
from adapter name. Each item records `supported`, `degraded`, or `unavailable`,
the evidence source, and optional constraints. A later probe writes a new
execution revision. Critical mutations re-negotiate if the snapshot is stale;
read-only commands may return the stale snapshot with freshness.

The manifest/probe contract is defined in [Adapters](adapters.md). Unknown
capability names are preserved by readers. A caller requesting a capability
with unknown semantics must fail closed rather than assume support.

## Task contract metadata

The envelope may contain a bounded summary or reference, not the full prompt:

```json
{
  "objective_summary": "Review the bounded design contract",
  "side_effect_boundary": "read_only",
  "acceptance_ref": "context-gentle-comet-maple-badger-valley-sparrow",
  "expected_artifact_kinds": ["report"],
  "continuation": {"same_session_required": true}
}
```

For Multica, the issue is authoritative for this contract. For direct work, the
launching parent supplies it. The summary must be bounded and redacted; prompts,
reasoning, transcripts, and native session databases are excluded.

## Parent, child, and supersession

Parent/child links express observation and callback relationships, not
automatic scheduling. A parent remains responsible for synthesis unless an
external authority such as Multica explicitly owns orchestration. Children get
a bounded objective, execution/context handles, callback destination, and
artifact expectations; they do not inherit credentials or raw parent history.

Supersession changes the recommended continuation target but never retargets an
old ID or cursor. Reads of a superseded execution return the record plus an
exact `next_action` for its replacement. Mutations against it fail with
`invalid_state` unless the command explicitly permits historical cleanup.

## Promotion as a recoverable saga

Promotion creates a new Multica execution envelope and links it to the direct
source; it does not change the direct envelope's authority or move its native
session. The promotion key derives from the source execution's origin, source
fingerprint, destination authority scope, and caller idempotency key. It excludes
timestamps and mutable summaries.

The recoverable states are:

```text
planned -> authority_created -> bindings_recorded -> active
                                      \-> recovery_required
```

The adapter must persist or query the exact promotion key in Multica-owned
metadata before it may advertise `promote: supported`. After a crash, recovery
queries that key and links the existing issue/run; it never creates a second
issue because the local receipt is absent. If Multica cannot support exact key
lookup, promotion fails with `capability_unavailable` rather than claiming
cross-restart idempotency.

The handoff contains only verified findings, bounded evidence references,
remaining acceptance criteria, source aliases, and SHA-256 digests for the
portable skill, selected context, and handoff content. The description is sent
on stdin; local file paths, prompts, transcripts, and credentials are excluded.
The same provenance is persisted in the destination task contract and promotion
event. Partial failure leaves the created Multica issue authoritative and
reports recovery actions. It is never rolled back by deleting authority-owned
work.

Promotion and continuation are separate choices. When requested, supersession
links the new execution and subscription rotation follows the two-phase rule in
[Events](events-and-subscriptions.md). Otherwise both executions remain active
and independently observable.

## Retention

Automatic retention is not implemented in the preview. Result bodies are
stored by default up to 1 MiB per execution and remain in the owner-only local
journal until an explicit digest-bound `agentctl data cleanup` operation.
Artifact content is not copied by default.

Alias bindings, source fingerprints, promotion receipts, and supersession links
must outlive every reference to them. Cleanup plans exact execution graphs,
record categories, logical bytes, and protected references, then rejects apply
if the reviewed plan digest changes. Promotion-linked executions remain
protected in the preview. Retention remains an operator decision, not a
compatibility guarantee.
