# LLM-friendly identifiers

## Goals

User-facing identifiers must be:

- easy for models and humans to read, copy, pronounce, and remember;
- type-scoped so a reference reveals what it names;
- collision-resistant across a multi-machine fleet;
- typo-detecting;
- stable for the object's lifetime;
- exact by default, without dangerous fuzzy mutation.

UUIDs and database integers may exist internally at a source authority, but
normal `agentctl` commands neither require nor prominently emit them.

## Canonical format

```text
<type>-<word>-<word>-<word>-<word>-<word>
```

Examples:

```text
exec-purple-monkey-dragon-river-candle
event-silver-otter-canyon-lantern-drift
sub-quiet-forest-copper-raven-signal
cursor-amber-willow-orbit-tiger-harbor
artifact-gentle-comet-maple-badger-valley
```

Initial type prefixes:

| Prefix | Object |
| --- | --- |
| `exec` | Normalized execution |
| `event` | Normalized event |
| `sub` | Subscription |
| `cursor` | Event-stream cursor |
| `delivery` | Callback delivery attempt/receipt |
| `artifact` | Artifact reference |
| `context` | Rendered context snapshot |
| `route` | Routing decision/explanation |

Prefixes are singular, lowercase, ASCII, and versioned as part of the CLI
contract.

## Encoding

The five words encode 50 random bits plus a 5-bit checksum using a curated
2,048-word list. Generation uses a cryptographically secure random source.

Properties:

- approximately 1.12 quadrillion possible random payloads;
- approximately 0.044% birthday-collision probability at one million generated
  IDs before database collision retries;
- one-in-32 chance that an arbitrary mistyped five-word payload passes checksum;
- authoritative stores still enforce uniqueness and regenerate on conflict.

This is an ergonomic identifier, not an authentication token. It must never be
treated as secret or as proof of authorization.

The exact bit packing and checksum algorithm will be frozen before the first
implementation release and covered by cross-language fixtures.

## Word-list requirements

The list is repository-owned and versioned. Words must be:

- common, concrete, and 3–10 ASCII letters;
- unambiguous in spelling and pronunciation;
- free of hyphens, spaces, diacritics, and inflection variants;
- screened for offensive, sensitive, or culturally risky combinations;
- distinct under case folding;
- not common CLI flags, reserved contextual references, or type prefixes;
- preferably single tokens in major target model tokenizers.

Avoid homophones and near-homographs such as `pair`/`pear`, `desert`/`dessert`,
or `angle`/`angel`.

## Resolution rules

Accepted references:

1. Full typed word ID — always accepted.
2. Full untyped five-word payload — accepted when exactly one type matches in
   the requested command's domain.
3. Reserved contextual reference — such as `@last`, `@current`, `@parent`, or
   `@mine`.
4. Word-prefix reference — read-only commands only, and only when unique.

Mutation commands reject fuzzy matching. A contextual reference is safe for a
mutation only when it resolves from explicit local invocation context; the CLI
must echo the resolved full ID before performing the operation.

Examples:

```bash
agentctl status purple-monkey-dragon-river-candle
agentctl await @last
agentctl cancel exec-purple-monkey-dragon-river-candle
```

Ambiguity is an error, never a prompt in noninteractive mode.

## URIs and cross-host references

Portable references use typed URIs while retaining the word ID:

```text
agentctl://m5-mbp/exec-purple-monkey-dragon-river-candle
codex://m5-mbp/thread-amber-willow-orbit-tiger-harbor
multica://scaling-forever/SCA-293/run-silver-otter-canyon-lantern-drift
```

Host aliases come from the shared fleet/resource contract. The URI does not
embed IP addresses, usernames, tokens, or filesystem paths.

## Source IDs

Opaque backend identifiers are retained in the execution envelope for exact
API calls and reconciliation. They appear under `source_ref` in JSON and may be
redacted according to sensitivity. Human output leads with the word ID.

An importer that encounters a word-ID collision keeps both source objects and
assigns one a new word ID, recording the old conflicting alias in an audit
event. Source authority IDs are never rewritten.

