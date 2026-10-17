# agentctl

Let your coding agent delegate to other coding agents.

Ask Claude Code to get a review from Codex, or have Codex hand a bug to Grok
in Cursor. agentctl launches the other agent's CLI, tracks the run, and brings
the answer back, even if the agent that asked has moved on or lost its context.

```text
You, in Claude Code:

  > Have Codex review this branch, then send its findings back and ask it to fix them.

What Claude runs:

  $ agentctl delegate --request-file review.json --prompt-file review.md --wait --content
  exec-snack-need-scrap-group-correct-marriage state=completed harness=codex
  <Codex's review>

  $ agentctl continue exec-snack-need-scrap-group-correct-marriage \
      --request-key fix-01 --prompt-file fix.md --wait --content
  <the same Codex session, now applying its fixes>
```

You talk to your agent in plain language. agentctl ships a skill that teaches
the agent these commands, so you never write the JSON yourself.

> **Pre-1.0:** commands, JSON output, and config formats may still change.

## Why use it

- **Ask for a model by name.** "codex", "grok", or "claude" resolves to the
  right CLI, model, and flags from your config. Your agent doesn't need to
  know any other CLI's flags.
- **Set it up once, use it on every machine.** Keep your agent and model
  choices in one Git repository. Each machine pulls the same config, and the
  installer adds the agentctl skill to every agent CLI it finds.
- **Answers don't get lost.** Every run gets an ID and a stored result. If the
  asking agent times out, crashes, or starts a new session, `agentctl inbox`
  shows what finished and `agentctl result` returns the answer. Retrying with
  the same request key returns the original run instead of starting a second
  one.

agentctl doesn't replace your agent CLIs, proxy model APIs, or store your
credentials or transcripts. Each CLI keeps its own login, permissions, and
sessions.

## Supported agents

| Agent | Executable |
| --- | --- |
| Claude Code | `claude` |
| Codex | `codex` |
| Cursor | `cursor-agent` |
| Devin | `devin` |
| ZCode | `zcode` |
| OMP | `omp` |
| OpenCode | `opencode` |
| Any CLI with structured output | configured |

Follow-up turns and steering depend on what each CLI supports;
`agentctl capabilities <agent>` reports the current answer. agentctl can also
dispatch work to [Multica](docs/optional-integrations.md) agents on other
machines.

## Install

Download the archive for your platform and `SHA256SUMS` from the
[latest release](https://github.com/Git-on-my-level/agentctl/releases/latest),
verify it, and run the bundled installer:

```bash
grep " agentctl_<version>_darwin_arm64.tar.gz\$" SHA256SUMS | shasum -a 256 -c -
tar -xzf agentctl_<version>_darwin_arm64.tar.gz
cd agentctl_<version>_darwin_arm64
scripts/install.sh --binary "$PWD/agentctl"
agentctl doctor
```

Releases are built for macOS and Linux on arm64 and amd64. The binary goes in
`~/.local/bin`, and the installer adds the agentctl skill to each agent CLI it
detects. agentctl updates itself once a day; `agentctl update policy notify`
or `off` changes that.

To build from source instead, you need Go:

```bash
git clone https://github.com/Git-on-my-level/agentctl.git
cd agentctl
make ci
go build -o build/agentctl ./cmd/agentctl
```

## Configure your agents

List the agents and models you want to delegate to in
`~/.config/agentctl/config.json`. Only listed entries can be launched by name,
and `default` breaks ties when a name matches more than one entry:

```json
{
  "schema_version": 1,
  "default_profile": "local",
  "profiles": {
    "local": {
      "agent_preferences": {
        "mode": "advisory",
        "preferred": [
          {"agent": "codex", "family": "gpt", "model": "<codex model>", "speed": "regular", "default": true},
          {"agent": "cursor", "family": "grok", "model": "<cursor model>", "speed": "regular"},
          {"agent": "claude-code", "family": "claude", "model": "<claude model>"}
        ]
      }
    }
  }
}
```

```bash
agentctl config doctor
```

To share this across machines, commit the same JSON as `config-bundle.json` in
a Git repository and point each machine at it:

```bash
agentctl config source init --remote git@github.com:you/agentctl-config.git
agentctl config source update   # pull later changes
```

agentctl adds flags that skip an agent's approvals or sandbox only when your
config grants it. See [Structured delegation](docs/structured-delegation.md)
for permissions, workspace trust, and matching rules, and
[Configuration](docs/configuration.md) for everything else.

## Learn more

`agentctl help` lists every command; `agentctl help <command>` explains one.

- [Structured delegation](docs/structured-delegation.md)
- [Follow-up turns](docs/follow-up-turns.md) and [steering](docs/steering.md)
- [Running several agents at once](docs/fanout.md)
- [Adapters](docs/adapters.md)
- [Architecture](docs/architecture.md)
- [State, security, and privacy](docs/state-security-and-privacy.md)
- [Roadmap](docs/roadmap.md)

To contribute, see [Contributing](CONTRIBUTING.md). Report vulnerabilities as
described in [Security](SECURITY.md).

## License

[Apache License 2.0](LICENSE). Third-party notices are in [NOTICE](NOTICE).
