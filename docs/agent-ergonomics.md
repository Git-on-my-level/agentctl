# Agent-first CLI ergonomics

## Design standard

The CLI must be easier for an LLM to use correctly than to misuse. Skills and
documentation explain intent, but correctness comes from deterministic command
contracts, validation, and safe defaults.

## Consistency

Every command that returns a document supports `--output text|json`. Commands
that use a configured authority, journal, or compiled context additionally
consume the relevant global selector:

```text
--profile <name>
--context-file <path>
--config <path>
--journal <path>
```

The canonical spelling places these global flags before the command. This is
required when a subcommand owns the same word for another purpose, notably
`agentctl --output json knowledge compile ... --output /bundle`.

`text` is the default. Waiting commands expose a bounded timeout where their
contract requires one. The CLI never emits ANSI color or cursor-control
sequences; its output is for agents and automation, not terminal decoration.

Commands that implement these concepts use the stable spellings:

```text
--plan
--idempotency-key <value>
```

`--plan` performs target resolution and read-only capability probes but does
not reserve names, create local mutation records, or write remote state.

Flag names do not vary between subcommands for the same concept. JSON field
names are stable snake_case. Times are UTC RFC 3339 plus explicit freshness.

## Text output contract

Text output is a compact, line-oriented agent interface. It leads with the typed
word ID, uses stable lowercase field names, omits null/default fields, and puts
one fact or next action on each line:

```text
exec-purple-monkey-dragon-river-candle-meadow state=running authority=native adapter=codex liveness=alive fresh=10s
next agentctl await exec-purple-monkey-dragon-river-candle-meadow --output text
```

Text is designed to minimize tokens and transcription mistakes, but JSON is the
normative programmatic contract. Text never uses tables, banners, box drawing,
color, localized labels, or explanatory prose unless `--explain` is requested.
Values requiring escaping use JSON string syntax. Repeated records use one line
per record; multiline native output is never inlined.

Errors follow the same compact grammar:

```text
error code=ambiguous_reference exit=4 retryable=false ref="purple-monkey"
next agentctl status exec-purple-monkey-dragon-river-candle-meadow --output text
```

## JSON output contract

JSON output contains:

```json
{
  "ok": true,
  "schema_version": 1,
  "result": {},
  "warnings": [],
  "next_actions": [
    {
      "label": "Wait for terminal state",
      "argv": ["agentctl", "await", "exec-purple-monkey-dragon-river-candle-meadow", "--output", "json"],
      "mutates": false,
      "side_effect_class": "read_only",
      "preconditions": []
    }
  ]
}
```

`next_actions` uses argv arrays, not prose commands requiring shell parsing.
Actions include required preconditions and whether they mutate state.

Normal success emits no decorative banners. Bounded JSON mode writes exactly
one JSON document to stdout. An explicitly streaming JSON command writes one
document per line and no terminal summary outside the stream. Diagnostics never
mix with stdout. Text diagnostics go to stderr by default; `--diagnostics json`
makes each stderr diagnostic one JSON line using a separate diagnostic schema.

## Error contract

Errors include:

```json
{
  "ok": false,
  "schema_version": 1,
  "error": {
    "code": "ambiguous_reference",
    "message": "Reference 'purple-monkey' matches two executions.",
    "retryable": false,
    "exit_code": 4,
    "details": {},
    "next_actions": []
  }
}
```

Stable error classes and exit codes distinguish usage, unavailable dependency,
auth required, conflict, timeout, attention, remote failure, and unknown state.
Error messages never recommend pasting credentials into a prompt.

## Exit semantics

`ok` describes the requested command, not merely whether parsing succeeded, and
matches exit status zero. Observation commands such as `status`, `events`, and
`result` exit 0 when the query succeeds even if the observed execution is
failed. Their `result.state` carries subject outcome.

`await` is intentionally outcome-sensitive: completed returns success; failed,
cancelled, orphaned, attention (when requested as a stop condition), timeout,
or conflicted/unknown outcome returns an error document with the last bounded
execution/event reference in `details`.

| Exit | Error code(s) | Meaning |
| ---: | --- | --- |
| 0 | none | Command completed successfully |
| 2 | `usage`, `unsupported_schema` | Invalid request or unsupported contract |
| 3 | `not_found`, `cursor_expired` | Exact object/checkpoint unavailable |
| 4 | `ambiguous_reference` | Reference has multiple exact candidates |
| 5 | `capability_unavailable` | Requested semantics are unsupported |
| 6 | `dependency_unavailable` | Required executable/service unavailable |
| 7 | `authentication_required`, `authorization_denied` | Identity/permission failure |
| 8 | `conflict`, `invalid_state` | Concurrent or lifecycle conflict |
| 9 | `timeout` | Command deadline elapsed |
| 10 | `attention_required` | Await stopped for actionable attention |
| 11 | `execution_failed`, `remote_failure` | Observed work or remote operation failed |
| 12 | `execution_cancelled` | Awaited execution was cancelled |
| 13 | `execution_unknown`, `unknown_state` | Outcome cannot be proven or is conflicted |
| 70 | `internal` | `agentctl` invariant or implementation failure |

The process emits at most one primary error document. Warnings do not change the
exit code. Termination by an operating-system signal follows platform shell
convention and may not produce a complete document.

## Reserved contextual references

The grammar reserves these future explicit-context references:

```text
@last       last object recorded in this explicit invocation context
@current    execution bound to the current environment/context file
@parent     parent execution
@mine       current caller's active execution, when unique
```

They are not resolved by the v0.1 CLI. Current commands require a full typed ID
or portable URI. When implemented, resolution will come only from an explicit
owner-only `--context-file` or `AGENTCTL_CONTEXT_FILE`; it will never consult
global recency. Machine-readable `next_actions` therefore use full typed IDs.

## No dangerous fuzzy matching

- Read-only lookups may accept unique word prefixes.
- Mutations require a full ID or an exact contextual reference.
- Names such as agent/display names are never silently fuzzy-matched for
  dispatch; a route resolver returns an exact typed ID first.
- Ambiguity fails with candidates and exact retry argv.

## Idempotency

Create, promote, subscribe, cancel, and callback-acknowledgement operations are
idempotent. Repeating an operation with the same key returns the existing
object and sets `reused: true`.

The CLI can derive an idempotency key from parent execution + bounded semantic
operation, but always reports it. Observation timestamps never enter semantic
dedupe keys.

Automatic keys include the authority scope and exact source fingerprints. A
derived key is allowed only when all semantic inputs are canonical and bounded;
otherwise the CLI requires the caller's key. Reusing a key with different
canonical inputs returns `conflict` with both input digests, never the existing
object as if it matched.

## Plans and side effects

Read commands perform no hidden refresh, Git fetch, cache synchronization, or
statistics update. If freshness requires a write, the CLI reports stale state
and offers an explicit `sync` or `refresh` action.

Mutations support `--plan`, which resolves exact targets, adapters, authority,
side effects, and callback scope without performing them.

Commands declare one of:

```text
read_only
local_operational_write
remote_coordination_write
external_side_effect
```

The class appears in plan and JSON output.

## Capability negotiation

`agentctl capabilities <name> --output json` is authoritative for
what can be streamed, polled, resumed, cancelled, or inspected. Agents never
need to infer support from backend names or versions.

The result distinguishes manifest support from the fresh instance probe and
reports `supported`, `degraded`, or `unavailable` plus constraints and
freshness. A degraded alternative requires explicit caller acceptance. The
contract is detailed in [Adapters](adapters.md#capability-negotiation).

## Input ergonomics

- Multi-line or sensitive input prefers stdin/file flags.
- File paths are resolved, bounded to the current working root by default, and
  echoed in plan output.
- Secrets are never accepted directly in ordinary flags when a file/stdin or
  credential-reference mechanism exists.
- Native command pass-through uses argv after `--`.
- The CLI never emits raw prompts, tokens, environment values, or native auth
  locations in standard output.

## Deterministic context

Every launched execution receives a small context document automatically. It
contains authority, execution ID, parent callback, relevant resource and
knowledge references, and exact completion expectations.

Models are not expected to remember that `agentctl` exists. A native or Multica
adapter injects the context only through a negotiated, reviewed mechanism. If
required injection is unavailable, launch fails before the child starts. A
portable skill explains optional deeper commands, but correctness does not
depend on Hermes or on any model reading a global profile.

## Lifecycle UX

Commands form a predictable grammar:

```text
agentctl run
agentctl attach
agentctl status
agentctl events
agentctl subscribe
agentctl await
agentctl result
agentctl promote
agentctl cancel
agentctl doctor
agentctl context
agentctl route explain
```

`status` observes. `result` retrieves. `await` waits. `cancel` mutates. No command
name mixes these responsibilities.

## Progress hygiene

Default text output is quiet:

- no unchanged polling ticks;
- no token-level reasoning;
- no routine tool narration;
- progress only when requested or semantically changed;
- one terminal package;
- attention events only for actionable decisions.

## Self-description

`agentctl schema`, `agentctl capabilities`, `agentctl help --output json`, and
`agentctl examples --output json` let an agent discover the live contract rather
than rely on a stale skill.

Every JSON schema and adapter manifest includes a version and canonical docs
reference. Unknown fields are tolerated on read; unknown required semantics fail
closed on mutation.

## Contract evolution

`schema_version` is the major version of one named document schema, not the CLI
release. Adding optional fields is compatible within a version. Changing field
meaning, required fields, canonicalization, enum meaning, ID encoding, or
security semantics requires a new major schema/semantics version and fixtures.

Readers preserve unknown fields when proxying or journaling. A reader may show
a newer document only as opaque/unverified data; it returns
`unsupported_schema` before normalization or mutation. Adapter manifests state
their minimum and maximum schema and semantics versions. `agentctl schema` and
`agentctl capabilities` expose the negotiated intersection so callers do not
infer compatibility from CLI versions.
