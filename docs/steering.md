# Steering

`agentctl steer` sends a new instruction to a native execution that is already
running. It exists for a parent agent that needs to redirect a child mid-task
without cancelling it and losing its context.

Steering does not add an authority. The native CLI still owns the session and
its conversation semantics; agentctl only carries a message to it through a
route that the native CLI itself provides.

```bash
agentctl steer exec-... --prompt-file steer.md --plan
agentctl steer exec-... --prompt-file steer.md
agentctl steer exec-... --prompt-file steer.md --allow-interrupt
```

## Ownership and the control channel

A native child belongs to the foreground `run` or `delegate` process that
launched it. `agentctl steer` is a separate invocation, so the two meet in an
owner-only spool directory beside the journal:

```text
<journal directory>/steer/<execution-id>/<time>-<request>.json
```

- The owner creates the directory when the launch negotiated a steering route
  and removes it when it returns. Its existence is the signal that requests
  will be serviced. No daemon or socket is involved.
- A request is written to a temporary name and renamed into place. The owner
  claims it by renaming it again, so claiming and withdrawal are exclusive:
  a caller that gives up either removes its unclaimed request, and knows
  nothing was delivered, or finds it already claimed.
- The owner applies requests in arrival order from its observation loop, so a
  delivery never races its own reads of the native session.
- The owner journals one `progress` event per request with `source_state`
  `steer_delivered` or `steer_rejected`. The caller waits for that event.

The request file is the only place the message exists at rest. It is
owner-only, lives for the time between the two processes' polls, and is
deleted once applied or withdrawn. The journal, events, and callbacks carry
the message digest and size only.

## Routes

The route is negotiated from the exact invocation when the execution launches
and stored as the `steer` capability in its envelope. It is never inferred
from an adapter name.

| Delivery | Capability status | What happens |
| --- | --- | --- |
| `live_input` | `supported` | The native CLI was launched in a streaming-input mode and agentctl holds its stdin open. The message is written to that stream. The agent takes it at its next turn boundary: between tool calls when it is using tools, otherwise after its current reply. |
| `interrupt_resume` | `degraded` | The native CLI is one-shot. agentctl stops the process and relaunches the same native session with the message. The in-flight turn is discarded. |

`degraded` follows the usual rule: it is used only when the caller permits the
weaker semantics, here with `--allow-interrupt`.

With `live_input` an execution can span several native turns. A native
terminal record ends the execution only when no message is still queued;
otherwise it is journaled as `turn_completed` and the answer to the queued
message becomes the final result. Each message the native CLI takes is
journaled as `input_acknowledged`.

`interrupt_resume` keeps the session but not necessarily the interrupted turn.
In a test where the agent read a file and was then steered, Claude Code still
knew the file's contents once that tool call had completed, while Codex kept
only the conversation up to the launch prompt. Files already written stay on
disk either way. Restate anything the agent must keep in the steering message.

With `interrupt_resume` the request waits until the native CLI has reported
its session id, because interrupting earlier would leave nothing exact to
resume. If the resume launch itself fails, the execution fails with
`steer_resume_failed`; it is not relaunched from scratch. An execution can be
steered more than once, and a follow-up turn started with `continue` can be
steered like any other execution on its route.

### What each adapter supports

Verified against the installed CLIs on 2026-10-01.

| Adapter | Route | Evidence |
| --- | --- | --- |
| Claude Code 2.1.286 | `live_input` with `--prompt-delivery stream` and `--input-format stream-json --output-format stream-json --replay-user-messages`; `interrupt_resume` for a one-shot argv | Live message queued mid-turn and injected between tool calls. `--resume <id>` after an interrupt kept the earlier context. |
| Codex 0.159.0 | `interrupt_resume` through `codex exec resume <thread>` | The resumed thread kept the earlier context. `--sandbox` is carried as `-c sandbox_mode=...` because `resume` has no such flag. An argv with a flag `resume` does not accept is not steerable. |
| Cursor 2026.09.23 | none | `--resume <chat>` after an interrupt answers without the interrupted turn's prompt, so the task would be lost. `cursor-agent acp` behaves the same after `session/cancel` and is not mapped. |
| OMP 17.0.6 | none | A session interrupted during its first turn cannot be resumed. `--mode rpc` has a working `steer` command, and `omp acp` kept context across cancel and re-prompt; this adapter maps neither. |
| Devin 3000.11.x | none | Print mode does not report its session id. `devin acp` kept context across cancel and re-prompt but is not mapped. |
| ZCode | none | One JSON document per run and no verified resume route. Not installed on the verification host. |
| Generic process, Multica | none | No native steering route. Multica owns run input. |

`delegate` launches Claude Code in the streaming-input form, so delegated
Claude Code work is steerable without interruption. Expert `run` gets the same
route only when the caller passes those flags and `--prompt-delivery stream`.

A prompt that the caller embedded in native argv cannot be steered: agentctl
can only re-deliver a message through a channel it attached itself. Use
`--prompt-file` or `--prompt-stdin`.

## Guarantees and failure semantics

- Success means the message reached the native session's input. It is not
  proof that the agent acted on it.
- Terminal, Multica-authority, and route-less executions are rejected before
  anything is queued.
- A request the owner does not take before `--timeout` (default 60 seconds) is
  withdrawn and reported `timeout`, retryable, with nothing delivered.
- A request the owner took but never recorded is reported `execution_unknown`
  and is not retried.
- `--idempotency-key` with the same message returns the recorded outcome, even
  after the execution finished, instead of delivering twice. Concurrent retries
  of one keyed request are applied once by the owner. Without a key, each
  invocation is a new request.
- An execution that is still starting has no inbox yet; the request waits
  briefly for it instead of failing.
- A stale owner lease is reported `execution_unknown`; nothing is queued.
- Steering does not survive the owner. It has the same process-scoped
  lifetime as the native child.

`steer` result:

```json
{
  "id": "exec-...",
  "state": "running",
  "adapter": "claude-code",
  "replayed": false,
  "steer": {
    "status": "delivered",
    "delivery": "live_input",
    "request_id": "sha256:...",
    "event_id": "event-...",
    "message_bytes": 108,
    "message_sha256": "sha256:..."
  }
}
```

Rejections use the normal error envelope with a `diagnostic_code`:
`steer_interrupt_not_permitted`, `steer_inbox_missing`, `steer_expired`,
`steer_invalid_state`, `steer_capability_unavailable`, or
`steer_resume_failed`.

## Not covered

- Cross-process `cancel` for native executions is unchanged. The spool could
  carry a cancel request, but that is a separate change.
- A finished execution. Send the next instruction to its session with
  [`agentctl continue`](follow-up-turns.md), which is the more common need in
  practice and works on more harnesses, because a completed turn keeps its
  context where an interrupted one may not.
- An ACP client would give Devin and OMP a live route, because cancelling a
  turn and prompting again in the same ACP session kept their context. It does
  not help Cursor, which drops a cancelled turn over ACP too. It is a second
  adapter transport and is tracked in the roadmap.
