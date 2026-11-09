# Operator rollout: journal busy fix

This procedure was prepared but **not executed** by the implementation agent.
The live journal and supervisor were left untouched. Merge/release approval
and host rollout remain the operator's responsibility.

## Review and preflight

Review PR `journal: fix busy degradation under concurrent load`, require green
CI for its exact head, and copy that reviewed SHA into `reviewed_sha` below.
Use the existing worktree or a fresh managed Git worktree on Ephemeral. Do not
reset another agent's checkout.

```sh
set -eu
cd /Volumes/Ephemeral/scratch/worktrees/agentctl-journal-busy
git fetch origin journal-busy-1109
reviewed_sha='<reviewed PR head SHA>'
test "$(git rev-parse HEAD)" = "$reviewed_sha" &&
  test "$(git rev-parse origin/journal-busy-1109)" = "$reviewed_sha" &&
  nice -n 10 make build VERSION="journal-busy-$(git rev-parse --short=12 HEAD)"
```

Before the first new-supervisor minute, preserve both binary and journal. This
is an atomic APFS clone, not a sequential copy of a changing database. Keep the
backup private and outside sweepable scratch. These are operator commands;
they create backup files but do not open the source with bbolt or lock it.

```sh
set -eu
rollout_backup="$HOME/.local/state/agentctl/rollback/journal-busy-$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -m 700 "$rollout_backup" &&
  cp -p "$HOME/.local/bin/agentctl" "$rollout_backup/agentctl"
export AGENTCTL_ROLLOUT_BACKUP="$rollout_backup"
python3 - <<'PY'
import ctypes, os
source = os.path.expanduser('~/.local/state/agentctl/journal.db')
target = os.path.join(os.environ['AGENTCTL_ROLLOUT_BACKUP'], 'journal.db')
lib = ctypes.CDLL('/usr/lib/libSystem.B.dylib', use_errno=True)
if lib.clonefile(source.encode(), target.encode(), 0):
    raise OSError(ctypes.get_errno(), 'atomic journal snapshot failed; stop rollout')
os.chmod(target, 0o600)
PY
```

## Install and verify

The managed installer reconciles/restarts an already-managed supervisor. Its
preflight preserves registered wrappers and refuses drift. It must report a
successful supervisor reconciliation; do not bypass a failure by manually
copying a binary or silently replacing the service definition.

```sh
./scripts/install.sh --binary "$PWD/build/agentctl" --dry-run &&
  ./scripts/install.sh --binary "$PWD/build/agentctl"
agentctl supervisor status
agentctl doctor
```

Verify the running supervisor's version and executable digest match the new
artifact (`shasum -a 256 build/agentctl`); do not rely solely on `agentctl --version`.
After at least one maintenance pass and several recovery cycles, check fresh
cycle timestamps, failure/retry counters, journal_busy errors and successful
normal delegate/await work. Use a harmless normal task with a new request key
as the canary. Retention logs name only aggregate removed counts. File bytes
need not fall: deleted pages become reusable. Existing uncollected, subscribed,
promoted or oversized histories may remain protected. This PR does not include
an online physical file replacement operation.

The new service defaults retention on: terminal history older than 30 days,
32 executions / 4096 events per batch, startup grace one minute, then five-minute
cadence. To opt out before installation, set its inherited service environment
and verify that no plist/wrapper overrides it:

```sh
launchctl setenv AGENTCTL_JOURNAL_RETENTION off
```

Alternatively configure the managed launcher to pass
`supervisor run --no-journal-retention`. Never run an extra supervisor alongside
the managed one. To re-enable the default, unset the environment override and
restart the reviewed managed service through the installer; do not edit the
live journal.

## Rollback

Use the same reviewed installer with the saved **previous installed binary**;
it reconciles the managed supervisor as part of the install. These commands
are for the operator, not part of the source fix:

```sh
launchctl setenv AGENTCTL_JOURNAL_RETENTION off
./scripts/install.sh --binary "$rollout_backup/agentctl" --dry-run &&
  ./scripts/install.sh --binary "$rollout_backup/agentctl"
agentctl supervisor status
agentctl doctor
```

Check running version/digest and fresh healthy cycles again. Schema and lock
compatibility stay at epoch 1, so binary rollback requires no journal migration.
Binary rollback stops future new-version maintenance; it does **not** resurrect
already expired terminal records. Keep the backup snapshot for a separately
reviewed recovery. Never restore it over an active journal, and never discard
post-snapshot executions by silently restoring history. `launchctl unsetenv
AGENTCTL_JOURNAL_RETENTION` removes the opt-out for a later approved rollout.
No automatic update-policy or fleet configuration change is included here.
