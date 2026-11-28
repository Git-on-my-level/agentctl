# Unattended native approval behavior

This audit covers tool permission approvals, not authentication, provider capacity,
workspace hooks or arbitrary user input. No label promises that a native turn
cannot wait for another reason. The native harness remains the policy authority.
A denied tool call does not necessarily make the whole turn fail: the model can
finish with an explanation. `fails-in-turn` means the approval decision is returned
to the turn rather than awaiting an operator.

| Delegate recipe | Classification | Evidence and limits |
| --- | --- | --- |
| Codex coding, coding grant | fails-in-turn for declined commands | Codex CLI 0.170.0 `exec` with its default human reviewer forces `approval_policy=never`; this recipe pins that policy. An unmatched command requiring approval is declined, and agentctl fails and cancels the run. The narrow generated `allow` rules bypass Codex's sandbox for matching command prefixes; agentctl generates them only when Codex's `CODEX_HOME/config.toml` marks the checkout's Git root `trust_level = "trusted"`. Provider execution is not verified in this audit. |
| Codex read_only | fails-in-turn | Adds `-c approval_policy="never"` to `--sandbox read-only`. Provider execution is not verified in this audit. |
| Codex coding, no grant | fails-in-turn for the default human reviewer | No allowlist or policy override is added. Codex CLI 0.170.0 `exec` forces `never` with its default human reviewer; declined commands are surfaced as failures and cancelled. Other reviewer/config combinations remain native behavior. |
| Cursor coding, trust and coding grants | fails-in-turn | Reviewed `--trust --force` recipe bypasses workspace/tool approval; live denial behavior not verified in this audit. Without the grants, unknown. |
| Cursor read_only | unknown | `--trust --mode ask` requires a separate trust grant; tool approval behavior on a real turn not verified. |
| Claude Code coding, coding grant | unknown | `--permission-mode bypassPermissions` skips permission prompts. Real unattended provider behavior is not verified in this audit. |
| ZCode yolo | unknown | The existing reviewed native evaluator retains always-ask checks. Whether every such check denies headless or parks is not verified; `constrained` remains the honest permission label. |
| ZCode read_only | fails-in-turn | Existing version-pinned 0.17.0 isolated plan profile denies execution/writes; AskUserQuestion/ExitPlanMode fail headless per the prior profile review. This audit does not reverify that real-host behavior. |
| Devin ACP, with or without coding grant | fails-in-turn | Adapter answers every request immediately: allowed `allow_once` only with the frozen grant, otherwise `reject_once` or cancelled. Denials emit attention and prevent successful completion; no unanswered approval RPC. Existing ACP transport fixtures cover both paths; provider execution not verified in this audit. |
| OMP | unknown | No reviewed approval-bypass flag is added. Native approval behavior not verified in this audit. |
| OpenCode coding, coding grant | unknown | OpenCode v2 `--auto` auto-approves permissions that are not explicitly denied; explicit denials remain in force and permissions report constrained. `read_only` is unavailable. Real approval behavior not verified in this audit. |
| OpenClaw local | unknown | Embedded-local structured output does not establish tool approval behavior. No approval flag is inferred; real approvals not verified. |
| Hermes coding | unknown | `--yolo` is added only with the coding grant. It bypasses dangerous-command prompts, not proven isolation or all gateway/session-policy checks; permissions remain constrained. Real approvals not verified. |

No recipe is newly claimed to deny every approval in-turn without real evidence.
The Codex coding grant pins headless denial for commands outside its scoped
allowlist; matching `allow` rules run outside the sandbox as described above.
Those rules are emitted only when the active Git checkout root is explicitly
trusted in Codex's user-owned `config.toml`; a profile grant alone never trusts
a checkout. `read_only` keeps its existing `approval_policy="never"` behavior.
Unknown recipes remain unknown rather than receiving an invented permission flag.

## Ten-minute approval watchdog

`delegation.unattended_coding_permissions` defaults to `false`. Only an explicit
grant adds the Codex allowlist in a private per-launch `CODEX_HOME` overlay.
Existing config and rules remain available, generated rules use a separate filename, and
persistent operator rules are not modified. Fresh homes get a persistent
`sessions` directory so rollout files survive overlay cleanup and later
`codex exec resume` launches. The generated allowlist covers Git status/diff/
log/show/branch and local add/commit operations; Go, Make, Node package manager,
Cargo, pytest, and unittest build/test commands. It does not allow Git push,
clean, or reset. A conflicting operator `forbidden` rule remains effective.

Codex CLI 0.170.0 source confirms that headless `exec` with the default human
reviewer forces `approval_policy=never`; the former `on-request` setting could
not create an interactive approval. The recipe now states `never` explicitly.
If Codex emits `item.completed` with a declined `command_execution`, or returns
its native `approval policy is Never; reject command` denial before creating a
command item, the adapter latches `native_command_declined`. It cancels the run
and reports failure through `delegate --wait`, `await`, and `result`, even if
the model later completes with an explanation.

Codex `--ephemeral` does not persist the rollout used to verify a pre-execution
denial. A delegated launch with `--ephemeral` is therefore rejected before the
child starts with `codex_ephemeral_delegation_unsupported`; the denial guarantee
continues to require persistent rollout evidence. Non-delegated ephemeral
launches remain usable: valid structured completion and typed declined command
items are handled normally, but a pre-execution denial without a command item
cannot be independently verified for those launches. Assistant prose is never
used as a substitute for that missing native evidence.

Codex's `allow` decision skips its sandbox for matching command prefixes. This
means an allowlisted `make test`, package-manager test, or build can execute
checkout-controlled code with the host account's privileges. Both the explicit
`unattended_coding_permissions` grant and Codex's per-checkout trust are
required. Codex stores trust in `CODEX_HOME/config.toml` under
`[projects."<absolute Git root>"]` with `trust_level = "trusted"`; agentctl
checks the canonical active Git root against that user-owned entry. Missing,
untrusted, malformed, or non-Git checkout state produces no generated allow
rules. Other commands remain subject to Codex's workspace sandbox and headless
policy. The Codex rule evaluator keeps an operator `forbidden` decision over an
agentctl `allow`, regardless of file order; this was checked with the installed
`codex execpolicy check` against both rule-file orders. The version-pinned Codex
sources document the headless approval override and the sandbox bypass for an
`allow` decision: [exec harness](https://github.com/openai/codex/blob/rust-v0.170.0/codex-rs/exec/src/lib.rs),
[exec policy evaluator](https://github.com/openai/codex/blob/rust-v0.170.0/codex-rs/core/src/exec_policy.rs),
and [command result events](https://github.com/openai/codex/blob/rust-v0.170.0/codex-rs/core/src/tools/events.rs).

When agentctl receives a native approval/permission attention event, the 10-minute
watch starts at that event's observed time. Stderr health events and unrelated
logs do not reset it; actual running turn activity does. At the boundary the
owner cancels the process and records a failed result with stable diagnostic
`native_approval_wait_timeout`. `delegate --wait` returns the failure directly;
`await` and `result` collect the same terminal failure. A caller-specified
execution `--timeout` remains independent and whichever deadline occurs first
applies. The watchdog duration is fixed at 10 minutes.

Coverage is limited to local native executions whose adapter emits a typed
approval/permission signal. It does not infer approval from silence, detect
authentication/provider/input waits, change Multica authority, widen grants, or
answer native prompts. The existing `native_approval_required` attention
diagnostic and repair action remain in place while the wait is active.

To repair a timeout, inspect `agentctl status <id>`, `agentctl events <id> --kind
attention`, and `agentctl result <id>`. Review the harness policy and the profile's
unattended coding grant. If an unlisted command needs unattended execution,
choose an attended run or add it through a reviewed agentctl policy update; do
not switch to unrestricted sandbox bypass. Retry with a new request key after
the policy is repaired.

The adapter never answers a native prompt on the user's behalf. Multica
attention keeps its existing authority-specific actions. An opaque stall that
emits no approval signal cannot be identified as approval attention.

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
