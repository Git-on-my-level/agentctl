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
- orients an agent in the current repository/worktree, configured authority,
  static adapter availability, and workspace-scoped execution state with one
  read-only command;
- discovers recent host-local work by state, adapter, exact metadata label, or unreconciled terminal collection;
- explains an actionable host-local inbox while keeping work outcome separate
  from tool liveness;
- optionally leaves a detached host-local worker owning a long-running native
  process after the launching shell exits;
- supports pre-launch subscriptions and at-least-once file, Unix-socket,
  command, and signed-webhook delivery;
- installs one allowlisted portable skill into detected supported harnesses;
- reconciles reviewed personal skill packs from a pinned Git config source;
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

Exact release builds default to automatic updates. On the first work-creating
CLI use that is due each UTC day, agentctl reads its binary-global owner-only state and, when
needed, starts one detached short-lived worker. The foreground command does not
wait for release discovery or installation. The worker downloads the matching
release archive, verifies it against the published `SHA256SUMS`, and runs its
packaged installer only when the current executable is owned by an agentctl
install manifest. It then exits; no updater service or persistent daemon is
created.
Commands advertised as read-only never trigger update discovery or managed-skill
maintenance as a hidden side effect.

Use `agentctl update status`, `agentctl update now`, or `agentctl update policy
auto|notify|off` to inspect or control this behavior. `notify` retains the
once-per-UTC-day `agentctl_update_available` warning without installing, while
`off` performs no check. `AGENTCTL_UPDATE_MODE` overrides the stored policy and
the legacy `AGENTCTL_UPDATE_CHECK=off` remains a hard-off override.

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
build/agentctl orient
build/agentctl doctor
build/agentctl capabilities codex --require launch,result_content
```

`orient` is the compact first call in an unfamiliar checkout. It reports the
current Git worktree/branch/HEAD/dirty/upstream state, selected profile and
authority, static executable availability, and active/recent journal records
whose stored workspace path is inside this checkout. It never fetches Git,
launches an adapter, probes remote authentication, or creates a missing journal.
Executions without stored workspace metadata are counted as `unscoped` instead
of being guessed into the current checkout. Static `healthy` adapter status
means only that its executable is locally runnable; configured Multica authority
health remains `unknown` because `orient` does not probe its remote service or
authentication.

`--limit` is one total execution-record budget, not a separate cap per list.
Newest active records are returned first because they are actionable; newest
terminal records fill any remaining slots. `matched` still reports the full
workspace-matched count, while `returned` proves the bounded projection size.

`doctor` performs the broader readiness inspection and can live-probe adapters;
use `doctor --static` when launches are not appropriate. Use `--output text`
when a compact human projection is preferable to the default JSON.

Launch a native CLI by placing its exact argv after `--`:

```bash
agentctl run -- codex exec --json "review the current change"
agentctl run -- cursor-agent --print --output-format stream-json --trust "scope this bug"
agentctl run -- cursor-agent --print --output-format stream-json --mode ask --trust "explain this code"
agentctl run -- claude --output-format stream-json "review this patch"
```

Known executable names select their built-in adapter. Use `--adapter` for an
ambiguous executable or a deliberate override. `run` waits for a terminal
result, has no default wall-clock timeout, and requires result-content support
by default. Use `--timeout 2h` when the caller needs a bound. Cancellation sends
the native process group `TERM`, waits five seconds, and then escalates to
`KILL`. The native CLI's arguments, permissions, and provider authentication
remain unchanged.

For work whose outcome is easy to confuse with native-process completion, pass
a bounded task contract separately from the prompt:

```json
{
  "objective_summary": "Diagnose the failing service",
  "side_effect_boundary": "read_only",
  "expected_artifact_kinds": ["root_cause_report"],
  "continuation": {"same_session_required": true}
}
```

```bash
agentctl run --task-contract task-contract.json -- \
  codex exec --json "perform the bounded diagnosis"
```

The file is strict JSON, a regular non-symlink file, and at most 64 KiB.
`status` and `result` retain the typed contract and report acceptance as
external-required: native completion does not prove that an expected artifact
exists or that its actual authority accepted it. Contract files never become
prompts, transcripts, or event payloads. Multica issues remain authoritative
for Multica contracts, so `run --adapter multica --task-contract ...` fails
closed; use the promotion flow instead.

For long-running work, label the execution and explicitly background it:

```bash
agentctl run --background --label review --label retrieval -- \
  cursor-agent --print --output-format stream-json --trust "review this change"
agentctl recent --state nonterminal --liveness alive --label review
agentctl await exec-... --no-timeout
agentctl result exec-...
agentctl result exec-... --content
agentctl workspace owners --path "$PWD"
```

`--background` starts a detached agentctl worker and returns after the execution
is visible in the local journal. The worker, not the launching shell, owns the
native process. It survives the caller exiting but is not restart-durable and
does not turn direct work into a scheduler or remote authority. The worker is
noninteractive and has no controlling terminal. Direct native adapters do not
gain a cross-process cancel route merely because they are backgrounded; use an
explicit `--timeout` when a hard stop is required unless the adapter advertises
durable cancellation. Background prompt delivery accepts `--prompt-file` and
`--prompt-stdin`: the parent reads and bounds the bytes before detaching, then
hands them to the worker through a one-shot pipe. Raw prompt bytes are not
placed in argv, the repository, or the durable journal.
Background launch also rejects `--idempotency-key` until its startup handshake
can return a reused execution's exact ID.
Labels are exact lowercase metadata names, may be repeated up to 16 times, and
never contain or derive from prompt text.

Direct launches also record a local Git worktree identity when `--cwd` (or the
current directory) is inside a repository. `agentctl workspace owners` is the
explicit path-bearing query for those records. With `--path`, it reports only
nonterminal executions launched from that exact linked worktree. This is
cleanup evidence, not an exclusive lock: `exclusive` is always false, and old
nonterminal journal rows without workspace metadata are counted separately so
absence is never silently presented as proof that a worktree is unused.

For a reusable multi-line prompt, select exactly one source and an explicit
delivery mechanism:

```bash
agentctl run --prompt-file "$PWD/task.md" --prompt-delivery argv -- \
  cursor-agent --print --output-format stream-json --trust

agentctl run --background --prompt-stdin --prompt-delivery argv -- \
  cursor-agent --print --output-format stream-json --trust < "/absolute/path/task.md"

agentctl run --prompt-stdin --prompt-delivery stdin -- \
  codex exec --json - < "$PWD/task.md"
```

Prompt files must be regular non-symlink files within the selected `--cwd`
(the current directory by default) and are bounded to 8 MiB. Prompt bytes are
read once, delivered without content rewriting, and never written to status,
events, plan output, or the journal; plan and idempotency metadata use only a
SHA-256 digest, byte count, source, and delivery mode. `argv` delivery appends
one positional argument. `stdin` delivery attaches only the selected prompt
bytes to the native child. agentctl never guesses delivery from an adapter
name.

To run one prompt through several explicit native commands, use a foreground
fan-out manifest:

```json
{
  "schema_version": 1,
  "prompt_file": "task.md",
  "prompt_delivery": "argv",
  "concurrency": 2,
  "children": [
    {
      "argv": ["cursor-agent", "--print", "--output-format", "stream-json", "--trust"]
    },
    {
      "argv": ["codex", "exec", "--json"]
    }
  ]
}
```

```bash
agentctl fanout --plan --manifest "$PWD/fanout.json"
agentctl fanout --manifest "$PWD/fanout.json"
```

`fanout` reads the prompt once, preallocates one `exec-*` ID per child, runs up
to two children concurrently by default, and returns every child ID and state.
The manifest requires `schema_version`, `prompt_file`, and at least one
`children[].argv`; `prompt_delivery` may be set globally or per child. The
prompt file and each relative `children[].cwd` resolve from the manifest
directory. A child with no `cwd` inherits the directory from which agentctl was
invoked. Use `agentctl schema list` to locate the normative manifest schema.
Results remain independently retrievable with `agentctl result <exec-id>`;
`--content` writes exact stored UTF-8 text without an envelope or added newline.
agentctl does not concatenate or synthesize answers. Fan-out is intentionally
foreground-owned: if its invoking process exits, native children are
terminated. Put explicit execution IDs in the manifest when subscriptions must
be created before launch.

For Cursor, omitting `--mode` selects its normal Agent behavior; `--mode ask`
is the read-only Q&A path. Cursor plan mode is rejected by default because its
one-shot terminal result is not yet reliable. `--allow-unreliable-result` is an
explicit escape hatch. Workspace trust remains visible in native argv; an
operator can authorize agents to pass `--trust` through advisory config.

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

If an execution ID was not retained, discover it from the host-local journal:

```bash
agentctl recent
agentctl recent --state nonterminal --adapter cursor
agentctl recent --state nonterminal --liveness alive
agentctl recent --liveness unreachable
agentctl recent --label review --limit 50
agentctl recent --unreconciled
agentctl inbox --stale-after 2h
```

`recent` returns newest-first bounded metadata only. It performs no native
refresh, reads no prompt or result records, and does not merge other hosts.
Repeating `--label` requires every selected label (AND semantics).
`--unreconciled` is the start-of-turn recovery query: terminal executions whose
result has never been acknowledged by `result` or a terminal `await`. Terminals
that already existed when acknowledgement tracking first write-opened the
journal are treated as reconciled so an upgrade does not flood the query with
history.

`inbox` is the read-only start-of-turn summary over the same journal. It names
terminal results that still need collection, current attention states, and
running or unreachable executions whose last observation exceeds an explicit
age bound (one hour by default, configurable from one minute through thirty
days). Each row includes stable reason codes and separate `work_health` and
`tool_health` fields: an unreachable tool is review-worthy but is not evidence
that the work failed. Terminal failures leave the inbox after `result` or a
terminal `await` acknowledges collection. `inbox` performs no native refresh,
result read, acknowledgement write, or cross-host merge. Conflicted normalized
evidence remains actionable even after collection, because acknowledging a
result does not reconcile contradictory authority observations.

If a command reports `diagnostic_code=journal_busy`, retry the same agentctl
invocation with bounded backoff. Do not silently switch to a raw native CLI;
that drops agentctl's supervision, journal, callbacks, and result recovery.

`status`, events, subscriptions, and callbacks contain metadata and references,
not the final answer. `result` is the explicit content-retrieval path.
A successful `result` or terminal `await` records a small local acknowledgement
stamp, so those commands are `local_operational_write` rather than read-only.
Callers can require provenance or a task-specific minimum size with
`result --require-result-source assistant --min-result-bytes N`.
Use `await --no-timeout` for an intentionally unbounded observer. Await still
stops on actionable attention unless `--ignore-attention` is explicit.
Executions launched with `run --timeout` record an absolute deadline;
`await --through-execution-deadline` waits through that deadline plus bounded
terminalization grace. Generated background next actions use this form and
also point nonblocking callers to durable subscription setup.

JSON callers must inspect the envelope's `.ok` field. In shell pipelines,
`$?` normally belongs to the final pipeline stage; use `set -o pipefail` or
capture the agentctl status explicitly rather than treating a downstream
formatter's zero exit as agentctl success.

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

`agentctl bootstrap update` detects supported harnesses, installs or upgrades
agentctl's embedded portable skill in canonical locations, and reconciles a
short marked delegation pointer in documented user-global instruction files
that already exist. It never creates instruction files or changes prose outside
the marked block. It leaves credentials, sessions, settings, caches, and legacy
copies alone. An existing agentctl-managed supervisor may be reconciled; a new
service is never created merely because a harness was detected.

```bash
agentctl bootstrap update --dry-run
agentctl bootstrap update
agentctl bootstrap status
```

The release installer performs the same detected-harness reconciliation by
default. Use `--binary-only` only when a binary-only installation is
intentional.

## Managed personal skills

A configured bundle may select a separate Git-backed Skill Hub manifest. The
config repository owns the source selection and update policy; the Skill Hub
owns manifests and skill bodies. Agentctl pins the Hub independently.

```bash
agentctl skills update --plan
agentctl skills update
agentctl skills status
agentctl skills diff <name>
```

Auto-clean upgrades unchanged marker-owned copies while preserving unmanaged
collisions and local drift for explicit review. `agentctl-portable` remains
bootstrap-owned and is rejected from Hub packs. Omitted skills are not deleted,
and Multica remains unsupported until its adapter advertises a reviewed remote
runtime-bundle installer. See [Managed Skill Hub packs](docs/managed-skills.md).

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
