# Events, subscriptions, and callbacks

## Delivery contract

The local journal and every external destination are at-least-once. There is no
exactly-once claim. Consumers deduplicate on the full `dedupe_key`; event word
IDs are stable local handles but may differ when the same authority event is
independently imported on another host.

`agentctl` guarantees within one origin journal:

- append before attempted delivery;
- a contiguous journal sequence per execution;
- replay from an explicit cursor while retained;
- no new sequence allocation for a known semantic dedupe key;
- bounded, visible delivery retry and dead-letter state;
- at most one accepted terminal event per execution;
- explicit promotion and supersession instead of silent retargeting.

It does not claim source ordering when a backend does not provide it, and never
sorts semantic events by wall-clock timestamps.

## Event shape

```json
{
  "schema_version": 1,
  "id": "event-silver-otter-canyon-lantern-drift-velvet",
  "origin_host_id": "host-amber-willow-orbit-tiger-harbor-gentle",
  "execution_id": "exec-purple-monkey-dragon-river-candle-meadow",
  "sequence": 42,
  "ordering": "source",
  "source_position": {
    "kind": "native_sequence",
    "value": "875"
  },
  "kind": "terminal",
  "state": "completed",
  "source_state": "turn.completed",
  "authority": "native",
  "adapter": "codex",
  "occurred_at": "2026-08-10T18:05:00Z",
  "observed_at": "2026-08-10T18:05:01Z",
  "dedupe_key": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "dedupe_version": 1,
  "payload": {
    "result_available": true,
    "outcome_execution_id": "exec-purple-monkey-dragon-river-candle-meadow",
    "availability": "stored",
    "content_available": true,
    "content_sha256": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
  }
}
```

`occurred_at` is nullable when the authority did not supply it. `observed_at`
is always the local journal time. Neither participates in journal ordering.
Payloads are bounded, kind-specific metadata and references, not native output.
Terminal callbacks never contain outcome content or failure messages; callers
dereference the typed execution with `agentctl result`.

## Ordering

For each execution, the journal assigns `sequence` starting at 1 in the same
transaction that appends the event. Re-observation of an existing dedupe key
returns its prior event ID and sequence. Gaps therefore indicate corruption or
retention, not routine concurrency.

`ordering` reports what can be asserted:

- `source`: the backend supplies a monotonic position scoped to the exact
  source execution;
- `observation`: the journal knows only first-observed order;
- `reconciled`: an adapter deterministically merged more than one authoritative
  source, such as exact Multica run state plus issue workflow state.

`source_position` is an opaque string plus a namespaced kind. It is never parsed
as an integer by core code. Adapters reject backward positions from one source
generation unless they explicitly detect a source reset and emit a health
event. Events from different executions are only partially ordered; consumers
must not compare their sequence numbers.

## Semantic deduplication

Each adapter owns a versioned, documented semantic projection. The key is:

```text
sha256("agentctl-event-v1\0" || adapter_name || 0x00 ||
       dedupe_version_u32_be || canonical_projection_json)
```

The projection uses agentctl canonical JSON v1 (sorted object keys, compact
UTF-8 strings and arrays, deterministic Go shortest-form finite numbers) and includes the authority scope,
full source fingerprint, event kind, and the strongest stable source event ID
or revision available. It excludes observation time, retry count, delivery
destination, and local word IDs.

When polling provides only snapshots, the projection describes a transition,
not a poll: prior normalized state/revision plus new normalized state/revision
and any authority-supplied generation. Unchanged snapshots emit no event.
Repeated occurrences such as two separate approval requests require a stable
source discriminator; without one the adapter exposes the latest snapshot but
must not invent distinct semantic events.

A full-hash collision with unequal projections is quarantined as journal
corruption. It is never resolved by overwriting. The word-ID collision rules
are separately defined in [Identifiers](identifiers.md).

## Event kinds and terminal conflicts

| Kind | Purpose |
| --- | --- |
| `started` | Execution accepted or started |
| `progress` | Bounded coarse progress, optional |
| `attention` | Parent/coordinator decision or intervention needed |
| `artifact` | New or changed artifact reference |
| `health` | Degradation, recovery, stale observation, or integrity conflict |
| `terminal` | Completed, failed, cancelled, or orphaned |
| `promoted` | Direct execution linked to a new durable execution |
| `superseded` | A replacement execution became the continuation target |

Before journaling `terminal`, an adapter re-fetches the strongest authoritative
state its negotiated capability allows. A later contradictory terminal claim
does not rewrite the terminal event or emit a second one. It emits a
`health` event with reason `terminal_conflict`, marks observation integrity
`conflicted`, and makes `await`/`result` return `unknown_state` until explicit
reconciliation. This preserves auditability without pretending the first claim
is still trustworthy.

Raw token deltas, chain-of-thought, tool chatter, every stdout line, prompts,
and transcripts are not normalized events.
Repeated equivalent unstructured stdout/stderr observations are coalesced into
one edge-triggered health event. `events --kind terminal` and repeated `--kind`
filters provide bounded diagnostic projections without turning native streams
into transcript storage.

## Cursors and replay

A cursor word ID names a local immutable checkpoint containing:

- origin journal and stream scope;
- last delivered execution sequence or journal position;
- hash of the event at that position;
- query/filter digest;
- creation and expiry times.

The cursor value is not the checkpoint itself and cannot be fabricated by a
client. A cursor is valid only for the same origin and exact filter digest.
The local v0.1 CLI resumes with `--after-sequence <n>` on one exact execution.
The Multica authority API separately returns filter-bound opaque cursors and
rejects changed filters, ahead-of-stream positions, and expired retained
prefixes. A universal cross-authority cursor is a future extension.

Retention that removes the checkpoint returns `cursor_expired` and the earliest
available cursor; it never silently resumes at the current tail. Pagination
returns events plus `next_cursor` in one read transaction. A subscriber advances
its delivery cursor only after destination acknowledgement.

## Subscriptions

A subscription binds an exact execution ID, event kinds, destination, expiry,
retry policy, optional coordinator execution, and a delivery cursor. The
current CLI intentionally does not create broad authority queries; reserved
query/scope filters fail closed in local fan-out. `agentctl subscribe create` listens for `terminal`,
`attention`, and `artifact` by default: these are the minimum events needed to
drive a parent agent without subscribing it to progress chatter. `--kind all`
is an explicit broad subscription, while `--kind <names>` narrows the set.
`--authority direct` is normalized to the stored native authority; display
labels are insufficient authority identifiers.

Task subscriptions expire after acknowledged terminal delivery by default.
Direct executions use the same acknowledgement idea: `result` and a terminal
`await` stamp collection so `recent --unreconciled` can name work whose result
was never dereferenced. Broad subscriptions require an explicit scope and retention estimate in plan
output. Creating, rotating, acknowledging, pausing, and cancelling a
subscription are idempotent mutations.

Promotion does not mutate a subscription target in place. Rotation creates a
new subscription for the promoted execution, links `replaces`/`replaced_by`,
and stops the old subscription only after the new one is durable. Until then,
duplicate delivery across both subscriptions is expected and converges by
dedupe key.

## Callback destinations

Persistent v0.1 destinations are:

```text
file            Owner-only NDJSON append
unix            Local Unix socket
webhook         Signed HTTP request with retry/outbox
command         Explicit argv receiving an owner-only event file path
```

Harness-specific callbacks are plugins over these primitives. The core journal
contains credential references, never platform credentials. `command` does no
shell interpolation, passes the event path as one argv element, uses a minimal
documented environment, and rejects executable paths that change between plan
and invocation.

## Webhook protocol and security

Every request carries a bounded callback envelope, not a raw adapter payload:

```json
{
  "schema_version": 1,
  "delivery_id": "delivery-gentle-comet-maple-badger-valley-sparrow",
  "subscription_id": "sub-quiet-forest-copper-raven-signal-harbor",
  "event_id": "event-silver-otter-canyon-lantern-drift-velvet",
  "event_dedupe_key": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "attempt": 3,
  "sent_at": "2026-08-10T18:05:03Z",
  "expires_at": "2026-08-10T18:10:03Z",
  "nonce": "base64url-random-value",
  "event": {}
}
```

The receiver verifies transport/Tailnet identity when available, an explicitly
configured signing profile, destination and subscription scope, body digest,
freshness, and replay state. Signing keys are referenced from owner-only local
configuration or an OS credential store and never appear in argv, URLs, plans,
or journals. Redirects are disabled. Destination planning rejects loopback,
link-local, metadata-service, and non-allowlisted address changes unless the
operator explicitly selected that local target.

The signature covers the exact request method, normalized target path, content
type, body digest, sent/expiry times, nonce, subscription ID, and delivery ID.
Version 1 uses HMAC-SHA256 over the documented canonical request fields.
Tailnet reachability alone is not accepted as message authentication.

An acknowledgement is a bounded JSON object containing the delivery ID, event
dedupe key, receiver ID, and acknowledgement time. A matching 2xx response or
documented duplicate response acknowledges delivery. A generic 2xx body does
not. `401`/`403` pauses delivery pending configuration repair; `404`/`410`
dead-letters the destination; `408`/`429`/5xx and connection failure retry with
bounded exponential backoff and jitter. Other 4xx responses dead-letter with
the response body discarded or redacted.

Receivers persist replay state at least through the sender's maximum retry and
clock-skew window. Recovery replays unacknowledged outbox entries; it never
generates a new event or delivery ID merely because the sender restarted.
Before transport I/O, the sender durably records the current attempt envelope.
A crash while that attempt is in flight resends the exact nonce/body and a
receiver that already acknowledged it returns the cached acknowledgement. A
scheduled later attempt keeps the delivery/event identity but receives a fresh
attempt number, nonce, and freshness window. Reusing a nonce with different
bytes is always rejected.

## Await and daemonless limits

`agentctl await` emits exactly one terminal, attention, timeout, or integrity
document and follows the exit contract in
[Agent-first CLI ergonomics](agent-ergonomics.md). Callback receipt proves only
delivery, never task success. Await stops on actionable `attention` by default
and returns `attention_required` with the execution's next action. A caller
that intentionally wants to continue waiting uses `--ignore-attention`; a
different bounded wait uses `--timeout <duration>`.

Without a managed supervisor, retry and polling continue only while the
foreground `run`, `await`, or an explicitly backgrounded process remains alive.
Later CLI invocations can recover durable local journal/outbox entries, but no
component wakes itself after logout or reboot. Cross-restart automatic delivery
therefore requires the optional supervisor; the daemonless MVP must not claim
otherwise.

## Optional Multica integration

The companion Multica fork exposes a server-side durable workspace event outbox
with monotonic source positions and filter-bound cursors. The adapter uses its
bounded `event list|watch` CLI. WebSocket delivery may reduce latency but is not
treated as the durable record. Issue/run mutation and review remain native
Multica operations rather than adapter-invented commands.
