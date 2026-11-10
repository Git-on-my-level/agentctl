# Journal contention and remote forwarding investigation

Measured on m5-mbp, 2026-11-10, 16:34–16:44 UTC, starting at `b182f2a`
(v0.21.8). No GCP tier was used. Live commands were limited to read-only
status/events/supervisor status; no live journal cleanup, compaction, supervisor
restart, provider launches, or remote host modifications were performed.

## 1. Two-minute journal starvation: not reproduced

A temporary Python harness created a private state directory, config, journal,
fake native CLI and fake SSH. While each suspect invocation ran, it invoked
`agentctl --journal <scratch>/state/journal.db --journal-busy-budget 0 recent --limit 1`
approximately every 400ms (including command time). The two SSH stages each
slept 3s; the fake native CLI slept 3s before its structured terminal answer.
All processes and temporary journals were reaped/removed. The build was an
unmodified-main binary. No model/provider request was made.

| Suspect | Exit | Reader samples | Busy samples |
| --- | ---: | ---: | ---: |
| remote-plan | 0 | 17 | 0 |
| local-foreground | 0 | 10 | 0 |
| local-detach | 0 | 15 | 0 |
| remote-launch | 0 | 17 | 0 |
| closed-stdout | -13 | 10 | 0 |

`closed-stdout` closed the caller's output pipe immediately; exit -13 is SIGPIPE.
The foreground child had completed and been reaped before output delivery.
These samples exclude a persistent lock across these particular SSH/native waits;
they do not prove absence of contention under a production workload.

Source review: `findDelegation` closes its read before SSH; remote admission
opens its writer after SSH and closes before collection; native launch closes
admission before starting/waiting for its child. The update worker's owner scan
uses `ps`, not the journal. Supervisor engine operations use `scopedJournal`,
which releases handles around individual store calls. Retention and event
bursts are bounded/paged, but disk latency and scheduling under load remain
possible causes. No speculative lock change is included.

A second temporary store-package test wrote 8 MiB chunks to a **scratch** bbolt
bucket, deleted that bucket, and measured ten reopens/small metadata commits.
This exercises a real persisted freelist; it is not just a sparse file resize.
The following are observed maxima, not production latency guarantees:

```
=== RUN   TestScratchJournalSizeAudit
    journal_size_audit_test.go:17: populated_mb=922 file_bytes=1032585216 free_pages=0 ro=true max_open=238.042µs max_commit=0s
    journal_size_audit_test.go:17: populated_mb=922 file_bytes=1032585216 free_pages=61933 ro=false max_open=6.412875ms max_commit=14.51275ms
    journal_size_audit_test.go:17: populated_mb=2300 file_bytes=2467971072 free_pages=0 ro=true max_open=202.917µs max_commit=0s
    journal_size_audit_test.go:17: populated_mb=2300 file_bytes=2467971072 free_pages=149456 ro=false max_open=15.442958ms max_commit=38.39725ms
--- PASS: TestScratchJournalSizeAudit (30.55s)
PASS
ok  	github.com/Git-on-my-level/agentctl/internal/store	30.982s

```

Large file size by itself did not explain a 120s hold. The synthetic freelist
and storage/cache state are not identical to either live journal. Retention
reuses freed pages but does not shrink the bbolt file. It intentionally protects
unacknowledged results, request-key bindings, source/lineage bindings and ANX
run links. Seven-day-old uncollected results are not eligible for the default
14-day acknowledged-result retention policy.

Evidence needed to settle the production cause: during the next busy interval,
capture the blocking open's exact host/journal path, owner PID and stack, command,
lock tenure and I/O latency. Identify the holder before collecting its stack;
do not infer a continuous 120s holder solely from repeated failed acquisitions.
An offline backup with the production freelist/page layout would allow safe
reproduction without experimenting on the live file.

### Review follow-up: next starvation instrumentation

The independent PR review (2026-11-11) found no demonstrated journal handle
held across network I/O or sleep in the inspected paths. Its strongest remaining
single-command hypothesis is `reconcile`: `cmd/agentctl/reconcile.go:163–182`
opens a write handle retained while classifying rows and proving process
ownership (`:367`, `:541`). A large stale journal could make those operations
slow. This is a hypothesis, not a reproduction of the incident. Instrument
write-handle tenure around that classification/ownership work next, recording
row counts and process-proof latency with the holder PID/stack. The alternative
is aggregate short writes from many agents starving successive opens; measure
individual holder tenure and acquisition gaps to distinguish it from a single
long hold. Reconcile is intentionally unchanged in this PR.

## 2. Busy-budget override: bug

`parseCommon` accepts the flag after subcommand flags. The resolved allowance
was omitted when constructing remote delegate/continue commands, so the remote
CLI selected its own 120s default. Detached local workers already inherit it.
Forward the resolved duration, including zero and environment-derived values,
through remote launch and terminal collection. Regression tests inspect 300s/0
receipts and exercise a locked scratch remote journal through the built binary
with a 70ms flag/environment budget. No 300s sleep is needed to prove propagation.

## 3. Await-any remote blindness: bug

The scan classified only cached local envelopes. Remote status intentionally
does not update that cache, and the supervisor cannot probe those rows locally.
Poll remote status after closing all journal handles, bounded to 3s per row
and by the overall wait deadline. Unchanged polls back off to 4s. Transport or
identity errors explicitly name the local row and offer per-id await. Collect
only the winning terminal through existing remote await/result, retaining its
remote terminal timestamp for `--since`. Other matches are not collected.
`--no-acknowledge` suppresses the local stamp; remote collection still
acknowledges the winning remote result, as per-id remote await does.

Tests cover prefix and exact selectors, success/failure/cancel/orphan,
acknowledgement suppression and prefix draining, stable resume timestamps,
and stalled SSH while a competing writer opens the scratch journal. Additional
regressions prove that remote integrity conflicts are actionable under both
kind filters, skewed terminal clocks remain visible without acknowledgement,
and collection keeps its journal busy budget. The real binary test blocks the
scratch remote journal for 700ms after status and for 11s under `--no-timeout`;
both complete successfully. Collection adds no internal wall-clock deadline and
uses the caller's deadline/context plus the configured contention allowance.
Multica remains a cached local mirror, refreshed by its supervisor/per-id await.

### Review regressions: mixed failures and scan bounds

On the pre-review-fix head, `TestAwaitAnyRemoteWinnerSurvivesProbeErrors`
reproduced a ready winner discarded by an unreachable host in either selector
order; `TestAwaitAnyRemoteProbeErrorWaitsForDeadline` returned a remote failure
after 30ms despite a 200ms wait. `TestAwaitAnyLabelPrefixCandidateLimit` observed
257 candidates and timed out after beginning SSH probes. The fixes retain
observed winners, report per-row failures as warnings, and defer a probe error
until no winner exists at the caller deadline. Eight workers, 3s probes and a
4s remote status scan budget bound fan-out; scans resume at the first row
interrupted by the scan budget so each row eventually gets a full probe window.
`TestAwaitAnyRemoteScanBoundsAndRotation` covers concurrency, cancellation/reaping,
budget and continued coverage. Prefixes over 256 matching rows fail explicitly
before SSH (including acknowledged rows); ordering and pending counts describe
only observed matches. Help and the portable skill document these limits.

## 4. Supervisor degradation: remote-row bug; m5 load symptom unproven

`SupervisorExecutions.ListNonTerminal` included remote forwarding rows. Native
binding reconstruction then failed because those rows deliberately have no
local native source binding. Exclude them from local recovery; await/await-any
retain remote observation authority. A regression reproduces the bad inventory
and verifies exclusion.

A read-only m5 supervisor status at 16:35:01 UTC instead reported an inventory
open failure (`journal busy after 5 attempts over 2.001s (budget 2s)`). This is a
separate contention symptom, not evidence of a remote binding failure. The
reported repeated-update CAS conflict can arise from concurrent revisions;
its exact occurrence tonight was not reproduced. Budget deferral by itself is
intentional and should not be treated as proof of failure.

## 5. OpenCode failures: classification bug; cwd collision not established

All three named executions have an `opencode.error` event with the allowlisted
classifier `provider.invalid-request`, followed by a failed terminal outcome:

| Execution suffix | Error event sequence | Error observed UTC | Terminal observed UTC |
| --- | ---: | --- | --- |
| coyote-extend-other-fish-people-apology | 77 | 15:19:53.983448 | 15:24:06.422914 |
| harvest-blue-usage-cabin-float-law | 122 | 15:25:13.990134 | 15:27:20.735283 |
| other-stem-critic-bachelor-aware-crunch | 25 | 15:04:02.578921 | 15:06:16.621814 |

The executions have distinct recorded source fingerprints. The evidence proves
provider request rejection after native activity, not why the provider rejected
it. The private `failed-output-<id>.txt` files are absent; this cache is only
produced for generic-process result extraction failures, not OpenCode provider
errors. No private native session database was inspected. The adapter retained
the classifier in events but only promoted explicit HTTP 429 to a normalized
failure; other structured failures became `kind: unknown`.

An unresolved `provider.invalid-request` now yields nonretryable
`opencode_provider_invalid_request`, kind/category `provider`, with the fixed
message “OpenCode provider rejected the request”. Tests cover adapter-to-result
classification and the public outcome projection; provider prose is not copied
into public failure output. Existing 429/cancellation/progress-clearing tests
protect capacity and unrelated completion failures. No cwd serialization rule
is added without evidence. A sanitized provider response or native diagnostic
from these exact runs is needed to distinguish payload/context/tool-schema
rejection from a possible native shared-state defect.

## Validation and operator follow-ups

During review validation on 2026-11-11, a Linux ARM64 container with Go 1.26.10
hung in the unchanged `TestOversizedStructuredRecordReturnsTypedDiagnostic`.
Clean `origin/main` (`b182f2a`) reproduced it with
`go test ./internal/adapter -run '^TestOversizedStructuredRecordReturnsTypedDiagnostic$' -count=1 -timeout 20s`:
`panic: test timed out after 20s`, naming that test. The native macOS adapter
package passed. This platform-specific test issue is outside the two review
fixes. The local complete-suite validation therefore runs config fixtures in
isolated Linux (avoiding the managed-Git conflicts below) and all other packages
on macOS; no test is skipped. Hosted CI validates its normal Linux environment.

The existing `TestRemoteThroughBuiltBinary` failed on **clean b182f2a** with
`not_found / load profile config` on this Mac. Its remote config used an
uncanonicalized `t.TempDir()` path through `/var`; config deliberately rejects
symlink ancestors. Canonicalizing the fixture directory makes it pass and lets
the new real remote-lock regression run. This was reproduced on clean main,
not attributed to the feature changes.

A full `make ci` attempt on 2026-11-10 at 16:50 UTC failed only in existing
config-source tests: managed Git refused their `agentctl@example.invalid`
fixture commits because the hermetic wrapper hides the global restamp identity.
`TestSourceMaterializesAnxOnInitUpdateAndRestore` reproduced this on clean
`b182f2a` too. A temporary, process-scoped managed-Git config pointing to a
scratch Git config containing only the already configured host name/email lets
the guard restamp those local fixture commits and makes that test pass. The
identity guard stays enabled; no host/global config or source fixture identity
is changed. The full CI/pre-push reruns used that isolated configuration. It exposed two
more pre-existing config-source failures: overriding the fixture's Git global
config prevents its `insteadOf` rewrite, and managed Git refuses its deliberate
force-push to a local bare `main`. Both `TestSourceUpdateAcceptsInsteadOfRewrittenRemote`
and `TestSourceUpdateRejectsNonFastForwardAndPreservesAppliedState` reproduced
on clean `b182f2a` with that same configuration. These host/test-environment
conflicts are not changes to agentctl behavior; the guards are left enabled.
All remaining CI targets are run separately and hosted CI uses its normal
unwrapped Git environment. Final results are recorded in the PR.

Release this PR through the normal reviewed release process before expecting
installed fleet behavior to change. No release or merge is part of this work.
For file shrinkage, plan an offline maintenance window: drain all owners and
observers, stop the managed supervisor with approval, create and verify a backup,
compact to a separate file with bbolt tooling, verify it, atomically replace with
owner-only permissions, restart, and validate identity/outcomes before deleting
the backup. Never compact or swap a journal while processes are using it.
Review uncollected results explicitly before acknowledgement/retention; do not
bulk-acknowledge failures merely to reduce disk usage. Request-key/lineage/ANX
protections can retain records even after acknowledgement. ANX outcome recording
was blocked by three identical authorization HTTP 500 responses before any write.
