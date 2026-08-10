---
name: agentctl-portable
description: Observe native or Multica work, subscribe to events, and select deterministic shared context without replacing the native agent.
version: 1
---

# agentctl portable

Use `agentctl` for deterministic routing explanations, normalized execution
state, subscriptions/callbacks, explicit Multica promotion, and compiled shared
context. The native CLI still owns prompts, sessions, model choice, tools, and
correctness. Multica still owns durable issues, runs, assignment, and review.

## Discover before acting

```bash
agentctl help --output json
agentctl schema list --output json
agentctl capabilities codex --output json
agentctl capabilities multica --profile <configured-profile> --output json
agentctl doctor --output json
agentctl config doctor --profile <configured-profile> --output json
```

Never infer a capability from the adapter name. An unavailable capability is
not permission to invent a weaker command or inspect raw harness state.

## Choose lifecycle explicitly

Use the native CLI directly for investigation, scoping, operations, or other
bounded work that does not deserve a review lifecycle. Use Multica for durable
code changes, PRs, long-running work, ownership, or review. Ask agentctl for a
deterministic explanation when uncertain:

```bash
agentctl route explain --model-family gpt
agentctl route explain --model-family glm --needs-pr
```

The native model routing is stable: GPT to Codex, Cursor models to Cursor,
Claude to Claude Code, and open-weight models such as GLM to OMP. agentctl does
not proxy the model.

## Direct execution

Everything after `--` is exact argv and is never reparsed as a shell command:

```bash
agentctl run --adapter codex -- codex exec --json "<bounded objective>"
agentctl run --adapter cursor -- cursor-agent --print "<bounded objective>"
agentctl run --adapter omp -- omp "<bounded objective>"
```

Retain the returned typed `exec-*` ID. Normal output redacts native session IDs,
paths, prompts, transcripts, and raw result bodies.

## Observe and subscribe

```bash
agentctl status <exec-id> --output json
agentctl events <exec-id> --after-sequence 0 --limit 100 --output json
agentctl subscribe create \
  --execution <exec-id> \
  --kind attention,artifact,terminal \
  --destination file \
  --target /absolute/path/events.ndjson \
  --ttl 24h
agentctl subscribe list --output json
agentctl await <exec-id> --timeout 10m --stop-on-attention --output json
agentctl result <exec-id> --output json
```

Persistent destinations are `file`, `unix`, `command`, or `webhook`. Delivery
is at-least-once. Deduplicate by the full event key. A receipt proves delivery,
not successful work. Automatic restart recovery requires `agentctl supervisor
run` under a reviewed host service.

For live direct work, preallocate the typed ID before spawning the foreground
runner so the parent can subscribe without parsing partial process output:

```bash
EXEC_ID=$(agentctl id generate exec | cut -d' ' -f1)
agentctl subscribe create --execution "$EXEC_ID" --kind terminal,attention \
  --destination file --target /absolute/path/events.ndjson
agentctl run --execution-id "$EXEC_ID" --adapter codex -- codex exec --json "<task>"
```

The runner uses bounded journal transactions and refreshes an expiring owner
lease so a separate callback supervisor does not falsely mark it unreachable.
Cross-process native cancel is only available when the adapter explicitly
advertises it.

## Promote to Multica

Promotion is explicit and does not move or copy the native session:

```bash
agentctl promote <exec-id> \
  --title "<durable objective>" \
  --handoff-file handoff.md \
  --plan \
  --output json
```

Remove `--plan` only after inspecting exact profile, workspace, server, app URL,
client key, and argv. Exact retries recover one issue. Changed semantics
conflict. Keep using Multica's native CLI for issue/run interaction and review.
Hosts that intentionally authenticate through Multica's root profile configure
the exact selector `--multica-profile @default`; this omits a named profile
without copying credentials or falling back from a missing name.

## Shared knowledge and context

Read-only validation and context selection never fetch Git:

```bash
agentctl knowledge validate --source source.json
agentctl knowledge sync --source source.json --checkout /private/checkout --plan
agentctl --output json knowledge compile --source source.json=/private/checkout --output /private/bundle
agentctl knowledge verify --bundle /private/bundle
agentctl context \
  --bundle /private/bundle \
  --repository /work/repo \
  --task-kind investigation \
  --query multica \
  --render /private/context.md
```

Sources may be structured, loose, or hybrid GitHub, Forgejo, or generic Git
repositories. Never compile credentials, raw prompts, transcripts, harness
databases, worktrees, or unreviewed private logs.

## Typed identifiers

Use full typed six-word IDs for mutations. Display labels and prefixes are not
mutation targets. Portable URIs contain only typed aliases; no credentials,
UUIDs, paths, or hostnames. If a foreign host cannot be resolved, return
`dependency_unavailable` rather than guessing or treating it as absent.
