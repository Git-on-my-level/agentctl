# Roadmap

## Phase 0 — Freeze contracts

- Review authority boundaries and non-goals.
- Freeze identifier encoding and curated word-list requirements.
- Version execution/event/error JSON schemas.
- Define stable exit codes.
- Define adapter capability manifest.
- Create fixture corpus for IDs, state transitions, malformed streams, and
  callback deduplication.

Exit criterion: two independent implementations could produce compatible IDs,
events, and errors from the written contract.

## Phase 1 — Daemonless local MVP

- `agentctl run`, `attach`, `status`, `events`, `await`, and `result`.
- Codex and Cursor adapters, based on proven structured CLI streams.
- Generic process adapter.
- Owner-only local journal with automatic TTL cleanup.
- Word IDs and contextual references.
- JSON output/error/next-action contract.
- Fixture-driven self-test and `doctor --json`.

Exit criterion: a parent can launch or attach to Codex/Cursor, background
`await`, and receive one accurate terminal package without polling logic in the
parent.

## Phase 2 — Multica and promotion

- Multica adapter with exact issue/run reconciliation.
- Explicit direct-to-Multica promotion with idempotency.
- Subscription rotation/supersession.
- Adapter-level bounded polling fallback.
- Multica profile/app URL/binary provenance checks in doctor.

Exit criterion: direct investigation can promote once into a single durable
issue, and both modes produce the same normalized terminal callback.

## Phase 3 — Shared context

- Git schema and validation pipeline.
- Compiled knowledge/policy bundle and lexical index.
- Atomic verified distribution via the shared Tailnet bootstrap path.
- Deterministic context matcher and renderer.
- Portable allowlisted skill distribution for major harnesses.
- Automatic context injection in adapters and Multica runtimes.

Exit criterion: Hermes, Codex, Claude, Cursor, OMP, and Multica receive the same
revisioned task-relevant context without copying a Hermes profile.

## Phase 4 — Reliable remote callbacks

- Optional managed local supervisor.
- Unix socket and signed Tailnet webhook destinations.
- Durable retry outbox, acknowledgements, dead letter, and recovery.
- Remote execution URIs and host capability discovery.
- Cross-restart subscription tests.

Exit criterion: terminal delivery survives sender or receiver restart and does
not duplicate user-visible completion.

## Phase 5 — Multica durable events

- Contribute a durable server-side event outbox to the Multica fork.
- Add sequence/cursor API and CLI event watch.
- Add event-sink delivery health and receipts.
- Prefer outbox over polling while preserving polling fallback.

Exit criterion: Multica task terminal events are durable and observable even
when issue status does not transition or WebSocket connectivity is interrupted.

## Deliberate non-goals

- universal chat UI or conversation store;
- another issue board;
- a model gateway;
- automatic model-performance self-modification;
- credential synchronization;
- raw transcript aggregation;
- replacement for Multica, native CLIs, `tailnetctl`, or `macctl`;
- automatic creation of Multica issues from prompt keywords.

## Early decisions still requiring prototypes

1. Exact 2,048-word list and tokenizer/spoken-confusion scoring.
2. Checksum bit layout and cross-language reference implementation.
3. Whether daemonless attach can observe all target native sessions reliably.
4. Minimal callback receiver interface supported by each parent harness.
5. Whether opaque native session IDs need sensitivity redaction by default.
6. Retention defaults after measuring event volume.
7. How Multica exposes stable user-facing word aliases without replacing its
   internal UUIDs and existing short issue IDs.

