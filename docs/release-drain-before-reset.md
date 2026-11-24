# Drain account allowance before reset

Native `delegate` pools now default to `drain_before_reset`. Among accounts
whose reported windows can fit the next run, agentctl prefers the earliest of
their latest resets. The eligibility calculation includes local launch debits,
an expected run cost of 0.01, and a reserve of 0.03 below the plan limit.
The profile can tune `expected_cost` and `epsilon`.

Receipts record the effective policy in `capacity.mode`. Unusable facts or a
failed journal scan fall back to `soonest_reset` with a warning. If every
candidate is over its limit, the least-used one still runs.
Drain mode warns when an account has explicit legacy limit settings.

To roll back selection on the new binary, set `policy` to `soonest_reset` on the
affected capacity profile or pool. Before reverting to an older binary, remove
the optional `policy`, `epsilon`, and `expected_cost` keys from its config and
bundles. Stored requests remain readable by older binaries. Consumers that
validate receipts against a schema should update their schemas for the new
`drain_before_reset` mode and reason.
