# State, security, and privacy

## State classes

### Shared and versioned

- schemas and adapter manifests;
- knowledge-source registry entries and pinned source revisions;
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

This state is namespaced by stable host word ID and local store generation. It
is not database-replicated between machines. Cross-host callbacks exchange
bounded documents and converge on full semantic dedupe keys; portable URIs keep
the origin host explicit.

### Never synchronized by `agentctl`

- raw prompts, reasoning, or transcripts;
- Hermes/Codex/Claude/Cursor/OMP session databases;
- authentication tokens, SSH keys, cookies, Keychain values;
- environment dumps;
- worktrees and uncommitted source;
- platform chat routing databases;
- arbitrary logs or product data.

Loose knowledge ingestion does not weaken these exclusions. Repository-level
ACL and sensitivity boundaries are checked before compilation; include globs
cannot override a source's exclusions or import credential-local/raw-data
paths. Every emitted unit retains source repository, commit, path, and content
digest provenance so removal or quarantine can target the actual authority.

## Filesystem policy

All mutable local state lives below `AGENTCTL_STATE_HOME`, then
`XDG_STATE_HOME/agentctl`, then the documented platform default. Cache data uses
`XDG_CACHE_HOME` or its platform equivalent. Resolved roots are shown by
`agentctl doctor` without exposing unrelated home contents. Unix defaults are
owner-only:

```text
~/.local/state/agentctl/       0700
~/.local/state/agentctl/**     0600 files / 0700 directories
~/.cache/agentctl/             rebuildable bundle/index cache
```

Temporary files use the same filesystem as their atomic destination when
required and are created owner-only. Symlink traversal and path escape are
rejected.

State writes take an exclusive store lock, write a checksummed record, flush
according to the command's durability class, and atomically advance the visible
revision. A second process either observes the old or new complete revision.
Read-only commands may take read locks and never repair state implicitly.

Permissions are verified on startup. A group/world-accessible state root causes
sensitive operations to fail closed with an exact remediation plan. File modes
do not protect against another process running as the same OS user; adapters and
callback commands are therefore treated as trusted code even though their
parsed output is untrusted data.

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

The host word ID and Tailnet reachability are locators/evidence, not signatures.
Webhook canonicalization, acknowledgement, retry classification, redirect, and
destination-resolution rules are normative in
[Events, subscriptions, and callbacks](events-and-subscriptions.md#webhook-protocol-and-security).

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

The initial threat model does not defend a journal from a malicious process
already executing as the same user with unrestricted filesystem access. Stronger
isolation requires OS sandboxing or a separate service identity and is not
implied by owner-only modes.

## Recovery

- Journal append and sequence allocation are one atomic transaction. Cursor
  advancement is a separate atomic acknowledgement transaction, so a crash may
  redeliver but cannot skip an unacknowledged event.
- On restart, nonterminal executions are re-probed before events resume.
- Unavailable evidence sets liveness/integrity unknown while retaining the last
  known state. `orphaned` requires proof that the exact attempt cannot continue
  and no authoritative result can be recovered.
- Delivery retries resume from durable receipts/outbox.
- Corrupt local operational state can be quarantined and rebuilt without
  mutating native or Multica authority.

Recovery never treats local absence as proof of remote absence. Promotion first
queries the authority-owned idempotency key, callback recovery retains delivery
and event IDs, and alias recovery compares full source fingerprints. Quarantine
is an explicit local write; read-only commands report corruption and a plan.
