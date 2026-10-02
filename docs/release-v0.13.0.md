# Agent Control v0.13.0

## Delegation repairs grounded in real usage

Prompt-file errors now recommend repairs accepted by the command that failed.
Structured requests report sanitized field paths, allowed keys, and placeholder
examples. Selection errors show bounded candidates from the configured catalog;
agentctl does not substitute a model or relax explicit constraints.

`config doctor` and `config bundle validate` now distinguish configuration syntax
from static launch-recipe compatibility. Static compatibility is not native
runtime or provider verification. Configured Devin model identifiers no longer
hit an outdated version whitelist, and Claude Code native `[1m]` model suffixes
are supported within that harness's recipe.

## Read-only usage export and explicit caller attribution

`recent` adds creation-time bounds, cursor pagination, caller filtering, and
full-filtered-set summaries. Exports identify their host-local journal, creation
cutoff, and retained-record coverage. State and acknowledgement filters remain
live per page; this is not a persisted snapshot. Preflight failures are explicitly
reported as not recorded. Exporting does not read results or acknowledge work.

Set `AGENTCTL_CALLER_HARNESS` at the invoking integration to declare a supported
caller harness. Attribution is immutable, caller-declared, and separate from the
execution adapter. Unattributed historical work remains unknown. Inherited
caller declarations are stripped from launched children and probes. An immutable
execution-owned snapshot preserves attribution across older supervisor rewrites;
there is no new journal bucket or migration.

## Authority-aware recovery guidance

`inbox` explains stale, unreachable, and conflicted work using recorded capability
and binding evidence. Suggested actions retain explicit journal/configuration
scope. Native owner-process limitations and verified Multica authority routes
remain distinct. Reading the inbox performs no refresh, probe, acknowledgement,
or reconciliation write.

## Upgrade and rollout

The portable skill distribution revision is `tree:v0.13.0`. Existing exact-release
installations may update the binary and managed skill under their configured
automatic-update policy on a due work-creating invocation. Read-only commands do
not trigger maintenance; `auto`, `notify`, and `off` policies remain respected.
Publication does not establish that every host has upgraded.

Archives are provided for macOS and Linux on amd64 and arm64. Verify downloaded
archives against `SHA256SUMS` and inspect the packaged installer dry run before
manual installation. Existing managed-content ownership safeguards remain in
place. Native CLIs and Multica retain execution authority and credentials.
