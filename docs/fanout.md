# Foreground delegation batches

`agentctl fanout` is a convenience over explicit native `run` calls, not a new
harness, task authority, or durable scheduler. A batch can give the same task to
several harnesses or give each harness a different task. Authentication,
permissions, models, sessions, and tool interfaces remain native.

## Use distinct tasks without inventing a harness API

From the checkout you want reviewed:

```bash
agentctl fanout --plan --manifest /path/to/agentctl/examples/fanout/fanout.json
agentctl fanout --manifest /path/to/agentctl/examples/fanout/fanout.json
agentctl recent --label review-batch
agentctl inbox --label review-batch
```

The [example manifest](../examples/fanout/fanout.json) delegates correctness to
Codex and operability to Cursor, using separate prompt files. Install and
configure those native CLIs first and adjust their exact argv as appropriate
for your environment. Prompt instructions are not a security sandbox; enforce
permissions through the native harness. In particular, agentctl does not infer
permission flags or automatically grant workspace trust.

A child without `cwd` works in the invoking directory. An explicit relative
child `cwd` is resolved from the manifest directory. agentctl never creates,
locks, or merges worktrees for a batch. Use separate existing worktrees for
parallel writers; the included example asks for independent read-only reviews.

## Manifest v1

The normative shape is [fanout-manifest.schema.json](../schemas/fanout-manifest.schema.json).
Schema-valid shared-prompt v1 manifests continue to work. The decoder now rejects
ambiguous or schema-invalid JSON that the previous implementation could accept.
The additive fields are:

| Field | Contract |
| --- | --- |
| `prompt_file` | Optional shared prompt; required on every child without a shared prompt. |
| `children[].prompt_file` | Replaces the shared prompt for that child. No concatenation or implicit context injection. |
| `children[].name` | Optional unique batch-local correlation name matching the label grammar. Echoed in the batch result, not stored as a separate task identity. |
| `labels` | Shared exact metadata labels, persisted on every child execution. |
| `children[].labels` | Stable union with shared labels; duplicates across scopes collapse, duplicates within a scope are invalid. At most 16 combined labels. |

`prompt_delivery` remains explicit (`argv` or `stdin`), with child settings
overriding shared settings. The default is `argv`. The selected prompt bytes
are appended as one positional argument, or supplied on stdin, using the
existing `run` transport. Native argv is not translated into provider-neutral
model, permission, session, or tool options. Native flags belong in `argv`.

Every prompt path is resolved and checked below the manifest directory, even
when the child runs elsewhere. Prompt files must pass the existing regular-file
and no-symlink checks. A file is read once per distinct normalized path before
any task launches. All children using that path share an in-memory snapshot,
including when their delivery modes differ. Later file edits do not change the
prepared batch. Limits are 8 MiB per prompt and 64 MiB of unique prompt bytes.
Prompt text never appears in the batch response; only source, delivery, byte
count, and SHA-256 digest do. Native argv delivery has the native platform's
process-argument limits and visibility; use supported stdin delivery for large
or sensitive prompts.

A manifest is at most 1 MiB, has 1–64 children, and admits 1–16 concurrent child
callbacks. Concurrency is 2 when omitted or explicitly 0; a CLI `--concurrency`
override must be 1–16. JSON must be one UTF-8 document: trailing documents,
unknown or case-aliased keys, duplicate object keys, and nulls fail closed.
Empty native argv entries are preserved, but NUL bytes are rejected. Native
argv vectors have at most 256 entries. Names and labels match
`^[a-z][a-z0-9_.-]{0,63}$`. Typed IDs additionally require their real checksum;
JSON Schema alone cannot check it. Cross-child name and execution-ID uniqueness
and the combined-label limit are enforced by the decoder.

## Preflight before side effects

Every normal batch and every `--plan` validates **all** children before any
native task launches. This covers prompt loading, per-child controls, working
directories, adapter selection, executable availability, and invocation-specific
capability requirements. Preflight can execute the adapter's declared read-only
native discovery/version probes; it is not a promise to execute no commands.

Preflight checks a journal only if it already exists. An existing child execution
ID is a conflict, not implicit replay, attachment, or resumption. A failed
preflight returns all prepared child IDs and errors without creating any child
execution records. The ordinary CLI's existing automatic maintenance policy is
unchanged; batch preflight itself creates no journal or task state.

Preflight is **not an atomic batch reservation**. Another process may claim an
ID, a binary may disappear, or permissions may change after validation. Actual
`run` calls recheck their own preconditions. Launch-time failures can therefore
leave a partially executed batch, and there is no rollback of native effects.
Do not blindly retry a failed manifest: inspect its IDs and journaled states.

## Ordering, cancellation, and collection

Callbacks are admitted in manifest order, with the configured concurrency
bound. At concurrency 1, execution is strictly sequential. At higher
concurrency, actual OS process start and completion order are not guaranteed.
The response's child order always matches the manifest, not completion order.

Without `--fail-fast`, independent children continue after a failure. With it,
a failed run or a recorded non-completed outcome cancels admitted siblings and
prevents further queued admissions once cancellation is observed. Existing
native cancellation and reaping remain responsible for process cleanup. The
batch waits for every admitted callback to finish cleanup before returning.
External caller cancellation also stops admission and drains admitted work.
This is cooperative foreground ownership, not crash or host-restart durability.
No queue is persisted and no automatic retry is attempted.

Each child response has its ID, adapter, optional name/labels, prompt digest, and:

| Field | Meaning |
| --- | --- |
| `launch_attempted` | The child `run` callback was admitted; it does **not** prove an OS process started. |
| `recorded` | The execution was actually found in the journal. |
| `state` | The observed journal state, omitted when no record exists. |
| `error` | The run/preflight/skip diagnostic, when present; a native failed state can instead be explained by its stored result. |

A skipped child has `launch_attempted: false`, `recorded: false`, no fabricated
execution state, and a cancellation diagnostic. Its allocated ID is not a
journal record. A timed-out child can have both an error and its actual durable
`cancelled` state. An existing-ID preflight conflict may show `recorded: true`
with `launch_attempted: false`, describing an existing record, not a new launch.

`success` means every child finished in lifecycle state `completed`; it does not
assert that a human accepted the task or that an external objective was met.
Failure envelopes retain the entire batch under `error.details.fanout`.
Batch preflight failures use the existing usage error (exit 2), cancelled
preflight uses the cancellation error, and unsuccessful execution uses the
existing execution-failed error. The machine-readable child diagnostics retain
the more specific underlying cause. See [error semantics](agent-ergonomics.md).

Results stay independently retrievable:

```bash
agentctl status <execution-id>
agentctl result <execution-id> --content
```

Batch observation does not acknowledge collection, so existing
`recent --unreconciled`, `inbox`, subscriptions, and `result` semantics remain
usable. Shared labels provide rediscovery without introducing a group database.
Names are response-local; use a child label when a role must be rediscoverable
from the journal. Use caller-preallocated execution IDs for subscriptions that
must exist before launch. IDs in a generated plan are not reserved.

## Authority boundary

This convenience runs native argv through existing adapters. It does not add
remote routing, remote task assignment, a second issue lifecycle, automatic
result synthesis, or a replacement conversation store. Multica-backed work
continues to use explicit `dispatch`/promotion and Multica's own authority.
No Multica adapter or authority behavior is changed by this batch extension.
