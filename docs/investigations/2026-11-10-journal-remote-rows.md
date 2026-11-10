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
and stalled SSH while a competing writer opens the scratch journal.
Multica remains a cached local mirror, refreshed by its supervisor/per-id await.

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
is changed. The full CI/pre-push reruns use that isolated configuration. Their
final results are recorded in the PR.

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
