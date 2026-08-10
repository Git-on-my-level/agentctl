# agentctl

`agentctl` is a portable, agent-oriented supervision, event, callback, and
context layer for work executed by native agent CLIs or coordinated by Multica.

It deliberately does not replace Codex, Claude Code, Cursor, OMP, Multica,
`tailnetctl`, or host management such as `macctl`:

- GPT work stays native to Codex;
- Cursor models stay native to Cursor;
- Claude stays native to Claude Code;
- open-weight models such as GLM stay native to OMP;
- Multica owns durable issues, runs, assignment, and review;
- `agentctl` owns normalized observation, subscriptions, callbacks, and compiled
  context handles.

## Current implementation

The v0.1 implementation includes:

- typed, checksum-validated six-word IDs and portable URIs;
- a hardened owner-only bbolt journal with CAS revisions, semantic event
  deduplication, subscriptions, delivery outbox, receipts, retries, and dead
  letters;
- direct execution adapters for Codex, Cursor, Claude Code, OMP, and generic
  structured processes;
- explicit, idempotent promotion of a direct execution to one Multica issue;
- compact `text` output and normative `json` output;
- deterministic knowledge validation, explicit Git sync, loose/structured/
  hybrid compilation, bundle verification/install, lexical selection, and
  bounded context rendering;
- owner-only file and Unix callbacks, explicit command callbacks, and signed
  webhook delivery with SSRF and DNS-rebinding protections;
- an optional restart-capable local supervisor plus launchd/systemd install
  plans;
- allowlisted portable skill distribution for Hermes, Codex, Claude, Cursor,
  OMP, and Multica;
- a companion Multica fork change adding durable workspace events and
  authority-owned issue-create idempotency keys.

The CLI is usable now, but v0.1 retains explicit boundaries: native sessions
are generally observable only by the process that launched them; remote host
attach/discovery is not implemented; supervisor installation is plan-only; and
Tailnet publishing of compiled bundles remains an external deployment step.
See [Implementation status](docs/implementation-audit.md).

## Quick start

Build and discover the live contract:

```bash
make ci
go build -o build/agentctl ./cmd/agentctl
build/agentctl help
build/agentctl schema list --output json
build/agentctl capabilities codex --probe --executable /path/to/codex
```

Wrap a native argv without reparsing anything after `--`:

```bash
agentctl run --adapter codex -- codex exec --json -m gpt-5.6-sol "review this change"
agentctl run --adapter cursor -- cursor-agent --print "scope this bug"
agentctl run --adapter omp -- omp "investigate the service"
```

For synchronous work, `run` returns the terminal `exec-*` envelope. For live
observation, preallocate the ID, create any subscriptions, and then start the
foreground runner from the parent agent's native process manager:

```bash
EXEC_ID=$(agentctl id generate exec | cut -d' ' -f1)
agentctl subscribe create \
  --execution "$EXEC_ID" \
  --kind terminal,attention \
  --destination file \
  --target /absolute/path/events.ndjson
agentctl run --execution-id "$EXEC_ID" --adapter codex -- codex exec --json "review this change"

agentctl status "$EXEC_ID"
agentctl events "$EXEC_ID" --after-sequence 0 --output json
agentctl await "$EXEC_ID" --timeout 10m
```

The runner opens the journal only for bounded transactions, so a separate
supervisor can deliver callbacks while work is active. Native cross-process
cancel remains unavailable unless that adapter advertises a reviewed durable
cancel mechanism; agentctl never guesses from a PID.

Configure an exact Multica authority and promote only when durable lifecycle
tracking is worth the review overhead:

```bash
agentctl config set-profile \
  --name fleet --default \
  --multica-executable /absolute/path/to/multica \
  --multica-profile desktop-multica-01 \
  --workspace-id <workspace-id> \
  --server-url https://multica-01.example \
  --app-url https://multica-01.example

agentctl promote exec-purple-monkey-dragon-river-candle-meadow \
  --title "Implement the durable change" \
  --handoff-file handoff.md \
  --plan
```

Remove `--plan` to create or recover the one authority-owned issue. Repeating
the same promotion returns the same Multica issue and the same stored agentctl
execution alias. Changing its semantics conflicts instead of silently creating
a second lifecycle.

## Knowledge sources

Knowledge remains in independently owned GitHub, Forgejo, or generic Git
repositories. Existing loose stores do not need migration.

```bash
agentctl knowledge validate --source source.json
agentctl knowledge sync --source source.json --checkout /private/checkout --plan
agentctl --output json knowledge compile \
  --source source.json=/private/checkout \
  --output /private/bundles/revision-1
agentctl knowledge verify --bundle /private/bundles/revision-1
agentctl context \
  --bundle /private/bundles/revision-1 \
  --repository /work/project \
  --task-kind investigation \
  --query multica \
  --render /private/context.md
```

Read-only context commands never fetch. Sync is explicit. Credentials, raw
sessions, prompts, transcripts, worktrees, and harness databases are never
compiled or distributed.

## Authority model

| Concern | Authority |
| --- | --- |
| Direct conversation and execution | Native agent CLI/session |
| Durable issue/run/review lifecycle | Multica |
| Network identity and shared resources | Tailscale + `tailnetctl` |
| Per-host desired state | `macctl` or equivalent host manager |
| Shared policy and knowledge | Independent private Git sources + verified bundle |
| Observation, subscriptions, callbacks | `agentctl` |
| Credentials, raw sessions, caches | Local harness/machine |

## Documents

- [Architecture](docs/architecture.md)
- [Identifiers](docs/identifiers.md)
- [Execution envelope](docs/execution-envelope.md)
- [Adapters](docs/adapters.md)
- [Events and callbacks](docs/events-and-subscriptions.md)
- [Agent ergonomics](docs/agent-ergonomics.md)
- [Context and knowledge](docs/context-and-knowledge.md)
- [Security and privacy](docs/state-security-and-privacy.md)
- [Implementation status](docs/implementation-audit.md)
- [Roadmap](docs/roadmap.md)

Machine-readable contracts live under [`schemas/`](schemas/).
