---
name: agentctl-portable
description: Route, launch, observe, retrieve, subscribe to, or promote native and Multica work through agentctl without replacing the underlying authority.
---

# agentctl portable

Use `agentctl` for deterministic routing, normalized execution state, durable
callbacks, explicit Multica promotion, and selected shared context. Native CLIs
still own prompts, sessions, models, tools, and correctness. Multica still owns
durable issues, runs, assignment, and review.

Resolve the binary from `AGENTCTL_BIN`, `PATH`, or
`$HOME/.local/bin/agentctl`. If it is unavailable, stop with
`dependency_unavailable`; do not inspect native session stores as a fallback.

## Discover just in time

Start with the one question relevant to the task:

```bash
agentctl doctor
agentctl help <topic>
```

Useful topics include `run`, `dispatch`, `recent`, `fanout`, `result`, `await`, `subscribe`, `capabilities`,
`bootstrap update`, `skills`, `promote`, `knowledge`, and `context`. Follow returned
read-only `next_actions` for deeper discovery. Do not preload every topic or
memorize version-specific flags in place of help.

`agentctl bootstrap update` reconciles the embedded portable skill and a short
delegation pointer in detected, existing harness instruction files; it never
creates those instruction files. Use `--dry-run` when inspecting another home
or narrowing an unfamiliar installation.

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

## Golden path

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
agentctl run --background --prompt-stdin --prompt-delivery argv -- cursor-agent --print --output-format stream-json --trust < "/absolute/path/task.md"
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

For long work that should outlive the launching shell, use explicit background
ownership and exact metadata labels:

```bash
agentctl run --background --label review --prompt-file "$PWD/task.md" --prompt-delivery argv -- cursor-agent --print --output-format stream-json --trust
agentctl recent --state nonterminal --liveness alive --label review
agentctl recent --liveness unreachable
agentctl recent --unreconciled
```

The detached host-local worker remains the native owner; it is not
restart-durable authority and has no controlling terminal. A direct adapter
does not gain cross-process cancellation; add `--timeout` when a hard stop is
required unless capabilities advertise a durable cancel route. Background mode
accepts argv, prompt files, and prompt stdin; the parent materializes prompt
bytes through a one-shot pipe before detaching. Use `recent` to recover
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
returned `exec-*` ID and attach subscriptions immediately when unattended
notification matters. `await` refreshes Multica workspace events itself;
it tolerates two consecutive retryable refresh failures and returns the third
failure with its authority diagnostic. This absorbs a brief observation fault
without hiding a persistently unavailable authority.
`status`, `events`, and `recent` remain cached read-only views, while the managed
supervisor provides continuous cross-restart reconciliation and callback
delivery.

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

Use `agentctl help subscribe` before durable callback setup. Delivery is
at-least-once, so deduplicate by the full event key. A receipt proves delivery,
not successful work. A managed supervisor is required only for cross-restart
delivery and must not be silently installed as a new service.

Permission-granting native flags remain explicit in argv. Consult
`agentctl config doctor` for advisory operator preferences: pass Cursor
`--trust` when that exact authorization is present, otherwise preserve the
native trust prompt. Never infer authorization for force/yolo, sandbox changes,
or MCP approval. For Cursor, omit `--mode` for normal Agent work, use
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
