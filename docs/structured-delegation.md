# Structured delegation

Available in v0.5.0. The parent agent interprets natural language; agentctl
resolves structured constraints against reviewed configuration and constructs the
native invocation. No roles, LLM router, or fuzzy launch matching is required.
Check `agentctl help delegate` before using an older installed binary.

## Caller recipe

1. Extract only the user's explicit harness, model family/version, host, and
   settings. Leave unspecified fields absent. Keep task text in the prompt.
2. Write a request with a stable key for this logical task. Inspect `--plan` when
   useful; it is read-only, can probe the executable version, and reserves nothing.
3. Run with `--wait` to require a successful, nonempty stored final answer. Inspect
   `requested`, `resolved`, and `provenance`. `--content` returns exact answer text.
4. Retry the same key and inputs after an uncertain response. Existing work is
   recovered, never relaunched. Use a new key only for a deliberate new task.

```json
{
  "schema_version": 1,
  "request_key": "review-change-01",
  "selector": {"harness": "cursor", "family": "grok", "version": "4.6"}
}
```

```sh
agentctl delegate --request-file request.json --prompt-file task.md --plan
agentctl delegate --request-file request.json --prompt-file task.md --wait
agentctl delegate --request-file request.json --prompt-file task.md --wait --content
```

`--prompt-stdin` is the alternative prompt source. Prompt files must be regular
non-symlinks within `--cwd` (default: current directory) and at most 8 MiB.
Request files are regular non-symlinks of at most 64 KiB. Unknown, duplicate,
case-aliased keys, nulls, empty strings, control characters, invalid UTF-8,
oversized strings, and trailing JSON fail before probing or launch.
The request schema is [delegate-request](../schemas/delegate-request.schema.json).
Metadata fields are public operational metadata; never put secrets in selectors,
keys, or labels. Only the key digest is stored. Prompt bytes are not journaled.

## Natural language stays with the parent

| User wording | Selector |
| --- | --- |
| `grok` | `{"family":"grok"}` |
| `grok 4.6` | `{"family":"grok","version":"4.6"}` |
| `cursor grok` | `{"harness":"cursor","family":"grok"}` |
| `cursor grok 4.6` | `{"harness":"cursor","family":"grok","version":"4.6"}` |
| `use Grok version 4.6 through Cursor` | `{"harness":"cursor","family":"grok","version":"4.6"}` |

These are interpretation examples, not a permutation dictionary. A version is a
string, never a number or range. Preserve explicit speed/effort in `settings`;
do not fill omitted fields from memory. At least `family` or exact native `model`
is required. Host aliases match exactly; remote delegation currently fails with
a capability diagnostic and a pointer to explicit Multica dispatch.

## Reviewed configuration

Add metadata to the existing profile; there is no separate role registry:

```json
{
  "route": {"this_host": "workstation"},
  "agent_preferences": {
    "mode": "advisory",
    "preferred": [
      {"agent":"cursor","family":"grok","version":"4.6","model":"cursor-grok-4.6-high","speed":"regular","default":true},
      {"agent":"cursor","family":"composer","version":"2.5","model":"composer-2.5","speed":"regular","default":true}
    ]
  }
}
```

This is a profile fragment, not a complete config file. Omitted host or the reserved `local` selector uses this machine;
`route.this_host` optionally supplies its configured name. `adapters.<harness>.executable` selects an
optional exact executable. Existing `use_for` entries with `alias:` provide
explicit family aliases for compatibility. Versions are explicit metadata;
agentctl never infers them from model slugs. Old configurations remain valid.

`delegate` treats preferred entries as its allowlist even when `run` preferences
are advisory. An omitted family/version may remain absent if the matching entry
has no metadata. Explicit constraints must all match; a default only breaks a tie
among compatible entries. Multiple compatible tuples without one unique default
return bounded candidates and `ambiguous_reference`. Case/outer whitespace
normalization is deterministic (native model IDs remain case-sensitive); there is no substring or edit-distance match.
Catalog order never selects a winner. Alias matches cannot discard version,
harness, model, or settings constraints.

Workspace trust is a separate grant: `delegation.cursor_workspace_trust: true`
on the profile allows the recipe to include Cursor `--trust`. A model alias or
allowlist entry never grants it. Review that grant before enabling it. Native
permissions, sandbox policy, credentials, tools, and sessions remain native-owned.
No force/yolo or approval-bypass flags are added.

## Plans, replay, and results

New plans expose the exact native argv without prompt bytes, delivery mode, adapter
preflight, tuple, configuration SHA-256, and defaulted fields. Plans for an existing
key return the frozen admitted recipe with `source=frozen_admission`; they do not
repeat preflight or claim current eligibility. Plan and wait flags cannot be combined. The nested native
preflight describes prospective launch effects; the plan itself writes no state
and triggers no automatic maintenance. Health probes establish invocation support,
not provider authentication or observed model identity. Admission rechecks support.

Request keys are scoped to the selected profile in this host-local journal.
The semantic digest includes the normalized selector, prompt digest, canonical
working directory, requested authority, timeout, and sorted labels. Wait/content
and result assertions are collection options and may change on replay. Changed
semantic inputs with the same key return `conflict`. Configured defaults and
executable changes cannot redirect existing work: replay recovers its frozen
binding before resolving the current catalog. Valid current config is still
required. Concurrent same-key admissions launch at most once. Missing/expired
owner evidence is reported as unknown; it never authorizes another launch.
Deleting the journal also deletes replay protection. The immutable delegation
binding and private prompt-free native recipe are retained in the existing idempotency graph so older supervisors
rewriting execution projections cannot erase it; retention removes both copies.

Native execution is foreground-owned. Even without `--wait`, the initial call
owns the native run until exit; a concurrent replay can return a running receipt.
`--wait` additionally enforces successful completion, retrieves content, and marks
collection only after delivery. It fails on attention, failure, cancellation,
unknown state, unavailable content, or an empty answer. Optional assertions are
`--require-result-source <source>` and `--min-result-bytes <n>`. A returned answer
is not proof of task correctness or external acceptance. Stored results remain
bounded by the existing native result contract.

`--timeout <duration>` bounds a new execution; no timeout is implied. Existing
runs retain their original deadline. `--label <name>` may repeat up to 16 distinct
labels. `--authority` accepts `auto`, `native`, or `multica`; this release supports
native local launches only. `--background`, task contracts, and external
`--context-file` are not yet supported by this command. Use live help for the
existing expert `run`, `fanout`, or `dispatch` interfaces when those are needed;
do not automatically weaken a failed structured request into an expert launch.

## Native and Multica boundaries

Cursor and Codex have recipes for exact requested models and supported settings;
OMP has a structured recipe with explicit thinking but no service-speed guarantee.
An unsupported setting fails before launch. The resolved tuple describes what
agentctl requested. Native providers can still report opaque or substituted model
identity; this release does not claim model attestation.

Multica remains the authority for durable remote tasks. Its current adapter lacks
a verified final-answer route and settings contract, so `delegate` rejects remote
and explicit Multica requests before creating anything. Existing `dispatch`
retains live exact assignee/runtime binding, authority idempotency, and lifecycle
tracking; there is no local fallback. Uniform remote delegation requires those
missing capabilities first.

## Errors and discovery

All errors use the existing envelope and stable `diagnostic_code` where supplied
by delegate; resolver and recipe diagnostics distinguish constraint failures.
Known execution IDs are retained. Exit classes include usage (2), missing content
(3), ambiguity (4), unavailable capability (5), authorization (7), conflict or
invalid result state (8), attention (10), execution failure (11), cancellation
(12), and unknown outcome (13). See the [execution contract](execution-envelope.md) for the full contract.

The request and result schemas, `help delegate`, and embedded portable skill ship
together. `agentctl bootstrap update` reconciles the installed guidance through
existing managed ownership rules. The design is tracked in
[issue 51](https://github.com/Git-on-my-level/agentctl/issues/51).
