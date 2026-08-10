# Repository instructions

This repository defines an agent coordination contract before implementation.

When changing the design:

- Preserve native agent CLIs and Multica as execution/task authorities.
- Do not add a second issue board, conversation store, or shared credentials DB.
- Keep all user-facing identifiers LLM-friendly and typed.
- Make every behavior deterministic without requiring a model to remember policy.
- Define JSON shapes, exit behavior, idempotency, and failure semantics alongside
  human-facing UX.
- Prefer explicit authority and provenance over a falsely unified state.
- Treat event delivery as at-least-once and require consumer idempotency.
- Keep raw prompts and transcripts out of the default `agentctl` journal.
- Design read-only commands without hidden network or filesystem mutations.

Before accepting a design change, check it against both direct/native and
Multica-backed execution modes.

