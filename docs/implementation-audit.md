# Implementation audit

Snapshot: current public-preview preparation branch.

This is the post-integration acceptance record. It separates implemented
contracts from intentional adapter/deployment limits; package primitives alone
are not counted unless a CLI, supervisor, or optional integration consumes
them.

## Mechanical status

- `make ci`: passes Go vet, all Go tests, schema checks, link checks, and build.
- `go test -race ./...`: passes.
- The distribution validator passes in disposable harness roots.
- Release packaging produces deterministic archives for macOS and Linux on
  amd64/arm64.
- Optional Multica compatibility is verified separately from the standalone
  release gate and is not a prerequisite for native execution.

## Implemented end-to-end

| Area | Evidence and boundary |
| --- | --- |
| IDs | Closed typed registry; frozen BIP-39 v1 list and digest; 60-bit payload + type-bound checksum; typed wrappers and portable URI parsing. Normal output never leads with native UUIDs. |
| Journal | Owner-only bbolt store; schema validation; CAS execution updates; event sequence; semantic-key recomputation; one terminal event; subscriptions, outbox, receipts, execution acknowledgements, and restart durability. |
| Native work | `run`, `recent`, read-only actionable `inbox`, local binding `attach`, `status`, `events`, `await`, `result`, and adapter-dependent `cancel`; inbox separates task outcome from tool liveness, uses a bounded caller-visible observation age, and keeps conflicted evidence actionable after collection; preallocated IDs for live observation/subscription; bounded journal transactions and expiring runner leases while children run; exact argv after `--`; deterministic adapter inference for known executables; bounded structured parsers; raw prompts, intermediate output, and transcripts excluded from the journal; final result content stored by default up to 1 MiB. |
| Routing | Deterministic `route explain` ranks reviewed host/model selectors and reports local versus explicitly configured remote placement. It never interprets task prose, creates an issue, dispatches work, or returns an execution handle. |
| Optional dispatch | Live Multica agent/runtime join by exact runtime binding; one online routed host/provider/concrete-model match; private exact assignee ID; required caller idempotency key; prompt stdin with digest-only journal retention; tracked Multica execution; foreground `await` event reconciliation; review/blocked attention mapping; no local fallback. |
| Optional promotion | Exact configured Multica binary/profile/workspace/server; plan mode; authority-owned semantic client key; exact retry; changed-input conflict; persisted source/target links; optional supersession; replay returns the same stored issue alias and one lifecycle. |
| Knowledge | Validated GitHub/Forgejo/generic registrations; explicit noninteractive sync; loose/structured/hybrid ingest; deterministic provenance, bundle, lexical index, verification, atomic install, selection, and bounded render. |
| Callbacks | Atomic event fanout; file, acknowledged Unix, command, and HMAC webhook transports; TTL, retry, pause, receipt, and dead-letter state; SSRF, DNS rebinding, redirect, expiry, and replay controls. |
| Supervisor | Real journal/runtime/outbox bridges; restart reprobe; owner-only Unix status RPC; one-shot or long-running cycle; launchd/systemd plans. |
| Portability | Allowlisted skill and Multica runtime bundle; detected-harness bootstrap reconciliation plus installer/status/doctor preserve unmanaged harness state and never copy auth, memories, sessions, settings, caches, prompts, or worktrees. |
| Optional Multica events | Transactional event outbox; workspace monotonic sequence; authenticated membership; exact filters; bounded cursor API; list/watch CLI; issue-create client-key idempotency; preview-first, bounded contiguous-prefix retention pruning in the companion integration. |

## Authority invariants

1. Native agent CLIs own direct session semantics and correctness.
2. When configured, Multica owns its durable issue, run, assignment, and review
   state; standalone operation does not require it.
3. agentctl may observe and correlate both, but does not invent a second issue
   lifecycle or silently promote direct work.
4. One promotion key identifies one semantic source/destination handoff. Exact
   retries reuse it; changed semantics conflict.
5. One dispatch key identifies one workspace-scoped task intent. Exact retries
   recover the same Multica issue and local execution; changed semantics conflict.
6. A callback receipt proves delivery only. It never marks work successful.
7. Shared knowledge is derived from explicit Git authorities. The compiled
   bundle is immutable data, not shared session state.
8. Credentials, raw transcripts, native databases, and worktrees remain local.

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
- `run` infers known adapters, has no default wall-clock kill, accepts bounded
  explicit file/stdin prompt transport, and preflights `launch,result_content`.
  `fanout` coordinates shared or distinct prompts across explicit native argv
  vectors with whole-batch preflight, names, and persisted labels, in the
  foreground without creating a group authority. `result` requires stored
  content and `await` stops on attention unless the caller supplies
  `--ignore-attention`. `recent --unreconciled` recovers terminal work whose
  result was never acknowledged.
- Subscriptions default to terminal/attention/artifact, and
  `bootstrap update` reconciles detected canonical roots while leaving
  unmanaged files, legacy copies, and new supervisor services untouched.

## Intentional preview constraints

These are explicit limits, not silently degraded promises:

- Native adapters generally supervise and reattach only within the launching
  process. On restart, unsupported native sessions become `unreachable` rather
  than being guessed alive or complete.
- `fanout` has the same foreground ownership boundary. It does not survive the
  invoking process, logout, or reboot, and the callback supervisor remains a
  delivery service rather than a native process manager.
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
- Bundle compilation/install is local. Publishing it into an operator's network
  or fleet bootstrap is a separate reviewed deployment step.
- Journal retention is not automatic in the preview. `data inventory` and digest-bound
  `data cleanup --plan|--apply` expose an explicit terminal-graph lifecycle. No
  nonterminal, active-subscription, delivery/receipt, partial-lineage, or
  promotion-linked execution is silently deleted.
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

## Standalone public-preview release gates

Before tagging a public preview:

1. pass full CI, focused race tests, schema/link checks, vulnerability checks,
   and deterministic release packaging on the exact release commit;
2. run native execution and result retrieval in a disposable home with no
   private configuration, Tailnet, fleet manager, or Multica installation;
3. regenerate and verify distribution revision/hash manifests;
4. install and remove the portable skill in disposable supported harness roots
   without changing unmanaged files;
5. scan the complete Git history and release archives for credentials and
   machine-specific data; and
6. preserve the prominent 1 MiB result-storage and absent-automatic-retention
   disclosures while cleanup remains an explicit operator-reviewed lifecycle.

Optional Multica, network, and fleet integrations have their own compatibility
canaries. Failure of an optional integration does not redefine standalone
support, but its capability must be reported accurately.

## Highest-value follow-ups

1. Define an operator-neutral read-only host/capability resolver and cross-host
   URI attach contract.
2. Add a reviewed publisher interface for operator deployment/checksum chains
   without making any network index an agent control plane.
3. Evaluate an automatic retention policy and physical journal compaction only
   after the explicit inventory and digest-bound cleanup workflow has
   operational evidence.
4. Expand golden schemas for subscriptions, deliveries, context results, and
   Multica cursor pages.
5. Add an independent canonical-event fixture reader alongside the implemented
   independent v1 ID codec and golden corpus.
