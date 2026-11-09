# Configuration

## Standalone default

Configuration is optional for local native-agent execution. Without a config
file, agentctl discovers built-in adapter executables from `PATH`, uses its
documented local state paths, and reports absent configuration as optional in
`doctor`.

## Location and format

The default JSON path is:

1. `AGENTCTL_CONFIG`, when set;
2. `$XDG_CONFIG_HOME/agentctl/config.json`; or
3. `~/.config/agentctl/config.json`.

The global `--config <absolute-path>` flag selects a file for one invocation.
The file is schema-versioned, must be a regular owner-only `0600` file, and may
not be reached through symlinked parent directories.
Load errors distinguish those file-security requirements from malformed JSON
or configuration validation failures. Writes still refuse unmanaged content
unless the caller explicitly enables overwrite.

```json
{
  "schema_version": 1,
  "default_profile": "local",
  "profiles": {
    "local": {
      "adapters": {
        "codex": {"executable": "/opt/agent-tools/bin/codex"},
        "cursor": {"executable": "/opt/agent-tools/bin/cursor-agent"}
      },
      "agent_preferences": {
        "mode": "advisory",
        "preferred": [
          {"agent": "cursor", "model": "composer-2.5", "speed": "regular", "use_for": "default"},
          {"agent": "cursor", "model": "cursor-grok-4.6-high", "speed": "regular", "use_for": "harder_tasks"}
        ],
        "notes": ["Never select fast model variants."]
      }
    }
  }
}
```

Host-local bootstrap policy is optional and omitted by default (`manage`). Set
it only to disable instruction-pointer writes:

```json
{
  "schema_version": 1,
  "bootstrap": {
    "instruction_pointers": "off"
  }
}
```

Git config bundles cannot include this field. Source init/update/restore
preserve an existing live value rather than treating it as bundle drift.

Use the CLI to write a profile atomically only for a manual host-local setup.
For a Git-backed live config, skip this command and initialize the source first.

```bash
agentctl config set-profile \
  --name local \
  --default \
  --adapter codex=/opt/agent-tools/bin/codex

agentctl config show
agentctl config validate
agentctl config doctor  # includes Git-source drift when configured
agentctl config doctor --live  # also asks native CLIs what they offer now
```

`config validate` is structural. `config doctor` additionally verifies the
selected profile's executable and authority provenance. Adapter and endpoint
credentials remain in the native CLI, operating-system credential store, or
another operator-owned secret system; they are not config fields.

`config doctor` compares config with compiled recipes only, so it can call a
model compatible that the native CLI no longer offers. `config doctor --live`
adds `live_catalog`: it asks each installed Codex (`debug models`), Cursor
(`--list-models`) and Devin (the options of an idle ACP session) CLI whether
each preferred model and effort is offered on this host now. It sends no prompt
and runs no model turn. Claude Code, ZCode and `devin-cloud` report `unverified`
because they have no reviewed catalog, or reading one would open a remote
session; a probe that cannot answer also reports `unverified`. A missing
executable or an absent model or effort is `unavailable`, which exits with
`dependency_unavailable` and the report at `error.details.report`. See
[live-catalog-report](../schemas/live-catalog-report.schema.json).

`agent_preferences` is ordered, portable guidance for callers and delegated
agents. Its only supported mode is `advisory`; agentctl does not invent a second
preference document or silently remap argv onto a different adapter or model.
`agent`, `model`, `speed`, `use_for`, and `notes` are intentionally text fields
so profiles can describe native tools without agentctl owning their vocabulary.

When a profile has `preferred[]`, that table is the reviewed adapter+model set
for `delegate`, `dispatch`, and `route explain`: off-policy selectors fail closed or return
no model match. Direct `run` stays caller-authoritative: off-policy argv emits
a warning and still launches. Without `preferred[]`, `route explain` still ranks
built-in adapter family aliases such as `glm` or `codex`.

Optional `route` data feeds `agentctl route explain <query>`. `this_host` is the
local machine id. `hosts` maps nicknames onto those ids. `placement.kind` names
whatever remote authority the operator configured (Multica is one kind).
For native delegate, optional `ssh_hosts` maps canonical host ids to SSH
destinations; absent entries use the canonical host id. It accepts up to 64
entries with nonempty ids/destinations of at most 128 bytes. SSH configuration
owns authentication and jump hosts; `AGENTCTL_SSH` overrides the executable.
Remote selectors and pools resolve on the destination, which must recognize the
forwarded host alias as local and already have the same absolute cwd. None
of these fields are required. Without `preferred[]`, the matcher still ranks
built-in adapter family aliases such as `glm` or `codex`. With `preferred[]`,
only those reviewed models match.
Without `this_host`, a matched host produces `need_this_host`; agentctl does not
guess fleet identity from an operating-system hostname. `use_for` contributes
route aliases only in the explicit `alias:name,other-name` form; ordinary prose
remains advisory text rather than executable routing policy.

Structured `delegate` also accepts optional `family`, `version`, `effort`, and
`default` metadata on preferred entries. `speed` may be omitted when a harness
has no service-speed control. Explicit requested settings must match a reviewed
entry and a supported native recipe. Claude Code supports effort values `low`,
`medium`, `high`, `xhigh`, and `max` through its native `--effort` flag; other
harnesses retain their own recipe limits. `delegation.cursor_workspace_trust`
is a separate boolean grant for Cursor workspace trust.
`delegation.unattended_coding_permissions` is a separate boolean grant that
lets `delegate` add Cursor `--force`, Codex
`--dangerously-bypass-approvals-and-sandbox`, Claude Code
`--permission-mode bypassPermissions`, OpenCode `--auto`, and Devin ACP
one-operation permission approvals on coding launches. Claude Code and OpenCode
refuse `read_only` because their edit enforcement is unverified. Neither grant
is implied by a model alias, and neither rewrites expert `run` argv. See [structured delegation](structured-delegation.md)
for the request schema, deterministic defaults, and replay contract. Upgrade the
binary before applying config fields an older version does not recognize.

## Agent Nexus run links

The top-level optional `anx.run_links` section configures how agentctl
registers labeled executions with Agent Nexus (see
[optional integrations](optional-integrations.md)):

```json
"anx": {
  "run_links": {
    "default": "labeled",
    "workspace_alias": "anx",
    "allowed_orgs": [],
    "executable": "anx",
    "flush_timeout_seconds": 10
  }
}
```

`default` accepts `labeled` (the default and zero-config behavior: register
runs carrying an `anx.card.<slug>` label), `all` (also register unlabeled
runs; these park as `card_unresolved` until ANX supports card-independent
run records), and `off`. Omit `workspace_alias` to use the ANX CLI's directory
rules/default for each run cwd; the resolved alias is persisted before delivery
and retained for retries. An explicit alias overrides cwd rules for new entries.
`workspace_alias`, `base_url`, and `config_dir` are passed
explicitly to the `anx` CLI; agentctl never hard-codes a base URL and reads no
ANX credentials — authentication and workspace resolution stay with the `anx`
CLI's own credential store. Free-text summary support is absent; the former
`summary_max_bytes` option is no longer accepted.
`flush_timeout_seconds` bounds detached delivery; it never delays a run return. `spool_dir` overrides the default
`<state dir>/anx-run-links` outbox. `allowed_orgs` is an optional owner
restriction (at most 64 names): empty, the runtime default, allows any owner;
a non-empty list restricts repository metadata and typed PR/issue evidence
to those owners. PR/issue evidence must also match the run CWD's local
`origin` owner/repository. Registrations send `cwd` and origin
`repository: {host, owner, name}` for any remote host. URL userinfo is stripped
before parsing. Git lookup clears all inherited `GIT_*` overrides. Repository
components need only be well-formed; there is no GitHub charset, 100-character,
IP-like-name or Tailnet-name restriction. Each outbound field still passes
credential detection; only a matching field is omitted.
This follows David's 2026-10-24 ruling permitting repository, user and local
path disclosure. Suggested shared-workspace restrictions are documented in
[the agentctl-config proposal](agentctl-config-run-links.md).
Registration v5 adds `cwd` and `repository`; existing v4 spool entries remain
deliverable with their typed evidence under the same validation and origin checks.
Result text/JSON lines are never parsed as evidence. Prompt, transcript and
answer text never go out. Card refs require a fresh ANX workspace catalog
confirmation in the delivery batch. Without this section, labeled runs still
register with these defaults.
`agentctl anx status` emits validated workspace aliases (`[redacted]`
otherwise), a credential-checked `spool_dir` path (`[redacted]` on a match),
spool `present`/`absent`, and pending/dead-letter counts. It remains read-only.

## Profile selection

`--profile <name>` selects an exact named profile. If it is omitted, the
configured `default_profile` is used where a command needs profile-backed
settings. Commands that do not need a profile continue to operate without one.
An advisory-guidance-only profile is valid even when it has no adapter or
Multica entry; it can be selected for callers that need preferences without
executable provenance checks.

Setting an existing profile merges provided fields by default. Use `--replace`
only when replacing the entire profile is intentional. Unknown schema versions,
missing selected profiles, incomplete Multica authority records, credentialed
URLs, and unsafe paths fail closed.

`config set-profile` does not author or clear `agent_preferences` or `delegation`. When it
updates adapter or Multica fields on a profile that already has reviewed
preferences, it preserves those preferences, including with `--replace`.

## Explicit operator-managed bundles

A team or individual may keep nonsecret profiles in a separate public or
private Git repository. The config-bundle schema is intentionally narrower than
the local config schema:

```json
{
  "schema_version": 1,
  "default_profile": "coordinated",
  "profiles": {
    "coordinated": {
      "adapters": {
        "codex": {"executable": "/opt/agent-tools/bin/codex"}
      },
      "agent_preferences": {
        "mode": "advisory",
        "preferred": [
          {"agent": "cursor", "model": "composer-2.5", "speed": "regular", "use_for": "default"}
        ]
      },
      "multica": {
        "executable": "/opt/agent-tools/bin/multica",
        "profile": "example-profile",
        "workspace_id": "example-workspace",
        "server_url": "https://coordination.example.com",
        "app_url": "https://coordination.example.com"
      }
    }
  }
}
```

Select one exact local file explicitly:

```bash
agentctl --config-bundle /absolute/path/to/config-bundle.json \
  config bundle validate
agentctl --config-bundle /absolute/path/to/config-bundle.json \
  config bundle show
agentctl --config-bundle /absolute/path/to/config-bundle.json \
  config bundle plan
```

The bundle must be a regular non-symlink file and must not be group- or
world-writable. A normal read-only Git checkout file may therefore be `0644`.
The commands report its exact source path, byte count, schema version, and
SHA-256 digest.

Composition with `--config-bundle` is read-only and invocation-scoped. Bundle
profiles are added after safe built-ins and the selected user config. A profile collision fails closed
unless the definitions are identical. A bundle default cannot replace a
different user-config default.

Bundles can contain adapter executable expectations, advisory agent
preferences, and an optional exact Multica authority. They cannot add adapter
arguments, callback commands,
installation roots, tokens, or arbitrary fields. An adapter executable in a
bundle is a profile/doctor expectation; it never rewrites the native argv after
`agentctl run --`.

For a durable Git-backed live config, initialize an explicit source:

```bash
agentctl config source init \
  --remote git@github.com:owner/agentctl-config.git \
  --ref main \
  --plan
agentctl config source init \
  --remote git@github.com:owner/agentctl-config.git \
  --ref main
agentctl config source status
agentctl config source update
agentctl config doctor
```

The default repository file is `config-bundle.json`; override it with
`--bundle <repository-relative-path>`. The default managed checkout is
`$XDG_DATA_HOME/agentctl/config-source`, falling back to
`~/.local/share/agentctl/config-source`; override it at initialization with
`--checkout <absolute-path>`.

`source init` and `source update` are explicit network and local-write actions.
They ask native Git/SSH to authenticate noninteractively, validate the fetched
bundle before applying it, materialize a canonical owner-only `0600` live
config, and record its commit and content digests in an owner-only state file.
On first setup, an existing valid live config may be adopted when the bundle
only adds missing profiles or profile fields. Any replacement or removal of an
existing value remains a conflict, and the error reports the specific reason.
An update reports `changed: true` when the pinned source revision advances,
even if that commit leaves the materialized bundle bytes unchanged.
The checkout is a managed cache, not an editing workspace. Dirty checkouts,
live-config edits, remote changes, invalid bundles, and non-fast-forward Git
history fail closed. Edit and push through a separate authoring clone.

`source status` and either command's `--plan` form perform no fetch and make no
filesystem changes. Plan output distinguishes the current plan invocation from
the eventual apply invocation and states that the remote and bundle were not
validated. Updates are never implicit during `run`, `doctor`, or config reads.
Repository credentials and SSH configuration remain entirely owned by native
Git/SSH.

The bundle may select a separate Git-backed Skill Hub manifest and update
policy. Skill bodies remain exclusively in that Hub rather than being copied
into the config repository. `agentctl skills update` pins a fast-forward Hub
revision and auto-reconciles clean managed copies; read-only projections never
fetch. See [Managed Skill Hub packs](managed-skills.md).

On a new Mac, verify prerequisites before initialization:

```bash
git --version
ssh -T git@github.com  # or the equivalent check for your Git host
agentctl config source init --remote git@github.com:owner/agentctl-config.git --plan
```

The repository must contain `config-bundle.json` at the selected ref unless
`--bundle` names another repository-relative file. The plan is a local syntax
and path review, not a network preflight. The real init reports typed Git
dependency or authorization failures.

If `source status` reports only `live_config` drift, inspect and restore the
exact already-pinned bundle without a fetch:

```bash
agentctl config source restore --plan
agentctl config source restore
```

Dirty checkout, checkout revision, or bundle drift is never overwritten by
restore. Fix the authoring repository or managed-checkout cause first. A
non-fast-forward remote update is rejected; restore the reviewed remote history
or choose a new source/ref rather than silently accepting rewritten history.
`doctor` includes configured source status and becomes unhealthy on source
drift.

Do not place API tokens, SSH keys, callback signing secrets, cookies, prompts,
results, or native session data in such a repository merely because it is
private.

## Precedence

The implemented precedence is deliberately small:

1. safe built-in behavior;
2. the selected user config file;
3. one explicit additive `--config-bundle`; and
4. explicit command-line flags and profile selection.

The Git source materializes the live user config; it is not an additional
runtime precedence layer. Local `config set-profile` changes deliberately
produce drift and must be reconciled in the source repository.

Environment variables select documented paths or context handles; they are not
a general policy scripting layer. Configuration is data, not executable code.

## Journal retention

The host-local `journal` key controls maintenance by an explicitly running
supervisor. It defaults to on with an age cutoff of 14 exact 24-hour days:

```json
{"journal":{"retention":{"enabled":true,"days":14}}}
```

Set `journal.retention.enabled` to `false` to disable automatic deletion.
Omitted or zero `days` means 14; the supported range is 1–3650. The supervisor
reloads this key each maintenance pass (at most every five minutes); a policy edit does not require a restart. Invalid
config defers retention and surfaces a maintenance error. Source sync preserves
this host-local policy; additive config bundles cannot set it.

The [policy schema](../schemas/journal-policy.schema.json) describes the value
under `journal`. Only acknowledged, terminal, lineage-free records strictly
older than the cutoff qualify. Subscriptions, delivery references and other
cleanup protections still apply. Request-key/immutable bindings, source-bound
records (including Multica), and ANX run links are exempt from automatic retention
so retries and external links keep their original target. ANX links also protect
manual cleanup. Manual cleanup replaces request-key bindings with compact durable refusal
tombstones: the same key and digest return non-retryable `conflict` (exit 8),
`diagnostic_code=execution_replay_expired`, and the original execution ID.
Changed inputs still conflict. A distinct key is required for deliberate new
work. Caller/delegation snapshots and outcomes are reclaimed.

Maintenance runs at most once every five minutes, with a 30-second deadline and
at most one batch of 128 events and one execution. Copied metadata pages and a
cursor survive a timeout in the running supervisor; no DB handle survives a page.
The same stable proof is reused for selection and commit, avoiding a second full
scan. Intervening commits invalidate it safely, including writes by old binaries.
Busy, conflict and timeout deferrals are logged to stderr and remain visible in
supervisor health as `last_maintenance_error` with `last_maintenance_error_at`
from the actual failed pass; cadence ticks do not refresh that timestamp. A
completed, unchanged bbolt generation needs only one short read on the next
pass. Subscription expiry, the next terminal ageing into the cutoff, or changed external link
protections forces a rescan; a deletion also invalidates the fast path so a
backlog continues draining.
No CLI read runs retention. No separate retention environment switch is used.
See [retention and cleanup](state-security-and-privacy.md#retention-and-cleanup).
