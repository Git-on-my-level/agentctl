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

Useful topics include `run`, `result`, `await`, `subscribe`, `capabilities`,
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
agentctl run -- omp -p --mode json "<bounded objective>"
```

Retain the returned full `exec-*` ID. `await` stops on attention by default and
`result` requires stored content by default:

```bash
agentctl status <exec-id>
agentctl await <exec-id>
agentctl result <exec-id>
```

Use `agentctl help subscribe` before durable callback setup. Delivery is
at-least-once, so deduplicate by the full event key. A receipt proves delivery,
not successful work. A managed supervisor is required only for cross-restart
delivery and must not be silently installed as a new service.

Permission-granting native flags remain explicit. In particular, never add
Cursor `--trust`, force/yolo, sandbox changes, or MCP approval on the caller's
behalf. `run --no-store-result` is also an explicit acceptance that delegation
may have no retrievable answer.

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
