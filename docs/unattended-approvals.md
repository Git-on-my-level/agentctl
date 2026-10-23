# Unattended native approval behavior

This audit covers tool permission approvals, not authentication, provider capacity,
workspace hooks or arbitrary user input. No label promises that a native turn
cannot wait for another reason. The native harness remains the policy authority.
A denied tool call does not necessarily make the whole turn fail: the model can
finish with an explanation. `fails-in-turn` means the approval decision is returned
to the turn rather than awaiting an operator.

| Delegate recipe | Classification | Evidence and limits |
| --- | --- | --- |
| Codex coding, coding grant | fails-in-turn | Existing `--dangerously-bypass-approvals-and-sandbox` bypasses tool approval. Provider execution not verified in this audit. |
| Codex read_only | fails-in-turn | Adds `-c approval_policy="never"` to `--sandbox read-only`; CLI 0.159.0 accepts this policy and rejects an invalid policy. Provider execution not verified. |
| Codex coding, no grant | unknown | Inherits native policy; no approval bypass is inferred. Native approval events require intervention. |
| Cursor coding, trust and coding grants | fails-in-turn | Reviewed `--trust --force` recipe bypasses workspace/tool approval; live denial behavior not verified in this audit. Without the grants, unknown. |
| Cursor read_only | unknown | `--trust --mode ask` requires a separate trust grant; tool approval behavior on a real turn not verified. |
| Claude Code auto mode | unknown | Auto mode keeps classifier checks and may deny tool calls. A denied call alone does not prove whether every native fallback fails headless or parks. No broader bypass is added. Real approval fallback not verified. |
| ZCode yolo | unknown | The existing reviewed native evaluator retains always-ask checks. Whether every such check denies headless or parks is not verified; `constrained` remains the honest permission label. |
| ZCode read_only | fails-in-turn | Existing version-pinned 0.17.0 isolated plan profile denies execution/writes; AskUserQuestion/ExitPlanMode fail headless per the prior profile review. This audit does not reverify that real-host behavior. |
| Devin ACP, with or without coding grant | fails-in-turn | Adapter answers every request immediately: allowed `allow_once` only with the frozen grant, otherwise `reject_once` or cancelled. Denials emit attention and prevent successful completion; no unanswered approval RPC. Existing ACP transport fixtures cover both paths; provider execution not verified in this audit. |
| OMP | unknown | No reviewed approval-bypass flag is added. Native approval behavior not verified in this audit. |
| OpenCode | unknown | Structured output is reviewed, but no unattended permission-bypass recipe is inferred. Native approval behavior not verified in this audit. |
| OpenClaw local | unknown | Embedded-local structured output does not establish tool approval behavior. No approval flag is inferred; real approvals not verified. |
| Hermes coding | unknown | `--yolo` is added only with the coding grant. It bypasses dangerous-command prompts, not proven isolation or all gateway/session-policy checks; permissions remain constrained. Real approvals not verified. |

No recipe is newly claimed `parks-in-attention` without real evidence. The issue
reports that outcome for the old Codex read-only recipe; the added policy repairs
that cause. Unknown recipes remain unknown rather than receiving an invented flag.

Native approval/permission attention events retain their stable diagnostic and
carry a fixed `repair_action`. `status` and delegated wait errors identify
`native_approval_required` for approval/permission source states. Their actions
point to attention evidence and explicit cancellation, not another unattended wait.
Cancel the parked turn, review the harness policy, then retry with a new request
key. The adapter never widens grants or answers a native prompt on the user's
behalf. Multica attention keeps its existing authority-specific actions. An opaque
stall that emits no approval signal cannot be identified as approval attention.

The Codex policy check used a temporary `CODEX_HOME` and `--strict-config`.
The positive invocation passed configuration loading and was stopped after 25
seconds; it did not complete a provider turn. The negative control rejected
`approval_policy="not-a-policy"` and listed `never` among the accepted variants.
No persistent Codex configuration was edited.

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
