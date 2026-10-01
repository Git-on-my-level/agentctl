# Follow-up turns

`agentctl continue` sends the next instruction to the native session of a
delegated execution that has finished. It is for the common loop where a
parent reads a result, then sends review feedback or the next step to the same
agent, which still has the whole conversation.

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
- The same native session. agentctl relaunches the native CLI with its own
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
| The harness has no verified resume route | `capability_unavailable`, `continue_route_unavailable`, with the reason |
| No native session id was recorded | `capability_unavailable`, `continue_session_unrecorded` |
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

The result shape is the [continue-result schema](../schemas/continue-result.schema.json).

## Which harnesses can be continued

The `resume` capability is negotiated from the exact invocation when the
execution launches, like `steer`, and is reported in the delegate plan's
preflight and in `status`. The adapter's `Resume` operation and `continue`
build their argv from the same verified route, so an adapter that reports
`resume` can perform it.

| Harness | Follow-up turn | Evidence (2026-10-01) |
| --- | --- | --- |
| Claude Code | yes | `--resume <session>` with streaming input; three-turn conversation kept both facts it was given, and a follow-up turn accepted a live steering message |
| Codex | yes | `codex exec resume <thread>`; same three-turn check, and a follow-up turn accepted interrupt-and-resume steering twice |
| Cursor | yes | `--resume <chat>` after a completed turn kept context across three turns. (An interrupted Cursor turn does not, which is why Cursor cannot be steered.) |
| OMP | yes, for expert launches only in practice | `--resume <session>` after a completed turn kept context when run by hand. It has no delegate recipe on a host that does not list it as preferred. |
| Devin | no | Print mode does not report its session id, so the exact session cannot be named. |
| ZCode | no | No verified resume route. Not installed on the verification host. |
| Generic process, Multica | no | No native session to resume; Multica owns its own continuation. |

Resume is a separate capability from steering because the two differ in
practice: Cursor and OMP keep a completed turn but lose an interrupted one.

## State and privacy

The native session id is recorded on the execution when it finishes, as a
`native_session` source binding. Like every opaque native reference it is
host-local, operator-private, and redacted from normal output; `continue
--plan` shows the native argv with the id replaced by `<native-session>`.
Executions that finished before this binding existed cannot be continued.

The follow-up prompt is handled exactly like a delegate prompt: delivered to
the native CLI, never journaled, identified only by its digest.

## Not covered

- Executions launched with expert `run`. They record no launch recipe.
- A follow-up on another host. The native session lives where it ran.
- Devin and ZCode, until they expose an exact session id or a resume route.
