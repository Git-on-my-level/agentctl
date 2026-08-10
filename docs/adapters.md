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
result      retrieve the backend's final result reference/summary
resume      optional; resume the same native session
cancel      optional; request native cancellation
artifacts   optional; enumerate references without copying content
```

Unsupported capabilities return a stable `capability_unavailable` error. They
never silently degrade into a different mutating action.

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

## Native pass-through

Launch receives an argv array after `--`; no shell re-parsing is permitted:

```bash
agentctl run --adapter codex -- codex exec --json -m gpt-5.6-sol "task"
```

The adapter may add only the minimum flags required to obtain structured events
when this is safe and documented. It must report added flags in `--explain`
output.

If an existing native flag conflicts with supervision, launch fails with a
structured explanation instead of overriding the caller silently.

## Initial adapters

### Codex

- Discover `thread_id` from structured events.
- Preserve Codex sandbox, model, effort, and worktree behavior.
- Support attach/resume only when the installed CLI exposes a verified route.
- Treat parent process exit and turn completion separately.

### Cursor

- Consume `stream-json`.
- Use `result.is_error`, not process exit code alone.
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

### Multica

- Bind to exact workspace, issue, and run IDs.
- Prefer a future durable Multica event outbox.
- Until then, compare exact run state and issue state through bounded polling.
- Treat run state as primary liveness and issue state as secondary workflow.
- Re-fetch authoritative state before emitting a terminal success package.
- Never create a Multica issue unless the caller explicitly selects Multica or
  invokes promotion.

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

`agentctl doctor` validates the installed adapter/backend pair. Version skew is
a first-class warning rather than an unstructured runtime surprise.

## Testing

Every adapter requires fixture tests for:

- session ID discovery;
- successful and failed terminal results;
- malformed/truncated output;
- process crash and orphan detection;
- reconnect/poll fallback;
- cancellation;
- event deduplication across reconnect;
- secret redaction;
- unsupported-version behavior.

Live smoke tests are optional and separately gated because they may consume
provider resources or mutate session caches.

