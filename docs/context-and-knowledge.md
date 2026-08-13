# Shared context and knowledge

## Goal

The optional knowledge subsystem gives different machines and agent harnesses
the same relevant, versioned operational context without copying harness
profiles, credentials, session databases, or one operator's private topology.
Standalone execution does not require a knowledge bundle.

## Authority and repository boundary

Independent public or private Git repositories remain the authoring
authorities. A source registration pins and classifies each repository, and an
explicit publishing workflow validates and compiles a versioned, checksummed
bundle. The bundle is immutable derived data, not a live session database.

Knowledge does not need to live in the agentctl repository or move into one
universal format. Separate repositories and access-control boundaries enforce
sensitivity; frontmatter labels help selection but do not replace repository
permissions.

| Layer | Contents |
| --- | --- |
| agentctl repository | Schemas, compiler/client behavior, fixtures, and portable skill |
| Optional configuration repository | Reviewed source registrations, overlays, routing policy, and bundle policy |
| Independent knowledge repositories | Project, operational, personal, or other domain corpora |
| Published bundle | Immutable derived index and assets for verified local use |

The optional configuration repository is a small control-plane input, not a
knowledge monorepo. A source repository remains independently cloneable and
useful without agentctl.

## Git hosting and source registration

The read path uses ordinary Git semantics and supports GitHub, Forgejo, and
generic Git servers over reviewed SSH or HTTPS remotes. Provider metadata is
optional. Authentication remains in native Git or SSH credential storage and
is never copied into agentctl state.

Each source is registered explicitly:

```yaml
schema_version: 1
id: repo-amber-willow-orbit-tiger-harbor-gentle
slug: operational-guides
mode: loose
remote:
  provider: generic
  url: ssh://git@code.example.com/platform/operational-guides.git
  credential_mode: native_git
ref: refs/heads/main
subpath: docs
sensitivity: project-confidential
ingest:
  include: ["**/*.md", "**/*.yaml", "**/*.yml", "**/*.json", "**/*.txt"]
  exclude: ["**/.git/**", "**/generated/**", "**/raw/**"]
  max_file_bytes: 1048576
```

The registration is configuration, not a vendored checkout or submodule. The
publisher records the resolved commit and tree/content digests in the bundle
lock. Moving a branch does not alter an already published bundle. Source sync
is explicit; read-only context commands never fetch or update Git state.

The machine contract is
[`schemas/knowledge-source.schema.json`](../schemas/knowledge-source.schema.json).

## Repository modes

### Structured

A structured repository contains validated records with explicit scope,
authority, sensitivity, expiry, and supersession metadata. Structured records
may participate in deterministic policy selection and required launch context.

### Loose

A loose repository is an allowlisted document corpus. It does not require
frontmatter or an agentctl-specific directory migration. The compiler:

1. walks only configured subpaths and file globs;
2. rejects symlink/path escape, oversized files, unsupported encodings, and
   secret-policy violations;
3. chunks text deterministically by document headings and bounded byte ranges;
4. assigns deterministic record IDs while retaining repository, commit, path,
   line range, and content-digest provenance; and
5. builds a deterministic lexical index without invoking a model.

Loose records are advisory. They may be selected by repository, path, explicit
tags, and lexical query, but cannot silently become required policy, routing
policy, or mutation authority.

### Hybrid

A hybrid source combines a loose corpus with a structured overlay. The overlay
may live in the source repository or in a separately reviewed configuration
repository. It can add scopes, aliases, supersession, and priority without
rewriting the underlying source and binds to commit-relative paths or content
digests.

Hybrid mode is useful for existing mixed repositories. Start with narrow
content globs, explicitly exclude raw data, logs, generated output, scripts, and
private subtrees, and add structured overlays only for documents that have been
reviewed for deterministic injection. Successful indexing does not prove every
tracked file is safe to distribute.

## Record metadata

Structured records carry validated metadata:

```yaml
schema_version: 1
id: knowledge-silver-otter-canyon-lantern-drift-velvet
slug: terminal-delivery
title: Terminal delivery contract
scope:
  projects: [example-platform]
  repositories: [example/platform-service]
  task_kinds: [coordination, reliability]
  host_roles: [coordinator]
sensitivity: project-confidential
authority: project-policy
owner: platform-team
reviewed_at: 2026-08-10
expires_at: null
supersedes: []
```

Validation rejects duplicate IDs, invalid authorities, broken references,
expired hard requirements, and unsupported sensitivity transitions. Loose
documents receive generated provenance metadata but do not acquire policy
authority.

## Compiled bundle

Compilation resolves exact source commits, validates content and overlays,
normalizes text deterministically, builds a lexical index and cross-reference
graph, and writes a content-addressed bundle. The manifest declares its schema
version, minimum reader semantics, word-list digest, asset hashes, source
commits, and selection limits.

The local cache is installed atomically only after full hash validation. Failed
updates preserve the previous valid revision. Unknown required features make a
bundle incompatible rather than partially usable.

## Context selection

`agentctl context` deterministically matches records using observable inputs:

- current repository root and Git remotes;
- explicit project, issue, or run references when available;
- explicitly configured host or functional roles;
- task kind and side-effect boundary;
- requested resource or capability needs; and
- record priority, review state, and expiry.

Selection does not depend on semantic model inference. Optional semantic search
may supplement discovery but is never the mandatory correctness path. Results
explain every match and retain exact source provenance.

## Launch injection

Adapters can expose a rendered context file and execution handle only through a
negotiated backend-specific mechanism: environment path, native argument,
native instruction file, or authority-owned artifact reference. The adapter
manifest states whether delivery to the worker is guaranteed or merely exposed
to the process.

If a task requires context and no reviewed mechanism is supported, dispatch
fails before launch. agentctl does not paste the context into a prompt, edit a
harness profile, or assume a remote worker shares the coordinator filesystem.
Rendered documents contain no credentials and are bounded by declared byte and
record limits.

## Portable skill distribution

The embedded portable skill can be reconciled into detected Hermes, Codex,
Claude, Cursor, and OMP harness roots. A separately packaged Multica runtime
bundle is optional. The skill teaches progressive `agentctl help <topic>`
discovery, but launch correctness never depends on a model reading the skill or
on a harness having global memory.

`bootstrap update` owns only manifest-bound agentctl assets. It does not copy
knowledge repositories, credentials, profiles, settings, prompts, sessions,
worktrees, or caches.

## Writes and offline behavior

Knowledge-source sync, compilation, installation, and proposal are explicit
mutations. Read-only validation, verification, selection, and rendering do not
fetch. Proposed changes return to the owning Git source through its normal
review policy; agentctl never turns one model observation into shared truth.

Dynamic execution, network reachability, machine health, and credentials do not
belong in Git knowledge. When a publisher is unavailable, clients use the last
verified bundle and surface its age, revision, and failed-refresh state.
