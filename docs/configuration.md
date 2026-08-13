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
      }
    }
  }
}
```

Use the CLI to write a profile atomically:

```bash
agentctl config set-profile \
  --name local \
  --default \
  --adapter codex=/opt/agent-tools/bin/codex

agentctl config show
agentctl config validate
agentctl config doctor
```

`config validate` is structural. `config doctor` additionally verifies the
selected profile's executable and authority provenance. Adapter and endpoint
credentials remain in the native CLI, operating-system credential store, or
another operator-owned secret system; they are not config fields.

## Profile selection

`--profile <name>` selects an exact named profile. If it is omitted, the
configured `default_profile` is used where a command needs profile-backed
settings. Commands that do not need a profile continue to operate without one.

Setting an existing profile merges provided fields by default. Use `--replace`
only when replacing the entire profile is intentional. Unknown schema versions,
missing selected profiles, incomplete Multica authority records, credentialed
URLs, and unsafe paths fail closed.

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

Composition is read-only and invocation-scoped. Bundle profiles are added after
safe built-ins and the selected user config. A profile collision fails closed
unless the definitions are identical. A bundle default cannot replace a
different user-config default. There is no bundle install or apply command.

Bundles can contain only adapter executable expectations and an optional exact
Multica authority. They cannot add adapter arguments, callback commands,
installation roots, tokens, or arbitrary fields. An adapter executable in a
bundle is a profile/doctor expectation; it never rewrites the native argv after
`agentctl run --`.

agentctl does not clone, pull, install, or update that repository. Ordinary Git,
configuration management, or a host manager owns checkout and revision
selection. This keeps repository access, rollout, and rollback under the
operator's existing authority while agentctl reports content provenance.

Do not place API tokens, SSH keys, callback signing secrets, cookies, prompts,
results, or native session data in such a repository merely because it is
private.

## Precedence

The implemented precedence is deliberately small:

1. safe built-in behavior;
2. the selected user config file;
3. one explicit additive `--config-bundle`; and
4. explicit command-line flags and profile selection.

Environment variables select documented paths or context handles; they are not
a general policy scripting layer. Configuration is data, not executable code.
