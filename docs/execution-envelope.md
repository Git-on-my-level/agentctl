# Execution envelope and lifecycle

## Purpose

The execution envelope is the smallest common record needed to observe work
across native agent CLIs and Multica. It deliberately excludes conversation
content and project-management semantics.

## Core record

```json
{
  "schema_version": 1,
  "id": "exec-purple-monkey-dragon-river-candle",
  "authority": "native",
  "adapter": "codex",
  "mode": "direct",
  "host": "m5-mbp",
  "state": "running",
  "source_state": "turn.started",
  "source_ref": {
    "kind": "codex_thread",
    "id": "opaque-source-id"
  },
  "capabilities": ["stream", "poll", "resume", "cancel"],
  "cwd": null,
  "repository": null,
  "parent_execution_id": null,
  "created_at": "2026-08-10T18:00:00Z",
  "started_at": "2026-08-10T18:00:01Z",
  "updated_at": "2026-08-10T18:00:10Z",
  "terminal_at": null,
  "observation": {
    "source": "native_stream",
    "observed_at": "2026-08-10T18:00:10Z",
    "fresh_for_seconds": 15
  }
}
```

Paths and opaque native IDs are sensitivity-controlled. Human output omits
them by default.

## Normalized states

```text
created
starting
running
waiting
attention
completed
failed
cancelled
orphaned
unknown
```

`attention` means execution may continue but a parent decision or intervention
is required. It is not a terminal state.

`unknown` is an observation result, not a guess. Adapters use it when they
cannot prove liveness or terminal state.

## State invariants

- Terminal states never transition back to nonterminal states for the same
  execution ID.
- A retry or resumed native turn may remain the same execution only if the
  source authority considers it the same session and task contract.
- A reassignment to a different Multica run or native session creates a new
  execution and records `supersedes`/`superseded_by` links.
- Observation freshness is independent of execution state.
- Source state is retained so callers can distinguish information lost during
  normalization.
- Completion is not success evidence by itself. Artifacts and acceptance must
  be verified by the owning parent/coordinator.

## Task contract metadata

The envelope may reference, but does not own, a task contract:

```json
{
  "objective": "...",
  "side_effect_boundary": "read_only",
  "acceptance_ref": "context-gentle-comet-maple-badger-valley",
  "expected_artifact_kinds": ["report"],
  "continuation": {
    "same_session_required": true
  }
}
```

For Multica, the issue is authoritative for this contract. For direct work, the
parent may provide the contract at launch. `agentctl` stores a reference or
small structured summary, not the full prompt.

## Parent and child executions

Parent/child links express observation relationships, not automatic scheduling.
The parent remains responsible for synthesis unless an external authority such
as Multica explicitly owns orchestration.

Fan-out should carry:

- parent execution ID;
- bounded child objective;
- callback destination;
- terminal and attention events;
- artifact references.

Children do not inherit credentials or raw parent transcripts through the
envelope.

## Promotion

Promotion from direct work to Multica creates:

1. one idempotent Multica issue;
2. a distilled handoff document;
3. a link from direct execution to issue and first run;
4. a promotion event;
5. a subscription rotation from the direct execution to the authoritative
   Multica run when requested.

The direct execution remains immutable historical evidence. Promotion never
claims the native session itself became portable.

## Retention

Default proposal:

- nonterminal executions: retain until terminal or explicitly abandoned;
- terminal operational envelopes/events: 14 days locally;
- delivery receipts and dead letters: 30 days;
- artifact content: never copied by default;
- word-ID aliases and promotion/supersession links: retain while referenced.

Retention is configurable by policy but always bounded and visible through
`agentctl doctor` and `agentctl storage plan`.

