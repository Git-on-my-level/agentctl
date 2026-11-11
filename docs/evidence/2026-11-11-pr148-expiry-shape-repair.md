# PR148: expiry evidence after independent round 5

Baseline: `3d5bb77841f67250e0e9d2e3e9511c9de818d533`, on main
`fd950e73600611165ada674fba35754877c69903`. This repairs the **same retained-membership
negative-evidence mechanism** identified in rounds 4 and 5. It is implementation
and regression evidence, not the parent's mandatory independent round 6 verdict.

With baseline production code and only the CLI regression extended, scope-only,
key-only, null/empty scope/key and missing/empty/null/invalid digest records caused
both a falsely empty child query and `ok: true, total: 0` inbox output. Fourteen
new malformed variants reproduced this in a 2.032s package run. Existing retained,
missing-member and valid-expiry controls remained in the same test. The repaired
initial CLI and store controls passed (2.798s and 1.321s respectively); additional
malformed digest/metadata combinations are included in final validation.

The shared decoder now recognizes compact expiry only when **both** scope/key
fields are absent, and full expiry only when **both** are present and equal the
authoritative index scope/key. Raw JSON presence distinguishes omitted fields
from explicit null/empty values. Both forms require `sha256:` plus 64 lowercase
hex digits, the representation emitted by `attributionDigest` and preserved by
`replayTombstone`. No unrelated mutation-record semantics are tightened. Retained
execution rows/buckets still cannot be hidden by expiry evidence.

Producer inspection: `putAttributionBinding` emits full scoped index metadata and
its SHA-256 attribution digest. Current `replayTombstone` emits exactly three
compact fields (`input_digest`, `object_type`, `object_id`) for external request
keys and removes internal membership metadata during cleanup; historical compact
serialization has the same three-field shape (see `2a7f997:internal/store/replay_tombstone.go`).
There is no claim that current cleanup produces full child-index expiry.
`TestChildrenExpiryProducerCompatibility` creates real attributed executions,
uses real digest-reviewed retention Plan/Apply, reads its actual compact replay
record, and verifies its preserved digest/identity and omitted scope/key. To model
retained historical membership it reinstalls that exact produced compact record;
the full compatibility control adds only the exact scope/key from the actual
attribution producer's saved index. Neither valid control invents a zero-valued
expiry fixture. Existing raw corruption fixtures now distinguish absent metadata
from present-empty mutation-record fields.

`TestChildExitInboxRequiresCompleteRetainedMembership` requires `ErrCorrupt` from
Children for malformed expiry and the precise `children_query_incomplete` guard,
collection health/reason and read-only recheck action from inbox. Both queries
leave complete journal bytes unchanged. Valid compact/full, retained collected/
uncollected, missing-member and tombstone-versus-retained controls remain.
No schema, epoch, migration, API shape, authority, write-side settlement or
read-side repair is added. Subscription laziness, history selection, bounds and
#147 membership certification/legacy-writer fences are unchanged.

Final focused race, normal pre-push and hosted exact-head results are recorded in
PR148. The local hook runs `make pre-push`, the shared CI subset; the three remaining
`make ci` targets (distribution, release skew, build) are validated separately so
the unchanged full test suite runs only once locally. Go 1.26.11, plain Git and
hermetic HOME/XDG/fake provider/notification fixtures are used. Prior-head hosted
CI had an unrelated, source-unchanged ANX descendant-process timing failure;
its failed-job retry and 20 local race repetitions passed. That anomaly is retained
as evidence, not claimed repaired by this change.

Focused stateful races on the repaired source passed with Go 1.26.11 / Apple Git
2.54.0: cmd/agentctl 105.718s, internal/store 60.342s, internal/runtime 1.552s.
The selected test families include child exit/collection/membership, all Children
controls (including actual producer retention), attribution hydration/cleanup,
waiter registration/loss, active inbox selection, automatic continuation, CAS and
native recovery. The command used `-race -p 1 -count=1`, avoiding concurrent heavy
package suites. Schema/portable-asset, distribution, release-skew and build checks
also passed before the normal push gate. No ANX source changes are present.
