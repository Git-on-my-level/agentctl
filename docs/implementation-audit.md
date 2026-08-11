# Implementation audit

Snapshot: 2026-08-11, v0.1 development branch.

This is the post-integration acceptance record. It separates implemented
contracts from intentional adapter/deployment limits; package primitives alone
are not counted unless a CLI, supervisor, or Multica endpoint consumes them.

## Mechanical status

- `make ci`: passes Go vet, all Go tests, schema checks, link checks, and build.
- `go test -race ./...`: passes.
- The distribution validator passes in disposable harness roots.
- Release packaging produces deterministic archives for macOS and Linux on
  amd64/arm64.
- The companion Multica worktree passes fresh migrations through 254, service,
  handler, server, CLI, bounded retention-pruner, migration lint, build, vet,
  and down-migration tests.

## Implemented end-to-end

| Area | Evidence and boundary |
| --- | --- |
| IDs | Closed typed registry; frozen BIP-39 v1 list and digest; 60-bit payload + type-bound checksum; typed wrappers and portable URI parsing. Normal output never leads with native UUIDs. |
| Journal | Owner-only bbolt store; schema validation; CAS execution updates; event sequence; semantic-key recomputation; one terminal event; subscriptions, outbox, receipts, and restart durability. |
| Native work | `run`, local binding `attach`, `status`, `events`, `await`, `result`, and adapter-dependent `cancel`; preallocated IDs for live observation/subscription; bounded journal transactions and expiring runner leases while children run; exact argv after `--`; deterministic adapter inference for known executables; bounded structured parsers; raw prompts/transcripts/results excluded from the journal. |
| Routing | Deterministic `route explain` keeps model choice in the native harness and chooses direct versus Multica from explicit work properties. It never creates an issue. |
| Promotion | Exact configured Multica binary/profile/workspace/server; plan mode; authority-owned semantic client key; exact retry; changed-input conflict; persisted source/target links; optional supersession; replay returns the same stored issue alias and one lifecycle. |
| Knowledge | Validated GitHub/Forgejo/generic registrations; explicit noninteractive sync; loose/structured/hybrid ingest; deterministic provenance, bundle, lexical index, verification, atomic install, selection, and bounded render. |
| Callbacks | Atomic event fanout; file, acknowledged Unix, command, and HMAC webhook transports; TTL, retry, pause, receipt, and dead-letter state; SSRF, DNS rebinding, redirect, expiry, and replay controls. |
| Supervisor | Real journal/runtime/outbox bridges; restart reprobe; owner-only Unix status RPC; one-shot or long-running cycle; launchd/systemd plans. |
| Portability | Allowlisted skill and Multica runtime bundle; detected-harness bootstrap reconciliation plus installer/status/doctor preserve unmanaged harness state and never copy auth, memories, sessions, settings, caches, prompts, or worktrees. |
| Multica events | Transactional event outbox; workspace monotonic sequence; authenticated membership; exact filters; bounded cursor API; list/watch CLI; issue-create client-key idempotency; preview-first, bounded contiguous-prefix retention pruning. |

## Authority invariants

1. Native agent CLIs own direct session semantics and correctness.
2. Multica owns durable issue, run, assignment, and review state.
3. agentctl may observe and correlate both, but does not invent a second issue
   lifecycle or silently promote direct work.
4. One promotion key identifies one semantic source/destination handoff. Exact
   retries reuse it; changed semantics conflict.
5. A callback receipt proves delivery only. It never marks work successful.
6. Shared knowledge is derived from explicit Git authorities. The compiled
   bundle is immutable data, not shared session state.
7. Credentials, raw transcripts, native databases, and worktrees remain local.

## Agent-first defaults

The release path is intentionally optimized for an agent that has only the
binary and progressive help available:

- `agentctl help <topic>` supplies usage, defaults, examples, and typed
  `next_actions`; the portable skill points agents there instead of duplicating
  every flag.
- JSON is the default output. `--output text` is an explicit compact escape.
- `doctor` discovers harnesses and checks launch, observation, and result
  readiness. `capabilities` returns a concise live summary; `--full` and
  `--static` are explicit manifest/probe escapes.
- `run` infers known adapters, bounds work to thirty minutes, and preflights
  `launch,result_content`. `result` requires stored content and `await` stops
  on attention unless the caller supplies `--ignore-attention`.
- Subscriptions default to terminal/attention/artifact, and
  `bootstrap update` reconciles detected canonical roots while leaving
  unmanaged files, legacy copies, and new supervisor services untouched.

## Intentional v0.1 constraints

These are explicit limits, not silently degraded promises:

- Native adapters generally supervise and reattach only within the launching
  process. On restart, unsupported native sessions become `unreachable` rather
  than being guessed alive or complete.
- Live cross-process observation uses a preallocated execution ID. Native
  cross-process cancellation remains unavailable unless the adapter exposes a
  reviewed durable cancel mechanism.
- `status` and `result` read the normalized journal envelope. A fresh native
  probe occurs through the runtime/supervisor only where the adapter declares a
  reviewed capability.
- Local journal pagination uses execution sequence, while Multica uses its own
  durable filter-bound cursor contract. A universal cross-authority cursor is
  not claimed.
- Outbound signed webhooks are implemented; agentctl is not a general inbound
  remote execution receiver.
- Creating a new supervisor service is plan-only. The operator or host manager
  owns reviewed service-file installation; bootstrap may update an
  already-managed supervisor but does not create one by detection alone.
- Bundle compilation/install is local. Publishing it into the Tailnet bootstrap
  and its checksum manifest is a separate reviewed deployment step.
- Journal retention is not automatic in v0.1. No record referenced by an
  execution, subscription, receipt, or promotion is silently deleted.
- The frozen BIP-39 word list is not optimized for speech confusion or cultural
  screening. The type checksum catches most transcription mistakes; a future
  codec version can improve word choice without rewriting v1 IDs.

## Security review summary

- Files and directories created by agentctl use owner-only modes and reject
  symlink components.
- Config and journal readers reject unmanaged or unsafe permissions.
- Callback documents and projections are strict, bounded JSON. Sensitive key
  names and raw transcript-shaped payloads are rejected.
- Webhook transport disables ambient proxies and redirects, resolves DNS again
  at connect time, and rejects public plaintext HTTP and private/link-local/
  metadata destinations unless the exact host was explicitly allowlisted.
- Git fetch is explicit and noninteractive; URLs with credentials, query, or
  fragments are rejected and diagnostics are redacted.
- Multica event payloads omit comment bodies, prompts, task results,
  transcripts, and storage URLs.

## Release gates

Before tagging v0.1:

1. land the companion Multica PR and deploy a version exposing `event
   list|watch` plus issue `--client-key`;
2. run one real direct execution, exact promotion replay, Multica event watch,
   and terminal callback canary against the target profile;
3. regenerate distribution revision/hash manifests from the release commit;
4. verify the configured Multica `app_url` and signed binary provenance;
5. install the portable skill into disposable Hermes, Codex, Claude, Cursor,
   OMP, and Multica roots and run the distribution doctor;
6. decide the first journal retention policy from measured data rather than
   enabling destructive cleanup by default.

## Highest-value follow-ups

1. Add a read-only Tailnet host/capability resolver and cross-host URI attach.
2. Add a reviewed bundle publisher that extends the existing `tailnetctl`
   bootstrap/checksum contract without making tailnetctl an agent control plane.
3. Add explicit retention inventory and approval-gated cleanup.
4. Expand golden schemas for subscriptions, deliveries, context results, and
   Multica cursor pages.
5. Add an independent canonical-event fixture reader alongside the implemented
   independent v1 ID codec and golden corpus.
