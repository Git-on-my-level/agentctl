# Supervisor and upgrade ownership

A running supervisor owns `supervisor.lock` in its state directory using a
nonblocking OS lock. The owner-only regular lock file is retained after exit;
its presence does not mean a process is running. Never delete it to bypass a
live owner. Contention returns the existing `already running` supervisor error
before recovery or socket mutation. Different sockets in the same state directory
still contend. Daemonless `RunOnce` keeps its existing bounded operation contract.

The lock lasts through startup recovery and final worker shutdown. A live socket
from an older release is refused even before RPC is available. Only a proven
stale socket is removed. Closing a listener removes its own socket inode, never
a replacement. Supervisor status includes `pid`, startup `version`, and
`executable_sha256`; CLI status requests time out after five seconds.

## Managed macOS upgrades

The launchd installer waits up to 30 seconds for the old label **and process** to
exit. It then installs the reviewed plist and verifies the replacement's running
RPC status, state directory, binary digest, and launchd PID (PID binding is
available on current releases). Startup verification is bounded to 120 seconds;
health may remain degraded when an adapter is unavailable. Degraded health is
separate from a failed replacement identity check.

The binary installer retains the previous binary and install manifest until
bootstrap and required launchd reconciliation succeed. On failure it restores
those files and attempts service recovery. Its exit remains nonzero. Fixed
`AGENTCTL_INSTALL_ROLLBACK` values, also exposed as `last_error_rollback` by update
status, distinguish `restored`, `binary_restored_bootstrap_retained`, and
`incomplete`. Bootstrap manages separate transactions: already updated skills
and pointers may remain, and are never described as rolled back. An incomplete
rollback prints a local recovery-backup path; updater state never stores raw
installer output. Abrupt power loss or SIGKILL may require manual recovery.

This automatic service upgrade contract applies to existing managed launchd
services. It does not install a new service, change native/Multica task authority,
or imply automatic systemd reconciliation.

## Explicit launcher registration

For an existing user-owned wrapper, first inspect a plan:

```sh
scripts/install-supervisor.sh --agentctl /absolute/bin/agentctl \
  --state-dir /absolute/state/agentctl \
  --register-wrapper /absolute/libexec/supervisor-wrapper.sh \
  --wrapper-interpreter /bin/bash --dry-run --output json
```

Remove `--dry-run` to register and install. Omit `--wrapper-interpreter` for an
executable script; supported explicit interpreters are `/bin/bash` and `/bin/sh`.
The wrapper must forward the supplied supervisor arguments to the reviewed
agentctl binary. Planning never executes it. Existing plist arguments must match
the exact wrapper prefix plus the reviewed supervisor arguments; additional
settings are refused. Registration binds the script and interpreter hashes in
the existing service ownership manifest. Subsequent ordinary upgrades reuse that
launcher and reject changes, even with `--force`; explicitly review and register
again after intentional edits. The wrapper is user-maintained and is not removed
by uninstalling the service.

## Explicit legacy skill adoption

```sh
agentctl --output json bootstrap adopt --harness hermes
agentctl bootstrap adopt --harness hermes --apply --expected-digest sha256:PLAN_DIGEST
```

The first command is read-only. Its result has `path`, `state`, `digest`,
`published_releases`, and `dry_run`. The digest covers every candidate file,
including metadata. The apply result additionally contains `backup` and state
`adopted`. Adoption requires one local harness; `--target-dir` selects an exact
noncanonical skills root. Shared Codex/OMP roots remain shared. Multica runtime
bundles continue to use their existing distribution installer.

Only bundled published release hashes are eligible. An optional release manifest
must match the same release, and an optional ownership marker must bind both
assets. Extra files, edited content, symlinks, or writable-by-others assets return
`conflict` without adoption. The original directory is retained below
`HOME/.local/share/agentctl/adoption-backups`, outside the skill discovery roots,
then replaced
with current embedded assets and ownership metadata. If writing fails, the old
directory is restored or its backup location is reported. A repeated apply with
an old digest is a conflict and does not create another backup. Adoption does not
remove compatibility copies, edit pointers, or change configuration; follow it
with normal bootstrap status/update as appropriate.
