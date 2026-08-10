# LLM-friendly identifiers

## Goals and limits

Every identifier emitted for ordinary agent use is a typed word ID. It
must be easy to copy, pronounce, validate, and distinguish from a display name.
Opaque UUIDs, database integers, native session IDs, and Multica short IDs remain
available to adapters as source bindings, but normal commands do not require or
lead with them.

A word ID is a locator, not a credential and not, by itself, a globally unique
database key. The globally unambiguous key is `(origin_host_id, word_id)` for
host-local objects or `(authority, source_fingerprint)` for imported authority
objects. Portable references therefore include the origin host.

## Canonical format

```text
<type>-<word>-<word>-<word>-<word>-<word>-<word>
```

Examples:

```text
exec-purple-monkey-dragon-river-candle-meadow
event-silver-otter-canyon-lantern-drift-velvet
sub-quiet-forest-copper-raven-signal-harbor
cursor-amber-willow-orbit-tiger-harbor-gentle
artifact-gentle-comet-maple-badger-valley-sparrow
```

Initial prefixes are a registry, not an open string:

| Prefix | Object |
| --- | --- |
| `exec` | Normalized execution |
| `event` | Normalized event |
| `sub` | Subscription |
| `cursor` | Local event-stream checkpoint |
| `delivery` | Callback delivery attempt or receipt |
| `artifact` | Artifact reference |
| `context` | Rendered context snapshot |
| `route` | Routing decision or explanation |
| `host` | Stable fleet host alias |
| `repo` | Registered knowledge-source repository |
| `knowledge` | Indexed knowledge record or deterministic loose-document unit |
| `source` | Redacted alias for an opaque native source |
| `project` | Imported project/workspace binding |
| `issue` | Imported durable issue binding |
| `run` | Imported durable run binding |

Display labels such as `m5-mbp`, `omi`, or `SCA-293` may be shown alongside a
word ID, but are never accepted as the sole target of a mutation.

## Version 1 encoding

The six words are six unsigned 11-bit indices into the version-1 2,048-word
list. Together they encode this 66-bit big-endian codeword:

```text
payload[60] || checksum[6]
```

For randomly allocated objects, `payload` is 60 bits from the operating
system's cryptographically secure random source. The checksum is the first six
bits of:

```text
SHA-256("agentctl-word-id-v1\0" || type_utf8 || 0x00 || payload_u64_be)
```

`payload_u64_be` is eight bytes, big-endian, with its high four bits zero. The
type prefix participates in the checksum, so changing `exec` to `event` is
detected. The codeword is split from most-significant to least-significant bit
into six word-list indices. Parsers reject noncanonical case, word count, type,
or checksum before consulting storage.

The 60-bit payload gives an approximate birthday-collision probability of
4.3e-7 at one million IDs (about 1 in 2.3 million), before uniqueness checks.
An arbitrary altered codeword has a 1-in-64 checksum pass rate. The checksum is
for transcription errors; authorization must never depend on it.

Each allocating store enforces uniqueness within `(object_type, origin_host)`
and retries with fresh randomness. Offline hosts can still generate the same
word ID. Import never merges on word ID alone: it compares origin and the full
source fingerprint, keeps both objects, and allocates a replacement alias for
the incoming binding if necessary.

## Deterministic derivation

Stable event identity and object allocation are deliberately separate:

- ordinary object word IDs are random and immutable after allocation;
- semantic event deduplication uses a full 256-bit `dedupe_key` defined in
  [Events, subscriptions, and callbacks](events-and-subscriptions.md);
- re-observing a semantic event returns the already journaled event ID;
- independent hosts may allocate different event IDs for the same semantic
  event and still converge by `dedupe_key` when records are imported.

This avoids presenting a truncated word ID as a collision-proof content hash.
Where a source binding must be recreated deterministically after local state
loss, its authoritative fingerprint is:

```text
sha256(canonical_json({authority, authority_scope, source_kind, source_id}))
```

The fingerprint is retained locally (and may be redacted in output). A word ID
is allocated for it on first import. An authority that needs the same alias on
multiple hosts must persist that alias as authority-owned metadata and expose
an exact lookup capability; otherwise each host uses its own alias and portable
URIs retain the origin host. `agentctl` never guesses or recomputes a shared
alias from a truncated hash.

## Frozen v1 word list

Version 1 uses the repository-owned English BIP-39 2,048-word list. Its exact
order is immutable and identified by a SHA-256 digest in fixtures and
manifests. BIP-39 was selected because it is stable, independently available,
and compact across model tokenizers.

It is not a custom speech-confusion or cultural-sensitivity list. Some words
are less pleasant or more confusable than an ideal agent-only list. The type
prefix, fixed six-word length, and type-bound checksum provide deterministic
validation, while a future encoding version may introduce a more strongly
screened list. Readers must retain the v1 list while v1 IDs are referenced.

Changing order or membership requires a new encoding version. Readers retain
old lists while referenced IDs remain within retention.

## Resolution and aliases

Accepted references, in decreasing safety, are:

1. a full typed word ID;
2. a portable typed URI;
3. an exact contextual reference such as `@current` once a future CLI version
   implements explicit context resolution;
4. a full untyped six-word payload, only when the command expects one type;
5. a unique leading-word prefix, only for read-only commands.

Mutation commands in v0.1 require a full typed ID or portable URI. Future
contextual references must resolve from an explicit invocation context. They reject display
labels, source IDs, untyped payloads, and prefixes. Noninteractive ambiguity
returns `ambiguous_reference` with typed candidates and exact retry argv.

Aliases are typed bindings with provenance, scope, and status:

```json
{
  "alias": "issue-quiet-forest-copper-raven-signal-harbor",
  "origin_host_id": "host-amber-willow-orbit-tiger-harbor-gentle",
  "authority": "multica",
  "source_fingerprint": "sha256:...",
  "status": "active",
  "superseded_by": null
}
```

One active alias maps to exactly one source fingerprint within its origin. A
source fingerprint may have historical superseded aliases. Alias conflicts
never use last-write-wins and never rewrite a source authority ID.

## Portable URIs

Portable references contain only typed word IDs:

```text
agentctl://host-amber-willow-orbit-tiger-harbor-gentle/exec-purple-monkey-dragon-river-candle-meadow
codex://host-amber-willow-orbit-tiger-harbor-gentle/source-velvet-comet-maple-badger-valley-sparrow
multica://host-amber-willow-orbit-tiger-harbor-gentle/project-silver-otter-canyon-lantern-drift-velvet/issue-quiet-forest-copper-raven-signal-harbor/run-purple-monkey-dragon-river-candle-meadow
```

The scheme determines the adapter; each path segment has a fixed expected
type. URIs never contain credentials, IP addresses, usernames, filesystem
paths, or raw authority IDs. Resolution may require the named host or authority
to be reachable; an unavailable resolver returns `dependency_unavailable`, not
`not_found`.

## Implemented fixtures

The repository publishes the ordered word-list digest and Go fixtures for
zero, maximum, and random payloads; every type prefix; wrong-type checksums;
invalid casing; URI parsing; and deterministic round trips. An independent
fixture reader and broader spoken-confusion corpus remain a post-v0.1 release
hardening item.
