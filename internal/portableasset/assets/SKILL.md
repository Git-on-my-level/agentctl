---
name: agentctl-portable
description: Route, launch, observe, retrieve, subscribe to, or promote native and Multica work through agentctl without replacing the underlying authority.
---

# agentctl portable

Use `agentctl` for deterministic routing, normalized execution state, durable
callbacks, explicit Multica promotion, and selected shared context. Native CLIs
still own prompts, sessions, models, tools, and correctness. Multica still owns
durable issues, runs, assignment, and review.

For optional coordination integrations, `agentctl identity --json` reports
versioned provider/native-conversation evidence without probes or writes.
Unknown IDs remain null. Native session IDs are provider-scoped hashes, not
resume handles or authenticated agent principals; consumers add their enrolled
host namespace and preserve explicit coordination identity. Installed harnesses
are inventory only. Generic agents do not need agentctl to register elsewhere.

Resolve the binary from `AGENTCTL_BIN`, `PATH`, or
`$HOME/.local/bin/agentctl`. If it is unavailable, stop with
`dependency_unavailable`; do not inspect native session stores as a fallback.

## Default launch

Kick off a new native agent with `delegate`, not `run`. For an explicitly
requested model, use `delegate` with a schema-versioned request file and only
the user's constraints. Read `help delegate`, plan with `--plan`, then execute
the same request and key with `--wait --content` (remove `--plan`). Do not
reconstruct a native model slug from an alias. Bare adapters or missing remote
runtimes are unresolved constraints, never substitution grants.

```bash
agentctl help delegate
agentctl delegate --request-file request.json --prompt-file task.md --plan
agentctl delegate --request-file request.json --prompt-file task.md --wait
```

Inspect `native.argv`, `native.env`, and `native.permissions` on the plan before launching.
Coding is the default access. It adds Cursor `--force` and Codex
`--dangerously-bypass-approvals-and-sandbox` only when
`delegation.unattended_coding_permissions` is true. `settings.access:
"read_only"` never adds those flags. Expert `run` does not apply this grant.
Do not hand-write cursor or codex argv for a new agent, and do not edit a
global CLI allowlist to widen permissions.

Hermes terminal processes, Claude Code Bash tasks, and Codex terminal sessions
can own a foreground agentctl process using their supported process lifecycle.
Retain that process handle and collect its completion. `run --background` is
rejected since v0.6.0. Use explicit Multica dispatch for durable task ownership.
The delegate plan reports ownership and collection semantics before launch.

For an external scratch prompt, replace `--prompt-file` with `--prompt-stdin`
and redirect the file into agentctl. Only expert `run` accepts
`--prompt-delivery`; preserve its verified delivery. `delegate`, `continue`,
`steer`, and `dispatch` use their own delivery contract and need only
`--prompt-stdin`. Fanout repairs change the shared or child manifest
`prompt_file` within the manifest directory.
An error includes a structured repair; it never copies prompt bytes into logs.

For results, prefer `result <id> --content` for exact text, `--summary` for compact
metadata, and `result --unreconciled --summary` for bounded bulk collection.
A pending result supplies an `await` action. Check the agentctl exit status and
JSON `ok` before inspecting fields; piping through `head` can hide a failing
exit code. Completion, delivery, and acceptance remain distinct.

A failed dispatch retains `last_operation_failure` in `status`. Resolve its
category and retry the original inputs with the original idempotency key.
Uncertain remote creation is never a reason to allocate a new key.
`update status` distinguishes observed binary version from installation records;
inspect bootstrap and supervisor status before treating an update as complete.
A failed installer reports rollback separately from bootstrap assets retained.
For a legacy skill collision, `help bootstrap adopt` describes a read-only plan
and explicit digest-bound adoption with a backup. Never overwrite custom skills.
A supervisor holds one state-directory lock through shutdown. Existing custom
launchers require explicit hash-bound wrapper registration; ordinary upgrades
preserve registered wrappers and refuse drift.

## Discover just in time

Start with the one question relevant to the task:

```bash
agentctl doctor
agentctl help <topic>
```

Useful topics include `delegate`, `run`, `dispatch`, `recent`, `fanout`, `result`, `await`, `continue`, `steer`, `subscribe`, `capabilities`,
`bootstrap update`, `skills`, `promote`, `knowledge`, and `context`. Follow returned
read-only `next_actions` for deeper discovery. Do not preload every topic or
memorize version-specific flags in place of help.

`agentctl bootstrap update` reconciles the embedded portable skill and a short
delegation pointer in detected harness instruction files. It appends a marked
block to existing unmarked files, creates missing documented files except OMP,
and repairs truncated or duplicate agentctl markers. Opt out with
`bootstrap.instruction_pointers=off` or `--no-instruction-pointers`. Use
`--dry-run` when inspecting another home or narrowing an unfamiliar
installation.

Exact release builds default to automatic updates. The first work-creating
invocation due on each UTC day starts a detached short-lived worker that verifies the matching
release archive and uses its packaged installer for managed installations; it
does not delay the foreground command or create a daemon. Commands advertised
as read-only never trigger this maintenance. Use `agentctl update
status` to inspect it. `agentctl update policy notify` retains the once-daily
`agentctl_update_available` prompt without installation, and `off` disables all
checks.

## Choose authority deliberately

Use direct native work for bounded investigation, scoping, and operations. Use
Multica for durable changes, PRs, multiple owners, review, or work that must
survive the parent. When uncertain, ask `agentctl route explain -- "<host?> <model>"`.
Pass only the short host/model selector, not task prose or nested delegation
instructions. It returns ranked reviewed hits plus a placement mode; empty
lists mean nothing was recognized, not an error. It never launches work,
verifies a remote runtime or Multica assignee, or creates an `exec-*` handle.
If `this_host` is not configured, local versus remote placement remains unknown.
Use `dispatch` when the selector names one configured host and one concrete
model and the task should be created under Multica authority. Work created
directly in Multica remains outside the host-local journal.

Never infer a capability from an adapter name. If doctor or capabilities says
a requirement is unavailable, report it; do not weaken the operation, invent a
native command, or scrape private harness state.

## Structured delegation: default for new agents

Read `agentctl help delegate` before `run`. Translate the user's explicit constraints into a
request; leave everything else absent. “grok” means `{"family":"grok"}`;
“cursor grok 4.6” means `{"harness":"cursor","family":"grok","version":"4.6"}`.
Other word orders produce the same constraints. Never guess a version or native
slug from memory. Add explicit speed/effort under `settings` when requested.

```json
{"schema_version":1,"request_key":"review-01","selector":{"family":"grok"}}
```

```bash
agentctl delegate --request-file request.json --prompt-file task.md --plan
agentctl delegate --request-file request.json --prompt-file task.md --wait
```

Agentctl resolves reviewed preferred entries, fills compatible defaults, and
builds native flags. Inspect `requested`, `resolved`, and `provenance`. Conflicts
or ambiguity launch nothing; use returned candidates to resolve only the missing
choice. Config needs explicit family/version metadata. Omitted host or `local` selects
this machine; `route.this_host` optionally supplies its configured name.
Workspace trust is a separate `delegation.cursor_workspace_trust` grant, never
implied by a model preference. Unattended coding permissions are a separate
`delegation.unattended_coding_permissions` grant. When it is true, coding
launches (omitted `settings.access`, or `"coding"`) include Cursor `--force`
and Codex `--dangerously-bypass-approvals-and-sandbox`. Set
`settings.access` to `read_only` for Cursor `--mode ask` and Codex
`--sandbox read-only`; that access never receives the bypass flags.
Harnesses without a reviewed bypass flag report `permissions: unsupported`
and are not given an inferred flag. Unavailable settings or hosts must not trigger
silent substitution or fallback to expert `run`.

Keep one stable request key for one logical task. Retry the same inputs to recover
its original execution and answer, including after defaults change. A changed
prompt/selector/cwd/authority/timeout/labels conflicts. An uncertain launch is
reported unknown and never automatically relaunched. A new key starts new work.
Keys are local to the profile and journal; retain that context with the execution
ID. Do not put secrets in request metadata. Prompt bytes remain separate.
Validation errors include sanitized field paths, allowed keys and a placeholder
request example; selection errors include bounded reviewed catalog candidates.
Preserve explicit constraints: examples and candidates never authorize fallback.
`config doctor` separates syntax/provenance from static `launch_recipes`
compatibility; `runtime_verified: false` means runtime/account/Multica readiness
has not been established.

`--wait` requires completed work with a nonempty stored answer and acknowledges
collection after delivery. `--content` returns exact answer text. Explicit
`--require-result-source` and `--min-result-bytes` assertions are available.
This is foreground-owned native work, with optional `--timeout`; background and
external context handles are not supported by delegate yet. Plans are read-only.
Multica delegation is rejected until its result/settings contract is available;
explicit `dispatch` retains its existing lifecycle-only guarantees. The ZCode
Flash recipe sets the non-secret `ZCODE_MODEL` selector; the host-managed
launcher needs its config from `zcode-cli-provision`. GLM-5.3 keeps the configured
default by clearing inherited selectors with `ZCODE_MODEL=`. Launch values
replace inherited entries. Missing variants fail before a prompt runs. Requested model/settings
are not proof of provider-side model identity or task correctness.

## Expert native path

`run` is for an exact argv you already decided. It does not apply delegate
permission defaults and must not be the path for kicking off a new agent.
Pass native argv exactly after `--`; agentctl does not shell-reparse it:

```bash
agentctl run -- codex exec --json "<bounded objective>"
agentctl run -- cursor-agent --print --output-format stream-json --trust "<bounded objective>"
agentctl run -- cursor-agent --print --output-format stream-json --mode ask --trust "<bounded read-only question>"
agentctl run -- omp -p --mode json "<bounded objective>"
```

For a reusable multi-line prompt, select an explicit source and delivery
mechanism. Prompt bytes are bounded and are not persisted by agentctl:

```bash
agentctl run --prompt-file "$PWD/task.md" --prompt-delivery argv -- codex exec --json
agentctl run --prompt-file "$PWD/task.md" --prompt-delivery argv -- cursor-agent --print --output-format stream-json --trust
agentctl run --prompt-stdin --prompt-delivery argv -- cursor-agent --print --output-format stream-json --trust < "/absolute/path/task.md"
agentctl run --prompt-stdin --prompt-delivery stdin -- codex exec --json - < "$PWD/task.md"
```

Never infer prompt delivery from an adapter name. Use `argv` only for a native
form that expects a positional prompt and `stdin` only for a verified
stdin-reading form. Use `agentctl fanout --manifest <path>` to delegate shared
or distinct tasks through explicit native argv vectors. It is foreground-owned,
returns independent child IDs, and never synthesizes results or creates a group
authority. The v1 manifest requires `schema_version`, `children[].argv`, and a
shared or per-child `prompt_file`. Child prompts replace the shared prompt;
`prompt_delivery` also supports a child override. Optional unique child `name`
values correlate responses; shared and child `labels` persist for rediscovery.
Prompt paths and explicit relative child working directories are manifest-relative;
an omitted child `cwd` inherits the invoking directory.

Every batch preflights all children before any task launch. Preflight can run
native read-only version probes and is not an atomic launch reservation.
`--fail-fast` cancels admitted siblings and skips queued children. Inspect
`launch_attempted`, `recorded`, `state`, and `error` separately: an allocated ID
or an attempted launch is not proof of a journaled execution or successful work.
On failure the report is in `error.details.fanout`. Existing IDs conflict;
fan-out has no automatic replay, retry, or restart durability. Do not blindly
rerun a partially executed manifest. Collect journaled results individually.
Discover the normative shape and limits with `agentctl help fanout` and
`agentctl schema list`.

`run` has no default wall-clock timeout. Add `--timeout` only when the caller
requires a bound. Native work remains owned by the invoking agentctl process;
the callback supervisor does not change launch ownership or durability.

For long work that must outlive this process, use Multica `dispatch`. Do not
pass `run --background`; that flag is rejected because agents treat its
journaled exit 0 as task completion. Parent-background a foreground
`agentctl run` when the parent already owns process lifetime.

```bash
agentctl run --label review --prompt-file "$PWD/task.md" --prompt-delivery argv -- cursor-agent --print --output-format stream-json --trust
agentctl recent --state nonterminal --liveness alive --label review
agentctl recent --liveness unreachable
agentctl recent --unreconciled
```

To send review feedback or the next step to an agent that has finished, use a
follow-up turn instead of a new delegation. Read `agentctl help continue`:

```bash
agentctl continue <exec-id> --request-key <new-key> --prompt-file "$PWD/feedback.md" --plan
agentctl continue <exec-id> --request-key <new-key> --prompt-file "$PWD/feedback.md" --wait --content
```

The native CLI resumes its own session, so the agent keeps the conversation;
do not restate the whole task. Each turn is a new execution: retain its ID and
continue from the latest completed turn. It reuses the original delegate's
recipe, permissions, and working directory. Only a completed delegated
execution can be continued; a refusal names the reason. Do not hand-write a
native resume argv through expert `run` when `continue` is available, and never
look up a native session id yourself. Use one new request key per turn; a retry
with the same key and prompt recovers that turn, or launches it if the first
attempt was refused because another turn was running.

To redirect a running native execution, read `agentctl help steer` and plan
first. The route is fixed when the execution launches and is never inferred
from the adapter name:

```bash
agentctl steer <exec-id> --prompt-file "$PWD/steer.md" --plan
agentctl steer <exec-id> --prompt-file "$PWD/steer.md"
```

`live_input` delivers the message at the agent's next turn boundary without
interrupting it. `interrupt_resume` stops the native process and resumes the
same session, so it requires `--allow-interrupt`; progress inside the
interrupted turn can be lost, so restate anything the agent must keep. When the
plan reports no route, steering is unavailable: report that, and do not cancel
and relaunch as a substitute unless the user wants the work restarted. Read
`steer.status`: `delivered` means the native session took the message, not that
the agent acted on it; `queued` means it is waiting in the session's input and
not taken yet, so retry with the same `--idempotency-key` to wait for it rather
than sending it again. A finished
execution cannot be steered; use `continue`. Steering needs a prompt that agentctl
delivered (`--prompt-file` or `--prompt-stdin`), not one embedded in native argv.

Native work remains owned by the invoking agentctl process. A direct adapter
does not gain cross-process cancellation; add `--timeout` when a hard stop is
required unless capabilities advertise a durable cancel route. For aggregate usage, use `recent --since <RFC3339> --until <RFC3339> --summary`.
For full metadata export, follow `next_cursor` with `--cursor`, preserving filters.
Cursors freeze creation cutoff, not live states or acknowledgements. Summary
and export never read answers or acknowledge. Preflight failures are not counted.
Harnesses can explicitly set `AGENTCTL_CALLER_HARNESS` (hermes, claude-code, codex,
cursor, omp, zcode, devin, other) on invocation; old/unset callers remain unknown.
The declaration is stripped from inherited child environments; nested callers
must identify themselves. `recent --caller unknown` finds unattributed records.

Use `recent` to recover
execution IDs from the local journal. It is read-only, newest-first,
prompt/result-record-free, and does not aggregate other hosts. Repeated label
filters use AND semantics. `--unreconciled` lists terminal executions whose
result has never been acknowledged by `result` or a terminal `await`. Terminals
that already existed when acknowledgement tracking first write-opened the
journal are not unreconciled. Labels are visible exact discovery metadata, never
mutation targets; do not place secrets or prompt/result content in them.

For durable cross-host work, plan and then dispatch through the configured
Multica authority:

```bash
agentctl dispatch --route "m5 sol" --title "Review the release" --prompt-file "$PWD/task.md" --idempotency-key release-review-v1 --plan
agentctl dispatch --route "m5 sol" --title "Review the release" --prompt-file "$PWD/task.md" --idempotency-key release-review-v1
```

`dispatch` resolves live Multica agents against their authoritative runtime,
not their display name, and requires exactly one online host/adapter/model
match. It never falls back locally. The caller key is required so a remote
success followed by a lost local response can be replayed without creating a
second issue. Agentctl creates or recovers the exact assigned issue in backlog,
persists its issue binding while the local execution is still `starting`, reads
the exact issue, and activates only when that read still reports backlog. A
retry that observes Multica has advanced the issue skips activation. Concurrent
external status changes remain Multica-owned and can race the CLI update. Before
the remote call agentctl reserves the exact assignee/runtime bindings; retry
recovers them by semantic mutation key instead of requiring the current fleet
topology to match again. Prompt bytes go only to
Multica stdin; agentctl stores their digest and authority bindings, not the
prompt. Retain the
returned `exec-*` ID. For issue-backed Multica executions, `await` and the managed supervisor refresh the exact bound issue via the native CLI. `done` is an issue-level terminal state; `in_review` and `blocked` require attention. An issue-status snapshot is not proof that a run succeeded or that its answer was captured. Workspace events are unavailable where capture triggers are disabled; an empty event page is never a completion signal. `status`, `events`, and `recent` remain cached views. Read the authoritative issue/run in Multica for final output; do not promise event-based callbacks or `result --content` for snapshot-only executions.

Retain the returned full `exec-*` ID when practical; otherwise recover it with
`recent`. `await` stops on attention by default and
`result` requires stored content by default. Both stamp collection on a
successful terminal return, so they are local operational writes:

```bash
agentctl status <exec-id>
agentctl await <exec-id>
agentctl result <exec-id>
agentctl result <exec-id> --content
agentctl recent --unreconciled
```

Foreground `run` reports the normalized execution envelope; a successful
agentctl invocation is not a claim that the native task completed successfully.
Inspect `result.state`, or use `await` when outcome-sensitive exit behavior is
required. `result --content` writes the exact stored UTF-8 text without a JSON
envelope or an added newline.

Use `await --no-timeout` for an intentionally unbounded observer. It still
stops on actionable attention unless `--ignore-attention` is explicit.
For an execution with a recorded run deadline, use
`await --through-execution-deadline`; generated background next actions select
that bound and offer subscription discovery for nonblocking callers.

Stale inbox entries include `recovery` and read-only inspection actions.
An `owner_process_only` route cannot recover a lost native owner; bound Multica
work keeps its issue/run authority. Cached status/events and unreachable liveness
do not establish completion. Do not auto-acknowledge or infer failure.

`agentctl reconcile` is the explicit journal repair. Read `help reconcile`,
then `--plan` before `--apply --plan-digest`. `--plan` writes nothing and
returns `plan_digest` over the candidate ids, actions, and proofs. Apply
writes nothing if that digest no longer matches, then proves each row again
before writing. It orphans a native execution only when the launcher-recorded
PID is provably gone and the last observation is older than `--stale-after`.
A numeric session id is not a PID. `journal_host_match` is the journal's stored
host id, not a machine fingerprint. The state is `orphaned` with `owner_lost`:
the outcome is unknown, not success or failure. Rows with no launch record stay
`ownership_unproven` unless `--include-legacy-unproven` is set. Eligible rows
are native, match the journal host, and are older than `--legacy-stale-after`
(default 168h, minimum 72h). They are listed under `legacy_orphan` with evidence
`heartbeat_absent` and outcome `owner_unproven_legacy`, never `owner_lost`. A
legacy numeric PID that currently exists is left unchanged and reported
`legacy_pid_present`; an unprovable absence is
`legacy_pid_unproven`. Apply also stamps
`bulk_reconciled` on old uncollected completed or cancelled results without
reading them. That source is visible on `recent` and `status` and makes the
result eligible for `data cleanup`. The `acknowledged` event stays in the
journal and is delivered only when a subscription's kind filter lists
`acknowledged`; `--kind all` does not include it. Failures, orphans, and integrity conflicts
stay visible unless `--include-failures` is set. Multica issue state is not
changed; local collection stamps are written. A generic process
that exits 0, or writes a file, is not success: plain stdout is not a result
contract, and a child's exit code is not the direct child's.

Use `agentctl help subscribe` before durable callback setup. Delivery is
at-least-once, so deduplicate by the full event key. A receipt proves delivery,
not successful work. A managed supervisor is required only for cross-restart
delivery and must not be silently installed as a new service.

Permission-granting native flags remain explicit on expert `run` argv. `delegate`
applies Cursor `--force` and the Codex bypass only from
`delegation.unattended_coding_permissions`, and never on read-only access.
Consult `agentctl config doctor` for advisory operator preferences: pass Cursor
`--trust` on expert `run` when that exact authorization is present, otherwise preserve the
native trust prompt. Never infer authorization for force/yolo, sandbox changes,
or MCP approval on `run`. For Cursor, omit `--mode` for normal Agent work, use
`--mode ask` for bounded read-only Q&A, and avoid `--plan`/`--mode plan` because
Cursor's one-shot plan completion is not reliably represented; agentctl rejects
it unless `--allow-unreliable-result` is explicit. `run --no-store-result` is
also an explicit acceptance that delegation may have no retrievable answer.

For automation that needs stronger answer guarantees, inspect
`agentctl help result` and use `--require-result-source assistant` and/or
`--min-result-bytes`. These are caller-selected assertions, not universal
quality heuristics.

If an operation returns `diagnostic_code=journal_busy`, retry the same agentctl
invocation with bounded backoff. Do not silently fall back to a raw native CLI:
that changes process-group supervision, journaling, callbacks, and result
recovery. Keep structured-output requirements exactly as reported by
`required_argv`.

## Promotion and shared context

Dispatch and promotion are explicit remote mutations. Inspect `agentctl help
dispatch` or `agentctl help promote` and plan unfamiliar authority selections
before creating or recovering a Multica issue. Dispatch starts new routed
Multica work; promotion links an existing direct execution to durable follow-up.
Promotion links work; it does not move or copy the native session.

Use `agentctl help knowledge` and `agentctl help context` for deterministic
shared context. Never compile credentials, raw prompts, transcripts, harness
databases, worktrees, or unreviewed private logs.

Use `agentctl help skills` before personal skill reconciliation. `skills plan`,
`status`, `doctor`, and `update --plan` are read-only. `skills update`
fast-forwards the independently selected Skill Hub and replaces only unchanged
marker-owned copies. Local edits remain drifted for `skills diff`, plan-first
`restore`, or plan-first `propose`; publication is always separate. The embedded
`agentctl-portable` skill is bootstrap-owned and cannot appear in a Hub pack.
Multica skill installation remains authority-owned and must be advertised by a
reviewed runtime-bundle installer.

## Identifier and privacy invariants

Use full typed six-word IDs for mutations. Display labels and prefixes are not
mutation targets. Portable URIs contain typed aliases, not credentials, native
UUIDs, or local paths. Status, events, and callbacks remain metadata-only;
explicit `result` is the supported final-answer read path.
