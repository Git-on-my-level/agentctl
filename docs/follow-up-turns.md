# Follow-up turns

`agentctl continue` sends the next instruction after a delegated execution
finishes. The default `--mode native` resumes the native conversation.
Explicit `--mode carry_forward` starts a fresh session from the frozen recipe
with the prior stored answer and the new instruction as quoted context. This
works for any eligible native harness, including harnesses without resume;
it never claims to preserve the original conversation.

```bash
agentctl continue exec-... --request-key review-01-round-2 --prompt-file feedback.md --plan
agentctl continue exec-... --request-key review-01-round-2 --prompt-file feedback.md --wait --content
```

To redirect an execution that is still running, use [steering](steering.md).

## What a follow-up turn is

- A new execution with its own ID, events, result, and collection state. Its
  `supersedes` names the execution it continues, and the result reports that
  ID as `continues`. Once the new turn completes, the continued execution's
  `superseded_by` points at it and a `superseded` event is appended to it.
- In native mode, the same native session. agentctl relaunches the native CLI with its own
  resume flag, so the agent keeps its conversation. agentctl stores no
  transcript and only an opaque, redacted reference to the session.
- The same recipe. The turn is launched from the frozen recipe of the original
  `delegate`: the same harness, model, settings, and permissions, in the same
  working directory. Only the session selection and the prompt differ. A third
  turn is built from that original recipe, not from the second turn's argv.
- Foreground-owned, exactly like `delegate`: `--wait`, `--content`, result
  assertions, `--timeout`, and labels behave the same. Labels default to the
  continued execution's labels.

## Rules

| Situation | Outcome |
| --- | --- |
| The execution is still running | `invalid_state`, `continue_source_running`; wait or steer |
| The execution failed, was cancelled, or is orphaned | `invalid_state`, `continue_source_not_completed`; an interrupted turn may not have kept its context |
| The execution was launched with expert `run` | `capability_unavailable`, `continue_launch_plan_missing`; no recipe was recorded |
| A later completed turn exists | `conflict`, `continue_not_latest`, with `latest_execution_id` |
| Another follow-up turn is running | `conflict`, `continue_in_progress`; nothing is launched |
| Native mode: the harness has no verified resume route | `capability_unavailable`, `continue_route_unavailable`, with the reason |
| Native mode: no native session id was recorded | `capability_unavailable`, `continue_session_unrecorded` |
| The original working directory is gone | `invalid_state`, `continue_cwd_missing` |
| Multica-authority execution | `capability_unavailable`; Multica owns issue and run continuation |

A failed or cancelled follow-up does not block the session: continue again
from the same completed turn with a new request key.

Two invocations that both pass these checks still launch only one turn. Each
journals its execution first and then looks again; the turn created first
launches and the other is terminalized as cancelled, with source state
`admission_refused`, without starting a native process. A refused turn
releases its request key, because nothing was sent: the same key can be used
again, either to retry once the session is free or to continue from the turn
that was admitted instead.

`--request-key` is required. A retry with the same key and the same prompt
recovers the turn it already started, including its answer, instead of sending
the instruction twice. The same key with a different prompt is a conflict.

A FAILED/CANCELLED keyed replay is explicitly marked `replayed: true` in the
success payload (including `--plan`) or `error.details` for `--wait`, with a
`next_action` for a deliberate new attempt using a new key. `reused: true`
continues to mean the existing execution was recovered. Idempotency never
relaunches failed work. Refused admissions still release their key.

The result shape is the [continue-result schema](../schemas/continue-result.schema.json).

## Explicit carry-forward rounds

```bash
agentctl continue exec-... --mode carry_forward --request-key review-round-2 --prompt-file feedback.md --plan
agentctl continue exec-... --mode carry_forward --request-key review-round-2 --prompt-file feedback.md --wait --content
```

The mode is an explicit choice, including for harnesses that support native
resume. There is no automatic fallback after a resume refusal or runtime
failure. Original harness, model, settings, permissions, environment selectors,
prompt delivery, and cwd come from the immutable admission. Current defaults
cannot redirect the round. Each subsequent round uses the original recipe,
never a previous resume argv. A native follow-up after a carry-forward resumes
the fresh session created by that round, where the adapter supports it.

Carry-forward reads the prior outcome without acknowledging collection. It
requires completed work with verified observation integrity and a complete,
nonempty, digest-verified stored answer with source provenance. The prior text
limit is **64 KiB (65536 UTF-8 bytes)**. Truncated results are refused, never
silently summarized. The whole composed JSON prompt, after escaping, must fit
**8 MiB (8388608 bytes)**. The new instruction must be valid UTF-8 and follow
the existing prompt-source confinement and size rules.

The prompt-only [carry-forward schema](../schemas/carry-forward-prompt.schema.json)
defines `schema_version: 1`, `mode: "carry_forward"`, `context_policy`,
`source_execution_id`, `prior_result: {source, sha256, bytes, media_type, text}`,
and `instruction`. JSON quoting keeps prior output structurally separate from
the new instruction; the policy tells the agent to treat it as quoted context.
This is a context boundary, not a model-level injection guarantee. The parent
must supply necessary task context in the instruction: the original prompt and
conversation were not retained. Only the immediate prior answer is carried.

Plans, receipts, and follow-up wait errors report:

```json
{"continuation":{"mode":"carry_forward","native_session_reused":false,
  "prior_result":{"source":"assistant","sha256":"sha256:<64 hex digits>","bytes":123}}}
```

Native turns report `{"mode":"native","native_session_reused":true}`.
Only provenance, digests, counts, and lineage are added to metadata. The
composed prompt and new instruction never enter the journal. The existing
bounded result store remains the sole answer store.

| Refusal before admission | Diagnostic | Exit |
| --- | --- | --- |
| Missing, empty, or policy-omitted prior answer | `continue_prior_result_missing` | 3 |
| Prior answer exceeds 64 KiB | `continue_prior_result_too_large` | 2 |
| Composed prompt exceeds 8 MiB | `continue_carry_forward_prompt_too_large` | 2 |
| Invalid UTF-8 instruction | `continue_prompt_invalid_utf8` | 2 |
| Truncated prior answer | `continue_prior_result_truncated` | 8 |
| Unverified evidence, absent or mismatched provenance/digest | `continue_prior_result_unverified` | 8 |

Other source, cwd, authority, lineage and admission refusals use the shared
rules above. Corrupt journal data follows the existing store error contract.
Plans are read-only and reserve no key. The semantic key includes source ID,
new prompt digest, timeout, explicit labels, and mode; native mode preserves
older native digests. Prior provenance is frozen at admission; replay does not
re-read the answer or revalidate source retention. A mode or prompt change with
the same key conflicts (8). `--wait` retains outcome exits: attention 10,
failure 11, cancellation 12, unknown 13; exact `--content` has no JSON wrapper.

Carry-forward uses the same `supersedes` edges, `superseded_by` convenience
pointer, one-round admission guard, and stale-source `continue_not_latest`
error with `latest_execution_id` as native resume. There is no `--latest`
flag in this release; callers use that returned ID. Failed/cancelled successors
do not advance the latest completed turn. Multica still owns its issue/run
continuation; neither mode relaunches Multica work locally.

## Design choices

The mode lives on `continue` so prompt handling, keyed recovery, collection,
and lineage admission have one implementation. Native request digests stay
compatible with existing journals. Immutable delegation provenance preserves
mode across result recovery and older supervisor projection rewrites.

Rejected alternatives:

- Automatic fallback from resume to a new launch would silently lose native
  conversation context and could duplicate a prompt after an uncertain failure.
- A separate `follow-up` command would duplicate admission and keyed recovery
  for the same lineage operation.
- Callers creating unrelated delegates would lose lineage/latest-turn checks
  and have to compose and track context themselves.
- Retaining prompts/transcripts or silently summarizing oversized answers would
  introduce a second conversation store or model-dependent policy.
- Relaunching FAILED/CANCELLED work under the same key would break retry safety.

## Verified native resume routes

The `resume` capability is negotiated from the exact invocation when the
execution launches, like `steer`, and is reported in the delegate plan's
preflight and in `status`. The adapter's `Resume` operation and `continue`
build their argv from the same verified route, so an adapter that reports
`resume` can perform it.

| Harness | Native resume | Evidence (2026-10-01) |
| --- | --- | --- |
| Claude Code | yes | `--resume <session>` with streaming input; three-turn conversation kept both facts it was given, and a follow-up turn accepted a live steering message |
| Codex | yes | `codex exec resume <thread>`; same three-turn check, and a follow-up turn accepted interrupt-and-resume steering twice |
| Cursor | yes | `--resume <chat>` after a completed turn kept context across three turns. (An interrupted Cursor turn does not, which is why Cursor cannot be steered.) |
| OMP | yes, for expert launches only in practice | `--resume <session>` after a completed turn kept context when run by hand. It has no delegate recipe on a host that does not list it as preferred. |
| Devin ACP | conditional | Loads the recorded session only when initialize advertises loadSession; print-mode runs cannot continue. |
| ZCode | no | No verified resume route. Not installed on the verification host. |
| Generic process, Multica | no | No native session to resume; Multica owns its own continuation. |

This table describes native resume. Explicit carry-forward uses a fresh
launch for any eligible native delegation, including ZCode and OpenClaw.

Resume is a separate capability from steering because the two differ in
practice: Cursor and OMP keep a completed turn but lose an interrupted one.

## State and privacy

The native session id is recorded on the execution when it finishes, as a
`native_session` source binding. Like every opaque native reference it is
host-local, operator-private, and redacted from normal output; `continue
--plan` shows the native argv with the id replaced by `<native-session>`.
Native mode requires this binding; carry-forward does not require a native session id.

The follow-up prompt is handled exactly like a delegate prompt: delivered to
the native CLI, never journaled, identified only by its digest.

## Not covered

Automatic latest-anchor resolution and detached continuation
are designed as [follow-up lifecycle PRs](generalized-lifecycle.md#follow-up-pr-designs).
`delegate --detach` starts parent-independent work today; this command retains
foreground ownership for both modes.

- Executions launched with expert `run`. They record no launch recipe.
- A follow-up on another host. The native session lives where it ran.
- Native resume for Devin print-mode runs and ZCode; explicit carry-forward is available with a frozen delegate recipe and eligible stored answer.

In native mode, Devin continuation starts a fresh ACP server and checks `loadSession` before
`session/load`. A missing capability produces `acp_load_session_not_advertised`
and sends no follow-up prompt. Replayed history never becomes the new result.
A plan is read-only and does not initialize an ACP server, so support remains
conditional until execution. The permission grant and configured model are
preserved from the original frozen recipe.
