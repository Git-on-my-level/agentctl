# PR148: retained-membership completeness after independent round 4

Baseline: `336e8e94897af3b8e58fee20a682f7b829a73ef7`, on main
`fd950e73600611165ada674fba35754877c69903`. This repairs the one retained-membership
negative-evidence mechanism identified by the parent's independent round 4. It
is implementation/regression evidence, not an independent review verdict.

`TestChildExitInboxRequiresCompleteRetainedMembership` first ran with baseline
production code and only the new test added. It reproduced clean `ok: true,
count: 0, total: 0` inbox results for a missing indexed child row, a missing row
with a valid collection stamp, a covered-only parent exit followed by a missing
row, and mismatched expiry ID/scope. The existing authoritative child query
correctly returned incomplete/corrupt coverage for those same journal bytes.
Retained uncollected/collected and compact/full valid-expiry controls passed.

The repair preserves the execution/acknowledgement prefilter's positive evidence,
but its negative result no longer permits settlement by itself. Before skipping
a marked otherwise-settled parent, a read-only scoped index proof verifies every
retained member against the complete hydrated snapshot or valid matching expiry.
The query and proof share exactly the same strict membership/expiry decoder.
A missing row is incomplete even with an acknowledgement; an expiry tombstone
cannot revoke a retained row. No new persisted state, epoch, schema, migration,
external/API shape, authority or repair write is introduced.

The metadata proof runs under the same held read-only journal handle in one
stable transaction, validates scope certification/generations and uses a shared
1.5s deadline, 100k selected-scope/entry, 64MiB index and 1MiB value bounds. Only
selected parent prefixes are sought. It reads no subscriptions, waiter sidecars,
events or outcomes. The existing live-child recheck phase retains its own shared
1.5s deadline, so collected history cannot spend that budget or repeatedly scan
subscriptions. After existing inbox snapshot loads, these two bounded phases may
add up to 3s. Unproven scopes stay explicitly incomplete rather than disappearing.

The CLI regression requires the precise incomplete guard, collection work health,
reason and read-only `children --execution` action, rather than accepting any error.
It compares the complete journal bytes before/after both query paths. Covered-only
missing rows do not invent uncollected child rows. Store coverage also checks that
writable handles, partial snapshots and cancelled proofs cannot certify settlement.

Retained gates include the 1,200 settled-history/older-active-parent fixture with
both 1,024/1,025 valid-subscription controls, strict acknowledgement/fanout checks,
#147 membership/legacy-writer/ABA/cleanup fences, immutable hydration, waiter-loss
rechecks, and native continuation/recursion/cancellation/deadline controls.
Validation uses plain Git first on PATH, isolated HOME/XDG and fake native/SSH/
notification fixtures. Actual final-head local/hosted gates are recorded in PR148;
the parent owns independent round 5 and any merge decision.

Focused stateful races passed on this repair: cmd/agentctl 171.382s,
internal/store 125.945s and internal/runtime 1.417s. The focused set included the
new CLI/store membership cases and all retained gates listed above. The initial
unrepaired CLI run failed in 1.088s with five false-clean cases; repaired CLI and
selected store corruption controls passed before the comprehensive race run.
