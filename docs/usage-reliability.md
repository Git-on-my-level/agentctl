# Usage reliability and recovery

Supervisor recovery uses a journal facade that opens bbolt only for each store
operation. Adapter/network calls hold no journal handle. CAS revisions and event
idempotency remain store-authoritative. Supervisor reprobes have a ten-second bound; explicit awaits retain their
caller-selected deadline. Repeated
failures back off per execution/revision from 10 to 160 seconds. A revised
execution is eligible immediately. Deferred failures still count as degraded
health. Backoff is in-memory scheduling, not an execution authority.

`status` may contain `last_operation_failure` with `stage`, allowlisted
`category`, `upstream_exit_code`, `retryable`, `remote_creation_uncertain`, and
`recorded_at`. It is historical failure metadata, not a terminal outcome. No raw
upstream stderr, prompt or credential is stored. Failed dispatches keep their
reserved key and bindings. The caller fixes the category then replays exactly
the same inputs and key; a timeout or malformed response does not prove that the
remote issue was not created. Error details report whether recording succeeded.

`update status` adds `observed_binary` (version, digest, expected portable-skill
digest, and bookkeeping comparison) separately from installer records.
`binary.last_error_stage` and `last_error_exit_code` identify a failed packaged
installation without retaining installer output. The installer emits only a
fixed stage marker on failure. `supervisor status` reports the running service's
startup version and executable digest, which can differ from a replaced binary.
Inspect `bootstrap status` for installed skill compatibility. These commands are
read-only; neither a mismatch nor a failed check implicitly updates/restarts.

A pending `result` supplies a mutating `await` next action for ordinary running
work, read-only event inspection for attention, and read-only status inspection
for a failed starting dispatch. An uncertain dispatch must first be resolved
using its original key. Unknown adapter errors enumerate
known names. Unknown `ps`/`list` and `agents` commands direct help to `recent` and
`delegate`. Prompt-root errors include a structured `repair`: remove the file
source, select stdin, keep native delivery and other flags, and redirect the
separately named input file. Repair metadata never includes prompt bytes or a
shell-reparsed command. Ordinary exit codes and JSON envelope version remain
unchanged. Structured errors take priority over the first useful native stderr
line, so a trailing usage hint no longer destroys the actual parser error.

Delegate plans add `lifecycle`: foreground ownership, no restart durability,
no background flag, explicit result-collection requirements, and conditional
cross-process cancellation. `--wait --content` requires completed work with a
stored answer. Multica dispatch keeps its lifecycle-only contract. Callback
receipts and native completion do not imply external acceptance; explicit
subscriptions remain at-least-once with consumer event-key deduplication.

The bootstrap updater recognizes the exact pre-v0.6 instruction body (including
its valid revision-bound form) as a managed historical artifact. It replaces
that body with the current instruction while preserving surrounding text.
Unknown/customized bodies still conflict. This repairs installations whose
previously shipped instruction pointer blocked the v0.6 upgrade preflight.
