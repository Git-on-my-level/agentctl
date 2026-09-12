# Repository instructions

This repository implements a portable agent coordination contract.

When changing code or design:

- Preserve native agent CLIs and Multica as execution/task authorities.
- Do not add a second issue board, conversation store, or shared credentials DB.
- Keep all user-facing identifiers LLM-friendly and typed.
- Make every behavior deterministic without requiring a model to remember policy.
- Define JSON shapes, exit behavior, idempotency, and failure semantics alongside
  compact agent-facing text.
- Prefer explicit authority and provenance over a falsely unified state.
- Treat event delivery as at-least-once and require consumer idempotency.
- Keep raw prompts and transcripts out of the default `agentctl` journal.
- Design read-only commands without hidden network or filesystem mutations.

Before accepting a design change, check it against both direct/native and
Multica-backed execution modes.

Run `make ci` and the focused race tests for changed stateful packages. Do not
claim a native or Multica capability that the current adapter can only infer.

## Structured delegation direction

For delegation-interface work, follow [the agreed structured delegation design](docs/structured-delegation.md):
the parent interprets natural language; agentctl resolves explicit constraints
and configured defaults deterministically. Keep roles outside the initial scope.
Check live help before using commands; ship schemas, help, and the portable
skill together with implementation.
