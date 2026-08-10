# agentctl

`agentctl` is a portable, agent-friendly supervision and context layer for work
executed by native agent CLIs or by Multica.

It does **not** replace Codex, Claude Code, Cursor, OMP, Multica, `tailnetctl`,
or host management such as `macctl`. It gives those systems a common execution
envelope, deterministic context, normalized events, and reliable callbacks.

## Design status

This repository currently contains the foundational design. No production CLI
or daemon is implemented yet.

The design optimizes for:

- direct agent delegation without creating project-management noise;
- optional promotion into a durable Multica lifecycle;
- native model/runtime ownership (Codex for GPT, Cursor for Cursor models,
  Claude Code for Claude, OMP for open-weight models such as GLM);
- LLM-friendly typed six-word identifiers instead of user-facing UUIDs;
- exact, machine-readable state and error contracts;
- event streams and callbacks even when a backend only supports polling;
- portable, deterministic context for Hermes, Codex, Claude, Cursor, OMP, and
  Multica runtimes;
- independent structured, loose, or hybrid knowledge repositories hosted on
  GitHub, Forgejo, or generic Git servers;
- compact agent-oriented text by default, with JSON when a strict machine
  contract is needed (streaming JSON uses NDJSON framing);
- no syncing of credentials, raw sessions, or harness databases.

## Proposed experience

Wrap a native command without changing its arguments:

```bash
agentctl run --adapter codex -- codex exec --json -m gpt-5.6-sol "review this change"
agentctl run --adapter cursor -- cursor-agent --print --model cursor-grok-4.5-high "scope the bug"
```

Attach to work created elsewhere:

```bash
agentctl attach codex://host-amber-willow-orbit-tiger-harbor-gentle/source-velvet-comet-maple-badger-valley-sparrow
agentctl attach multica://host-amber-willow-orbit-tiger-harbor-gentle/project-silver-otter-canyon-lantern-drift-velvet/issue-quiet-forest-copper-raven-signal-harbor/run-purple-monkey-dragon-river-candle-meadow
```

Subscribe through one interface:

```bash
agentctl subscribe exec-purple-monkey-dragon-river-candle-meadow --on attention,terminal,artifact --to parent
agentctl await exec-purple-monkey-dragon-river-candle-meadow
agentctl events --after cursor-quiet-forest-copper-raven-signal-harbor --output json
```

Promote only when durable coordination becomes valuable:

```bash
agentctl promote exec-purple-monkey-dragon-river-candle-meadow --to multica --project project-silver-otter-canyon-lantern-drift-velvet --brief-file handoff.md
```

## Authority model

| Concern | Authority |
| --- | --- |
| Direct conversation and execution | Native agent CLI/session |
| Durable issues, runs, ownership, review lifecycle | Multica |
| Network identity and shared resource resolution | Tailscale + `tailnetctl` |
| Per-host desired state | Host manager such as `macctl` |
| Shared policy, knowledge, and portable skills | Independent private Git sources, compiled into a verified bundle |
| Normalized observation, subscriptions, and callbacks | `agentctl` |
| Credentials, raw sessions, worktrees, caches | Local harness/machine |

`agentctl` owns operational observation state, not the task's business
lifecycle. Its caches and local journal must be rebuildable or TTL-bound.

The daemonless CLI persists observation/outbox state but performs work only
while a command process is alive. Automatic retry after logout/reboot requires
the optional managed supervisor. IDs are stable locators, while full semantic
hashes drive event deduplication and full authority fingerprints bind imported
objects.

## Documents

- [Architecture](docs/architecture.md)
- [Identifiers](docs/identifiers.md)
- [Execution envelope and lifecycle](docs/execution-envelope.md)
- [Adapters](docs/adapters.md)
- [Events, subscriptions, and callbacks](docs/events-and-subscriptions.md)
- [Agent ergonomics](docs/agent-ergonomics.md)
- [Shared context and knowledge](docs/context-and-knowledge.md)
- [State, privacy, and security](docs/state-security-and-privacy.md)
- [Roadmap](docs/roadmap.md)

Machine-readable draft schemas live under [`schemas/`](schemas/).
