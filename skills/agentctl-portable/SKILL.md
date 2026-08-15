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

Useful topics include `run`, `fanout`, `result`, `await`, `subscribe`, `capabilities`,
`bootstrap update`, `promote`, `knowledge`, and `context`. Follow returned
read-only `next_actions` for deeper discovery. Do not preload every topic or
memorize version-specific flags in place of help.

`agentctl bootstrap update` reconciles the embedded portable skill across
detected canonical harness roots. Use `--dry-run` when inspecting another home
or narrowing an unfamiliar installation.

## Choose authority deliberately

Use direct native work for bounded investigation, scoping, and operations. Use
Multica for durable changes, PRs, multiple owners, review, or work that must
survive the parent. When uncertain, ask `agentctl route explain`.

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
agentctl run --prompt-stdin --prompt-delivery stdin -- codex exec --json - < "$PWD/task.md"
```

Never infer prompt delivery from an adapter name. Use `argv` only for a native
form that expects a positional prompt and `stdin` only for a verified
stdin-reading form. Use `agentctl fanout --manifest <path>` when one prompt must
run through several explicit native argv vectors; fan-out is foreground-owned,
returns independent child execution IDs, and does not synthesize their results.
The v1 manifest requires `schema_version`, `prompt_file`, and `children[].argv`;
set `prompt_delivery` globally or per child. Prompt files and relative child
working directories resolve from the manifest directory, while an omitted
child `cwd` inherits the invoking process directory. Discover the normative
shape with `agentctl help fanout` and `agentctl schema list`.

`run` has no default wall-clock timeout. Add `--timeout` only when the caller
requires a bound. Native work remains owned by the foreground agentctl process;
the callback supervisor does not make it detached or restart-durable.

Retain the returned full `exec-*` ID. `await` stops on attention by default and
`result` requires stored content by default:

```bash
agentctl status <exec-id>
agentctl await <exec-id>
agentctl result <exec-id>
```

Use `await --no-timeout` for an intentionally unbounded observer. It still
stops on actionable attention unless `--ignore-attention` is explicit.

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

## Promotion and shared context

Promotion is an explicit remote mutation. Inspect `agentctl help promote` and
plan unfamiliar authority selections before creating or recovering a Multica
issue. Promotion links work; it does not move or copy the native session.

Use `agentctl help knowledge` and `agentctl help context` for deterministic
shared context. Never compile credentials, raw prompts, transcripts, harness
databases, worktrees, or unreviewed private logs.

## Identifier and privacy invariants

Use full typed six-word IDs for mutations. Display labels and prefixes are not
mutation targets. Portable URIs contain typed aliases, not credentials, native
UUIDs, or local paths. Status, events, and callbacks remain metadata-only;
explicit `result` is the supported final-answer read path.
