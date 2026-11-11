# Child expiry evidence: one unambiguous decoding boundary

## Bounded repair lineage

This report covers the merged child-query dependency in #147 and the retained
membership repair lineage in #148 through review 6 on
`7c80c548a53918bdab7da8401e4afe3eef9183a6`. It is a scoped failure-class report,
not a new repository registry or an independent merge verdict.

| Prior remedy | Later verified recurrence |
| --- | --- |
| #147 rejected expiry markers that hide a retained execution. | #148's execution-only inbox prefilter bypassed the query when an uncollected row was missing. Review 4 required retained-membership proof. |
| `3d5bb77` shared the index decoder with that proof. | Review 5 showed partial scope/key and missing-digest expiry still authorizing false settlement. |
| `7c80c54` validated producer forms, field presence and digest representation. | Review 6 showed duplicate JSON scope members being reduced to the final value and again authorizing a clean inbox. |

The recurring failed assumption was that a successfully decoded expiry object
was sufficiently unambiguous to prove a child had been purged. Each field-level
repair left the permissive JSON interpretation in front of that proof.

## Shared remedy and scope

Before an expiry record can establish settlement, the shared membership decoder
now validates the complete JSON object's member uniqueness. An iterative token
walk rejects duplicate members in nested objects as well as the root. Root
aliases for scope, key, input digest, object type and object ID use the same
Unicode case-folding equivalence as the typed decoder, so a differently spelled
alias cannot overwrite authoritative evidence.

The existing producer-form, identity, digest, retained-row, scope-generation,
legacy-writer and processing checks remain in place. Both child discovery and
the inbox's negative-evidence shortcut use this boundary. The change does not
add a journal scan, write, migration, schema epoch or a general policy for other
journal records. Existing record-byte and processing bounds still apply. Unique
extension fields, including nested arrays/objects and opaque numbers, remain
compatible. Actual attribution and retention producer controls remain valid.

The actioned class is tracked by the existing PR and the lead's owning decision
card. It is not declared closed by this report: exact-head CI, resolved review
threads and a fresh independent merge verdict remain required.

## Verification observed on 2026-11-11

Focused Go 1.26.11 tests passed: store 0.415s, CLI 3.132s. They include the shared
JSON rule, actual producer-based compact/full controls, and the full inbox/query
regression with complete journal-byte comparisons.

Keeping these new regressions while overlaying only `internal/store/children.go`
from exact baseline `7c80c54` reproduced **11 false clean inbox cases**. The
counterfactual compiled and exited 1 for the expected semantic failures, not a
build error. Cases cover every authoritative root member, case/escaped/Unicode
scope aliases, compact expiry, and duplicate extension members. Repaired reads
retain the incomplete guard and read-only recheck action.

Focused races passed: store 3.625s, CLI 76.894s. This includes the 1,200-history
and 1,024/1,025-subscription controls, missing-member proofs, prior corruption
checks and subscription/fanout parity.

Reproduction uses plain Git first for fixture identities, Go 1.26.11, isolated
HOME/XDG roots and fake native/notification tools:

```sh
scripts/go-hermetic.sh go test ./internal/store ./cmd/agentctl \
  -run '^(TestChildExpiryJSONMembers|TestChildrenExpiryProducerCompatibility|TestChildExitInboxRequiresCompleteRetainedMembership)$' \
  -count=1 -timeout=90s
scripts/go-hermetic.sh go test -race ./internal/store ./cmd/agentctl \
  -run 'TestChildExpiryJSONMembers|TestChildrenExpiryProducerCompatibility|TestChildExitInboxRequiresCompleteRetainedMembership|TestChildExitInboxCollectedHistoryCannotStarveActiveParent|TestChildMembership|TestChildrenCorrupt|TestChildrenMissing|TestChildrenOptional|TestChildrenSubscription' \
  -count=1 -timeout=120s
```

For the counterfactual, use a Go overlay mapping the absolute current
`internal/store/children.go` path to a copy from `git show
7c80c548a53918bdab7da8401e4afe3eef9183a6:internal/store/children.go`; do not restore
or mutate the working checkout. Run the CLI regression's
`expired-(duplicate|compact-duplicate)` subtests with that overlay.

These probes make no real provider, page, Telegram, ANX, SSH, GCP or production
calls. Normal pre-push and exact-head hosted results belong in the PR once they
complete; this evidence does not claim those pending gates have passed.
