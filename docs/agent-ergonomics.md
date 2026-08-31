# Agent-first CLI ergonomics

## Design standard

The CLI must be easier for an LLM to use correctly than to misuse. Skills and
documentation explain intent, but correctness comes from deterministic command
contracts, validation, and safe defaults.

## Consistency

Every command that returns a document supports `--output text|json`. JSON is
the default because it is the deterministic machine contract; callers opt into
the compact human projection with `--output text`. Commands
that use a configured authority, journal, or compiled context additionally
consume the relevant global selector:

```text
--profile <name>
--context-file <path>
--config <path>
--journal <path>
```

The canonical spelling places these global flags before the command. A
subcommand may own the same word for another purpose, notably
`agentctl knowledge compile ... --output /bundle`; the global output selector,
when needed, still appears before `knowledge`.

`json` is the default. Waiting commands expose a bounded timeout where their
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

Warnings follow the primary command output as one compact `warning` line. They
do not change the command result or exit code:

```text
warning code=agentctl_update_available current_version=v0.3.2 latest_version=v0.3.3 release_url=https://github.com/Git-on-my-level/agentctl/releases/tag/v0.3.3
```

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
      "label": "Wait through execution deadline",
      "argv": ["agentctl", "await", "exec-purple-monkey-dragon-river-candle-meadow", "--output", "json", "--through-execution-deadline"],
      "mutates": true,
      "side_effect_class": "local_operational_write",
      "preconditions": []
    }
  ]
}
```

`next_actions` uses argv arrays, not prose commands requiring shell parsing.
Actions include required preconditions and whether they mutate state. They are
derived from the durable execution contract: a bounded run points at its
recorded deadline, while an unbounded run labels the default ten-minute wait
honestly. Background results also offer subscription discovery for callers that
cannot block.

Normal success emits no decorative banners. Bounded JSON mode writes exactly
one JSON document to stdout. An explicitly streaming JSON command writes one
document per line and no terminal summary outside the stream. Diagnostics never
mix with stdout. Text diagnostics go to stderr by default; `--diagnostics json`
makes each stderr diagnostic one JSON line using a separate diagnostic schema.

Warnings are typed, bounded notices. They may appear on either success or error
documents and never change `ok`, the primary error, or the process exit code.

## Invocation-triggered release updates

An exact release build defaults to `auto`. Before starting a worker, each
work-creating CLI invocation performs one read-only lookup of the binary-global
owner-only update state. The first eligible invocation due each UTC day starts a detached short-lived
worker and immediately continues with the requested command. The worker
coalesces with concurrent workers, checks GitHub's latest release, downloads the
matching platform archive, verifies its published SHA-256, and invokes the
packaged installer only for an executable bound to a valid agentctl install
manifest. Running commands keep their existing executable image; subsequent
commands use the atomically replaced binary. The worker then exits. There is no
updater daemon, timer, or dependency on the optional callback supervisor.
Commands advertised as read-only never trigger the worker or managed-skill
maintenance as a hidden side effect.

Update state is stored under the platform state home (by default
`~/.local/state/agentctl`) rather than beside a selected journal, because one
binary must make one daily check across all journals. Failed discovery attempts
and retryable download or install failures back off for one hour. Development
and dirty builds do not check.

`agentctl update policy notify` checks synchronously at most once per UTC day
and attaches `agentctl_update_available` without downloading the archive.
`agentctl update policy off` performs no check. `AGENTCTL_UPDATE_MODE` can
override the stored `auto`, `notify`, or `off` policy for a process; the legacy
`AGENTCTL_UPDATE_CHECK=off` values (`0`, `false`, and `disabled` included) remain
a hard-off compatibility path. `agentctl update status` is read-only and
`agentctl update now` explicitly performs a foreground check and managed update.

## Error contract

Errors include:

```json
{
  "ok": false,
  "schema_version": 1,
  "warnings": [],
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
and offers an explicit `sync` or `refresh` action. `result` and a terminal
`await` are the exception that is declared in help: they record one
acknowledgement stamp, the same idea as subscription delivery acknowledgement,
and therefore advertise `local_operational_write`.

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

`agentctl capabilities <name>` is authoritative for what can be streamed,
polled, resumed, cancelled, or inspected. It returns a concise live viability
projection by default. `--full` requests the static manifest, while `--static`
suppresses the live probe; `--require` narrows the projection to capabilities
that a planned operation needs. Agents never need to infer support from backend
names or versions.

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

Context selection is explicit in v0.1. `agentctl context` deterministically
selects and optionally renders a bounded document; a launch receives a context
handle only when the caller supplies `--context-file` or
`AGENTCTL_CONTEXT_FILE`. Launch never performs a hidden fetch or model-driven
memory lookup.

A native adapter passes the handle only through its negotiated mechanism. The
current built-ins expose an environment path but do not guarantee that the
model consumes it, so the handle is advisory rather than a correctness gate. A
portable skill explains deeper commands, but correctness does not depend on
Hermes or on a model reading a global profile.

## Lifecycle UX

Commands form a predictable grammar:

```text
agentctl run
agentctl recent
agentctl inbox
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

`recent` discovers. `status` observes. `result` retrieves. `await` waits.
`inbox` explains what needs attention. `cancel` mutates. No command name mixes
these responsibilities.

## Actionable work inbox

`agentctl inbox` is a read-only projection, not a second issue board. It
selects current attention, uncollected terminal results, and running or
unreachable executions whose last normalized observation exceeds a bounded
age. Stable reason codes say why each execution matched. `work_health` reports
task or collection state; `tool_health` reports normalized liveness. In
particular, `tool_unreachable` never claims that the underlying task failed.

The default stale bound is one hour. `--stale-after` accepts one minute through
thirty days, making the review policy explicit and bounded. The command reads
neither result bodies nor native session files and performs no adapter status
call, acknowledgement, fetch, or cache update. A successful `result` or
terminal `await` is the existing collection boundary that clears a terminal
item.

## Safe agent-first defaults

Defaults encode the common, evidence-preserving path. Each has an explicit
escape for callers that intentionally need weaker or broader behavior:

- `doctor` discovers local harnesses and checks whether they can launch, be
  observed, and return a result. It includes bootstrap, journal, configuration,
  supervisor, and live adapter readiness; `--adapter` narrows the check and
  `--static` skips live probes.
- `bootstrap update` detects supported harnesses and reconciles their
  canonical skill roots. It upgrades managed assets and installs missing
  portable skills, but refuses drifted assets, does not delete legacy copies,
  and never creates a new supervisor service implicitly. `--dry-run` previews
  the exact paths and `--harness` narrows detection.
- `run` infers an adapter from a known executable, has no default wall-clock
  timeout, and preflights both `launch` and invocation-scoped `result_content`.
  `--timeout` adds
  a caller-selected bound; `--no-timeout` remains an explicit compatibility
  spelling. `--adapter` and `--allow-missing-result` are explicit overrides;
  exact native argv remains everything after `--` unless the caller selects a
  prompt source and `argv` delivery.
- `run --background` starts a detached host-local worker and returns only after
  the execution is durable. The worker survives the caller but not a host
  restart, is noninteractive, and does not gain a cross-process cancel route
  unless the adapter advertises one. The parent materializes bounded prompt
  stdin through a one-shot pipe before detaching. Background idempotency keys
  remain unavailable.
- `--label` records up to 16 exact lowercase metadata names. `recent` returns
  the newest 20 host-local executions by default and filters by exact state,
  adapter, or label without reading prompt or result records. Repeated label
  filters use AND semantics. `--unreconciled` returns terminal executions whose
  result has never been acknowledged; terminals that predate acknowledgement
  tracking on that journal are omitted.
- `inbox` returns the newest 20 actionable executions without refreshing an
  adapter or reading a result. It includes current attention, unreconciled
  terminals, and running or unreachable work whose observation age is at least
  `--stale-after` (one hour by default, bounded from one minute to thirty days).
  `work_health` describes task/collection state while `tool_health` repeats
  normalized liveness; `tool_unreachable` explicitly does not assert task
  failure. A collected terminal failure drops out instead of creating a second
  resolution database.
- `--prompt-file` and `--prompt-stdin` are mutually exclusive, bounded prompt
  sources. `--prompt-delivery argv|stdin` is explicit and defaults to `argv`
  only after a source is selected. Prompt bytes are excluded from plan output,
  the journal, events, and status; only digest, byte count, source, and delivery
  participate in plan and idempotency metadata.
- `fanout --manifest` reads one prompt file once and runs explicit child argv
  vectors with bounded foreground concurrency. It returns per-child execution
  IDs and states but creates no group authority and performs no synthesis. A
  v1 manifest requires `schema_version`, `prompt_file`, and `children[].argv`.
  The prompt file and relative child working directories are manifest-relative;
  a child with no `cwd` inherits the invoking process directory.
- `result` requires bounded stored content or a structured failure, and fails
  closed on conflicted evidence. `--allow-empty` is for metadata-only inspection; `--summary`
  intentionally returns the bounded preview. A successful dereference writes an
  acknowledgement stamp so `recent --unreconciled` can forget the execution.
- `await` uses a ten-minute timeout and stops on actionable attention, returning
  `attention_required` with a next action. `--timeout` changes the bound and
  `--no-timeout` removes it; `--through-execution-deadline` uses a recorded run
  deadline plus bounded terminalization grace; `--ignore-attention` opts into
  continued waiting.
  A terminal return acknowledges the execution; attention and timeout do not.
- `subscribe create` listens for `terminal`, `attention`, and `artifact` by
  default, expires after acknowledged terminal delivery, and has a bounded
  twenty-four-hour TTL. Transient delivery failures retry with bounded backoff
  until that TTL; permanent delivery classes still dead-letter. `--kind all`
  is an explicit broad subscription.

These defaults do not weaken authority boundaries: permission flags such as
Cursor `--trust`, remote promotion, supervisor service creation, and cleanup
remain visible mutations that require explicit caller intent.

An operator may record advisory authorization for Cursor workspace trust in a
profile. Agents consult `config doctor` and pass `--trust` explicitly when that
authorization exists; agentctl never inserts it. For Cursor automation, normal
Agent mode is the omitted-mode default, Ask is the supported read-only mode,
and Plan fails closed unless unreliable-result acceptance is explicit.

## Progress hygiene

Default output is quiet (both JSON and the explicit text projection):

- no unchanged polling ticks;
- no token-level reasoning;
- no routine tool narration;
- progress only when requested or semantically changed;
- one terminal package;
- attention events only for actionable decisions.

## Self-description

`agentctl help` lists the command surface. `agentctl help <topic>` is the
progressive, just-in-time contract: it returns usage, defaults, examples, and
typed `next_actions` that point to related topics. `agentctl schema`,
`agentctl capabilities`, and `agentctl doctor` expose the live contract and
readiness rather than requiring an agent to rely on a stale skill. JSON is
already the default; use `--output text` when a human-readable projection is
preferable.

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
