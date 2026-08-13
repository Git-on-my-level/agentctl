# Adapter contract

## Principle

Adapters observe and invoke native tools without replacing them. Model routing,
permissions, authentication, session semantics, and backend-specific arguments
remain owned by the native backend.

## Required adapter interface

An adapter declares metadata and implements the capabilities it actually
supports:

```text
probe       installation, version, auth/readiness, supported features
launch      execute an argv array and discover the native session reference
attach      bind to an existing native reference
snapshot    obtain current state with observation freshness
events      stream or poll normalized events from a cursor
result      retrieve the backend's terminal state/result metadata
result_content retrieve bounded final answer content when the adapter proves it
resume      optional; resume the same native session
cancel      optional; request native cancellation
artifacts   optional; enumerate references without copying content
```

Unsupported capabilities return a stable `capability_unavailable` error. They
never silently degrade into a different mutating action.

The interface is transport-neutral. An in-process adapter, subprocess plugin,
Multica runtime adapter, and remote read-only observer must expose the same
manifest/probe shapes; core code does not assume Hermes, a shared home
directory, or access to a harness session database.

## Capability negotiation

The static manifest declares what an adapter implementation knows how to do.
`probe` produces an instance-specific result for the exact executable, backend
version, profile, host, and authority endpoint. Dispatch uses the intersection
of requested semantics, manifest support, successful probe evidence, and caller
policy. It never infers support from adapter name alone.

Registered version-1 capability names are:

```text
launch attach snapshot events result resume cancel artifacts history
context_injection promotion durable_idempotency durable_events remote_callback
```

Each capability reports:

```json
{
  "name": "events",
  "status": "degraded",
  "source": "bounded_poll",
  "semantics_version": 1,
  "constraints": {
    "history": false,
    "ordering": "observation",
    "minimum_poll_seconds": 15
  },
  "reason": "Backend has exact status lookup but no durable event stream"
}
```

`supported` means all versioned semantics are available under the stated
constraints. `degraded` is usable only when the caller explicitly permits the
reported weaker semantics. `unavailable` is not silently converted to another
operation. Unknown capability names are preserved by readers; unknown semantics
fail closed for mutations.

Probe results include adapter/backend versions, executable digest/path
sensitivity, profile/endpoint fingerprint, probe time, freshness, and whether
each probe action was read-only. A change to any of those inputs invalidates the
cached result. Critical dispatch and promotion require a fresh probe; ordinary
status may show a stale snapshot explicitly.

## Capability order

Observation uses the strongest available source in this order:

1. Durable backend event stream/outbox
2. Live structured event stream
3. Structured status API
4. Structured native state files
5. Bounded polling of authoritative CLI/API output
6. Process exit plus result-file inspection

The selected source is reported in every snapshot. Fallback is observable and
may emit a `degraded` health event.

Fallback never upgrades guarantees. Polling cannot advertise durable history,
source ordering, or cross-restart wakeup merely because it reconstructs current
state. The event contract records `observation` or `reconciled` ordering.

## Native pass-through

Launch receives an argv array after `--`; no shell re-parsing is permitted:

```bash
agentctl run -- codex exec --json -m gpt-5.6-sol "task"
```

For known executable names, `run` infers the adapter (`codex`, `cursor-agent`,
`claude`, and `omp`). `--adapter <name>` remains an explicit override when an
executable is ambiguous or the caller has a reviewed authority mapping. The
argv after `--` is passed unchanged in either case.

The adapter may add only the minimum flags required to obtain structured events
when this is safe and documented. It must report added flags in `--explain`
output.

If an existing native flag conflicts with supervision, launch fails with a
structured explanation instead of overriding the caller silently.

Context injection is separately negotiated. An adapter declares one or more
reviewed mechanisms (`environment_path`, `native_argument`,
`native_instruction_file`, `authority_artifact_ref`) and whether the harness
actually guarantees delivery to the worker. If required context cannot be
delivered, launch fails before starting the native command. The generic process
adapter may provide `AGENTCTL_CONTEXT` and `AGENTCTL_EXECUTION` environment
handles but cannot claim the child model read them.

## Initial adapters

### Codex

- Discover `thread_id` from structured events.
- Preserve Codex sandbox, model, effort, and worktree behavior.
- Support attach/resume only when the installed CLI exposes a verified route.
- Treat parent process exit and turn completion separately.

### Cursor

- Consume `stream-json`.
- Use `result.is_error`, not process exit code alone.
- Prefer terminal `result` content, but retain the last bounded assistant text
  as a fallback when a successful terminal envelope has no result body. Tool,
  reasoning, and user content are never eligible fallback sources.
- Report `empty_terminal_result` and `assistant_message_fallback` as
  metadata-only diagnostics when that fallback is required.
- Preserve workspace trust and approval-mode semantics visibly.
- Retain Cursor session ID for attach/resume.

### Claude Code

- Consume structured streaming output when available.
- Preserve native permission mode and hooks.
- Do not read unrelated Claude project/session history during attachment.

### OMP

- Prefer ACP/session events.
- Keep provider/model routing in OMP.
- Treat OMP statistics commands as potentially mutating local caches and do not
  use them for a read-only probe unless explicitly documented.

### Multica (optional)

- Bind to exact workspace, issue, and run IDs.
- Prefer the companion fork's durable Multica workspace event outbox.
- Until then, compare exact run state and issue state through bounded polling.
- Treat run state as primary liveness and issue state as secondary workflow.
- Re-fetch authoritative state before emitting a terminal success package.
- Never create a Multica issue unless the caller explicitly selects Multica or
  invokes promotion.
- Advertise promotion/durable idempotency only if an exact promotion key can be
  stored and queried in Multica-owned metadata after local-state loss.
- Report separately whether context can be passed as a runtime file, artifact,
  or structured reference; do not assume a Hermes profile exists in the worker.

### Generic process

- Observe PID, process group, exit status, stdout/stderr/result paths.
- Never interpret successful exit as domain success without an explicit result
  contract.

## Adapter manifests

Each adapter publishes a machine-readable manifest containing:

- adapter version and schema version;
- executable discovery rules;
- compatible backend version ranges;
- capabilities;
- structured-output requirements;
- read-only probe commands and whether they update caches;
- known failure classifications;
- sensitivity of source references and artifacts.

The draft machine contract is
[`schemas/adapter-manifest.schema.json`](../schemas/adapter-manifest.schema.json).
Manifests include their semantics/schema versions and a compatibility policy;
they do not use a single unqualified boolean `capabilities` list.

`agentctl doctor` validates every detected installed adapter/backend pair by
default and answers whether each can launch, be observed, and return a result.
`--adapter <name>` narrows the check; `--static` reports
manifest/configuration evidence without live probes. Version skew is a
first-class warning rather than an unstructured runtime surprise.

`agentctl capabilities <name>` returns a concise live viability projection by
default. `--full` exposes the static manifest and `--static` suppresses probing;
`--require launch,result_content` makes readiness for a specific operation
machine-checkable. A live probe is read-only and its freshness is reported in
the result.

## Testing

Every adapter requires fixture tests for:

- session ID discovery;
- successful and failed terminal results;
- malformed/truncated output;
- process crash and orphan detection;
- reconnect/poll fallback;
- cancellation;
- event deduplication across reconnect;
- source reset, out-of-order, and duplicate event handling;
- capability downgrade and stale probe invalidation;
- context-injection failure before child launch;
- promotion crash recovery and authority-key lookup;
- secret redaction;
- unsupported-version behavior.

Live smoke tests are optional and separately gated because they may consume
provider resources or mutate session caches.
