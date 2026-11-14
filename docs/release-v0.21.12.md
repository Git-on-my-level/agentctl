# v0.21.12

Capacity pools now retire completed-work estimates when a provider usage
observation includes that work. This prevents recent delegation history from
inflating measured usage and prematurely sending work away from an account whose
allowance resets sooner.

The fix applies to every pool, including Codex, Claude, and Cursor accounts.
Each usage window uses its own observation and pressure; running work retains a
reservation, and long-running jobs remain reserved after completion until the
next observation. Reset turnover clears completed work from the previous window.
The documented inclusive headroom boundary is now stable under floating-point
rounding, including configured thresholds.

Schemas, help, the portable skill, and the embedded distribution describe the
same policy. Named-model requests and idempotent replays retain their existing
selection semantics.

See the [dated investigation and runtime evidence](evidence/2026-11-15-capacity-routing.md).
