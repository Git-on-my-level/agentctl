# State, security, and privacy

## State classes

### Optionally shared and versioned

- schemas and adapter manifests;
- knowledge-source registry entries and pinned source revisions;
- operator-defined roles and capability policy;
- reviewed model/executor routing policy;
- portable skills and runbooks;
- knowledge index metadata;
- public/nonsecret resource aliases.

### Shared through optional existing authorities

- durable-coordinator issues, runs, comments, ownership, and artifact
  references;
- operator-selected network service/resource contracts;
- Git branches, commits, PRs, and CI state.

### Host-local operational state

- execution envelopes;
- normalized journal and event cursors;
- bounded final result bodies, stored by default up to 1 MiB per execution;
- subscription definitions;
- callback outbox, acknowledgements, and dead letters;
- execution result-collection acknowledgements;
- opaque native resume references;
- last verified shared-context bundle.
- bounded daily release-check timestamps and public release metadata.

This state is namespaced by stable host word ID and local store generation. It
is not database-replicated between machines. Automatic journal retention is not
implemented in the preview. Cross-host callbacks exchange bounded metadata
documents and converge on full semantic dedupe keys; portable URIs keep the
origin host explicit.

### Never synchronized by `agentctl`

- raw prompts, reasoning, intermediate output, or transcripts;
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

## Output sensitivity boundaries

agentctl does not implement caller roles or role-dependent rendering. Normal
`status` and `result` projections omit execution working-directory and
repository paths plus opaque native source IDs. Events and callbacks carry
bounded normalized metadata rather than prompts, transcripts, or result
bodies.

Operator-facing diagnostics are intentionally more explicit: configuration
provenance, data inventory, bootstrap status, and install plans can expose
selected local paths, content digests, host-scoped IDs, and artifact metadata.
Treat their JSON as operator-private and redact it before sharing. Credentials
remain forbidden from configuration and normalized journal records rather than
being protected by an output-role system.

## Result storage and event minimization

Normalized events contain state and references, not full native output. By
default, the local journal stores the final UTF-8 outcome body, bounded to 1 MiB
per execution. The explicit `result` command returns it; status, subscriptions,
callbacks, and ordinary events do not copy that content. A successful `result`
or terminal `await` also records a collection acknowledgement so
`recent --unreconciled` can name work that finished but was never read. `--no-store-result`
records an omission tombstone when the caller deliberately does not want result
content persisted. Artifact events identify kind, authority, digest, and
retrieval reference without copying content.

### Retention and cleanup

Automatic retention is not implemented in the preview. Operators must include
the journal in their local data-handling and backup policy and should not run
sensitive work unless this persistence is acceptable.

`agentctl data inventory` is a read-only aggregate of the journal file,
logical bucket bytes, record counts, terminal/nonterminal executions, outcomes,
and stored result bytes. `agentctl data cleanup --before <RFC3339> --plan` is
also read-only. It selects only complete terminal execution graphs strictly
before the cutoff and reports every eligible execution, record category,
logical byte count, protected execution and protection reason.

Deletion requires `--apply --plan-digest <sha256:...>`. Apply recomputes the
plan in one journal write transaction and rejects it if the reviewed digest no
longer matches. The mutation deletes the execution, outcome, events, event
indexes/dedupe records, terminal index, execution idempotency keys, and any
result-collection acknowledgement together.
It is idempotent when the caller reviews the resulting empty plan before a
repeat apply. The logical byte estimate does not promise immediate shrinkage of
the bbolt file; no physical compaction is performed.

Nonterminal graphs, partial parent/supersession graphs, active subscription
filters/cursors/coordinators, and every retained outbox delivery or receipt are
protected. Promotion-linked executions are conservatively never eligible in
this preview because those records can encode authority handoff and recovery
state. Stopped subscriptions alone do not protect a graph, but their retained
deliveries and receipts do. Cleanup never removes subscription, outbox,
receipt, or promotion records themselves.

Debug logs are never required for normal correctness and have a separate
retention switch. Secret scanning fixtures cover errors and malformed native
output.

## Command safety

- Read-only commands have no hidden refresh/fetch/cache-write behavior except
  the documented fail-open daily release notice. That check performs at most
  one bounded GitHub metadata read per UTC day, writes only public release
  metadata and timestamps to a separate owner-only cache, never opens the
  journal, and can be disabled with `AGENTCTL_UPDATE_CHECK=off`. Live capability
  and doctor probes are bounded, declared read-only checks and report freshness;
  a probe that mutates a native cache is not used as a read. `result` and a
  terminal `await` declare `local_operational_write` because they stamp
  collection.
- Native commands are passed as argv, never reconstructed shell strings.
- Callback command adapters receive a path to an event document.
- Mutations use exact IDs/context references and idempotency keys.
- External side effects require explicit mode and appear in `--plan`.
- Destructive cleanup reports exact paths/bytes/retention before action.
- `bootstrap update` is the explicit local reconciliation boundary: detection
  is automatic within the command, but only canonical manifest-bound skill
  assets and already-managed supervisor files may be changed. New services,
  legacy-copy removal, and permission-granting flags remain explicit.

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
