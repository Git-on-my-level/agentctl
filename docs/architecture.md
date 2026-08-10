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
can opt into Multica without changing the normalized event and callback
contract used to observe completion.

Three rules constrain every component:

1. An observation can cite an authority but cannot become that authority.
2. A local alias or cache entry cannot silently become fleet-shared state.
3. A capability is negotiated from evidence, not inferred from a product name.

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

The Multica issue is authoritative after issue-only promotion. Once Multica
binds an exact run, that run becomes the primary execution/liveness source and
the issue remains the workflow source. A nested task or unbound run event is
evidence only and cannot terminalize an issue-only envelope.

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

Promotion is explicit and idempotent only when the Multica adapter can persist
and query an exact authority-owned promotion key. It is a recoverable saga, not
an atomic cross-system transaction. Repeating it returns the existing issue;
absence of a local receipt is never evidence that no issue was created. See
[Execution envelope and lifecycle](execution-envelope.md#promotion-as-a-recoverable-saga).

## Components

### CLI

The CLI is the stable interface for agents and automation. Every command
supports compact text and structured JSON, and reports source authority,
observation time, and freshness. Text is token-efficient and line-oriented;
JSON is the normative machine contract.

### Adapter library

Adapters translate native state and events into the execution envelope without
owning model selection or conversation semantics. Native arguments pass through
as an argv array after `--`.

### Local journal

The local journal contains operational metadata only. It is owner-only,
append-oriented, bounded by retention, and partially rebuildable from native
sources where adapters support exact history. Word aliases, delivery receipts,
and observation-order events may not be reconstructable, so rebuildability is
reported per record rather than promised globally. It is not a transcript store.

### Optional local supervisor

The initial CLI can supervise foreground processes without a daemon. A small
managed local supervisor is justified only for cross-restart subscriptions,
remote callbacks, or retrying a delivery while no parent process is alive.

The supervisor must remain host-local. It exposes a Unix socket by default;
Tailnet HTTP exposure is explicit, authenticated, and read-only unless a
separate mutation capability is granted.

### Daemonless boundary

The CLI-only design and the optional supervisor have intentionally different
guarantees:

| Behavior | Daemonless process | Managed supervisor |
| --- | --- | --- |
| Wrap and observe a foreground child | While the process lives | Yes |
| Attach/poll durable backend state | While a command lives | Continuously |
| Persist journal/outbox before exit | Yes | Yes |
| Retry automatically after command exit or logout | No | Yes |
| Wake a remote parent after reboot | No | Yes, after recovery |
| Recover queued work on the next manual invocation | Yes | Yes |

Backgrounding `agentctl await` is still process-scoped supervision. The MVP
must not describe it as reboot-, logout-, or crash-surviving delivery. Installing
or enabling the supervisor is an explicit host-state mutation owned by a host
manager when one exists.

### Shared context bundle

One or more private Git repositories are the authoring sources. They remain
independently owned and may be structured, loose, or hybrid corpora hosted on
GitHub, Forgejo, or another Git server. A publisher validates and compiles their
pinned revisions with policy, portable skills, and routing data into a
content-addressed bundle with a machine-readable index. A client installs a
verified local cache and continues to work from the last valid revision when
the publisher is unavailable.

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

Host-local IDs are globally qualified by their origin host ID. Imported source
objects bind a word alias to a full authority/source fingerprint. Bare IDs are
resolved only in the current explicit origin context; cross-host references use
the URI rules in [Identifiers](identifiers.md). Display labels never authorize a
mutation.

## Local and shared convergence

The local journal is the authority only for `agentctl` observations, cursor
checkpoints, and delivery attempts created on that host. It is not replicated as
a database. Cross-host consumers exchange bounded event/callback documents and
converge on full semantic dedupe keys.

State becomes shared only through an existing owner:

- promotion/idempotency bindings are stored as Multica metadata when Multica
  owns the durable lifecycle;
- host and resource aliases come from the fleet/resource authority;
- context revisions come from the verified Git bundle;
- native session/run outcome remains at the native CLI or Multica.

Conflicting local aliases are preserved with provenance and re-aliased; they
never use last-write-wins. Promotion and supersession are append-only relations,
not in-place authority changes.

## Portability boundary

Core contracts use process, file, Unix socket, and signed HTTP primitives. They
do not depend on Hermes APIs or a particular agent's memory layout. Backend
adapters declare whether they can inject a context path, environment handle,
native instruction reference, or Multica artifact/reference. If no reviewed
mechanism exists, `context_injection` is unavailable and required injection
fails before launch. A generic process adapter never edits prompts to simulate
support.

## Failure boundaries

- Multica unavailable during requested Multica dispatch: fail closed; do not
  create a shadow local issue.
- Callback destination unavailable: retain a bounded local outbox and retry.
- Context publisher unavailable: use the last verified bundle and mark it stale.
- Native event stream unavailable: fall back through adapter capability order.
- Parent unavailable: execution may continue; terminal delivery remains queued.
- Adapter cannot prove terminal state: report unknown liveness/integrity while
  retaining the last known state; never infer success.
- Local crash during promotion: reconcile the authority-owned promotion key;
  never create from local absence alone.
- Contradictory terminal evidence: preserve both claims, mark integrity
  conflicted, and fail outcome-dependent commands closed.
