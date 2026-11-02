# v0.21.3

Concurrent native execution and supervisor recovery now spend less time holding
the journal's process-wide file lock. Writable opens no longer commit unchanged
schema metadata, native stream bursts yield the writer lock every 16 events,
and status/recent release read handles before rendering output. Event append
sequence allocation uses the last index key instead of traversing the index.
Darwin/Linux lock acquisition polls the same descriptor every 1-4ms within the
existing timeout instead of repeatedly missing short release windows under
bbolt's 50ms retry interval. Existing shared/exclusive locks and old binaries
remain interoperable; snapshot/copy opens are unchanged.
Steering reads its sequence boundary directly from that index and bounds final
delivery reconciliation reads.

An isolated synthetic test reached 50 simultaneously running quiet native
fixtures with no launch, replay, status, recovery or collection errors. This is
not an unlimited writer-throughput guarantee: a separate 30-fixture burst of
up to 600 distinct observations/second still produced seven bounded supervisor
lock-acquisition failures (previously 46), while all foreground operations and
terminal collection succeeded. The journal remains a single-writer database.

Detached launches commit an execution ID before worker creation. Normal
receipts report `launch_status: launched`; missing worker responses return
`launch_status: admitted`, which requires status/await to establish launch and
outcome. Worker creation failure reports and terminalizes the same execution.
Exact-key replay never starts a replacement. The worker inherits the resolved
journal-busy budget, including a flag override or explicit zero.

Supervisor recovery preserves active runner leases with one read. After five
minutes without fresh evidence it can mark a native launch orphaned only with
kernel proof that both its native launch and collecting owner identities are
gone, a matching journal host and an unchanged revision. Legacy rows without
owner identity remain unknown for explicit operator reconciliation. Wall-clock
boot comparisons are not used as ownership proof. Linux process proofs require
a matching recorded PID namespace and launch-time procfs attestation even when
the PID is present. Both launch and inspection verify that `/proc/1/ns/pid`
matches `/proc/self/ns/pid`; inherited or hidden procfs views and legacy Linux
identities without `procfs_pid_namespace_ino` remain unproven.
Live, recently observed, parked and unproven
legacy executions are preserved. Orphaned means the outcome is unknown.

The journal schema version and compatibility epoch remain unchanged. An optional
`owner_launch` field records the collecting process's kernel identity; older
projections may omit it, which makes automatic repair conservative. No migration,
record deletion or process cancellation is required. Existing owners may keep
running during the binary replacement. The new worker accepts the old launch
protocol to tolerate an in-flight v0.21.1 parent crossing the replacement.
Legacy rows without launch or collector identity need explicit reconciliation; a
parent lost between admission and worker creation leaves a known starting row.

`recent` execution rows now expose the recorded `capacity` object used by
delegate receipts/plans, when a pool was selected.

Preflight notes:

- Settings belong inside `selector.settings`; unknown top-level fields are
  refused by strict request decoding before a native launch.
- Run `delegate --plan` on the exact request before launch. A model outside the
  configured preference table is refused; `config doctor --live` can check
  catalog availability for supported harnesses, with no prompt. Static checks
  cannot prove provider/account availability for every harness.
- `--prompt-file` is confined to cwd. For an external scratch prompt use
  `--prompt-stdin < /absolute/path/task.md`; this preserves the confinement rule
  without copying prompt text into logs or the journal.
