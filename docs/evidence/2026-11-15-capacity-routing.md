# Account routing investigation, 2026-11-15

The [m5 snapshot](2026-11-15-capacity-routing-before.json) records the provider
observations and the most recent account decisions, with their UTC timestamps.
The newest decision rejected the sooner-resetting Codex account even though its
provider usage left ample headroom.

## Cause

`recentPoolLaunches` counted every running execution plus every execution created
within `max_age_seconds`. Selection added `launch_debit` for all of them to the
published provider usage. A subsequent provider observation did not retire the
completed jobs already included in that usage. The facts freshness limit thus
became an unrelated reservation lifetime. Continued delegations could also
retain the original capacity choice; a new request is needed to exercise a new
selection, and replay must preserve its original choice.

At the exact headroom boundary, subtraction also rounded the remaining headroom
below the documented inclusive floor. This made the snapshot's inflated pressure
ineligible. A distinct regression uses pressure above that boundary to ensure the
observation fix is tested independently of the floating-point fix.

The same selection engine handles every configured pool, so this affected Claude
and Cursor as well. It also dropped a long-running job after completion if its
creation time was outside the freshness interval. Multiwindow accounts need a
separate reservation cutoff for each observation, rather than an account-wide
counter or the facts file's publication time when `observed_at` is available.

## Fix and acceptance

Selection now compares terminal times with each window's observation, keeping a
reservation for running work and completions newer than that observation. Without
usable observations, recent completions retain a bounded reservation. Each
window calculates pressure independently; the highest pressure limits the
candidate, and equal pressure prefers the earlier known reset. Resets retire
prior-window completed work. The exact headroom boundary is inclusive.

Regression tests cover the production failure for all three account families,
observation fallback and republishing, old-start/recent-completion jobs, running
work, window filters, reset turnover, future observations, equal-pressure window
ordering, replay, and actual account launcher selection. Full validation and
installed-runtime evidence are recorded alongside the release after shipping.

Named models remain caller-authoritative and explicit Multica dispatch remains
under Multica authority. This change adds no provider probes, credentials store,
background service, or journal writes to read-only plans.

An attempted Multica validation handoff hit the existing
[CLI compatibility defect #85](https://github.com/Git-on-my-level/agentctl/issues/85):
the current CLI rejects `--client-key` during flag parsing. Native agentctl
validation on m5 was used instead. No Multica task was created by that invocation.
