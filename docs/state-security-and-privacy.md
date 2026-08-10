# State, security, and privacy

## State classes

### Shared and versioned

- schemas and adapter manifests;
- fleet roles and capability policy;
- reviewed model/executor routing policy;
- portable skills and runbooks;
- knowledge index metadata;
- public/nonsecret resource aliases.

### Shared through existing authorities

- Multica issues, runs, comments, ownership, and artifact references;
- Tailnet service/resource contract;
- Git branches, commits, PRs, and CI state.

### Host-local operational state

- execution envelopes;
- event cursor and short-retention normalized journal;
- subscription definitions;
- callback outbox, acknowledgements, and dead letters;
- opaque native resume references;
- last verified shared-context bundle.

### Never synchronized by `agentctl`

- raw prompts, reasoning, or transcripts;
- Hermes/Codex/Claude/Cursor/OMP session databases;
- authentication tokens, SSH keys, cookies, Keychain values;
- environment dumps;
- worktrees and uncommitted source;
- platform chat routing databases;
- arbitrary logs or product data.

## Filesystem policy

All mutable local state lives below an XDG-compatible state root and defaults to
owner-only permissions:

```text
~/.local/state/agentctl/       0700
~/.local/state/agentctl/**     0600 files / 0700 directories
~/.cache/agentctl/             rebuildable bundle/index cache
```

Temporary files use the same filesystem as their atomic destination when
required and are created owner-only. Symlink traversal and path escape are
rejected.

## Authentication and authorization

Word IDs are locators, never credentials.

Adapters use the native CLI's configured authentication. `agentctl` does not
extract or centralize tokens. Callback credentials are stored as local
references and redacted from plans, logs, process arguments, and event payloads.

Remote callbacks authorize:

- Tailnet/source identity;
- subscription ID;
- allowed event scope;
- request signature and freshness;
- replay/idempotency key.

A valid callback cannot mutate or cancel an execution unless it uses a separate
explicit mutation capability.

## Sensitivity classes

Proposed classes:

```text
public
fleet-internal
operator-private
project-confidential
credential-local
```

Repository and transport boundaries enforce classes. Output renderers redact
paths, native source IDs, host details, and artifact metadata according to the
caller's role and requested output mode.

## Event minimization

Normalized events contain state and references, not full native output. Result
summaries are opt-in, bounded, and redacted. Artifact events identify kind,
authority, digest, and retrieval reference without copying content.

Debug logs are never required for normal correctness and have a separate
retention switch. Secret scanning fixtures cover errors and malformed native
output.

## Command safety

- Read-only commands have no hidden refresh/fetch/cache-write behavior.
- Native commands are passed as argv, never reconstructed shell strings.
- Callback command adapters receive a path to an event document.
- Mutations use exact IDs/context references and idempotency keys.
- External side effects require explicit mode and appear in `--plan`.
- Destructive cleanup reports exact paths/bytes/retention before action.

## Threat boundaries

The initial design assumes:

- a local unprivileged user may run several trusted harnesses;
- a native agent may emit malformed or adversarial output;
- callback delivery may be duplicated, reordered, delayed, or replayed;
- a backend may report success incorrectly;
- a shared context publisher may be stale or temporarily unavailable;
- machines may have different adapter/backend versions.

Therefore all native output is parsed as untrusted data, external success claims
are independently verifiable, callbacks are idempotent, and version skew is
reported before critical dispatch.

## Recovery

- Journal append and cursor updates are atomic and crash-safe.
- On restart, nonterminal executions are re-probed before events resume.
- Unknown liveness yields `unknown`/`orphaned` according to explicit adapter
  evidence, never automatic success.
- Delivery retries resume from durable receipts/outbox.
- Corrupt local operational state can be quarantined and rebuilt without
  mutating native or Multica authority.

