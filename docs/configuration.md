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
```

`config validate` is structural. `config doctor` additionally verifies the
selected profile's executable and authority provenance. Adapter and endpoint
credentials remain in the native CLI, operating-system credential store, or
another operator-owned secret system; they are not config fields.

`agent_preferences` is ordered, portable guidance for callers and delegated
agents. Its only supported mode is `advisory`; agentctl reports it through
`config show`, `config doctor`, and the top-level `doctor`, but never checks a
native model catalog, blocks a different model, or changes direct CLI argv.
`agent`, `model`, `speed`, `use_for`, and `notes` are intentionally text fields
so profiles can describe native tools without agentctl owning their vocabulary.

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

`config set-profile` does not author or clear `agent_preferences`. When it
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
