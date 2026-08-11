# Shared context and knowledge

## Goal

Agents on different machines and in different harnesses should receive the same
relevant operational context without copying Hermes profiles or depending on a
model to browse a large memory store correctly.

## Authority

Independent private Git repositories are the authoring authorities. A source
registry pins and classifies them, and a publishing pipeline validates and
compiles a versioned, checksummed bundle for clients. The bundle is data, not a
live shared session database.

Knowledge does not need to live in the `agentctl` repository or be migrated into
one universal format. Existing repositories such as the Omi knowledge store can
remain independently owned and unchanged. Separate repositories or bundles
enforce sensitivity boundaries. Frontmatter labels are useful for selection but
are not a substitute for repository/ACL separation.

The intended storage split is:

| Layer | Contents |
| --- | --- |
| `agentctl` code repository | Schemas, compiler/client behavior, fixtures, and portable skill |
| Small fleet registry repository | Reviewed source registrations, overlays, routing policy, and bundle policy |
| Independent knowledge repositories | Omi knowledge, personal/ops knowledge, project corpora, and future domain stores |
| Published bundle | Immutable derived index/assets for fast verified client consumption |

The registry repository is small control-plane configuration; it is not a new
knowledge monorepo. A knowledge repository remains independently cloneable and
useful without `agentctl`.

## Git hosting and source registry

The read path uses ordinary Git semantics and supports GitHub, Forgejo, and
generic Git servers over reviewed SSH or HTTPS remotes. Host-provider metadata
is optional and enables links or contribution workflows; it is never required
to fetch, verify, compile, or read knowledge. Authentication remains in native
Git/SSH credential storage and is not copied into `agentctl` state.

Each source is registered explicitly:

```yaml
schema_version: 1
id: repo-amber-willow-orbit-tiger-harbor-gentle
slug: omi-knowledge
mode: loose
remote:
  provider: forgejo
  url: ssh://git@git-01.tail.example/omi/knowledge.git
  credential_mode: native_git
ref: refs/heads/main
subpath: .
sensitivity: project-confidential
ingest:
  include: ["**/*.md", "**/*.yaml", "**/*.yml", "**/*.json", "**/*.txt"]
  exclude: ["**/.git/**", "**/generated/**", "**/raw/**"]
  max_file_bytes: 1048576
```

The registry entry is configuration, not a vendored checkout or Git submodule.
The publisher records the resolved commit and tree/content digests in the bundle
lock. Moving a branch does not change an already published bundle. Source sync
is an explicit command or publisher job; read-only context commands never fetch
or update Git state.

The draft registry contract is
[`schemas/knowledge-source.schema.json`](../schemas/knowledge-source.schema.json).

## Repository modes

### Structured

A structured repository contains validated records with explicit scope,
authority, sensitivity, expiry, and supersession metadata. Structured records
may participate in deterministic policy selection and required launch context.

### Loose

A loose repository is an allowlisted document corpus. It does not need
frontmatter, a directory migration, or agentctl-specific files. The publisher:

1. walks only configured subpaths and file globs;
2. rejects symlink/path escape, oversized files, unsupported encodings, and
   secret-policy violations;
3. chunks text deterministically by document headings and bounded byte ranges;
4. assigns deterministic record IDs while retaining repository, commit, path,
   line range, and content digest provenance;
5. builds a deterministic lexical index without invoking a model.

Loose records are advisory knowledge. They may be discovered by repository,
path, explicit tags from the source registry, and lexical query, but they cannot
silently become mandatory safety policy, routing policy, or mutation authority.
Promoting a loose observation into policy requires an explicit structured
record and normal Git review.

### Hybrid

A hybrid source combines a loose corpus with a structured overlay.
The overlay may live in the source repository or in a separately reviewed
registry repository, so an existing corpus can gain scopes, aliases,
supersession, and priority without being rewritten. Overlay entries bind to
commit-relative paths or content digests and cannot alter source content.

This is the recommended onboarding path for the current Omi knowledge store:
register it as `loose` immediately, then add a small external overlay only for
documents that should be injected deterministically or treated as reviewed
operational guidance.

The current Omi checkout is also a useful compatibility fixture because it is a
mixed Forgejo corpus: Markdown project and incident documents coexist with JSON
evidence, scripts, templates, private subtrees, and log artifacts. Its initial
registration should be deny-by-default—narrow content globs, explicit exclusion
of private/raw/log/script paths, repository-level confidentiality, and later
path-level overlays where a smaller sensitivity boundary is proven. Successful
indexing must not imply that every tracked file is safe to distribute or inject.

## Record metadata

Knowledge and policy records include validated metadata:

```yaml
schema_version: 1
id: knowledge-silver-otter-canyon-lantern-drift-velvet
slug: multica-terminal-delivery
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

- `manifest.json` with every source repository ID, provider, resolved commit,
  creation time, compatibility, and per-asset hashes;
- `sources.lock.json` pinning source revisions, ingestion rules, overlay
  revisions, and source-tree/content digests;
- structured registries for fleet roles, resources, routes, and policies;
- a deterministic token-to-record lexical index spanning structured and loose
  knowledge;
- selected Markdown/runbook assets;
- allowlisted portable skill packages;
- schema files and cross-reference graph.

The local cache is installed atomically after full hash validation. Failed
updates preserve the previous valid revision.

The manifest declares a bundle schema version, minimum reader semantics,
ordered word-list digest, and the exact canonicalization used for asset hashes.
Unknown required feature names make the bundle incompatible; clients keep the
last compatible verified revision rather than partially loading a new one.

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
  "id": "context-gentle-comet-maple-badger-valley-sparrow",
  "bundle_revision": "git:...",
  "matches": [
    {
      "record_id": "knowledge-silver-otter-canyon-lantern-drift-velvet",
      "source_repo_id": "repo-amber-willow-orbit-tiger-harbor-gentle",
      "reason": ["repository", "task_kind"],
      "path": "runbooks/multica-terminal-delivery.md",
      "source_commit": "git:...",
      "content_digest": "sha256:..."
    }
  ]
}
```

## Launch injection

Adapters provide the rendered context file and execution handle only through a
negotiated backend-specific mechanism. Supported mechanism classes are an
inherited environment path, a native argv option, a native instruction-file
option, or an authority-owned artifact/reference. The adapter manifest states
whether delivery to the worker is guaranteed or merely exposed to the process.

If the task contract requires context and no reviewed mechanism is supported,
dispatch fails before launch. It never pastes context into a prompt, edits a
Hermes profile, or assumes a Multica worker shares the coordinator filesystem.
The rendered document contains no credentials, uses portable references rather
than local paths where crossing runtimes, and is bounded by declared byte and
record limits.

One portable `agentctl` skill may be distributed to Hermes, Codex, Claude,
Cursor, OMP, and Multica runtimes. It explains deeper discovery and operational
commands, but launch correctness does not depend on the skill being installed,
on a model choosing to read it, or on a harness having a global memory system.

## Harness distribution

Distribution is allowlisted per harness:

- link or install only named portable skills;
- preserve unmanaged local skills and agent definitions;
- never sync harness auth, sessions, memories, settings, plugins, or caches;
- validate frontmatter and source revision;
- report managed drift separately from unmanaged state.

Multica skill copies are generated distribution artifacts, not the authoring
source of truth.

`agentctl bootstrap update` is the normal release reconciliation path. It
detects installed supported harnesses, deduplicates canonical roots, and
installs or upgrades only the embedded manifest-bound skill. It may update a
supervisor that agentctl already manages, but it does not create a new service,
delete legacy copies, or modify unmanaged skills. Use `bootstrap status` and
`doctor` for read-only inventory, and `agentctl help <topic>` for the current
command contract; the skill remains a concise routing and guardrail layer
rather than a copy of every flag.

## Writes

Knowledge writes are explicit Git operations against the owning source through
its normal review policy. `agentctl knowledge propose` may validate and stage a
record, commit to a caller-selected branch, or use an optional GitHub/Forgejo
provider adapter to open a pull request. Without a provider adapter it returns
the exact native Git next actions. It never silently turns a model observation
into shared truth or writes to a loose source merely because that source was
read during a task.

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
