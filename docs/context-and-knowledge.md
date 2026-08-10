# Shared context and knowledge

## Goal

Agents on different machines and in different harnesses should receive the same
relevant operational context without copying Hermes profiles or depending on a
model to browse a large memory store correctly.

## Authority

Private Git is the authoring authority. A publishing pipeline validates and
compiles a versioned, checksummed bundle for clients. The bundle is data, not a
live shared session database.

Separate repositories or bundles enforce sensitivity boundaries. Frontmatter
labels are useful for selection but are not a substitute for repository/ACL
separation.

## Record metadata

Knowledge and policy records include validated metadata:

```yaml
schema_version: 1
id: multica-terminal-delivery
title: Multica terminal delivery contract
scope:
  projects: [multica]
  repositories: [Git-on-my-level/multica]
  task_kinds: [coordination, reliability]
  host_roles: [authenticated_coordinator]
sensitivity: operator-private
authority: multica
owner: agent-platform
reviewed_at: 2026-08-10
expires_at: null
supersedes: []
```

CI rejects duplicate IDs, invalid authorities, broken references, expired hard
requirements, and unresolved supersession chains.

## Compiled bundle

The publisher produces:

- `manifest.json` with source revision, creation time, compatibility, and
  per-asset hashes;
- structured registries for fleet roles, resources, routes, and policies;
- a deterministic SQLite FTS index for exact lexical discovery;
- selected Markdown/runbook assets;
- allowlisted portable skill packages;
- schema files and cross-reference graph.

The local cache is installed atomically after full hash validation. Failed
updates preserve the previous valid revision.

## Context selection

`agentctl context` deterministically matches records using observable inputs:

- current repository root and Git remotes;
- explicit Multica project/issue/run;
- current host and functional roles;
- requested task kind and side-effect boundary;
- explicit resource/capability needs;
- record priority, review state, and expiry.

It does not perform semantic model inference. Optional semantic search may be an
additional discovery tool, never the mandatory correctness path.

The output explains why each record matched and identifies its revision:

```json
{
  "id": "context-gentle-comet-maple-badger-valley",
  "bundle_revision": "git:...",
  "matches": [
    {
      "record_id": "multica-terminal-delivery",
      "reason": ["repository", "task_kind"],
      "path": "runbooks/multica-terminal-delivery.md"
    }
  ]
}
```

## Launch injection

Adapters automatically provide the rendered context file and execution handle
to the native harness through a reviewed backend-specific mechanism. The
context contains no credentials and is bounded in size.

One portable `agentctl` skill is distributed to Hermes, Codex, Claude, Cursor,
OMP, and Multica runtimes. It explains deeper discovery and operational commands,
but launch correctness does not depend on the model choosing to read it.

## Harness distribution

Distribution is allowlisted per harness:

- link or install only named portable skills;
- preserve unmanaged local skills and agent definitions;
- never sync harness auth, sessions, memories, settings, plugins, or caches;
- validate frontmatter and source revision;
- report managed drift separately from unmanaged state.

Multica skill copies are generated distribution artifacts, not the authoring
source of truth.

## Writes

Knowledge writes are explicit Git operations through normal review policy.
`agentctl knowledge propose` may validate and stage a record or open a branch/PR,
but it never silently turns a model observation into shared truth.

Dynamic state does not belong in Git:

- task/run/ownership state stays in Multica;
- reachability stays in Tailscale/`tailnetctl` observations;
- machine health stays in host-local tools;
- execution state stays native plus the bounded local `agentctl` journal;
- credentials and raw sessions stay local.

## Offline behavior

When the publisher is unavailable, clients use the last verified bundle and
surface age, revision, and failed-refresh state. Service reachability and bundle
freshness are separate health dimensions; a healthy service does not make stale
policy current.

