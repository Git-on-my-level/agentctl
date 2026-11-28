# Unattended native approval behavior

This audit covers tool permission approvals, not authentication, provider capacity,
workspace hooks or arbitrary user input. No label promises that a native turn
cannot wait for another reason. The native harness remains the policy authority.
A denied tool call does not necessarily make the whole turn fail: the model can
finish with an explanation. `fails-in-turn` means the approval decision is returned
to the turn rather than awaiting an operator.

| Delegate recipe | Classification | Evidence and limits |
| --- | --- | --- |
| Codex coding, with or without the coding grant | fails-in-turn for declined commands | Agentctl pins `--sandbox workspace-write` and `approval_policy=never`. A command needing interactive approval is declined, latched as `native_command_declined`, and fails back to the parent. No generated command rules expand execution permissions. Provider execution is not verified in this audit. |
| Codex read_only | fails-in-turn | Adds `approval_policy=never` to `--sandbox read-only`. Provider execution is not verified in this audit. |
| Cursor coding, trust and coding grants | fails-in-turn | Reviewed `--trust --force` recipe bypasses workspace/tool approval; live denial behavior not verified in this audit. Without the grants, unknown. |
| Cursor read_only | unknown | `--trust --mode ask` requires a separate trust grant; tool approval behavior on a real turn not verified. |
| Claude Code coding, coding grant | unknown | `--permission-mode bypassPermissions` skips permission prompts. Real unattended provider behavior is not verified in this audit. |
| ZCode yolo | unknown | The existing reviewed native evaluator retains always-ask checks. Whether every such check denies headless or parks is not verified; `constrained` remains the honest permission label. |
| ZCode read_only | fails-in-turn | Existing version-pinned 0.17.0 isolated plan profile denies execution/writes; AskUserQuestion/ExitPlanMode fail headless per the prior profile review. This audit does not reverify that real-host behavior. |
| Devin ACP, with or without coding grant | fails-in-turn | Adapter answers every request immediately: allowed `allow_once` only with the frozen grant, otherwise `reject_once` or cancelled. Denials emit attention and prevent successful completion; no unanswered approval RPC. Existing ACP transport fixtures cover both paths; provider execution not verified. |
| OMP | unknown | No reviewed approval-bypass flag is added. Native approval behavior not verified in this audit. |
| OpenCode coding, coding grant | unknown | OpenCode v2 `--auto` auto-approves permissions that are not explicitly denied; explicit denials remain in force and permissions report constrained. `read_only` is unavailable. Real approval behavior not verified. |
| OpenClaw local | unknown | Embedded-local structured output does not establish tool approval behavior. No approval flag is inferred; real approvals not verified. |
| Hermes coding | unknown | `--yolo` is added only with the coding grant. It bypasses dangerous-command prompts, not proven isolation or all gateway/session-policy checks; permissions remain constrained. Real approvals not verified. |

No recipe is newly claimed to deny every approval in-turn without real evidence.
Unknown recipes remain unknown rather than receiving an invented permission flag.

## Ten-minute approval watchdog

When agentctl receives a native approval/permission attention event, the fixed
10-minute watch starts at that event's observed time. Stderr health events and
unrelated logs do not reset it; actual running-turn activity does. At the
boundary agentctl cancels the process and records a failed result with stable
diagnostic `native_approval_wait_timeout`. `delegate --wait` returns the failure
directly; `await` and `result` collect the same terminal failure. A caller-specified
execution `--timeout` remains independent, and whichever deadline occurs first
applies.

Codex's headless `approval_policy=never` normally returns a declined command
rather than waiting. If Codex emits `item.completed` with a declined
`command_execution`, or returns its native “approval policy is Never; reject
command” denial before creating a command item, the adapter latches
`native_command_declined`, cancels the run, and reports failure even if the model
later completes with an explanation. The denial is verified against the current
turn's persistent Codex rollout; delegated launches using `--ephemeral` are
rejected before the child starts because that evidence is not retained.

The watchdog applies to local native executions whose adapter emits a typed
approval/permission signal. It does not infer approval from silence, detect
authentication/provider/input waits, change Multica authority, widen grants, or
answer native prompts. The existing native_approval_required attention
diagnostic and repair action remain in place while the wait is active.

To investigate a timeout, inspect `agentctl status <id>`,
`agentctl events <id> --kind attention`, and `agentctl result <id>`. Review the
harness policy and retry with a new request key after the policy is repaired.
The adapter never answers a native prompt on the user's behalf. An opaque stall
that emits no approval signal cannot be identified as approval attention.

## Hermes wire evidence

The installed Hermes version was `v0.22.4+131.g3b4dca6`.
Its `hermes chat --help` advertises `--query-file -`, `--format
stream-json`, model, reasoning, yolo and native resume. The recipe sends a single
UTF-8 query through managed stdin and closes it at EOF. Expert invocations must
use the same managed delivery and structured output. Read-only access and
explicit service speed fail before launch.

`internal/adapter/testdata/hermes-gateway-failure.jsonl` pins stdout from the real
installed binary on 2026-10-23: `system/init` then `result` with `session_id`,
`exit_code`, `text`, tokens, duration and optional error. The local installed
`hermes_cli/stream_json.py` confirms the same terminal shape on success. Tests
synthesize success records from that inspected shape; they are not recordings of
a successful provider turn. Text/tool events and stderr are never answer sources.
Only a nonempty terminal `result.text` with explicit `exit_code: 0`, no error,
and a successful process exit is accepted. A delta-only exit or late process
failure discards the answer.

The live gateway failed with `Gateway connection failed; no local fallback`.
The help-advertised `--run-budget` was also rejected by this host's gateway CLI
policy. No host configuration was changed, no local fallback was attempted,
and the recipe adds neither budget nor max-turn creation overrides.
Provider-backed success, permissions and continuation are **not verified**.
Resume is advertised as degraded for this reason. Continuation argv uses only
`chat --format stream-json --resume SESSION --query-file -`; model, provider,
reasoning, yolo, max-turn and budget overrides are removed because Hermes owns
the resumed session's frozen policy. Interrupt-and-resume steering is unavailable.
