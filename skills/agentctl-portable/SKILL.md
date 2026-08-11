---
name: agentctl-portable
description: Observe native or Multica work, subscribe to events, select deterministic shared context, and verify portable agentctl installation coverage without replacing the native agent. Use for direct-vs-Multica routing, cross-harness setup checks, durable callbacks, promotion, or shared knowledge selection.
---

# agentctl portable

Use `agentctl` for deterministic routing explanations, normalized execution
state, subscriptions/callbacks, explicit Multica promotion, and compiled shared
context. The native CLI still owns prompts, sessions, model choice, tools, and
correctness. Multica still owns durable issues, runs, assignment, and review.

Resolve the binary once. Do not assume a GUI, launchd, SSH, or native-agent
environment inherited an interactive shell `PATH`:

```bash
AGENTCTL=${AGENTCTL_BIN:-}
if [ -z "$AGENTCTL" ]; then AGENTCTL=$(command -v agentctl 2>/dev/null || true); fi
if [ -z "$AGENTCTL" ] && [ -x "$HOME/.local/bin/agentctl" ]; then
  AGENTCTL=$HOME/.local/bin/agentctl
fi
test -n "$AGENTCTL" && test -x "$AGENTCTL" || {
  printf '%s\n' 'agentctl unavailable: set AGENTCTL_BIN or install ~/.local/bin/agentctl' >&2
  exit 6
}
```

This skill requires agentctl v0.1.4 or newer. Run bootstrap status before a
cross-host handoff; duplicate or drifted skill registrations are not healthy.

## Discover before acting

```bash
"$AGENTCTL" help --output json
"$AGENTCTL" bootstrap status --output json
"$AGENTCTL" schema list --output json
"$AGENTCTL" capabilities codex --output json
"$AGENTCTL" capabilities multica --profile <configured-profile> --output json
"$AGENTCTL" doctor --output json
"$AGENTCTL" config doctor --profile <configured-profile> --output json
```

Never infer a capability from the adapter name. An unavailable capability is
not permission to invent a weaker command or inspect raw harness state.

## Choose lifecycle explicitly

Use the native CLI directly for investigation, scoping, operations, or other
bounded work that does not deserve a review lifecycle. Use Multica for durable
code changes, PRs, long-running work, ownership, or review. Ask agentctl for a
deterministic explanation when uncertain:

```bash
"$AGENTCTL" route explain --model-family gpt
"$AGENTCTL" route explain --model-family glm --needs-pr
```

The native model routing is stable: GPT to Codex, Cursor models to Cursor,
Claude to Claude Code, and open-weight models such as GLM to OMP. agentctl does
not proxy the model.

## Direct execution

Everything after `--` is exact argv and is never reparsed as a shell command:

```bash
"$AGENTCTL" run --adapter codex -- codex exec --json "<bounded objective>"
"$AGENTCTL" run --adapter cursor -- cursor-agent --print --output-format stream-json --trust "<bounded objective>"
"$AGENTCTL" run --adapter omp -- omp -p --mode json "<bounded objective>"
```

Retain the returned typed `exec-*` ID. Status and events redact native session
IDs, paths, prompts, and transcripts. The explicit `result` read returns the
bounded final answer without requiring native session-file discovery.

## Observe and subscribe

```bash
"$AGENTCTL" status <exec-id> --output json
"$AGENTCTL" events <exec-id> --after-sequence 0 --limit 100 --output json
"$AGENTCTL" --output json subscribe create \
  --execution <exec-id> \
  --kind attention,artifact,terminal \
  --destination file \
  --target /absolute/path/events.ndjson \
  --ttl 24h
"$AGENTCTL" subscribe list --output json
"$AGENTCTL" await <exec-id> --timeout 10m --stop-on-attention --output json
"$AGENTCTL" result <exec-id> --output json
```

Persistent destinations are `file`, `unix`, `command`, or `webhook`. Delivery
is at-least-once. Deduplicate by the full event key. A receipt proves delivery,
not successful work. Automatic restart recovery requires `agentctl supervisor
run` under a reviewed host service.

For live direct work, preallocate the typed ID before spawning the foreground
runner so the parent can subscribe without parsing partial process output:

```bash
EXEC_ID=$("$AGENTCTL" id generate exec | cut -d' ' -f1)
"$AGENTCTL" --output json subscribe create --execution "$EXEC_ID" --kind terminal,attention \
  --destination file --target /absolute/path/events.ndjson
"$AGENTCTL" run --execution-id "$EXEC_ID" --adapter codex -- codex exec --json "<task>"
```

The runner uses bounded journal transactions and refreshes an expiring owner
lease so a separate callback supervisor does not falsely mark it unreachable.
Cross-process native cancel is only available when the adapter explicitly
advertises it.

Use `result <exec-id> --require-content --output json` when delegation cannot
succeed without a final answer. `result --summary` returns the deterministic
preview. New runs store the bounded final answer by default; use
`run --no-store-result` only when the caller intentionally accepts an omission
tombstone. Never grep native session or rollout files to recover an answer.

## Promote to Multica

Promotion is explicit and does not move or copy the native session:

```bash
"$AGENTCTL" promote <exec-id> \
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
"$AGENTCTL" knowledge validate --source source.json
"$AGENTCTL" knowledge sync --source source.json --checkout /private/checkout --plan
"$AGENTCTL" --output json knowledge compile --source source.json=/private/checkout --output /private/bundle
"$AGENTCTL" knowledge verify --bundle /private/bundle
"$AGENTCTL" context \
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
