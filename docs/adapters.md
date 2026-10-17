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
steer       optional; deliver a new instruction to a running session
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
launch attach snapshot events result resume steer cancel artifacts history
context_injection promotion durable_idempotency durable_events remote_callback
```

`resume` and `steer` are negotiated from the exact argv and prompt delivery,
like `result_content`. `resume` means a later invocation can continue the
native session once its turn has completed; see
[Follow-up turns](follow-up-turns.md). `supported` means a live input stream; `degraded` means the
session is interrupted and resumed. See [Steering](steering.md) for the routes
each adapter has and the evidence behind them.

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

The manifest's version action declares its timeout. Version-probing adapters
use five seconds by default; ZCode gets 30 seconds because its Electron CLI can
be delayed by host load. A timeout reports the adapter, configured timeout,
elapsed time, and that the host may be overloaded; it does not classify the
executable as missing.

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
`claude`, `omp`, `zcode`, and `devin`). `--adapter <name>` remains an explicit override when an
executable is ambiguous or the caller has a reviewed authority mapping. The
argv after `--` is passed unchanged in either case.

When the caller explicitly selects `run --prompt-file` or `--prompt-stdin`,
prompt transport is a separate reviewed operation. `--prompt-delivery argv`
appends one exact UTF-8 positional argument; `--prompt-delivery stdin` attaches
the exact bounded bytes as child stdin; `--prompt-delivery stream` sends the
prompt as the first message of the adapter's live input protocol and keeps
stdin open for steering, and fails before launch unless the argv selects that
protocol. Adapters do not infer the mechanism
from a backend name, and prompt bytes are not persisted in the execution
envelope. Without a prompt source, child stdin remains disconnected and argv is
unchanged.

The adapter may add only the minimum flags required to obtain structured events
when this is safe and documented. It must report added flags in `--explain`
output.

Capability negotiation is invocation-scoped. Backend probing establishes what
an installed adapter can support; the exact argv establishes what this launch
actually supports. When stored result content is required, Codex and ZCode require
`--json`, Cursor and Claude require `--output-format stream-json`, OMP
requires `--mode json`, and Devin accepts ACP (`devin acp`) or expert print mode (`-p`). A missing or conflicting mode fails before dependency
probing, journal creation, or child launch. agentctl reports the required mode
but never silently rewrites the caller's argv.

If an existing native flag conflicts with supervision, launch fails with a
structured explanation instead of overriding the caller silently.

ZCode discovery checks `zcode` on PATH, then the legacy
`~/.zcode/server/agents/glm/zcode-agent`. Current ZCode.app builds bundle the
CLI (`Contents/Resources/glm/zcode.cjs`) but install neither of those. In that
case the probe fails with `diagnostic_code: zcode_launcher_missing`. Its
details list the searched paths and, when the bundled CLI exists, name it in
`app_bundled_cli` and give a remediation. The remediation is a PATH launcher
that runs the bundle under `ELECTRON_RUN_AS_NODE=1` with both provider-config
environment variables set. agentctl does not launch the app bundle itself,
because the model default lives in a provider config that the operator owns.

Context injection is separately negotiated. An adapter declares one or more
reviewed mechanisms (`environment_path`, `native_argument`,
`native_instruction_file`, `authority_artifact_ref`) and whether the harness
actually guarantees delivery to the worker. If required context cannot be
delivered, launch fails before starting the native command. The generic process
adapter may provide `AGENTCTL_CONTEXT` and
`AGENTCTL_CONTEXT_FINGERPRINT` environment handles but cannot claim the child
model read them. Native child execution metadata is defined in the
[execution envelope](execution-envelope.md#native-child-environment).

## Initial adapters

### Codex

- Discover `thread_id` from structured events.
- Preserve Codex sandbox, model, effort, and worktree behavior.
- Support attach/resume only when the installed CLI exposes a verified route.
- Treat parent process exit and turn completion separately.

### Cursor

Thinking, assistant, and tool envelopes describe phases within a task. Their
`completed`, `done`, or failed subtypes/statuses do not terminate the task, and
tool results are never assistant answers. The task's explicit result envelope
owns completion. If the process exits or reaches its deadline without that
result, phase completion cannot turn the outcome into success. This rule is
specific to Cursor's native stream; Multica lifecycle statuses keep their own
meaning.

- Consume `stream-json`.
- Use `result.is_error`, not process exit code alone.
- Prefer terminal `result` content, but retain the last bounded assistant text
  as a fallback when a successful terminal envelope has no result body. Tool,
  reasoning, and user content are never eligible fallback sources.
- Report `empty_terminal_result` and `assistant_message_fallback` as
  metadata-only diagnostics when that fallback is required.
- Classify a locked macOS login keychain during the read-only version probe as
  authentication-required with an actionable diagnostic, without copying raw
  native stderr into the error envelope.
- Preserve workspace trust and approval-mode semantics visibly.
- Treat omitted `--mode` as Cursor's normal Agent behavior and `--mode ask` as
  read-only Q&A. Reject `--plan` and `--mode plan` by default because Cursor can
  emit an intermediate assistant message followed by a successful terminal
  envelope without a completed plan body; the explicit
  `--allow-unreliable-result` escape hatch records caller acceptance.
- Emit metadata-only progress phases (`initializing`, `thinking`, `assistant`, `tool`,
  and `completing`) without tool output or reasoning.
- Preserve a bounded, redacted stderr diagnostic in explicit failed outcomes
  when no structured error is available; events and callbacks remain
  metadata-only.
- Label stored content provenance (`assistant_terminal_result` or
  `assistant_message_fallback`) so callers can assert an assistant source.
- Retain Cursor session ID for attach/resume. Resume is supported after a
  completed turn only; an interrupted turn loses its prompt.

### Claude Code

- Consume structured streaming output when available.
- Pass configured effort after the model as `--effort <level>` for `low`,
  `medium`, `high`, `xhigh`, and `max`; other values such as Codex-only
  `ultra` fail recipe validation.
- Reject explicit service speed and fast model variants as before.
- Preserve native permission mode and hooks.
- Do not read unrelated Claude project/session history during attachment.

### ZCode

- Consume the final `--json` result document when present.
- On terminal failure, classify provider code `1308` or HTTP `429` as retryable
  capacity and preserve a valid reset time when present. Other diagnostic text
  stays available to the shared classifier: `usage limit reached` is capacity,
  while generic quota/rate-limit wording is `quota_exceeded`.
- Replace unknown provider stderr with a bounded generic failure reason. Raw
  stderr, request identifiers, and trace identifiers are not stored.

### Devin

Delegation launches `devin acp --model <configured-model>` as an ACP v1 client
over newline-delimited JSON-RPC 2.0 stdio. ACP requires a nonempty, valid UTF-8
prompt delivered through agentctl-managed stdin. Both plans and launches reject
argv/stream delivery or missing prompt evidence before any ACP server starts;
`--no-store-result` and `--allow-missing-result` do not bypass this validation.
Invalid UTF-8 fails with usage diagnostic `acp_prompt_invalid_utf8` before
probing, execution admission, or JSON encoding, preserving the admitted bytes.
Wrong delivery reports `acp_prompt_delivery_required`; an empty prompt reports
`acp_prompt_required` (the CLI prompt loader can reject empty input earlier).
Capability negotiation requires content-free delivery and byte-count evidence.
The adapter also validates direct launches before exec. These checks do not
change Multica dispatch or expert print-mode prompt handling. The flow is `initialize` (version 1,
empty `clientCapabilities`) → `session/new` (`cwd`, `mcpServers: []`) →
`session/prompt` (`sessionId`, `prompt: [{type: "text", text: ...}]`). The returned
`sessionId` binds the execution's native session. `agent_message_chunk` text
forms the result. A `tool_call` or `tool_call_update` marks a boundary: the
next assistant message chunk discards earlier narration, and subsequent chunks
of that message are joined. A trailing tool update alone does not erase the
buffer. This uses tool activity as the message boundary, without assuming
provider-specific message IDs. Message and tool updates emit only bounded phase metadata.
Prompts, replayed history, tool input/output, and raw RPC errors never enter the
journal. The native session store remains Devin's authority.

Only `stopReason: "end_turn"` succeeds. `cancelled` cancels; `refusal`,
`max_tokens`, `max_turn_requests`, and unknown reasons fail. JSON-RPC errors
fail with `acp_rpc_error` and their numeric code, without their message/data.
Invalid messages, oversized streams, missing session IDs, and premature EOF
fail with explicit `acp_*` diagnostics. Cancel sends the `session/cancel`
notification, waits for the prompt result, then escalates to process termination
within the caller's grace period. EOF closes a finished server; a two-second
shutdown bound prevents unreaped children.

`delegation.unattended_coding_permissions` maps to the frozen native plan's
`AGENTCTL_DEVIN_ACP_APPROVE=1` environment entry (`0` without the grant).
Only an explicit launch environment entry counts; ambient values do not grant
permissions. Permission requests for the current session select the first
`allow_once` option only with that grant. Otherwise the client selects the first
offered `reject_once` option, emits attention, and fails the turn with
`acp_permission_denied`. If no reject option is offered, it falls back to the
ACP `cancelled` outcome. An actually client-cancelled turn always receives the
`cancelled` outcome, even when reject options exist.
No persistent `allow_always` grant is inferred. File-system and terminal client
services are not advertised; all unsupported client requests receive JSON-RPC
`-32601`, without accessing files. Read-only access remains refused because ACP
read-only enforcement has not been verified.

Follow-up turns require `agentCapabilities.loadSession: true`, checked at each
initialization, before sending `session/load` with the recorded ID, cwd, and
empty MCP list. Missing support fails with `acp_load_session_not_advertised`;
there is no fallback to a new session. Load replay is excluded from the answer.
The internal argv descriptor `--agentctl-acp-session <id>` is removed before
exec and converted into `session/load`; it is not a Devin flag. Public continue
plans redact its ID. Existing request-key admission and replay semantics apply;
transport errors never trigger an automatic new session or prompt retry.

Real Devin verification covered initialization, a tool-free prompt, and loading
the same session to recover a marker from the prior turn. Permission, tool,
error, and cancellation cases are covered by the fixture. Steering remains
unavailable because real cancel plus reprompt was not verified for this adapter.
Multica continues to own its issue/run lifecycle; this transport changes only
direct native execution, not Multica dispatch or continuation guarantees.

Expert `run -- devin -p ...` retains the plain-text parser and process-exit
result behavior. A welcome banner is stripped. Print mode still cannot report
a session ID or continue. Delegation no longer selects print-mode flags.

Protocol references: [initialization](https://agentclientprotocol.com/protocol/v1/initialization),
[session setup](https://agentclientprotocol.com/protocol/v1/session-setup),
[prompt turns](https://agentclientprotocol.com/protocol/v1/prompt-turn), and
[permissions](https://agentclientprotocol.com/protocol/v1/tool-calls).

### OMP

- Consume `--mode json` session events for direct one-shot runs. Retain bounded
  assistant text from authoritative `message_end` records and prefer the last
  assistant message in `agent_end` as the terminal source. Thinking, tool,
  user, and provider payloads are never eligible result content.
- Require `--mode json` when stored result content is expected; preserve native
  argv exactly and fail before launch unless the caller explicitly permits a
  missing result.
- Label stored content provenance (`assistant_terminal_result` or
  `assistant_message_fallback`) so callers can assert an assistant source.
- Prefer ACP/session events for future attach or resume support.
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
- The direct child is the process-group leader. Its exit status is the only
  exit status agentctl records. A grandchild's exit code is not that status.
- Stdout and stderr are pipes drained before the direct child is reaped, so a
  structured terminal record a child writes on the inherited stdout is kept.
- Plain text is not a result. Exit 0 with no structured terminal record is
  `orphaned` and `result_extraction_failed`, with empty stored content. A
  non-zero exit is `failed` and `native_execution_failed`; unstructured stdout
  is not stored.
- `hermes -z` follows that second pattern. Its oneshot mode prints a plain
  final answer and exits 0 when the turn produced text, 1 when it completed
  without text, 2 when the turn failed, and 130 when interrupted. It does not
  emit the terminal JSON record this adapter requires. A report file the
  process writes on disk is outside the result contract, so agentctl cannot
  know that the work succeeded. Exit 0 therefore stays `orphaned`; a non-zero
  exit stays `failed` with no stored content.

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
