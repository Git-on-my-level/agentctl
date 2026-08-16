# Roadmap and phase status

This roadmap records what landed in v0.1 and what remains. A phase is not a
claim that every backend has identical capabilities; adapter constraints are
part of the contract.

## Phase 0 — Contracts: implemented

- Frozen v1 six-word codec, ordered 2,048-word list, digest, typed prefixes,
  checksum, typed wrappers, URI parser, and golden fixtures.
- Versioned execution, event, error, adapter-manifest, and knowledge-source
  schemas.
- Stable JSON-first envelopes and exit codes, with explicit compact text.
- Versioned semantic event projection and store-side recomputation.
- Capability manifests, probe actions, privacy bounds, and malformed-output
  fixtures.
- Progressive `agentctl help <topic>` discovery with typed `next_actions` and
  safe agent-first defaults documented by the live command contract.

The v1 word list is the frozen English BIP-39 list. It is portable and audited
by digest but is not a custom speech-confusion-optimized list. A future codec
version may use a screened list; v1 order and membership will not change.

## Phase 1 — Daemonless local execution: implemented with adapter constraints

- `run`, `recent`, local `attach`, `status`, `events`, `await`, `result`, and `cancel`.
- Codex, Cursor, Claude Code, OMP, generic-process, and Multica event adapters.
- Owner-only journal, normalized source bindings, terminal conflict handling,
  context handles, JSON-first output, and explicit compact text.
- Known executable names infer adapters; `run` has explicit optional timeout
  and prompt transport plus result-content preflight. Explicit `--background`
  starts a detached host-local worker, labels support exact discovery, and
  foreground `fanout` runs one prompt across explicit child argv vectors.
  `await` stops on attention by default, and
  `result` requires a stored content or failure body unless `--allow-empty` is supplied.
  `recent --unreconciled` lists terminal executions whose result has not been
  acknowledged; `result` and a terminal `await` record that acknowledgement.

Each native launch remains owned by one agentctl process. Foreground runs use the
caller; `--background` creates a detached worker that survives the caller but
not a host restart. Another process cannot claim it can attach to the native
session unless that CLI exposes a reviewed durable attach/status API.

## Phase 2 — Optional Multica and promotion: implemented at the authority boundary

- Exact configured binary/profile/workspace/server; no ambient default profile.
- Config provenance doctor and application URL support.
- Explicit `promote --plan` and execute path.
- Authority-owned `sha256:` client key, semantic conflict detection, exact
  remote replay, one local target envelope, source/target promotion links, and
  optional supersession.
- Workspace event observation with bounded polling.
- Issue-only promotion remains issue-authoritative: nested task/run terminal
  events are evidence until an exact run binding exists.

`agentctl` creates/reconciles the durable issue but does not reimplement
Multica run dispatch, assignment, status mutation, or review.

## Phase 3 — Shared context: implemented locally and portably

- GitHub, Forgejo, and generic Git source registrations.
- Explicit noninteractive Git sync.
- Loose, structured, and hybrid ingestion with path, size, UTF-8, secret, and
  symlink controls.
- Deterministic bundle, source lock, lexical index, provenance, verification,
  atomic install, selection, and bounded rendering.
- Allowlisted skill/runtime distribution for major harnesses and Multica.
- Detected-harness bootstrap reconciliation for canonical portable-skill roots;
  already-managed supervisor assets may be upgraded, while new services and
  legacy-copy cleanup remain explicit.

Publishing a verified bundle through a network or fleet bootstrap is an
operator/deployment integration, not an implicit `context` side effect. The CLI
does not modify an operator's network tools or fetch during a read.

## Phase 4 — Durable callbacks and supervisor: implemented

- Durable subscription/outbox/receipt/retry/pause/dead-letter state.
- Atomic event-to-subscription fanout.
- Owner-only file and acknowledged Unix delivery.
- Explicit argv command delivery.
- HMAC webhook envelopes, strict acknowledgements, replay/expiry validation,
  HTTPS-by-default, DNS re-resolution, SSRF/rebinding defense, and redirect
  refusal.
- Restart recovery and an owner-only Unix supervisor control socket.
- Read-only launchd/systemd plans; a release update may reconcile an existing
  managed supervisor but does not install a new service by detection alone.

The supervisor is host-local. Remote callback receivers are separate trusted
services; agentctl does not install them or infer credentials.

## Phase 5 — Optional Multica durable events: implemented in a companion fork

- Transactional workspace event outbox with monotonic sequence.
- Authenticated workspace-isolated cursor API.
- Filter-bound cursors, bounded pages, malformed/ahead conflict handling, and
  explicit expired-cursor recovery.
- `multica event list|watch` with quiet polling and JSONL output.
- Issue-create client-key idempotency used by promotion.
- Preview-first, bounded retention pruning with explicit retention and apply
  gates; expired cursors recover through the documented `410` contract.

Delivery sinks stay in agentctl. Multica exposes durable authority events; it
does not duplicate agentctl callback destinations, receipts, or retry policy.

## Post-v0.1 work

- remote host capability discovery and cross-host URI attach;
- an operator-facing publisher interface for integrating compiled bundles into
  a reviewed deployment/checksum chain;
- automatic retention policy only after the explicit inventory and
  digest-bound terminal-graph cleanup command has operational mileage;
- native cross-restart attach where a backend gains a reviewed API;
- service installation commands after the plan format has deployment mileage;
- an optional speech-confusion-optimized ID codec v2;
- delivery/receipt schemas and an independent canonical-event fixture reader.

## Non-goals

- universal chat or transcript store;
- another issue board or model gateway;
- model selection hidden inside agentctl;
- credentials or session database synchronization;
- automatic Multica issue creation from prompt keywords;
- replacement for native CLIs or an operator's optional coordination, network,
  or host-management tools.
