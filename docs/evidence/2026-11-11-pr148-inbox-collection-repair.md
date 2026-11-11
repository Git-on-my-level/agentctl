# PR148: inbox collection evidence and main integration

This candidate integrates main `fd950e73600611165ada674fba35754877c69903`
with a normal merge (`9f21fc8afa3e399ddb0c6aba77cd1b5124cdabd9`). Main includes
#147's final strict child-discovery guards and #150's OpenCode transport recovery.
This is implementation and regression evidence, not an independent review verdict.

## False-settlement reproduction

On the integrated merge before repair, the parent's isolated
`TestLeadProbeMismatchedAcknowledgementCannotSettleParent` reproduced a clean
`ok: true, count: 0, total: 0` inbox despite a completed, uncollected child.
Only the parent had been acknowledged. A record stored under another valid typed
execution key falsely named that child in its JSON `execution_id`.

The committed CLI regression is
`TestChildExitInboxValidatesCollectionSnapshot`. With only acknowledgement.go,
children.go and inbox.go overlaid from `d4c94e155e4e2275010830fbaff863472649f5a5`,
the `wrong-key`, `invalid-source` and `missing-time` cases all fail: each gets a
clean empty success rather than the required structured corruption failure.
This overlay preserves the exact old production paths responsible for settlement;
the new test is run from this candidate. The parent's separate exact-d4c94e1
probe and the integrated pre-repair reproduction agree with it.

The repair shares #147's selected acknowledgement validation with the global
snapshot already loaded by inbox. The authoritative typed bucket key must agree
with the JSON execution identity; collection time must be nonzero and source
must be result, await or bulk_reconciled. Invalid JSON, nested buckets and invalid
keys also fail closed. Inbox returns exit 70, internal error,
`diagnostic_code: inbox_acknowledgement_corrupt`, `complete: false`, and no result.
No new journal scan, write, authority or settlement state is introduced. Other
acknowledgement APIs retain their existing semantics.

The regression checks those precise CLI fields, valid collected/uncollected
controls, and a running-child control that remains an obligation despite a
stamp. Every case compares the complete journal bytes before and after the read.

## Retained contracts

- The 1,200 collected historical pairs and 1,024/1,025 subscription scale cases
  remain in `TestChildExitInboxCollectedHistoryCannotStarveActiveParent`.
  Historical exit/outcome evidence remains immutable; collected history spends
  no shared recheck budget. Covered nonterminal children remain obligations.
- Lazy subscription reads call main's shared `decodeFanoutSubscription`, retaining
  identity/key validation, compatibility filtering and unrelated-record fanout
  parity. Existing count, byte, page, lock and processing bounds remain intact.
- Main's required-bucket, acknowledgement and retained-tombstone corruption
  guards, scoped membership generations, certified writer transactions, legacy
  fences, ABA and cleanup tests are retained. Production writer audit finds
  bbolt.Update only in Journal.update; raw writes occur only in corruption and
  mixed-version test fixtures.
- OpenCode's newly supported route uses the same frozen continuation admission,
  verified version and native session existence checks as #150. Fake built-binary
  coverage verifies one automatic turn and recursive suppression for 2.2.6,
  while an unverified version stays flagged without another native turn.
- Exact-ID await locks are evidence of live collection only while held. Attention
  and unacknowledged terminals remain actionable; parent waiters do not cover
  children, and await-any does not establish per-child coverage.

The focused race run passed for cmd/agentctl (199.391s), internal/store
(138.001s) and internal/runtime (1.478s), covering child exit/query, corruption,
scale, waiter loss, hydration, retention, membership and OpenCode. The strengthened
`TestChildExitFlagAndContinuationExactlyOnce` separately passed under the race
detector (2.471s): two simultaneous callbacks from the same pending parent plus
a later stale replay still launch only one continuation.

Validation uses plain system Git first on PATH, isolated HOME/XDG roots and fake
native/SSH/notification fixtures. It does not contact providers, pages, Telegram,
GCP or production. Full local and exact-head hosted validation are recorded in
PR148; the parent owns the final independent review and merge decision.
