# Design: lane fan-in — label-prefix subscribe, await-any, stall attention, journal-busy retry, fallback selector, retry-of

Status: design v1 (2026-10-25). Implemented in this repository:
[journal-busy retry](#journal-busy-retry), [`await-any`](#await-any), and
[label-prefix subscribe](#label-prefix-subscribe). Specified only:
[stall attention](#stall-attention), [`--fallback-selector`](#fallback-selector),
and [`--retry-of`](#retry-of).

## Problem

A coordinator ("lead") that runs several delegated executions at once misses
completions because agentctl has no fan-in primitive:

- `subscribe create` binds one exact execution, so each lane needs its own
  subscription created after launch. A lane launched without one is invisible
  to callbacks.
- `await` waits for one execution. Waiting for "the first of N" means N
  background shells and a hand-written join.
- An execution can report `state: running`, `liveness: alive` for hours while
  its native stream is silent. Nothing reports that.
- `delegate` returns exit 6 `journal_busy` under contention, so every caller
  writes its own sleep-and-retry loop around agentctl's own lock.
- A provider capacity or quota failure before the agent starts work needs a
  new launch with a hand-bumped request key and a hand-picked fallback model.

## Boundary

agentctl reports facts about executions and absorbs contention on its own
journal. It does not hold work queues, deadlines, approvals, ownership or
lease policy, deploy logic, or knowledge of any particular program. A label
prefix is an opaque string to agentctl; what a prefix means (a program, a
lane family) is the caller's convention. Which execution "owns" a piece of
work, and what happens after a lane ends, belongs to the coordination
authority (ANX cards, Multica issues), not to agentctl.

Both modes are covered: label-prefix subscriptions and `await-any` read the
host-local journal, so they see native executions and the local mirror of
Multica-backed executions. Multica-authority state refreshes only through the
supervisor or a per-execution `await` (see [failure modes](#failure-modes)).

## Labels and prefixes

Execution labels match `^[a-z][a-z0-9_.-]{0,127}$` and are immutable after
creation. A label prefix uses the same pattern, so it is any non-empty prefix
of a valid label. `/` is not a label character; use `.` as the hierarchy
separator (`two-lane.`, not `two-lane/`). A prefix matches an execution when
any one of its labels starts with the prefix. Because labels are immutable,
whether an execution matches never changes.

Launchers should end a family prefix with a separator so it cannot match a
sibling family. For example, a two-lane launcher labels each lane
`two-lane.<lane>` (`two-lane.glm`, `two-lane.codex`) and a per-card launcher
labels `card-<handle>` (`card-x7k2`); a coordinator then waits on
`--label-prefix two-lane.` or `--label-prefix card-x7k2`. `<lane>` and
`<handle>` must themselves fit the label pattern: lowercase ASCII letters,
digits, `_`, `.` and `-` only, and never `/` or uppercase. A launcher that
derives them from free text (a branch name, a card title) must normalize first
(`feat/Lane-A` becomes `feat.lane-a`); `run` and `delegate` reject an invalid
label with exit 2 rather than rewriting it. Note that `card-x7k2` as a prefix
also matches `card-x7k20`; append a separator (`card-x7k2.`) when handles can
share a prefix.

## Journal-busy retry

Commands that create or wait for work absorb journal lock contention instead
of returning `journal_busy` on the first conflict.

| Item | Value |
| --- | --- |
| Commands with a default budget | `run`, `delegate` (including its detached worker), `dispatch`, `continue`, `await`, `await-any` |
| Default budget | `120s` |
| Other commands | historical two short attempts (budget `0`) |
| Override | global `--journal-busy-budget <duration>`, else `AGENTCTL_JOURNAL_BUSY_BUDGET`, else the command default |
| Range | `0` (two attempts) to `30m`; anything else is a usage error, exit 2 |
| Backoff | each attempt polls the lock for up to 2s (50ms inside context-bound engine calls), then sleeps a uniformly jittered delay in [d/2, d] starting at d = 100ms, doubling to a 5s cap |
| Stops early | any open error other than lock contention; caller context cancellation or deadline |

An exhausted budget returns the existing error unchanged in class:
`dependency_unavailable`, exit 6, `retryable: true`,
`details.diagnostic_code: journal_busy`, plus `busy_attempts`,
`busy_waited_ms`, and `busy_budget_ms`. The detached delegate worker inherits
the launcher's environment, so `AGENTCTL_JOURNAL_BUSY_BUDGET` applies to it; a
`--journal-busy-budget` flag on the launcher does not cross the worker
boundary. The worker's owned-execution writes already wait indefinitely, as
before.

Idempotency is unchanged: the retry repeats only the local lock acquisition,
never adapter I/O or a remote mutation, and a retried `delegate` with the same
request key still recovers the original execution. Read-only diagnostics keep
fast failure so `status` or `inbox` never hang silently behind a writer.

## Await-any

```text
agentctl await-any (--execution ID ... | --label-prefix PREFIX)
  [--timeout DURATION | --no-timeout] [--kinds terminal,attention]
  [--since RFC3339] [--include-acknowledged] [--no-acknowledge]
```

| Flag | Meaning |
| --- | --- |
| `--execution ID` | repeatable or comma-separated; at most 256 distinct IDs; every ID must exist (`not_found`, exit 3) |
| `--label-prefix P` | every execution, existing or created later, with a label starting with `P`; the set is rescanned on every poll |
| `--timeout` / `--no-timeout` | mutually exclusive; the default is no timeout, as for `await` |
| `--kinds` (alias `--kind`) | subset of `terminal`, `attention`; default both. `stall` is reserved: until [stall attention](#stall-attention) ships it fails with `capability_unavailable`, exit 5, `diagnostic_code: stall_unavailable` |
| `--since T` | only matches whose `trigger_at` is strictly after `T` |
| `--include-acknowledged` | label mode only: also match terminals that were already collected |
| `--no-acknowledge` | do not stamp collection on the returned terminal |

Exactly one of `--execution` and `--label-prefix` is required (exit 2).

### Matching

Each poll (one second apart) reads one journal snapshot. A candidate matches
when:

- **terminal**: its state is `completed`, `failed`, `cancelled`, or
  `orphaned`. In label mode a terminal that is already acknowledged, or that
  predates acknowledgement tracking, is skipped unless
  `--include-acknowledged`. In explicit mode a named terminal always matches,
  like `await` (level-triggered); drop it from the list or pass `--since` to
  move past it. `trigger_at` is `terminal_at`.
- **attention**: its state is `attention`. `trigger_at` is the
  `observed_at` of its newest journaled attention event, or `updated_at` when
  the authority recorded none.
- **integrity**: its observation integrity is `conflicted`, whatever its
  state. Reported as `matched_kind: integrity`; `trigger_at` is
  `updated_at`.

The match with the oldest `trigger_at` wins, ties broken by execution ID, so
the choice is deterministic. `pending_matches` counts the other matches in the
same snapshot; a caller loops until it reaches 0.

### Result and exit codes

Exit codes follow `await`:

| Outcome | Exit | Document |
| --- | --- | --- |
| completed | 0 | success, [`await-any-result`](../../schemas/await-any-result.schema.json) |
| failed | 11 | error `execution_failed` |
| cancelled | 12 | error `execution_cancelled` |
| orphaned | 13 | error `execution_unknown` |
| attention | 10 | error `attention_required`, with the same next actions as `await` |
| conflicted evidence | 13 | error `unknown_state` |
| deadline elapsed | 9 | error `timeout`, details `selector`, `kinds`, `watched` |
| interrupted | 12 | error `execution_cancelled` |

The success result and every match error carry `execution_id`, `state`,
`matched_kind`, `trigger_at`, `pending_matches`, `watched` (candidates in the
snapshot), `selector` (`{"label_prefix": ...}` or `{"execution_ids": [...]}`),
`acknowledged`, and `labels`; errors carry them in `error.details`. The
success result also names the adapter and offers `result <id> --content` as
its next action. `await-any` never prints answer content; collect it with
`result`.

### Acknowledgement

Only the returned execution can be acknowledged, and only when it matched as
a terminal: the stamp has source `await`, exactly as `await` writes it.
Other matches, attention, and integrity results are never acknowledged. With
`--no-acknowledge` the returned terminal stays in `recent --unreconciled` and
`inbox`. A label-mode loop therefore drains naturally: each call returns, and
collects, one new terminal, and the next call moves on.

`--since` is the resume point for kinds that acknowledgement does not clear
(attention, integrity, explicit-mode terminals): pass the previous
`trigger_at`. Two matches with the identical nanosecond `trigger_at` across
calls can be skipped by `--since`; rely on `pending_matches` within one call.

## Label-prefix subscribe

```text
agentctl subscribe create --label-prefix PREFIX --destination file|unix|webhook|command --target T
  [--kind terminal,attention,...] [--ttl DURATION] [--authority direct|multica]
```

- `--label-prefix` and `--execution` are mutually exclusive (exit 2). The
  prefix is validated as above.
- The stored filter gains `label_prefix` (see
  [subscription schema](../../schemas/subscription.schema.json)); it is part of
  the filter digest, so a cursor bound to one prefix cannot be reused for
  another.
- Fan-out happens in the event-append transaction. For a label-prefix
  subscription the store reads the emitting execution's immutable labels in
  that same transaction. If the execution record is missing the event does not
  match (fail closed).
- It matches events appended **after** creation by existing and future
  executions. There is no backfill; a caller that may have missed earlier
  events uses `await-any --label-prefix` (level-triggered) or `inbox`.
- It never auto-expires on a terminal (`auto_expire_on_terminal: false`, and
  the store ignores the flag for label scopes). It ends at its TTL (default
  24h) or by `subscribe cancel`.
- Default kinds stay `terminal`, `attention`, `artifact`. `--kind stall` is
  reserved and fails with exit 5 until stall attention ships.
- Delivery remains at-least-once with the existing callback envelope;
  consumers deduplicate on `event_dedupe_key`.

The supervisor must run for unattended delivery, as for any subscription.

## Stall attention

Specified, not implemented. A running native execution whose observed native
activity has not advanced for a bounded quiet period becomes visible as an
attention event with a distinct reason.

- **Setting.** `delegate` and `run` take `--stall-after DURATION` (default
  `20m`, `0` disables, maximum `24h`). The value is recorded on the execution
  envelope as `stall_after_seconds` (nullable; absent on old records means
  "default"). This is a journal-format addition and goes through a design
  issue before implementation, per [CONTRIBUTING](../../CONTRIBUTING.md).
- **Activity.** The clock is `max(progress_at, started_at)`: `progress_at` is
  already advanced only by meaningful admitted native events, never by probes
  or heartbeats. Before implementation, each adapter must be verified to
  advance `progress_at` during a turn (not only at session start and result);
  an adapter that cannot is excluded from stall detection and reports that
  as a capability (`stall_detection: unsupported`) rather than emitting false
  stalls.
- **Detector.** The owning process (foreground `run`, or the detached delegate
  worker) checks once a minute while it owns the native child. The supervisor
  performs the same check for executions it observes. Both use the journal's
  semantic deduplication, so concurrent detectors converge.
- **Event.** On crossing the threshold: kind `attention`, payload
  `{"reason": "stalled", "quiet_seconds": n, "stall_after_seconds": n,
  "last_activity_at": t}`, dedupe projection = execution ID + reason +
  `last_activity_at`, so one quiet period yields one event. The execution
  state stays `running` and liveness is unchanged: a stall is a hint, not a
  verdict, and must not make `await` exit 10.
- **Clearing.** When activity advances past `last_activity_at`, the detector
  appends a `health` event with reason `stall_cleared`. A later quiet period
  produces a new `stalled` event (a different `last_activity_at`).
- **Filters.** `stall` is a filter token, not a new event kind on the wire:
  `--kind stall` (subscribe) and `--kinds stall` (`await-any`) select
  attention events and states whose reason is `stalled`. A stalled event does
  **not** match `attention`, so existing subscriptions with default kinds see
  no new deliveries. `await-any` reports `matched_kind: stall`, exit 10.
- **Inbox.** `inbox` adds reason `stalled` with `quiet_seconds`, distinct from
  the existing `running_observation_stale` (which is about agentctl's own
  observation freshness, not native activity).

## Fallback selector

Specified, not implemented.

```text
agentctl delegate --request-file R --fallback-selector SELECTOR ...
```

- `SELECTOR` uses the delegate request selector grammar (for example
  `family=glm`). It is resolved at plan time; `--plan` shows both the primary
  and fallback resolution, and a fallback that resolves to the same
  agent/model as the primary is a usage error.
- **Trigger.** Only a failure that the adapter classified with
  `failure.category: capacity` or diagnostic code `quota_exceeded`, observed
  **before the agent started work**: no native session was bound, or the
  native turn produced no assistant output and no tool activity. Any other
  failure, or a capacity failure after work began, returns as today.
- **Effect.** agentctl launches one fallback execution with the derived key
  `<request_key>~fallback` and the same prompt, cwd, labels, and permission
  defaults. The original execution stays `failed` with its diagnostic and gains
  a `superseded` event naming the fallback; the fallback records
  `supersedes: [original]`. The command returns the fallback's receipt or
  result, with `fallback_of` and the original failure category in the result.
- **Idempotency.** Replaying the identical invocation recovers the same pair:
  the original key replays its failed execution, and the fallback key replays
  the fallback. There is at most one fallback hop.
- **Detach.** With `--detach`, the detached worker performs the fallback, and
  the receipt names the original; `await` on the original follows
  `superseded` to the fallback.

## Retry-of

Specified, not implemented.

```text
agentctl delegate --retry-of EXEC_ID [--request-file R] (--prompt-file P | --prompt-stdin) ...
```

- `EXEC_ID` must be a terminal `failed`, `cancelled`, or `orphaned`
  delegated execution in this journal (otherwise `invalid_state`, exit 8).
- The new request key is derived, never invented by the caller:
  `<root_key>~retry-<n>`, where `root_key` is the original key with any
  `~retry-<n>` suffix removed and `n` is one more than the highest retry
  number already journaled for that root. The derivation is deterministic, so
  repeating the same `--retry-of` invocation after a lost receipt finds the
  existing retry instead of creating another.
- The selector, cwd, and labels default to the original's; explicit flags or
  a request file override them. The prompt must be supplied again (agentctl
  does not store prompts).
- The retry records `supersedes: [EXEC_ID]` and the original gains a
  `superseded` event, so subscriptions and `await-any` see the hand-off.

## Failure modes

| Failure | Behavior |
| --- | --- |
| Journal held longer than the budget | exit 6 `journal_busy` with attempt accounting; the caller may retry the identical invocation |
| Label-mode `await-any` over a large journal | one `ListExecutions` per second; cost grows with retained history. Use `data` retention or a narrower prefix |
| Multica-authority execution in `await-any` | reads the cached local mirror; refreshes require the supervisor (or a per-execution `await`, which reprobes) |
| Event appended for an execution whose record is missing | label-prefix subscriptions do not match it |
| Subscription created after the event | no backfill; use `await-any` or `inbox` |
| Conflicted evidence | `await-any` keeps returning it (exit 13) until reconciled; skip with `--since` |
| `stall` requested before it ships | exit 5 `stall_unavailable`, never a silent no-op |

## Out of scope

Work queues, deadlines, approvals, ownership or lease policy, retry policy
beyond the single bounded hops above, and any program-specific logic. The
coordinator composes `await-any`, subscriptions, and its own ledger (for
example ANX cards and leases); agentctl does not decide what to do after a
lane ends.

## Test plan

Implemented parts, with the tests that cover them:

- Journal-busy retry (`cmd/agentctl/journal_busy_test.go`): waits through a
  released lock, bounded engine calls absorb contention, exhaustion returns
  typed `journal_busy` with accounting, cancellation wins, budget `0` keeps two
  attempts, flag/env/default precedence and validation.
- `await-any` (`cmd/agentctl/await_any_test.go`): label mode returns the oldest
  unreconciled terminal and acknowledges only it; drains then times out with
  exit 9; `--include-acknowledged`; explicit mode is level-triggered and
  `--since` moves past a trigger; wakes on an execution created after the call
  started; failed terminal exit 11 with and without `--no-acknowledge`;
  attention exit 10 and `--kinds` filtering; usage and `stall` errors.
- Label-prefix subscribe (`internal/store/subscriptions_outbox_test.go`,
  `internal/subscription/subscription_test.go`,
  `cmd/agentctl/subscription_test.go`): existing and future executions match,
  unrelated and unlabeled ones do not, a terminal acknowledgement does not stop
  the subscription, prefix validation and digest binding, CLI mutual
  exclusion, and an end-to-end `run` producing exactly one pending delivery.

For the specified parts, the implementation PR adds: per-adapter
verification that `progress_at` advances mid-turn; a fake-clock stall
detector test (emit once, clear on activity, re-emit on a new quiet period,
no state change, `attention` filters exclude `stalled`); fallback
classification fixtures (capacity before work starts versus after);
replay-idempotency tests for `~fallback` and `~retry-<n>` keys; and
`superseded` delivery to a label-prefix subscription.
