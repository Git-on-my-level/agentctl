# PR147: corrupt-state repair after independent round4

Reviewed and recreated-worktree baseline:
`9799439e7f837d82ae75dbf17abd2b8033873388`.
Disk branch, clean tree and published draft PR head were reconciled before edits.
This batch repairs only the three verified findings. The PR remains draft for
the parent's new independent review; this evidence is not a merge verdict.

## Repairs

1. Selected terminal acknowledgements are decoded before collection suppression.
   The typed execution ID must match the selected key, the timestamp must be
   nonzero, and the source must be one of the existing result/await/bulk-reconciled
   sources. Malformed or mismatched records and nested acknowledgement buckets
   return `ErrCorrupt`; oversized acknowledgement values hit the existing bounded
   query error before decoding. Absent acknowledgements remain actionable.
2. Expired indexes validate the typed index-key ID against ObjectID before skip.
   Legacy compact tombstones may omit Scope/Key; provided values must agree.
   A tombstone is accepted only when the target execution key is absent, including
   absence of a nested bucket at that key. A retained target returns `ErrCorrupt`.
3. Missing required idempotency/execution buckets return `ErrCorrupt` in read-only
   schema validation and in the page reader. The schema check also protects
   explicit parent identity lookup before it can hydrate attribution through the
   missing index. It never creates a bucket or attempts a migration.

The changes retain all query limits and the existing scope/membership proof,
writer certification, mixed-version fence, waiter/attention, attribution,
remote-admission and retention contracts. Optional historical acknowledgement,
subscription and membership metadata remain optional on read. No schema version,
public API, JSON output, compatibility epoch or lifecycle authority changes.

The CLI preserves existing error conventions: corruption encountered by discovery
is an `internal` structured error (exit 70); unavailable explicit parent identity
evidence first becomes `children_query_incomplete` (exit 9). Neither is a successful
empty result. No identity error mapping was broadened in this repair.

## Meaningful regressions

`TestChildrenCorruptAcknowledgement` covers malformed JSON, null/empty records,
wrong or invalid typed IDs, invalid/missing source, missing/invalid time, nested
buckets and oversized values. Controls cover absent acknowledgements and every
allowed source. Parent and caller-session scopes run against the same journal.

`TestChildrenCorruptTombstone` covers compact/full tombstones over retained rows,
missing/wrong/invalid ObjectID, malformed key IDs, mismatched Scope/Key and nested
execution targets. Compact/full tombstones whose target is absent remain valid.

`TestChildrenMissingRequiredBucket` covers both required buckets without a panic.
`TestChildrenOptionalHistoricalMetadataNeedsNoUpgrade` removes optional metadata
and still discovers the uncollected child without creating that metadata.

Every store case asserts exact journal-byte equality before/after read-only
queries. The CLI regression exercises all three findings through both explicit
parent and caller-session arguments, checks structured nonzero errors with no
`complete: true`, blocks automatic maintenance, and checks unchanged journal bytes.
No native launch, notification or network is needed by the new tests.

The existing paged-membership legacy-tombstone fixture now deletes its target in
that same simulated legacy cleanup transaction. It still proves that mutation
between pages fails incomplete and a stable retry succeeds. A tombstone over a
retained execution is now explicitly a corrupt-state regression, not a valid
legacy-cleanup control.

## Exact previous-head counterfactuals

A Go overlay restores **both changed production files**, `children.go` and
`store.go`, to the exact baseline above; the new test files remain current.
Each finding is probed separately so the old missing-bucket panic cannot prevent
the other regressions from running. `BASELINE_OVERLAY` names that overlay JSON in
disposable test storage:

```sh
scripts/go-hermetic.sh go test -overlay="$BASELINE_OVERLAY" ./internal/store \
  -run '^TestChildrenCorruptAcknowledgement$' -count=1
scripts/go-hermetic.sh go test -overlay="$BASELINE_OVERLAY" ./internal/store \
  -run '^TestChildrenCorruptTombstone$' -count=1
scripts/go-hermetic.sh go test -overlay="$BASELINE_OVERLAY" ./internal/store \
  -run '^TestChildrenMissingRequiredBucket$' -count=1
```

All three baseline commands exit **1** as expected. The first fails **11** cases;
the second fails **9** cases. They reproduce `count=0 error=<nil>` for corrupt
records in both scopes. The third reproduces a nil-pointer panic at the missing
idempotency cursor. The repaired non-race probes pass for both packages:

```sh
scripts/go-hermetic.sh go test ./cmd/agentctl ./internal/store \
  -run 'TestChildren(Corrupt|MissingRequired|OptionalHistorical)' -count=1
```

## Required validation

Full `make ci` passed, including all Go packages, schema/link/script and
embedded/distribution checks, build, and live-owner release skew against
v0.19.0 and v0.21.9 (epoch 1). Linux systemd fixtures are skipped on this macOS
host. The focused race packages passed (cmd/agentctl 130.007 s, internal/store
167.428 s). Normal pre-push and exact-head hosted results are recorded in the
final PR body.
All commands below use portable names for the actual disposable test storage,
git-only shim and notification command stubs. The parent-provided shim was checked
to resolve git to plain `/usr/bin/git`; task git operations use the managed wrapper.

```sh
export PYTHONDONTWRITEBYTECODE=1
export TMPDIR="$TEST_SCRATCH/tmp"
export PATH="$PLAIN_GIT_DIR:$NOTIFY_BLOCK_DIR:$PATH"
make ci
scripts/go-hermetic.sh go test -race ./cmd/agentctl ./internal/store \
  -run 'TestChildren|TestCleanup|TestAttribution|TestRemote|TestWaiter|TestAutomaticRetention|TestAutomaticCleanup|TestManagedChildren|TestNativeAdmission|TestAwaitContinues|TestAcknowledg' \
  -count=1
git diff --check
```

The inherited retained-attributed-records coverage, lost-receipt uncertainty,
live-per-page lifecycle observations, graph retention, conservative retries for
older writers/pruning, and possible unused stamps after older cleanup remain.
This repair does not waive any finding or authorize shipping. PR148 and other
branches/worktrees are untouched. No review was launched, ANX written, GCP tier
used, production/credential/spend operation performed, real page/Telegram sent,
or host installation/merge/tag/release attempted. The parent records outcomes and
owns the next independent verdict.
