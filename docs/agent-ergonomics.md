# Agent-first CLI ergonomics

## Design standard

The CLI must be easier for an LLM to use correctly than to misuse. Skills and
documentation explain intent, but correctness comes from deterministic command
contracts, validation, and safe defaults.

## Consistency

Every command supports:

```text
--output human|json|ndjson
--profile <name>
--context <context-word-id>
--explain
--no-color
--timeout <duration>
```

Mutating commands additionally support:

```text
--plan
--idempotency-key <value>
--input-file <path>
--input-stdin
```

Flag names do not vary between subcommands for the same concept. JSON field
names are stable snake_case. Times are UTC RFC 3339 plus explicit freshness.

## Output contract

Human output is concise and leads with the typed word ID. JSON output contains:

```json
{
  "ok": true,
  "schema_version": 1,
  "result": {},
  "warnings": [],
  "next_actions": [
    {
      "label": "Wait for terminal state",
      "argv": ["agentctl", "await", "@last", "--output", "json"]
    }
  ]
}
```

`next_actions` uses argv arrays, not prose commands requiring shell parsing.
Actions include required preconditions and whether they mutate state.

Normal success emits no decorative banners. JSON mode never mixes logs with
stdout; diagnostics go to stderr as structured JSON when requested.

## Error contract

Errors include:

```json
{
  "ok": false,
  "error": {
    "code": "ambiguous_reference",
    "message": "Reference 'purple-monkey' matches two executions.",
    "retryable": false,
    "details": {},
    "next_actions": []
  }
}
```

Stable error classes and exit codes distinguish usage, unavailable dependency,
auth required, conflict, timeout, attention, remote failure, and unknown state.
Error messages never recommend pasting credentials into a prompt.

## Contextual references

Agents should rarely copy a full ID twice. Supported references include:

```text
@last       last object produced by this invocation context
@current    execution bound to the current environment/context file
@parent     parent execution
@mine       current caller's active execution, when unique
```

Resolution comes from an explicit context file or environment handle created by
`agentctl`, not global recency guesswork. In scripts, full IDs remain preferred.

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

`agentctl capabilities --adapter <name> --output json` is authoritative for
what can be streamed, polled, resumed, cancelled, or inspected. Agents never
need to infer support from backend names or versions.

When a requested capability is absent, the error offers supported alternatives
without silently changing behavior.

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

Models are not expected to remember that `agentctl` exists. Native launch
adapters inject the context path through the safest backend-supported mechanism.
A portable skill explains optional deeper commands.

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

Default output is quiet:

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

