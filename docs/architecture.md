# Architecture

## Thesis

`agentctl` is a local supervision and context layer, not a universal agent
runtime and not a project-management system. Its standalone core runs without
a fleet controller, shared network, or durable coordination service.

The system separates four concerns that are often accidentally coupled:

1. **Task authority** — normally the native session, or an explicitly selected
   optional durable coordinator.
2. **Execution** — Codex, Claude Code, Cursor, OMP, or another native backend.
3. **Observation** — normalized state, events, health, and artifacts.
4. **Delivery** — waking a parent agent or sending a callback reliably.

This keeps ordinary work lightweight while an operator can opt into durable
coordination without changing the normalized event and callback contract used
to observe completion.

Three rules constrain every component:

1. An observation can cite an authority but cannot become that authority.
2. A local alias or cache entry cannot silently become fleet-shared state.
3. A capability is negotiated from evidence, not inferred from a product name.

## Planes and owners

| Plane | Owner | `agentctl` responsibility |
| --- | --- | --- |
| Direct execution | Native CLI/session | Launch or attach; retain native reference |
| Optional durable coordination | Multica | Create only when explicitly selected; subscribe and reconcile |
| Optional resource topology | Operator-selected network/index tool | Consume explicit structured output; do not reimplement resolution |
| Optional host state | Operator-selected host manager | Consume context/doctor output when present |
| Optional shared context | Git-authored verified bundle | Select and render deterministic context |
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
observation time, and freshness. JSON is the default normative machine
contract; callers opt into text with `--output text` when a compact line
projection is useful. `agentctl help <topic>` provides progressive, just-in-time
usage, defaults, examples, and typed `next_actions` so a skill need not carry
every flag.

### Adapter library

Adapters translate native state and events into the execution envelope without
owning model selection or conversation semantics. Native arguments pass through
as an argv array after `--`.

### Local journal

The local journal is owner-only and append-oriented. It contains normalized
operational metadata plus an explicitly retrievable final result body of at
most 1 MiB per execution by default. It does not store raw prompts,
intermediate output, reasoning, or transcripts. Automatic retention is not
implemented in the preview. Word aliases, results, delivery receipts, and
observation-order events may not be reconstructable, so rebuildability is
reported per record rather than promised globally.

### Optional local supervisor

The initial CLI can supervise foreground processes without a daemon. A small
managed local supervisor is justified only for cross-restart subscriptions,
remote callbacks, or retrying a delivery while no parent process is alive.

The supervisor remains host-local and exposes an owner-only Unix socket.
Remote callback delivery is explicit and authenticated; agentctl does not
publish the supervisor as a general remote execution endpoint.

### Daemonless boundary

The CLI-only design and the optional supervisor have intentionally different
guarantees:

| Behavior | Daemonless process | Managed supervisor |
| --- | --- | --- |
| Wrap and observe a foreground child | While the process lives | Yes |
| Own an explicit background native child | Detached worker until exit | Not owned by callback supervisor |
| Attach/poll durable backend state | While a command lives | Continuously |
| Persist journal/outbox before exit | Yes | Yes |
| Retry automatically after command exit or logout | No | Yes |
| Wake a remote parent after reboot | No | Yes, after recovery |
| Recover queued work on the next manual invocation | Yes | Yes |

`run --background` is also process-scoped supervision: a detached agentctl
worker owns the native child, while another command observes its journaled
metadata. It is noninteractive, has no controlling terminal, and is not reboot-
or crash-surviving authority. A direct adapter does not gain cross-process
cancellation unless its negotiated capability explicitly provides it. Backgrounding
`agentctl await` only detaches an observer and does not change ownership.
Creating
or enabling a new supervisor service is an explicit host-state mutation owned
by a host manager when one exists. `agentctl bootstrap update` may reconcile an
already-managed supervisor during a release update, but never creates a new
service merely because a harness was detected.

### Bootstrap reconciliation

`agentctl bootstrap status` is a read-only inventory of binary resolution,
detected harnesses, canonical skill roots, managed revisions, and drift. The
default `agentctl bootstrap update` discovers supported harnesses, deduplicates
shared roots (Codex and OMP commonly share `~/.agents/skills`), and atomically
installs or upgrades only manifest-bound portable-skill assets. `--dry-run`
shows exact paths and side effects. Legacy copies, unmanaged skills, and new
supervisor services remain untouched unless an explicit cleanup or host-manager
operation authorizes them.

`scripts/install.sh` composes this reconciliation into a binary release: after
the managed binary is installed it runs the same detected update and refreshes
an existing launchd supervisor only when its manifest is agentctl-owned.
`--binary-only` is the explicit binary-only escape.

### Shared context bundle

One or more independently owned public or private Git repositories may be
authoring sources. They may be structured, loose, or hybrid corpora hosted on
GitHub, Forgejo, or another Git server. A publisher validates and compiles
pinned revisions into a content-addressed bundle with a machine-readable index.
A client installs a verified local cache and continues to work from the last
valid revision when the publisher is unavailable.

## Routing policy

`agentctl route explain` is advisory and deterministic. It never creates an
issue or launches an agent.

A selector string such as `studio omp` is ranked against optional config
keywords (`route.hosts`, `agent_preferences.preferred`, plus built-in adapter
family aliases). The result is ranked host and model lists plus a placement
mode (`local`, `remote`, `need_placement`, `ambiguous_host`, or `no_host`).
Empty lists mean nothing was recognized. The caller chooses among hits.

The legacy flag form (`--model-family`, `--needs-pr`, …) still explains
direct versus a durable authority. Uncertainty in that form resolves to
`direct`, because promotion remains available later.

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
  owns the optional durable lifecycle;
- host and resource aliases, when used, come from the operator's chosen
  fleet/resource authority;
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
