# Events, subscriptions, and callbacks

## Delivery contract

Events are delivered at least once. Every consumer must deduplicate using the
stable event ID. `agentctl` never promises exactly-once external delivery.

The system does promise:

- deterministic semantic event IDs;
- atomic local journal append before delivery;
- ordered delivery per execution when the backend supplies order;
- bounded retries with visible backoff;
- delivery receipts;
- dead-letter retention;
- one terminal semantic event per execution;
- explicit supersession when observation moves to a new execution.

## Event shape

```json
{
  "schema_version": 1,
  "id": "event-silver-otter-canyon-lantern-drift",
  "execution_id": "exec-purple-monkey-dragon-river-candle",
  "sequence": 42,
  "kind": "terminal",
  "state": "completed",
  "source_state": "turn.completed",
  "authority": "native",
  "adapter": "codex",
  "occurred_at": "2026-08-10T18:05:00Z",
  "observed_at": "2026-08-10T18:05:01Z",
  "dedupe_key": "sha256:...",
  "payload": {
    "result_available": true,
    "artifact_ids": []
  }
}
```

Semantic dedupe keys exclude observation time and delivery-attempt data.

## Event kinds

| Kind | Purpose |
| --- | --- |
| `started` | Execution accepted/started |
| `progress` | Bounded coarse progress, optional |
| `attention` | Parent decision or intervention needed |
| `artifact` | New or changed artifact reference |
| `health` | Observation degradation, recovery, stale heartbeat |
| `terminal` | Completed, failed, cancelled, or orphaned |
| `promoted` | Direct execution promoted to durable authority |
| `superseded` | Observation moved to a replacement execution |

Raw token deltas, chain-of-thought, tool chatter, and every stdout line are not
normalized events.

## Subscriptions

A subscription binds:

- one or more execution IDs or a narrow authority query;
- event kinds;
- callback destination;
- expiry/TTL;
- retry policy;
- optional parent execution ID;
- delivery cursor.

Task-specific subscriptions expire immediately after acknowledged terminal
delivery unless explicitly configured otherwise.

Broad subscriptions require an explicit scope and are read-only observers. The
CLI must make their potential event volume visible before creation.

## Callback destinations

Initial destinations:

```text
parent          Foreground/background process completion contract
stdout          NDJSON or one terminal JSON document
file            Atomic append to owner-only NDJSON
unix            Local Unix socket
webhook         Signed HTTP callback with retry/outbox
command         Explicit argv template receiving an event file path
```

Harness-specific callbacks should be small plugins built on these primitives,
not embedded platform credentials in the core journal.

`command` callbacks never interpolate event fields into a shell string. The
event is written to an owner-only temporary file and its path is passed as one
argv value.

## Await semantics

```bash
agentctl await exec-purple-monkey-dragon-river-candle --output json
```

`await`:

- emits exactly one terminal or timeout document;
- exits 0 for observed `completed`;
- exits a stable nonzero code for failed/cancelled/orphaned/timeout/unknown;
- never treats callback receipt as proof of task success;
- can be safely backgrounded by a parent harness that wakes on process exit.

## Polling fallback

Polling is an adapter implementation detail with a public health contract:

- bounded interval with jitter;
- conditional/backoff requests when supported;
- exact native execution reference;
- atomic cursor/state writes;
- no model calls on unchanged state;
- no output on quiet ticks unless requested in debug mode;
- freshness threshold and stale alert;
- deterministic fixture/self-test independent of credentials.

Polling stops after terminal delivery and acknowledgement. Persistent fleet
subscriptions use a managed supervisor and explicit retention policy instead of
per-task cron proliferation.

## Remote delivery

Cross-host callbacks use Tailnet identities and signed requests. The originating
host retains an outbox until the receiver acknowledges the event or the bounded
policy moves it to dead letter.

The callback URL contains no reusable credential. Authentication material stays
in owner-only local configuration. A receiver authorizes the sending host and
subscription, not merely possession of a word ID.

## Multica integration

The preferred long-term Multica adapter consumes a server-side durable event
outbox with sequence IDs. WebSocket events provide low latency but are not, by
themselves, a durable delivery record.

Until that exists, the adapter polls both:

1. exact run state for liveness/terminal outcome;
2. issue state for workflow/attention state.

It emits a terminal event if the exact run terminates even when the issue does
not transition. This prevents board-state lag from swallowing completion.

