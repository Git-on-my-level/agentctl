# Managed Skill Hub packs

Agentctl reconciles reviewed, non-secret skills from an independently pinned
Git Skill Hub. The config repository selects the source, manifest, and update
policy; it does not duplicate skill bodies. Git and SSH own authentication, and
native harnesses continue to interpret and execute installed skills.

## Configuration

Add a top-level selection to `config-bundle.json` and every full host bundle:

```json
{
  "skills": {
    "source": {
      "remote": "https://git.example.test/owner/skill-hub.git",
      "ref": "main",
      "manifest_path": "manifests/agentctl/fleet-core.json"
    },
    "update_policy": "auto-clean"
  }
}
```

`update_policy` is `manual` or `auto-clean`. Auto-clean uses the same short-lived
daily worker as binary update discovery but has independent policy and state.
It fast-forwards the pinned Skill Hub and applies only unambiguous installs,
upgrades, and provenance updates. Binary policy `notify` or `off` does not turn
off Skill Hub auto-clean.

The managed checkout defaults to
`~/.local/share/agentctl/skill-source`; its owner-only state is
`~/.local/state/agentctl/skill-source.json`. It is a derived cache, never an
authoring checkout.

## Manifest

The selected Skill Hub manifest uses schema v1:

```json
{
  "schema_version": 1,
  "skills": [
    {
      "name": "example-skill",
      "path": "software-development/example-skill",
      "targets": ["claude", "codex", "cursor", "hermes", "omp"]
    }
  ]
}
```

`agentctl-portable` is reserved and rejected in every pack because bootstrap
owns and updates its embedded copies. Multica remains workspace/server scoped
and is unsupported until an adapter advertises a reviewed bundle installer.

## Commands and side effects

```bash
agentctl skills update --plan  # no fetch or write
agentctl skills update         # fast-forward fetch + auto-clean reconcile
agentctl skills plan           # pinned local projection, read-only
agentctl skills status         # pinned local projection, read-only
agentctl skills reconcile      # strict local reconcile, no network
```

All Git advances are fast-forward only. A dirty checkout, changed remote,
invalid fetched manifest, unsafe path, symlink, oversized tree, or unsupported
file fails closed. Skill replacement is atomic per destination and records the
source remote, exact commit, manifest digest, and content digest in
`.agentctl-skill.json`.

## Clean upgrades and drift

An installed tree that still matches its marker is safe to replace when the Hub
digest advances. Content that differs from its marker is `drifted` and is never
overwritten automatically. An unmarked collision is `conflict` and is never
adopted implicitly. Auto-clean may update unrelated clean skills while reporting
preserved drift.

Use the plan-first resolution workflow:

```bash
agentctl skills diff <name> [--harness codex]
agentctl skills restore <name> [--harness codex]
agentctl skills restore <name> --apply [--harness codex]
agentctl skills propose <name> [--harness codex]
agentctl skills propose <name> --apply [--harness codex]
```

`diff` reports changed files and both digests. Restore first writes a durable
backup under `~/.local/share/agentctl/skill-backups/`. Propose creates a local
Skill Hub branch and linked worktree under `skill-proposals/`, copies the local
delta without the managed marker, and validates the complete manifest. It never
commits, pushes, opens a PR, or publishes.

Use `--worktree-root <absolute-path>` on `skills propose`, or set
`AGENTCTL_SKILL_PROPOSAL_ROOT`, when local Git policy requires linked worktrees
to live on a particular volume. The override changes only where the review
worktree is created; the configured Skill Hub checkout remains authoritative.

Skills removed from a manifest are not deleted. Removal remains a separate
future plan-bound operation.

## Native roots

| Harness | Root |
| --- | --- |
| Codex and OMP | `~/.agents/skills` |
| Cursor | `~/.cursor/skills` |
| Hermes | `~/.hermes/skills` |
| Claude Code | `~/.claude/skills` |

Codex and OMP deduplicate because they share a canonical root. Skill packs stay
non-secret and cannot contain installation hooks, credentials, prompts, results,
sessions, or symlinks.
