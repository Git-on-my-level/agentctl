# Read-only identity evidence

`agentctl identity --json` is an optional, versioned integration boundary. It
reports evidence about the current provider and native conversation separately
from the per-turn agentctl execution and local executable inventory. It does not
register an agent, grant access, resume work, or replace the native/Multica
execution authority. Standalone agents do not need agentctl to register with an
external coordination service.

## Invocation and precedence

```sh
agentctl identity --json
agentctl identity --execution exec-example --json
agentctl identity --provider custom-agent --native-session-id example-session --json
```

Use a real typed execution ID returned by agentctl in the second example. JSON
is the default; `--output text` provides compact agent-facing evidence.

- An explicit provider/session pair is a caller assertion. A session ID requires
  a provider. This mode does not borrow ambient execution or host evidence.
- An explicit `--execution` reads that exact execution from the journal. It
  cannot be combined with provider/session flags.
- Otherwise existing `AGENTCTL_EXECUTION_ID`, `AGENTCTL_ADAPTER`, or
  `AGENTCTL_AUTHORITY` signals a managed context. A valid execution ID is looked
  up in the existing journal. The record supplies execution evidence, but an
  implicit managed context with conflicting native provider markers or a
  different Codex conversation ID leaves the whole caller association unknown.
  Native children launched outside agentctl can inherit a parent's managed
  context. Explicit `--execution` remains an exact journal query regardless
  of ambient markers. This conservative rule means an actual managed Claude
  child launched under Codex can return unknown until explicitly queried with
  `--execution`; inherited evidence cannot prove which nesting direction occurred.
  Missing/inaccessible journal evidence leaves identity explicitly
  unknown or self-reported; it never creates a journal.
- In a managed context, `CODEX_THREAD_ID` is never adopted as the session ID:
  it is only checked for conflicting evidence. A child may have inherited its
  parent's conversation ID. Without managed context, a bounded
  `CODEX_THREAD_ID` is a self-reported native hint only when no competing
  `CLAUDECODE` or `CURSOR_AGENT_COMPLETED_PATH` marker is present. Competing
  markers leave provider/session unknown in either nesting direction. Other harnesses can use the
  explicit interface; unsupported inference remains unknown.

The command performs bounded, owner-checked read-only journal access and PATH
lookups. It does not run native executables, inspect transcript stores, scan
process arguments, read authentication/config files, probe a network service,
install skills, start maintenance, or write filesystem state. Use the existing
`doctor`, `capabilities`, `skills`, and `bootstrap` interfaces for their separate
purposes. An inventory entry means only an executable was found on PATH; it does
not identify the calling agent or establish authentication/health/capability.

## JSON contract

The normal success envelope contains `result.schema_version` equal to
`agentctl.identity.v1`. The envelope's own `schema_version` remains integer 1.
The result schema is [identity-report.schema.json](../schemas/identity-report.schema.json).

```json
{
  "schema_version": "agentctl.identity.v1",
  "provider": {"id": "codex", "provenance": "native_environment", "confidence": "self_reported"},
  "native_session": {"id": "sha256:<64 lowercase hex digits>", "id_kind": "provider_session_sha256", "provenance": "native_environment", "confidence": "self_reported"},
  "execution": {"id": null, "provenance": "unknown", "confidence": "unknown"},
  "environment": {"os": "linux", "arch": "amd64", "host_id": null, "host_provenance": "unknown"},
  "capabilities": {
    "resume": {"status": "unknown", "provenance": "unknown", "confidence": "unknown", "scope": "agentctl_native_adapter", "reason": "not_observed"},
    "history": {"status": "unknown", "provenance": "unknown", "confidence": "unknown", "scope": "agentctl_native_adapter", "reason": "not_observed"},
    "logs": {"status": "unknown", "provenance": "unknown", "confidence": "unknown", "scope": "agentctl_native_adapter", "reason": "not_observed"}
  },
  "harnesses": [{"provider_id": "codex", "availability": "available", "provenance": "path_lookup"}]
}
```

Missing identity uses JSON null and `unknown` evidence, never a process ID,
executable name, display alias, or invented conversation. Provider names are
lowercase tokens, at most 64 ASCII characters; `claude-code` normalizes to
`claude`, and `generic` to `generic-process`. Native inputs are at most 256 UTF-8
bytes and contain no control characters. No raw native ID, local path, labels,
model arguments, prompts, transcript, credential, or arbitrary environment value
is rendered, even in error details. Identity-bearing correlators are still
operator metadata: share them only with authorized consumers. Hashes are not
anonymization or proof of identity.

The correlator is `sha256:` plus SHA256 of the UTF-8 bytes of each of these four
strings, each followed by a NUL byte, in order:

1. `agentctl.identity.v1`
2. `native_session`
3. Canonical provider ID
4. Exact native session ID

Input control-character rejection makes tuple boundaries unambiguous. The hash
is stable across invocations and different execution IDs for the same native
conversation, and changes for a different provider or native conversation.
Consumers must namespace it by their independently enrolled host and
server-authenticated principal. The journal's optional host ID is evidence,
not host enrollment or authorization. Do not equate this correlator with an ANX
principal, use it as a bearer credential, or pass it to a native resume command.
Preserve an explicit caller-selected coordination identity such as `--as`.

For authorized local operations, keep `execution.id` alongside the correlator:
`status`, `events`, `result`, and (where supported) `continue` still resolve that
typed execution through the user-owned journal and its private native binding.
The hash does not remove those existing routes or reveal their private inputs.
`events` is normalized operational history and `result` is bounded final output,
not a native transcript/log API. An unmanaged session with no execution ID
requires its native provider's own authorized lookup interface.

Confidence is `observed`, `self_reported`, or `unknown`; even observed native
stream data is not provider attestation. Capability status is `supported`,
`unsupported`, or `unknown`, with scope and reason. A completed turn with a recorded recipe and native
session can support `agentctl_continue`, but this is not a readiness check or a
promise that a launch will succeed. The existing continue preflight must still
check completed/latest turn, cwd, availability, permissions, and conflicts.
Expert `run` executions without a frozen delegation recipe remain unsupported.
Multica owns its issue/run continuation; those IDs are never promoted to native
conversation IDs. Native history/log retrieval has no route in this contract;
`unsupported` there is scoped to agentctl, not a claim about every provider UI.

## Early observation, failures, and compatibility

Managed native runs retain the existing private native-session binding when
launch or the running result first exposes a valid native ID, normally at the
next 50 ms observation tick. This is idempotent for the same ID, remains separate
from execution IDs/source aliases, and adds no transcript/event store or journal
migration. A PID placeholder is never a conversation ID. Providers that have
not emitted a session ID continue to report null.

Successful reports, including unknown evidence, exit 0. Invalid/mixed/duplicate
flags or invalid identifiers exit 2. Explicit execution lookup exits 3 if the
record is unavailable or 6 if the journal is unavailable; implicit discovery
instead succeeds with unknown/self-reported evidence. Errors do not echo the
input or filesystem diagnostics. Repeated reads have no side effects.

Consumers should reject unrecognized major report versions, tolerate additional
fields, preserve unknown capability status, and skip correlation-based session
registration while the native session is null. This interface does not change
native CLI permission controls, skill delivery, automatic updates, releases, or
external registration policy.
