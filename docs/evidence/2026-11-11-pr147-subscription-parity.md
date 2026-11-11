# PR147 subscription evidence and fanout parity

Reviewed baseline: `d20744400abb79a5e9b4c9f16318700db472fc97`.

## Verified defect and repair

Discovery accepted active subscription metadata after compatibility checks but
without the typed identity validation used by real event fanout. A malformed
subscription ID could therefore suppress a running child even though fanout
could not deliver its terminal notification. An understood active unrelated
record with invalid identity also aborts the fanout transaction; a valid matching
record elsewhere in the bucket cannot establish coverage in that situation.

`decodeFanoutSubscription` now shares the existing JSON decode,
`fanoutUnderstands`, and `validateStoredSubscriptionIdentity` path between
fanout and discovery. For understood records it also checks bucket key/record ID
agreement. This reuses validation of the subscription, origin, execution filters,
coordinator, replacement and cursor identities. A key mismatch now returns
`ErrCorrupt` directly rather than failing later during delivery lookup.

Compatibility checks retain their order: inactive records, unknown filter fields,
and unsupported destinations are skipped as before, without treating them as
coverage. A typed identity failure in a supported active record returns
`ErrCorrupt`, even for an unrelated filter. Malformed JSON or structurally
undecodable records abort both scans before compatibility filtering. Existing
null-record handling is unchanged. No new record-envelope requirements are added.

The complete bounded subscription scan finishes before any child result is
returned. Record/byte/deadline/read-lock bounds, scope membership certification,
legacy fences, attention and exact-waiter behavior are unchanged. The original
acknowledgement, tombstone and required-bucket repairs are untouched. There are
no API, JSON, schema, compatibility-epoch or migration changes; reads perform no
repair or maintenance writes. Corruption uses the existing structured CLI
exit-70 error and never a successful complete result.

## Regressions

`TestChildrenSubscriptionFanoutParity` exercises the real transactional fanout
and both parent/caller-session discovery scopes. Cases include invalid typed
subscription IDs, valid but mismatched IDs, malformed keys, invalid host/filter/
coordinator/cursor identities, malformed JSON/record shapes, and a valid matching
record preceding an invalid unrelated record. It checks delivery rollback when
the metadata scan aborts. Delivery metadata is inspected without running a
notification worker. Controls cover exact and unrelated valid subscriptions,
inactive records, expired records, unknown filters/destinations and null records.

`TestChildrenSubscriptionEvidenceReadOnlyCLI` checks both scope forms for
malformed IDs, key disagreement, malformed records and scan-wide corruption,
plus valid and compatibility controls. Store and CLI probes compare journal
bytes before/after fresh read-only queries. CLI probes assert structured
nonzero errors with no `complete: true` on corruption and reject maintenance.

## Exact-baseline counterfactual

Keep the new tests, but restore these production files from exact baseline
`d20744400abb79a5e9b4c9f16318700db472fc97` using a Go overlay:

- `internal/store/children.go`
- `internal/store/subscription_fanout.go`

Portable variables below identify the disposable overlay, git-only shim,
notification blockers, cached exact Go 1.26.11 SDK and test storage. The overlay
maps the absolute source filenames to copies obtained with `git show` from the
baseline; it does not modify the checkout.

```sh
unset GOROOT
export GOTOOLCHAIN=local GOENV=off PYTHONDONTWRITEBYTECODE=1
export PATH="$PLAIN_GIT_DIR:$NOTIFY_BLOCK_DIR:$GO12611_BIN:$PATH"
export TMPDIR="$TEST_SCRATCH/tmp"
export GOCACHE="$TEST_CACHE/go-cache"
export GOMODCACHE="$TEST_CACHE/go-mod-cache"
export GOPATH="$TEST_CACHE/gopath"

scripts/go-hermetic.sh go test -overlay "$OVERLAY_JSON" ./internal/store \
  -run '^TestChildrenSubscriptionFanoutParity$' -count=1
scripts/go-hermetic.sh go test -overlay "$OVERLAY_JSON" ./cmd/agentctl \
  -run '^TestChildrenSubscriptionEvidenceReadOnlyCLI$' -count=1
```

Both commands exit 1 as expected. The store command fails eight cases: invalid
subscription ID, valid ID disagreement, malformed key, invalid origin/filter/
coordinator/cursor identities, and invalid unrelated active identity. The CLI
command fails four cases in both scopes: invalid ID, valid ID disagreement,
malformed key, and invalid unrelated active identity. Each CLI failure reproduces
exit 0 with `complete: true` and `count: 0` on the baseline. Malformed-record and
compatibility controls pass on both versions.

Without the overlay, the focused store and CLI probes pass, including all
original round4 corruption/read-only checks and the existing cross-version
fanout test:

```sh
scripts/go-hermetic.sh go test ./internal/store ./cmd/agentctl \
  -run 'TestChildrenSubscription|TestChildrenCorrupt|TestChildrenMissing|TestChildrenOptional|TestFanoutSkipsSubscriptionsFromAnotherRelease' \
  -count=1
```

Measured outcome: store 4.149 s, CLI 1.195 s; both pass.

## Final validation commands

```sh
make ci
scripts/go-hermetic.sh go test -race ./cmd/agentctl ./internal/store \
  -run 'TestChildren|TestFanout|TestSubscription|TestCleanup|TestAttribution|TestRemote|TestWaiter|TestAutomaticRetention|TestAutomaticCleanup|TestManagedChildren|TestNativeAdmission|TestAwaitContinues|TestAcknowledg' \
  -count=1
git diff --check
```

Focused race outcome: CLI 139.613 s, store 139.401 s; both pass on Go 1.26.11.

The first full CI attempt passed CLI/store but failed the unchanged
`TestReview3OriginIsLocalConfigurationOnly` in `internal/anxrun` with a nil local
origin lookup. That subprocess has a 500 ms deadline. With plain git verified,
ten isolated repetitions passed in 4.756 s; no ANX source was changed. Full CI
was restarted after concurrent race testing finished and passed in full,
including ANX (11.711 s), CLI (345.466 s), all validators, live-owner release
skew against v0.19.0/v0.21.10 at epoch 1, and the build. Linux-only systemd
fixtures were skipped on macOS. The initial failure is recorded rather than
counted as a passing run; no unrelated source repair or waiver was used.

The PR body records terminal full-validation, normal pre-push and exact-head
hosted results. The parent owns the next independent verdict. No live provider,
SSH endpoint, real notification, GCP, production or ANX call is used by these
probes. PR148 is untouched and needs no API/JSON reconciliation for this repair.
