# Architecture

## Thesis

`agentctl` is a common supervision and context layer, not a universal agent
runtime and not a project-management system.

The system separates four concerns that are often accidentally coupled:

1. **Task authority** — whether a native session or Multica owns the work.
2. **Execution** — Codex, Claude Code, Cursor, OMP, or another native backend.
3. **Observation** — normalized state, events, health, and artifacts.
4. **Delivery** — waking a parent agent or sending a callback reliably.

This allows direct investigative work to remain lightweight while durable work
can opt into Multica without changing how parents observe completion.

## Planes and owners

| Plane | Owner | `agentctl` responsibility |
| --- | --- | --- |
| Direct execution | Native CLI/session | Launch or attach; retain native reference |
| Durable coordination | Multica | Create only when explicitly selected; subscribe and reconcile |
| Resource topology | `tailnetctl` | Consume structured output; do not reimplement resolution |
| Host state | `macctl` or host equivalent | Consume context/doctor output when present |
| Shared context | Git-authored verified bundle | Select and render deterministic context |
| Callback transport | Local supervisor/outbox | Normalize, retry, acknowledge, and expire |

No plane silently becomes authoritative for another plane.

## Modes

### Direct

The native session is authoritative. `agentctl` wraps or attaches to it and
stores only an execution envelope, normalized events, subscription cursors, and
delivery receipts.

Direct mode is the default recommendation for bounded investigation, scoping,
inventory, read-only review, second opinions, and one-owner operations. A long
runtime alone does not require Multica.

### Multica

The Multica issue and exact run are authoritative. `agentctl` observes the run
as the primary liveness source and the issue as a secondary workflow source.

Multica is recommended when the work needs multiple owners, cross-machine
handoffs, a parent that may disappear, review visibility, a long-lived PR fix
loop, a multi-stage lifecycle, or explicit operator tracking.

### Attach

An execution may be launched without `agentctl` and attached later using its
native reference. An adapter reports whether the backend can stream history,
only observe future events, poll state, resume, or cancel.

### Promote

Promotion creates a new durable Multica lifecycle from a direct execution. It
does not move or copy the native session. It writes a distilled handoff with
verified findings, evidence, remaining acceptance criteria, artifact refs, and
the original execution reference.

Promotion is explicit and idempotent. Repeating it returns the existing issue.

## Components

### CLI

The CLI is the stable interface for humans and agents. Every command supports
structured output and reports source authority, observation time, and
freshness.

### Adapter library

Adapters translate native state and events into the execution envelope without
owning model selection or conversation semantics. Native arguments pass through
as an argv array after `--`.

### Local journal

The local journal contains operational metadata only. It is owner-only,
append-oriented, bounded by retention, and safe to rebuild from native sources
where possible. It is not a transcript store.

### Optional local supervisor

The initial CLI can supervise foreground processes without a daemon. A small
managed local supervisor is justified only for cross-restart subscriptions,
remote callbacks, or retrying a delivery while no parent process is alive.

The supervisor must remain host-local. It exposes a Unix socket by default;
Tailnet HTTP exposure is explicit, authenticated, and read-only unless a
separate mutation capability is granted.

### Shared context bundle

Private Git is the authoring source. CI validates and compiles policy, knowledge,
portable skills, and routing data into a content-addressed bundle with a
machine-readable index. A client installs a verified local cache and continues
to work from the last valid revision when the publisher is unavailable.

## Routing policy

`agentctl route explain` is advisory and deterministic. It never creates an
issue or launches an agent.

The caller explicitly selects `direct` or `multica`. If the caller asks for an
automatic recommendation, uncertainty resolves to `direct`, because promotion
is available later and unwanted Multica cards impose review cost.

Model-family routing is separate from lifecycle routing. A default reviewed
policy may prefer:

| Model family | Native executor |
| --- | --- |
| OpenAI GPT | Codex |
| Anthropic Claude | Claude Code |
| Cursor Composer/Grok | Cursor |
| GLM and other open-weight models | OMP |

Cross-family routes remain explicit experiments or fallbacks rather than
default aliases.

## Authority-safe references

Every execution reports:

- its `agentctl` word ID;
- its authority (`native` or `multica`);
- adapter/backend;
- host identity;
- opaque native reference;
- Multica issue/run reference when applicable;
- provenance and observation time.

An `agentctl` ID is a stable handle for observation. It does not replace the
native or Multica ID at the source. Debug output may expose opaque source IDs;
normal output uses word IDs and resolvable typed URIs.

## Failure boundaries

- Multica unavailable during requested Multica dispatch: fail closed; do not
  create a shadow local issue.
- Callback destination unavailable: retain a bounded local outbox and retry.
- Context publisher unavailable: use the last verified bundle and mark it stale.
- Native event stream unavailable: fall back through adapter capability order.
- Parent unavailable: execution may continue; terminal delivery remains queued.
- Adapter cannot prove terminal state: report `unknown`, never infer success.

