# Generalized delegated-work lifecycle

## Gap audit

Audited against `origin/main` at `5dc2b8a118f2da982cb9bf8d13301e91e0661560`
and installed `agentctl help dispatch`, `continue`, `steer`, `result`, `recent`,
and `await` on 2026-10-22. Source references below describe the existing
contract; this change adds detached native admission and content collection.

| Need | Existing capability or real gap | Evidence |
| --- | --- | --- |
| Survive the parent without wrappers or scratch markers | `dispatch` already returns a durable `exec-*` binding to a Multica issue, with reserved assignee/runtime and replay protection. It never falls back locally. Native `delegate` and `continue` own the process until exit; expert `run --background` is explicitly refused. **Gap:** parent-independent structured native launch. | `cmd/agentctl/dispatch.go:69`, `:186`; `cmd/agentctl/runtime.go:667`; `docs/structured-delegation.md`, Plans, replay, and results; `docs/follow-up-turns.md:28`. |
| Retry `journal_busy` | Already bounded journal-open retry plus a retryable diagnostic that instructs identical invocation and bounded backoff. Keyed delegate/dispatch admission protects against duplicate launches. Do not add an unbounded retry loop or bypass journaling. | `cmd/agentctl/main.go:1327`, `:1570`; `cmd/agentctl/delegate.go:466`; `internal/store/delegation.go:1`. |
| Follow the latest execution in multiple rounds | `continue` already creates a new execution, freezes the original recipe, records `supersedes`, and recovers keyed retries before checking current session state. Older turns return `continue_not_latest` and `latest_execution_id`. **Gap:** an explicit operation that resolves an anchor to its latest turn. `recent` is read-only discovery, not a continuation handle. | `cmd/agentctl/continue.go:127`, `:225`, `:254`; `cmd/agentctl/recent.go:56`; `docs/follow-up-turns.md:17`. |
| Review rounds without native resume | `continue` already uses negotiated exact-invocation resume routes for Codex, Claude Code, Cursor, OMP, and conditional Devin ACP session loading. ZCode and other route-less invocations fail closed. `steer` targets running work through an actual native route and cannot replace a completed follow-up. **Gap:** explicit fresh launch carrying forward a bounded prior answer. | `internal/adapter/steer.go:66`, `:173`, `:210`; `cmd/agentctl/continue.go:235`; `cmd/agentctl/steer.go:351`; `docs/steering.md`, Guarantees and failure semantics. |
| One terminal wake-up with content | `await` already returns once on completion, failure, cancellation, orphaning, attention, timeout, or integrity conflict. `subscribe --kind terminal` and deduplicated terminal events already exist; subscriptions expire after acknowledged terminal delivery. Callbacks intentionally contain metadata, never answer text. `result --content` already dereferences exact stored text. **Gap:** combine waiting and content delivery in one invocation. Exactly-once external delivery is not available or promised. | `cmd/agentctl/main.go:1098`, `:748`; `cmd/agentctl/subscription.go:95`; `docs/events-and-subscriptions.md:5`, `:59`, `:164`, `:176`. |

## Implemented contract

```sh
agentctl delegate --request-file request.json --prompt-file task.md --detach --plan
agentctl delegate --request-file request.json --prompt-file task.md --detach
agentctl await exec-... --content
```

`delegate --detach` resolves the same explicit constraints and reviewed defaults
as foreground delegation. It starts the same binary in an independent session
and process group on Unix (detached process and new process group on Windows).
Windows detachment has not been tested; no Windows runtime survival claim is
made. That process owns the existing native adapter observation loop. Native CLIs
remain the session, execution, permission, and account authorities. There is no
new scheduler, task board, conversation store, credential store, or service.
The explicit executable, reviewed environment, permissions, cwd, timeout and
frozen delegation recipe are preserved, including configured account wrappers.
No harness-specific detachment or assumed resume capability is added.

The versioned internal launch envelope travels through an anonymous stdin pipe.
It includes the prompt bytes in memory only, never in worker argv, a journal,
spool file, or log. Once handed off, the caller's request and prompt files are
no longer needed by the owner. The execution cwd, config, native session files,
and journal must remain available. Deleting the worktree where an agent is
working is outside the survival guarantee.

A successful new admission uses the existing
[delegate result schema](../schemas/delegate-result.schema.json):

```json
{
  "id": "exec-...",
  "origin_host_id": "host-...",
  "state": "running",
  "liveness": "alive",
  "authority": "native",
  "reused": false,
  "requested": {"family": "example"},
  "resolved": {"harness": "cursor", "model": "example", "host": "local", "authority": "native", "settings": {"access": "coding"}},
  "provenance": {"configuration_sha256": "sha256:...", "defaulted": ["settings.access"]},
  "request_sha256": "sha256:...",
  "lifecycle": {"owner": "detached_process", "parent_independent": true, "reboot_recovery": false}
}
```

The example abbreviates typed IDs and digests; callers retain full values.
The receipt is sent only after launch bindings and the started event have been
committed. It proves admission and observation ownership, not task success.
Collection is not acknowledged. Existing `exec-*` is the durable lookup handle;
there is no extra job ID. New receipt next actions include `await <id> --content`.

`--detach --plan` performs existing read-only preflight, reports prospective
ownership, and spawns no owner. `--detach --wait` is a usage error (2); content
assertions still require `--wait`, so they cannot be mixed into admission.
Normal foreground delegation stays unchanged. Detachment is a collection/owner
choice, excluded from the semantic input digest: same key and semantic inputs
recover the original execution, even across foreground/detached collection.
Replay never migrates an existing foreground execution or claims it detached.
Concurrent same-key owners use existing atomic admission and launch at most once.
A changed prompt, selector, cwd, authority, timeout, or labels conflicts (8).

Failure before native launch returns the normal bounded error, with an execution
ID when admission was recorded. If the collector is cancelled, the worker exits
without a receipt, or the receipt is unavailable after the bounded 60-second
handshake, admission returns `execution_unknown` (13), retryable, with
`diagnostic_code=detached_receipt_unknown`. This is uncertainty, not permission
to relaunch. Replay the same key and inputs. A lost response does not cancel the
owner; a broken receipt pipe does not terminate it. The worker runs no automatic
update/bootstrap maintenance. Journal contention keeps its existing bounded
retry and `journal_busy` contract.

The owner survives parent exit, parent restart, and termination of the parent's
process group. It does not survive reboot, explicit termination of the owner,
or platform policies that kill all descendants independently of process groups.
Unknown ownership never authorizes an automatic restart. Use existing explicit
reconciliation for a provably lost native owner. `--timeout` is the native
execution deadline; killing or timing out an observer does not cancel work.
Cross-process native cancel and steering capabilities are unchanged.

`await --content` keeps existing attention and deadline options and terminal
exit classes. On completed work it writes exact stored UTF-8 text without an
envelope or added newline, then stamps collection with source `await`. Missing
content returns `not_found` (3) and leaves collection unacknowledged. Delivery
failure (70) likewise leaves it unacknowledged. Failure (11), cancellation (12),
and orphaning (13) return structured errors and keep existing terminal
acknowledgement behavior. Attention (10), timeout (9), and conflicted evidence
(13) leave collection unacknowledged. Use `--ignore-attention` explicitly to
wait through intervention. Plain `await` retains its metadata result shape.

## Direct and Multica modes

Detachment is a local ownership mechanism, not a new authority. It applies to
any supported structured native recipe, including non-resumable harnesses.
Native adapter tests still determine launch/result/resume guarantees; a version
probe does not prove account authentication or real model identity.

Multica already provides parent-independent task ownership through `dispatch`.
Dispatch, exact issue refresh in `await`, event deduplication, and remote
idempotency are unchanged. `delegate --authority multica --detach` still fails
with the existing capability diagnostic: the adapter lacks verified settings
and final-answer routes. Snapshot-only Multica completion followed by
`await --content` returns `multica_result_route_unavailable`, never invented
content or a successful native-task claim. The same wait/content rule applies
to both authorities whenever stored content is actually available.

For asynchronous wake-up, use existing `subscribe create --execution exec-...
--kind terminal` with an explicit destination and the existing managed supervisor
when delivery must outlive observers. A terminal event is accepted at most once
per execution, but delivery remains at-least-once. Deduplicate on
`event_dedupe_key`, then dereference `result <id> --content`; callback receipt
proves delivery only. This change does not put answer text in callbacks or
silently install a supervisor.

## Follow-up PR designs

1. **Latest-turn addressing:** add opt-in `continue <anchor-exec-id> --latest`.
   Reuse the existing typed execution as an anchor and traverse authoritative
   `supersedes` edges, not the convenience back-pointer. Resolve one latest
   completed turn, refuse an active successor, branch ambiguity, missing retained
   links, cycles, or mixed authority. Bind the caller's stable anchor, request
   key, prompt digest and other semantic inputs before resolving; replay must
   recover the frozen chosen source even after the lineage advances. Reserve
   the successor atomically under the source so concurrent callers cannot fork.
   Return both `anchor_execution_id` and `continues` in the existing result
   envelope. Keep each round's events and collection separate. For Multica,
   exact issue/run continuation must use an advertised authoritative operation;
   unavailable support returns capability_unavailable (5), never local fallback.
2. **Explicit carry-forward round (implemented):** use `continue <exec-id> --mode carry_forward`
   with the same stable request-key, prompt-source and result collection rules.
   Re-launch the original frozen recipe without native resume argv. Read a
   completed, integrity-verified stored answer without acknowledging it, and
   deliver a versioned JSON prompt envelope containing `source_execution_id`,
   prior-result provenance/digest/text, and the new instruction. Treat prior text
   as quoted context, not executable policy. The caller supplies necessary task
   context because the original prompt is not retained. Refuse unavailable or
   truncated results with no truncation policy. See [follow-up turns](follow-up-turns.md) for shipped size bounds and diagnostics.
   Journal only digests, byte counts and the relation `carry_forward`; neither
   the composed prompt nor a second transcript is stored. The new result reports
   `mode: carry_forward` and `native_session_reused: false`. This is available
   by explicit choice for any eligible harness, never a silent fallback from a
   native resume failure. For snapshot-only Multica, fail before launch until
   an authoritative result/round operation is verified.
3. **Detached continuation:** reuse the detached-owner transport for follow-up
   admission after moving the existing per-session admission guard into a
   serializable/atomic reservation. Do not send a Go closure over the wire or
   bypass the current continue race protection. Foreground `continue` remains
   available in this PR.

Each addition must publish its request/result schema changes, help, skill,
idempotency race tests, and explicit unsupported-authority diagnostics together.
Events remain at-least-once with existing full-key consumer deduplication.

## Alternatives rejected

- Extending Multica `dispatch` to silently launch local selectors: conflates
  remote issue authority with local process ownership and weakens its guarantees.
- A second durable job/thread registry: duplicates existing execution IDs,
  mutation bindings and supersession links without solving another requirement.
- A restart daemon plus prompt spool: adds service installation and raw prompt
  retention to solve parent exit. Reboot recovery needs a separate explicit
  authority and persistence design.
- Re-enabling expert `run --background`: the structured delegation path already
  owns deterministic selector resolution and permissions; retain expert argv
  semantics and its existing explicit rejection.
- Exactly-once or content-bearing callbacks: contradicts at-least-once delivery
  and metadata-only events. Existing deduplication plus explicit dereference is
  sufficient; one blocking `await --content` now covers the simpler caller.
