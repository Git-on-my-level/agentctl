# Managed skill packs

Agentctl can reconcile a reviewed, non-secret skill pack from the exact Git
revision already pinned by `agentctl config source`. Git and SSH continue to own
network authentication. Native harnesses continue to interpret and execute the
installed skills.

Managed skills are separate from `config-bundle.json`. The default repository
layout is:

```text
agentctl-config/
├── config-bundle.json
├── skill-pack.json
└── skills/
    └── example-skill/
        ├── SKILL.md
        └── scripts/
```

## Manifest contract

`skill-pack.json` has this v1 shape:

```json
{
  "schema_version": 1,
  "skills": [
    {
      "name": "example-skill",
      "path": "skills/example-skill",
      "targets": ["codex", "cursor", "hermes", "omp"]
    }
  ]
}
```

`name` is a lowercase hyphenated slug and becomes the native skill-directory
name. `path` is repository-relative, must stay outside `.git`, and must contain
a regular `SKILL.md`. `targets` is an exact allowlist containing one or more of
`claude`, `codex`, `cursor`, `hermes`, `omp`, or `multica`.

The normative machine-readable contract is
[`schemas/skill-pack.schema.json`](../schemas/skill-pack.schema.json). The
runtime additionally rejects symlinks, non-regular files, duplicate names,
reserved marker files, skill trees over 512 files or 16 MiB, manifests over
1 MiB, and paths that escape the pinned checkout.

Every command returns the same versioned report shape defined by
[`schemas/skill-pack-report.schema.json`](../schemas/skill-pack-report.schema.json).
For a read-only command, `changed` is the number of actions reconciliation would
apply and `applied` is zero. A successful reconcile returns the converged final
actions, `changed: 0`, and the number of completed writes in `applied`.

## Commands and side effects

```bash
agentctl skills plan
agentctl skills status
agentctl skills doctor
agentctl skills reconcile
```

`plan`, `status`, and `doctor` are equivalent read-only projections in v1.
They perform no fetch and no filesystem writes. `reconcile` performs no network
access; use `agentctl config source update` separately to advance the reviewed
Git revision.

All commands require a configured, clean, in-sync config-source checkout.
`--manifest <repository-relative-path>` selects a non-default manifest.
`--home <absolute-path>` and `--harness <comma-separated-names>` support an
explicit host or harness scope. Without `--harness`, only detected native
harnesses are eligible.

The canonical native roots are:

| Harness | Skill root |
| --- | --- |
| Codex and OMP | `~/.agents/skills` |
| Cursor | `~/.cursor/skills` |
| Hermes | `~/.hermes/skills` |
| Claude Code | `~/.claude/skills` |

Codex and OMP actions for the same skill deduplicate into one filesystem
operation because they share a canonical root. Multica is workspace/server
scoped and has no guessed local root. A `multica` target is reported as
`unsupported` until an adapter advertises a reviewed runtime-bundle installer.

## Ownership, provenance, and drift

Every installed skill contains an owner-only `.agentctl-skill.json` marker
binding its name, target harnesses, source remote, exact source commit, manifest
digest, and content-tree digest. Agentctl changes only directories with a valid
marker whose current content still matches that marker.

An unmanaged collision reports `conflict`. A local edit to a previously managed
skill reports `drifted`. Either condition causes `reconcile` to fail before its
first skill write. Agentctl never overwrites or adopts either state implicitly.

Each skill replacement is atomic and rollback-protected. The whole pack is
preflighted before writes begin, but v1 does not provide a cross-directory
filesystem transaction: if a later filesystem operation fails, earlier skills
may already be current. Repeating `reconcile` is idempotent and converges the
remaining actions. Concurrent reconciles fail with `conflict` through an
owner-only local lock.

Skills removed from the manifest are not deleted in v1. Removal will require a
separate explicit, plan-bound operation so manifest edits cannot silently erase
native skill directories.

## Exit behavior

Read-only commands return a successful document even when their report has
`healthy: false`; callers inspect action states and counters. `reconcile`
returns:

- exit 0 after every eligible declared skill is current;
- `unsupported_schema` / exit 2 for another manifest schema version;
- `not_found` / exit 3 for a missing manifest or skill source;
- `capability_unavailable` / exit 5 when no Git config source is initialized;
- `conflict` / exit 8 for config-source drift, unsupported Multica actions,
  unmanaged destinations, managed-content drift, or another active reconcile;
- `usage` / exit 2 for malformed manifests, paths, targets, or flags; and
- `internal` / exit 70 for unexpected local failures.

Skill repositories remain non-secret inputs. Do not store API tokens, SSH keys,
cookies, callback secrets, prompts, results, or native session data in a skill
pack. Agentctl does not execute installation hooks or skill scripts during
reconciliation.
