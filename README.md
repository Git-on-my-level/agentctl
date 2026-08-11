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
- normative `json` output by default, with an explicit compact `text` escape;
- deterministic knowledge validation, explicit Git sync, loose/structured/
  hybrid compilation, bundle verification/install, lexical selection, and
  bounded context rendering;
- owner-only file and Unix callbacks, explicit command callbacks, and signed
  webhook delivery with SSRF and DNS-rebinding protections;
- an optional restart-capable local supervisor plus launchd/systemd install
  plans; updates reconcile an already-managed supervisor but never create a
  new service implicitly;
- allowlisted portable skill distribution for Hermes, Codex, Claude, Cursor,
  OMP, and Multica;
- a companion Multica fork change adding durable workspace events and
  authority-owned issue-create idempotency keys.

The CLI is usable now, but v0.1 retains explicit boundaries: native sessions
are generally observable only by the process that launched them; remote host
attach/discovery is not implemented; creating a new supervisor service remains
an explicit plan-derived host operation; and Tailnet publishing of compiled
bundles remains an external deployment step.
See [Implementation status](docs/implementation-audit.md).

## Quick start

Build and discover the live contract. JSON is the default so agents can consume
one stable document; add `--output text` only for a compact human projection:

```bash
make ci
go build -o build/agentctl ./cmd/agentctl
build/agentctl help
build/agentctl help run
build/agentctl doctor
build/agentctl capabilities codex --require launch,result_content --executable /path/to/codex
build/agentctl capabilities codex --static --full
build/agentctl bootstrap status
```

The portable skill resolves `agentctl` deterministically from `AGENTCTL_BIN`,
`PATH`, then `$HOME/.local/bin/agentctl`; it does not assume an interactive
shell. `doctor` answers whether detected agents can launch, be observed, and
return a result. `bootstrap status` reports the exact binary resolution plus
missing, duplicate, noncanonical, or drifted skill registrations across the
harnesses present on a machine. Use `agentctl help <topic>` for just-in-time
syntax and typed `next_actions` instead of relying on a copied runbook.

Wrap a native argv without reparsing anything after `--`. Adapter names are
inferred from known executable names; retain `--adapter` when an executable is
ambiguous or when an explicit override is part of the authority contract:

```bash
agentctl run -- codex exec --json -m gpt-5.6-sol "review this change"
agentctl run -- cursor-agent --print --output-format stream-json --trust "scope this bug"
agentctl run -- omp "investigate the service"
```

For synchronous work, `run` returns the terminal `exec-*` envelope. For live
observation, preallocate the ID, create any subscriptions, and then start the
foreground runner from the parent agent's native process manager:

```bash
EXEC_ID=$(agentctl id generate exec --output text | cut -d' ' -f1)
agentctl subscribe create \
  --execution "$EXEC_ID" \
  --destination file \
  --target /absolute/path/events.ndjson
agentctl run --execution-id "$EXEC_ID" -- codex exec --json "review this change"

agentctl status "$EXEC_ID"
agentctl events "$EXEC_ID" --after-sequence 0
agentctl await "$EXEC_ID"
agentctl result "$EXEC_ID"
```

The runner opens the journal only for bounded transactions, so a separate
supervisor can deliver callbacks while work is active. A short-lived runner
lease prevents that supervisor from misclassifying a process-owned session as
unreachable; after a crash the lease expires and ordinary recovery resumes.
Native cross-process cancel remains unavailable unless that adapter advertises
a reviewed durable cancel mechanism; agentctl never guesses from a PID.
The final normalized answer is stored with the execution and addressed by its
portable `agentctl://host-.../exec-...` result reference; callers never need to
search native rollout files. `result` requires stored content by default;
`result --allow-empty` is the explicit metadata-only escape. Status, events,
and callbacks remain metadata-only.

`await` uses a bounded ten-minute timeout and stops on actionable attention by
default. Use `--timeout` for a different bound or `--ignore-attention` when a
caller intentionally wants to continue waiting. `run` applies a thirty-minute
bound and preflights both `launch` and `result_content`; use `--no-timeout`,
`--allow-missing-result`, or `--no-store-result` only when the surrounding
authority explicitly permits those weaker semantics.

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
a second lifecycle. The Multica description receives bounded handoff content on
stdin plus content digests for the installed portable skill, selected context,
and handoff. Local file paths, prompts, and transcripts are not sent.

## Portable skill reconciliation

`agentctl bootstrap update` is the normal idempotent path. It detects supported
harnesses, deduplicates shared roots, and installs or upgrades only the
embedded portable skill in canonical locations. It updates a supervisor that
agentctl already manages, but does not create a new launchd/systemd service or
delete legacy copies by default. For Codex and OMP, `~/.agents/skills` is the
canonical shared root; compatibility copies such as `~/.codex/skills` should be
removed only through an explicit cleanup after `bootstrap status` proves the
canonical registration. Other canonical roots are `~/.hermes/skills`,
`~/.claude/skills`, and `~/.cursor/skills`.

```bash
agentctl bootstrap update --dry-run
agentctl bootstrap update
agentctl bootstrap update --harness cursor
agentctl bootstrap status
```

The lower-level distribution scripts remain useful for disposable roots and
explicit harness-manager integrations. Upgrade is allowed only when the
installed revision manifest proves that every managed asset is unmodified;
uninstall removes only manifest-bound files and preserves unrelated harness
content.

The release installer follows the same policy: `scripts/install.sh` updates the
binary, then runs `bootstrap update` across detected harnesses and reconciles a
launchd supervisor only when its existing manifest proves agentctl already
manages it. `--binary-only` is the explicit escape for a binary-only rollout.

## Knowledge sources

Knowledge remains in independently owned GitHub, Forgejo, or generic Git
repositories. Existing loose stores do not need migration.

```bash
agentctl knowledge validate --source source.json
agentctl knowledge sync --source source.json --checkout /private/checkout --plan
agentctl knowledge compile \
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
