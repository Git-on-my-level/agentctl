# agentctl

`agentctl` is a local, agent-oriented control layer for native coding-agent
CLIs. It launches an existing CLI, records normalized lifecycle metadata,
delivers bounded events and callbacks, and stores a retrievable final result.
The native CLI keeps ownership of model selection, authentication, permissions,
and session behavior.

The standalone path needs no control-plane service, private network, fleet
manager, or private configuration repository. Optional integrations add durable
coordination and operator-specific topology without changing the core execution
contract.

> **Public preview:** agentctl is pre-1.0. Command, JSON, configuration, and
> journal formats may evolve with documented migrations. Review the
> [known constraints](docs/implementation-audit.md#intentional-preview-constraints)
> before relying on it for unattended or durable work.

## What it does

- launches Codex, Cursor, Claude Code, OMP, or a structured generic process;
- returns JSON by default and exposes progressive `agentctl help <topic>`
  discovery;
- allocates typed, checksum-validated word IDs and portable result references;
- persists normalized state and bounded final results in an owner-only local
  journal;
- supports pre-launch subscriptions and at-least-once file, Unix-socket,
  command, and signed-webhook delivery;
- installs one allowlisted portable skill into detected supported harnesses;
- optionally validates, compiles, and renders context from independently owned
  Git knowledge sources; and
- optionally promotes direct work into a configured Multica authority.

It is not a model gateway, transcript database, issue tracker, credential
manager, or replacement for the agent CLI being supervised.

## Install a release

Release archives contain the binary, installer, schemas, documentation, and
portable skill. Download both the archive for your platform and
`SHA256SUMS` from the [latest release](https://github.com/Git-on-my-level/agentctl/releases/latest),
then verify the archive before installing it. For example, on Apple silicon:

```bash
VERSION=$(gh release view --repo Git-on-my-level/agentctl --json tagName --jq .tagName)
ARCHIVE="agentctl_${VERSION}_darwin_arm64.tar.gz"

gh release download "$VERSION" \
  --repo Git-on-my-level/agentctl \
  --pattern "$ARCHIVE" \
  --pattern SHA256SUMS
grep " $ARCHIVE\$" SHA256SUMS | shasum -a 256 -c -
tar -xzf "$ARCHIVE"
cd "agentctl_${VERSION}_darwin_arm64"

scripts/install.sh --binary "$PWD/agentctl" --dry-run
scripts/install.sh --binary "$PWD/agentctl"
agentctl doctor
```

Use `darwin_amd64`, `linux_amd64`, or `linux_arm64` for another supported
platform. On Linux, replace `shasum -a 256` with `sha256sum`. The default
install prefix is `~/.local`; ensure `~/.local/bin` is on `PATH`. The installer
does not download code and previews its changes with `--dry-run`.

## Build from source

Requirements: a supported platform, the Go version declared in `go.mod` or
newer, and at least one native agent CLI. Building and the read-only discovery
commands do not require private configuration.

```bash
git clone https://github.com/Git-on-my-level/agentctl.git
cd agentctl
make ci
go build -o build/agentctl ./cmd/agentctl

build/agentctl help
build/agentctl doctor
build/agentctl capabilities codex --require launch,result_content
```

`doctor` reports detected adapters, journal readiness, optional configuration,
portable-skill state, and supervisor state. Use `--output text` when a compact
human projection is preferable to the default JSON.

Launch a native CLI by placing its exact argv after `--`:

```bash
agentctl run -- codex exec --json "review the current change"
agentctl run -- cursor-agent --print --output-format stream-json --trust "scope this bug"
agentctl run -- claude --output-format stream-json "review this patch"
```

Known executable names select their built-in adapter. Use `--adapter` for an
ambiguous executable or a deliberate override. `run` waits for a terminal
result, uses a 30-minute timeout, and requires result-content support by
default. The native CLI's arguments, permissions, and provider authentication
remain unchanged.

For live observation, allocate the execution ID and subscription before launch:

```bash
EXEC_ID=$(agentctl id generate exec --output text | cut -d' ' -f1)
agentctl subscribe create \
  --execution "$EXEC_ID" \
  --destination file \
  --target "$PWD/agentctl-events.ndjson"
agentctl run --execution-id "$EXEC_ID" -- codex exec --json "review this change"

agentctl status "$EXEC_ID"
agentctl events "$EXEC_ID" --after-sequence 0
agentctl await "$EXEC_ID"
agentctl result "$EXEC_ID"
```

`status`, events, subscriptions, and callbacks contain metadata and references,
not the final answer. `result` is the explicit content-retrieval path.

## Data-storage disclosure

By default, agentctl stores the final UTF-8 result body in its local journal,
bounded to **1 MiB per execution**. This makes delegated output retrievable
without scraping a native harness's session files. `--no-store-result` records
an omission tombstone instead; `result --allow-empty` permits a metadata-only
read.

**Automatic journal retention is not implemented in this preview.** Inspect
owner-only local usage with `agentctl data inventory`. Cleanup is operator
initiated: `agentctl data cleanup --before <RFC3339> --plan` reports exact
eligible graphs, protected references, logical bytes, and a plan digest. Apply
requires that digest with `--apply --plan-digest ...` and rejects a changed
plan. Promotion-linked executions remain conservatively protected. Do not run
agentctl with sensitive prompts or results unless this local persistence is
acceptable. See [State, security, privacy, and retention](docs/state-security-and-privacy.md).

## Configuration

Configuration is optional for the standalone native-agent path. When used, it
is a versioned, owner-only JSON document with named profiles. It records exact
native executable expectations for `doctor`, optional advisory
agent/model/speed preferences, and an optional runtime-effective Multica
authority. Preferences are discoverable guidance only: config does not contain
credentials, enforce a model choice, or alter the exact argv supplied to
`run --`.

The default path is `$XDG_CONFIG_HOME/agentctl/config.json`, falling back to
`~/.config/agentctl/config.json`. `AGENTCTL_CONFIG` or the global `--config`
flag selects an explicit path.

Operator-specific executable and optional authority profiles can live in a
separately reviewed repository as a narrow config bundle. Use it for one
invocation, or explicitly materialize it as the owner-only live config:

```bash
agentctl --config-bundle /absolute/path/to/config-bundle.json \
  config bundle validate
agentctl --config-bundle /absolute/path/to/config-bundle.json \
  config bundle plan
agentctl --config-bundle /absolute/path/to/config-bundle.json \
  --profile coordinated doctor
agentctl config source init \
  --remote git@github.com:owner/agentctl-config.git \
  --ref main \
  --plan
agentctl config source init \
  --remote git@github.com:owner/agentctl-config.git \
  --ref main
agentctl config source update
agentctl config source status
agentctl config source restore --plan  # only for live-config drift
```

The invocation-scoped bundle composes additively. It can state advisory agent
preferences, but cannot enforce them or rewrite native argv. It cannot replace
a different user profile/default, provide adapter arguments, configure callbacks
or install roots, or contain secrets. The plan reports its SHA-256 provenance
and does not mutate local config. A configured Git source updates only on the
explicit `source update` command, accepts fast-forwards only, and fails closed
on checkout or live-config drift. Git/SSH continues to own credentials. See
[Configuration](docs/configuration.md).
First-time source setup may safely add missing bundle fields to an existing
valid live config, but never replaces or removes an existing value implicitly.

Choose one setup path:

- durable team or personal Git config: run `config source init` before any
  `set-profile` command;
- one-off bundle inspection: pass `--config-bundle` for that invocation; or
- manual host-local config: use `config set-profile` and do not initialize a
  Git source for the same file.

For a private SSH remote on a new Mac, first verify `git --version` and normal
Git-host SSH access. agentctl runs Git noninteractively and never owns SSH keys
or tokens. Release installs use `~/.local/bin` by default; ensure that directory
is on `PATH`. macOS intentionally uses the same XDG-style config and data paths
as Linux.

Manual host-local setup remains available:

```bash
agentctl config set-profile \
  --name local \
  --default \
  --adapter codex=/absolute/path/to/codex \
  --adapter cursor=/absolute/path/to/cursor-agent
agentctl config validate
agentctl config doctor
```

## Portable skill reconciliation

`agentctl bootstrap update` detects supported harnesses and installs or upgrades
only agentctl's embedded portable skill in canonical locations. It leaves
unmanaged files, credentials, sessions, settings, caches, and legacy copies
alone. An existing agentctl-managed supervisor may be reconciled; a new service
is never created merely because a harness was detected.

```bash
agentctl bootstrap update --dry-run
agentctl bootstrap update
agentctl bootstrap status
```

The release installer performs the same detected-harness reconciliation by
default. Use `--binary-only` only when a binary-only installation is
intentional.

## Support matrix

| Area | Preview support | Boundary |
| --- | --- | --- |
| macOS | amd64 and arm64 release targets | launchd supervisor reconciliation is implemented for an already-managed service |
| Linux | amd64 and arm64 release targets | explicit systemd-user installer consumes the reviewed plan; rerun it after binary updates because binary install does not create or restart the service |
| Codex | built-in structured-output adapter | attach, observation, result, and cancel beyond the launching process depend on native CLI support |
| Cursor | built-in `stream-json` adapter | workspace trust and approval behavior remain Cursor-owned |
| Claude Code | built-in structured-output adapter | permissions and hooks remain Claude-owned |
| OMP | built-in structured-output adapter | some event and result-content semantics are reported as degraded |
| Generic process | explicit structured-result adapter | a zero exit code alone is not treated as task success |
| Multica | optional configured integration | promotion and durable workspace events require a compatible Multica deployment |

The exact live answer for an installed backend comes from
`agentctl capabilities <adapter>` or `agentctl doctor`, not from this table.
See [Adapters](docs/adapters.md) for capability semantics.

## Optional integrations

The following are not required for local use:

- Multica for durable issue/run/review coordination and explicit promotion;
- Tailscale or another network layer for operator-managed reachability;
- `tailnetctl`, `macctl`, or another fleet tool for resource and host desired
  state; and
- independently owned Git repositories for shared knowledge or private policy.

These systems retain their own authority. agentctl consumes explicit references
or configuration and does not centralize their credentials or databases. See
[Optional integrations](docs/optional-integrations.md).

## Project status and contributing

The implementation and its limits are tracked in the
[implementation audit](docs/implementation-audit.md) and [roadmap](docs/roadmap.md).
For changes, read [Contributing](CONTRIBUTING.md), the
[Code of Conduct](CODE_OF_CONDUCT.md), and [Support](SUPPORT.md). Report
vulnerabilities through the process in [Security](SECURITY.md).

## Documentation

- [Architecture](docs/architecture.md)
- [Configuration](docs/configuration.md)
- [Adapters](docs/adapters.md)
- [Execution envelope](docs/execution-envelope.md)
- [Events, subscriptions, and callbacks](docs/events-and-subscriptions.md)
- [Identifiers](docs/identifiers.md)
- [Context and knowledge](docs/context-and-knowledge.md)
- [State, security, privacy, and retention](docs/state-security-and-privacy.md)
- [Optional integrations](docs/optional-integrations.md)
- [Agent ergonomics](docs/agent-ergonomics.md)
- [Implementation status](docs/implementation-audit.md)
- [Roadmap](docs/roadmap.md)

Machine-readable contracts live under [`schemas/`](schemas/).

## License

Licensed under the [Apache License 2.0](LICENSE). Third-party notices, including
the vendored BIP-39 English word list, are recorded in [NOTICE](NOTICE).
