# Agent Control v0.12.0

## Native follow-up turns and steering

[PR #73](https://github.com/Git-on-my-level/agentctl/pull/73) adds `agentctl continue`
for a follow-up instruction to a completed delegated native session and
`agentctl steer` for redirecting supported running executions. Each continuation
is a separate execution with its own result and events. Native CLIs keep session,
model, permission, and execution authority. Expert `run` executions without a
frozen delegation recipe do not gain continuation support. Live delivery and
interrupt/resume have different capability and acknowledgement boundaries; see
[follow-up turns](https://github.com/Git-on-my-level/agentctl/blob/v0.12.0/docs/follow-up-turns.md) and [steering](https://github.com/Git-on-my-level/agentctl/blob/v0.12.0/docs/steering.md).

## Read-only identity and capability evidence

[PR #75](https://github.com/Git-on-my-level/agentctl/pull/75) adds
`agentctl identity --json` with the versioned `agentctl.identity.v1` report. It
separates provider evidence, a privacy-preserving native-conversation correlator,
per-turn execution identity, and installed harness inventory. Native session
correlation becomes available when the running native stream first emits it.
Unknown identity and capability remain explicit. Conflicting inherited native
or managed context fails closed; explicit `--execution` remains an exact journal
query. No raw native session ID, credential, prompt, transcript, or local path is
exposed. The command performs no network probes or filesystem writes.

The interface is optional. Generic agents and external coordination tools do not
need agentctl to register, and this release does not register agents or change
ANX cloud state. Consumers must keep their independently authenticated agent and
enrolled host namespace separate from provider/session evidence. See the
[identity contract](https://github.com/Git-on-my-level/agentctl/blob/v0.12.0/docs/identity.md) for confidence, hashing, and capability scopes.

## Upgrade and rollout

Existing exact-release installations default to automatic updates under their
configured policy. Publishing this stable release can therefore lead those
clients to update the binary and reconcile managed skills on their next due
work-creating CLI invocation. Read-only commands, including `identity`, do not
trigger automatic maintenance. `agentctl update status` reports the current
policy; existing `auto`, `notify`, and `off` choices remain respected.

This release aligns the portable distribution revision with `tree:v0.12.0`, so
release-specific skill drift checks compare the matching embedded assets.
There is no new journal migration, credential setup, or agent-registration step.
Installers preserve their existing ownership and managed-content safeguards.
Download the appropriate archive and `SHA256SUMS`, verify its checksum, and
review the packaged installer dry run before manual installation.

Supported release targets: macOS and Linux, each for amd64 and arm64. ANX OSS,
SaaS, and cloud deployment remain independent release steps; this Agent Control
release does not imply those changes are deployed.

## Publication and retries

A release created through GitHub's UI can be staged as a prerelease while the
existing tag workflow builds, scans, and uploads its artifacts. Keep it marked
as a prerelease until the published assets and checksums are verified, then
promote it to stable to begin the normal update rollout. The workflow preserves
an existing release's notes and draft/prerelease/publication flags.

Rerunning publication verifies matching existing asset bytes and uploads only
missing assets. Conflicting existing bytes fail visibly and are never silently
overwritten. Local checksums are rechecked after the workflow artifact download;
creating a release still requires an already-existing tag.
