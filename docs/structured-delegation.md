# Structured delegation

Available in v0.5.0. The parent agent interprets natural language; agentctl
resolves structured constraints against reviewed configuration and constructs the
native invocation. No roles, LLM router, or fuzzy launch matching is required.
Check `agentctl help delegate` before using an older installed binary.

## Caller recipe

1. Extract only the user's explicit harness, model family/version, host, and
   settings. Leave unspecified fields absent. Keep task text in the prompt.
2. Write a request with a stable key for this logical task. Inspect `--plan` when
   useful; it is read-only, can probe the executable version, and reserves nothing.
3. Run with `--wait` to require a successful, nonempty stored final answer. Inspect
   `requested`, `resolved`, and `provenance`. `--content` returns exact answer text.
4. Retry the same key and inputs after an uncertain response. Existing work is
   recovered, never relaunched. Use a new key only for a deliberate new task.

```json
{
  "schema_version": 1,
  "request_key": "review-change-01",
  "selector": {"harness": "cursor", "family": "grok", "version": "4.6"}
}
```

```sh
agentctl delegate --request-file request.json --prompt-file task.md --plan
agentctl delegate --request-file request.json --prompt-file task.md --wait
agentctl delegate --request-file request.json --prompt-file task.md --wait --content
```

`--prompt-stdin` is the alternative prompt source. Prompt files must be regular
non-symlinks within `--cwd` (default: current directory) and at most 8 MiB.
Request files are regular non-symlinks of at most 64 KiB. Unknown, duplicate,
case-aliased keys, nulls, empty strings, control characters, invalid UTF-8,
oversized strings, and trailing JSON fail before probing or launch.
The request schema is [delegate-request](../schemas/delegate-request.schema.json).
Metadata fields are public operational metadata; never put secrets in selectors,
keys, or labels. Only the key digest is stored. Prompt bytes are not journaled.

## Natural language stays with the parent

| User wording | Selector |
| --- | --- |
| `grok` | `{"family":"grok"}` |
| `grok 4.6` | `{"family":"grok","version":"4.6"}` |
| `cursor grok` | `{"harness":"cursor","family":"grok"}` |
| `cursor grok 4.6` | `{"harness":"cursor","family":"grok","version":"4.6"}` |
| `use Grok version 4.6 through Cursor` | `{"harness":"cursor","family":"grok","version":"4.6"}` |

These are interpretation examples, not a permutation dictionary. A version is a
string, never a number or range. Preserve explicit speed/effort in `settings`;
do not fill omitted fields from memory. At least `family` or exact native `model`
is required. Host aliases match exactly; remote delegation currently fails with
a capability diagnostic and a pointer to explicit Multica dispatch.

## Reviewed configuration

Add metadata to the existing profile; there is no separate role registry:

```json
{
  "route": {"this_host": "workstation"},
  "agent_preferences": {
    "mode": "advisory",
    "preferred": [
      {"agent":"cursor","family":"grok","version":"4.6","model":"cursor-grok-4.6-high","speed":"regular","default":true},
      {"agent":"cursor","family":"composer","version":"2.5","model":"composer-2.5","speed":"regular","default":true}
    ]
  }
}
```

This is a profile fragment, not a complete config file. Omitted host or the reserved `local` selector uses this machine;
`route.this_host` optionally supplies its configured name. `adapters.<harness>.executable` selects an
optional exact executable. Existing `use_for` entries with `alias:` provide
explicit family aliases for compatibility. Versions are explicit metadata;
agentctl never infers them from model slugs. Old configurations remain valid.

`delegate` treats preferred entries as its allowlist even when `run` preferences
are advisory. An omitted family/version may remain absent if the matching entry
has no metadata. Explicit constraints must all match; a default only breaks a tie
among compatible entries. Multiple compatible tuples without one unique default
return bounded candidates and `ambiguous_reference`. Case/outer whitespace
normalization is deterministic (native model IDs remain case-sensitive); there is no substring or edit-distance match.
Catalog order never selects a winner. Alias matches cannot discard version,
harness, model, or settings constraints.

Workspace trust is a separate grant: `delegation.cursor_workspace_trust: true`
on the profile adds Cursor `--trust` for both coding and read-only recipes.
Read-only Cursor delegation refuses before launch with `workspace_trust_required`
without this grant, even if the directory was previously trusted. A model alias or
allowlist entry never grants it. Unattended coding permissions are a second
grant: `delegation.unattended_coding_permissions: true` adds Cursor `--force`,
Codex `--dangerously-bypass-approvals-and-sandbox`, Claude Code
`--permission-mode auto`, and grants Devin ACP one-operation permission approvals on coding launches (the default
`settings.access`). `settings.access: "read_only"` uses Cursor `--mode ask`
and Codex `--sandbox read-only -c approval_policy="never"` and never adds those coding permission flags.
ZCode read-only (reviewed for ZCode CLI 0.17.0 only; any other version
refuses with `zcode_read_only_unavailable`) layers every control the CLI
offers, because none is sufficient alone: `--mode plan` (Bash still runs in
plan mode); `--disallowed-tools=` naming every built-in tool that writes,
executes, spawns, messages or reaches the network; a private empty `HOME` per
launch, so user-level hooks, custom commands, plugins and settings never load
(the provider config the host launcher would choose is passed by absolute
path); and a reviewed `<cwd>/.zcode/config.json` turning plugins, MCP,
subagents, skills, memory and hooks off, since ZCode seeds its official plugins
(including a JavaScript REPL exposed as an MCP tool) even into an empty home.
Only Read, Glob, Grep, TodoRead, ReadSessionContext and AskUserQuestion remain;
AskUserQuestion and ExitPlanMode fail headless. Inheritance of `ZCODE_*`, `OTEL_*`,
`NODE_*`, `ELECTRON_*`, `DYLD_*`, `LD_*` and npm config variables is deny by
default: each inherited one is blanked unless agentctl sets it, so nothing can
preload code into ZCode's Electron-as-Node process. Storage, the session DB, logs (except the activity log directory this agentctl process itself created
for the launch; a caller-supplied one is never kept), the mailbox and the XDG directories live inside the
isolated home; messaging and model telemetry are off; `PATH` is system-only and the
temporary directories are inside the home, because Read hands PDFs to Poppler
from `PATH`, which writes temporary files (ZCode bundles its own ripgrep); and the provider config
is copied into the home, since ZCode may rewrite the file it is given while
reading it. The launch refuses
(`zcode_read_only_unavailable`) when the prompt starts with `/` after any
Unicode white space or format character (ZCode expands a leading command, which
can run shell lines before the model); when the executable is not an absolute
path; when the working directory is not a canonical path free of symlinks;
when `zcode.json`, `.zcode/config.json` or `.env` exists in the working
directory or any ancestor (project configs merge from the repository root and
can define hooks; ZCode loads the nearest `.env` upward, which can redirect its
settings); or when `.zcode` is a symlink. The profile is created no-clobber and
no-follow just before launch. After the child is reaped it is moved to an
unused random name and deleted only if it is still the same file (by inode)
with the reviewed bytes; anything else is restored or left in place. The
isolated home is then removed. Known residual: at startup ZCode
runs fixed, read-only `git` queries (rev-parse, status, log, config) to
describe the working directory; prompt text cannot change their arguments, and
the environment disables optional index locks, user and system git config,
terminal prompts and `core.fsmonitor`. A process concurrently rewriting the
working directory or its ancestors during the launch is out of scope. Expert `run`
never applies any of this. Claude Code
(`claude` or `claude-code`) refuses `read_only` with `access_unavailable`: no
reviewed mode guarantees blocking edits and shell writes in `--print`. Coding
launches report `permissions: constrained` with or without the grant: auto mode
retains native classifier checks and can deny tool calls. Without the grant
Claude Code adds no permission flag. The explicit native
`--permission-mode auto` form was verified with Claude Code 2.1.300 on m5-mbp. See the native
[permission-mode contract](https://code.claude.com/docs/en/permission-modes). Devin
refuses `read_only` with `access_unavailable`: ACP read-only enforcement is
unverified. Devin uses `devin acp --model <configured-model>` with stdin prompt
delivery. ACP rejects invalid UTF-8 before admission; it never substitutes JSON replacement characters for task bytes. Its plan records `AGENTCTL_DEVIN_ACP_APPROVE=1` with the grant and `0`
without it. Permission denials emit attention and prevent successful completion.
Harnesses without a reviewed unattended permission recipe are labeled
`permissions: unsupported` and are not given an inferred flag. Expert `run`
never reads either grant. Without an explicit `--adapter`, expert `run` and
fanout infer native adapters, including `generic` / `generic-process`, from
configured executables in the selected profile. If no implicit profile resolves,
built-in executable-name inference still applies; an explicitly requested missing
`--profile` fails.
Exact or resolved paths (including symlinks and PATH lookup) outrank basename
matches; built-in executable names are the fallback. Conflicting equal-strength
matches across harnesses return `run_adapter_ambiguous` before launch; use an
explicit `--adapter` (fanout child `adapter`) to resolve them. Native argv remains
unchanged. Review both grants before enabling them.

## Plans, replay, and results

New plans expose the exact native argv without prompt bytes, delivery mode, adapter
preflight, tuple, configuration SHA-256, and defaulted fields. Plans for an existing
key return the frozen admitted recipe with `source=frozen_admission`; they do not
repeat preflight or claim current eligibility. Plan and wait flags cannot be combined. The nested native
preflight describes prospective launch effects; the plan itself writes no state
and triggers no automatic maintenance. Health probes establish invocation support,
not provider authentication or observed model identity. Admission rechecks support.

The nested preflight also reports the `steer` capability negotiated for that
exact argv. Claude Code is launched with streaming input (`prompt_delivery`
`stream`), so a delegated Claude Code session takes `agentctl steer` without
being interrupted. Codex is steerable by interrupt and resume. Other harnesses
report `steer` unavailable with the reason. See [Steering](steering.md).

After a delegated execution completes, `agentctl continue <id>` sends a
follow-up turn to the same native session using this frozen recipe. The result
of a completed, resumable delegation carries that as a `next_action`. Explicit
`continue --mode carry_forward` starts a fresh round with the prior stored answer
for any eligible native delegation; it never implies native resume. See
[Follow-up turns](follow-up-turns.md).

Request keys are scoped to the selected profile in this host-local journal.
The semantic digest includes the normalized selector, prompt digest, canonical
working directory, requested authority, timeout, and sorted labels. Wait/content
and result assertions are collection options and may change on replay. Changed
semantic inputs with the same key return `conflict`. Configured defaults and
executable changes cannot redirect existing work: replay recovers its frozen
binding before resolving the current catalog. Valid current config is still
required. Concurrent same-key admissions launch at most once. Missing/expired
owner evidence is reported as unknown; it never authorizes another launch.
FAILED/CANCELLED replay reports `replayed: true` and a deliberate new-key retry
action, including in `--plan` and `--wait` errors; it never relaunches.
Deleting the journal also deletes replay protection. The immutable delegation
binding and private prompt-free native recipe are retained in the existing idempotency graph so older supervisors
rewriting execution projections cannot erase it; retention removes both copies.

Native execution is foreground-owned by default. Use `--detach` for a separate
owner that survives parent exit, then `await <id> --content` to collect once.
Detachment does not promise reboot recovery and cannot be combined with `--wait`.
See [the generalized lifecycle contract](generalized-lifecycle.md) for admission,
receipt-loss recovery, and native/Multica boundaries. Without `--detach`, even
without `--wait`, the initial call
owns the native run until exit; a concurrent replay can return a running receipt.
`--wait` additionally enforces successful completion, retrieves content, and marks
collection only after delivery. It fails on attention, failure, cancellation,
unknown state, unavailable content, or an empty answer. Optional assertions are
`--require-result-source <source>` and `--min-result-bytes <n>`. A returned answer
is not proof of task correctness or external acceptance. Stored results remain
bounded by the existing native result contract.

`--timeout <duration>` bounds a new execution; no timeout is implied. Existing
runs retain their original deadline. `--label <name>` may repeat up to 16 distinct
labels. `--authority` accepts `auto`, `native`, or `multica`; this release supports
native local launches only. Task contracts and external
`--context-file` are not yet supported by this command. Use live help for the
existing expert `run`, `fanout`, or `dispatch` interfaces when those are needed;
do not automatically weaken a failed structured request into an expert launch.

## Capacity pools

A pool lets `delegate` choose an account from plan usage, so a caller can ask for
work rather than for a model. Request it with `{"pool":"work"}` in place of a
family or model. A request naming a family or model never takes a pool, and pool
requests refuse harness, family, version, model, speed and effort.

agentctl never measures usage or calls a provider. Another tool publishes a facts
file (`schemas/capacity-facts.schema.json`), and agentctl reads it at launch:

```json
{"schema_version":1,"generated_at":"2026-11-01T09:00:00Z","accounts":[
  {"id":"account-a","windows":[{"name":"weekly","used_ratio":0.4,"resets_at":"2026-11-05T00:00:00Z","observed_at":"2026-11-01T08:35:00Z"}]}
]}
```

Pools live in the profile. A candidate must also resolve through
`agent_preferences`, so a pool can't widen the reviewed allowlist. `executable`
selects that account's launcher, which owns its authentication:

```json
{"capacity":{"file":"/var/lib/capacity.json","max_age_seconds":3600,"switch_threshold":0.95,"pools":{
  "work":{"mode":"balance","candidates":[
    {"harness":"codex","model":"example-model","account":"account-a"},
    {"harness":"codex","model":"example-model","account":"account-b","executable":"/usr/local/bin/codex-b"}
  ]}
}}}
```

- A candidate is eligible below its threshold: its own `switch_threshold`, else
  the profile's, else 0.95. `cutoff_exempt` keeps it eligible at any usage, for an
  account meant to run past its allowance.
- `windows` names the windows that limit a candidate; absent means all of them. A
  window past its `resets_at` counts as empty, and one whose `observed_at` is older
  than `max_age_seconds` is ignored.
- `priority` (the default) takes the first eligible candidate, in order.
- `balance` takes the lowest used ratio plus `launch_debit` (default 0.02) for each
  launch this journal still has running or started within `max_age_seconds`, which
  spreads a fan-out while facts lag. Ties go to the soonest reset, then to order.

A pool doesn't block work on a number that may be stale. Missing, stale or invalid
facts use configured order and warn `capacity_facts_unusable`; `balance` still
spreads by recent launches. If every candidate is over its threshold, the least
used one runs and warns `capacity_all_over_threshold`. The refusals are an unknown
pool name (`delegate_unknown_pool`) and no candidate matching the allowlist
(`delegate_pool_no_candidate`).

Plans, results and the execution record carry `capacity`: the pool, account,
reason, facts status and age, and each candidate as it looked. A plan's choice is
advisory, since launch chooses again. Replaying a request key returns the original
choice, and `continue` keeps the frozen launch plan, so it stays on the account
holding the session. The stored selector is the resolved harness and model, so an
older binary can still read the record after a rollback.

Known limits: recent launches come from this journal, so other hosts' launches
appear only once the facts catch up. A provider limit error doesn't bench an
account, because those failures are attributed to a harness rather than an account
today; the recorded account makes that possible later. Multica delegation remains
refused.

## Native and Multica boundaries

Cursor, Codex, and Claude Code have recipes for exact requested models and
supported settings. Claude Code passes effort through `--effort` for `low`,
`medium`, `high`, `xhigh`, and `max`; it still has no reviewed service-speed
flag. Its `settings.auto_compact_window` is a launch setting, not a catalog
attribute: it never selects an entry, and other harnesses refuse it. OMP has a structured recipe with explicit thinking but no service-speed
guarantee.
An unsupported setting fails before launch. The resolved tuple describes what
agentctl requested. Native providers can still report opaque or substituted model
identity; this release does not claim model attestation.

Multica remains the authority for durable remote tasks. Its current adapter lacks
a verified final-answer route and settings contract, so `delegate` rejects remote
and explicit Multica requests before creating anything. Existing `dispatch`
retains live exact assignee/runtime binding, authority idempotency, and lifecycle
tracking; there is no local fallback. Uniform remote delegation requires those
missing capabilities first.

## Errors and discovery

All errors use the existing envelope and stable `diagnostic_code` where supplied
by delegate; resolver and recipe diagnostics distinguish constraint failures.
Known execution IDs are retained. Exit classes include usage (2), missing content
(3), ambiguity (4), unavailable capability (5), authorization (7), conflict or
invalid result state (8), attention (10), execution failure (11), cancellation
(12), and unknown outcome (13). See the [execution contract](execution-envelope.md) for the full contract.

The request and result schemas, `help delegate`, and embedded portable skill ship
together. `agentctl bootstrap update` reconciles the installed guidance through
existing managed ownership rules. The design is tracked in
[issue 51](https://github.com/Git-on-my-level/agentctl/issues/51).

## Actionable validation and static compatibility

Request errors include a stable violation, bounded sanitized `field_path`,
sorted `allowed_keys` for the containing object, and an `example_request` with
explicit placeholders. Unknown keys outside the bounded ASCII identifier syntax
are replaced with `<unknown>`. Values and prompt text are never echoed. The
example requires a new logical-task key and a reviewed configured model; it is
not a default model selection.

No-match/ambiguity errors include at most 32 sorted, deduplicated reviewed
catalog candidates, the full `candidate_count`, `candidates_truncated`,
`candidate_selectors`, and names of supplied constraint fields. Selectors
reproduce nonempty catalog constraints; overlapping catalog entries can still
be ambiguous. Nothing is selected or launched automatically. Preserve explicit
user constraints and inspect the catalog rather than weakening a failed request.

Prompt-root repair instructions follow the originating command. `delegate`,
`continue`, `steer`, and `dispatch` accept piped `--prompt-stdin` and do not accept
expert `run`'s `--prompt-delivery` flag. `run` repairs preserve native delivery.
Fanout errors identify the shared or child manifest `prompt_file` to repair
within the manifest directory. Root confinement and symlink checks remain intact.

`config doctor` and `config bundle validate` separately expose `syntax_valid`
and static `launch_recipes` compatibility. Existing `valid` retains its
syntax/composition/provenance meaning. Doctor emits the selected profile report;
bundle validation emits reports keyed by profile. A provenance failure retains
the partial doctor report at `error.details.report` with its existing error exit.
See [launch-recipe-report](../schemas/launch-recipe-report.schema.json).
`config doctor --live` adds the explicit native catalog check described in
[configuration](configuration.md).

Recipe compatibility tests exact configured harness/model/settings and aliases
without executing binaries, contacting providers or resolving Multica assignees.
`runtime_verified` is always false. Request-specific settings can still be
unsupported even when the configured default passes. Native runtime, account,
model availability, authentication, and Multica readiness are separate checks.
Devin passes exact configured model IDs instead of a frozen version list, while
continuing to refuse fast/priority variants and unsafe argument forms. Claude
Code's documented native `[1m]` suffix is preserved only for that adapter; other
harnesses retain their reviewed bracket grammar. Neither change remaps models.

ZCode's host-managed launcher accepts `ZCODE_MODEL=glm-5.3` or
`ZCODE_MODEL=glm-5.3-flash`. The Flash recipe selects its provisioned
provider config through this environment value; the GLM-5.3 recipe preserves
the existing configured default by emitting `ZCODE_MODEL=` to clear any inherited
selector. Launch values replace inherited entries. It adds no native `--model`
flag. Provision the Flash config with `zcode-cli-provision` before selecting
Flash; GLM-5.3 needs no per-model variant.
Plans expose these reviewed, non-secret values in `native.env`; the immutable
admission retains them for retries and follow-up turns. Arbitrary caller env
and credentials are not accepted through this recipe mechanism.
ZCode effort is inspected at preflight from the selected provider config's
`config.defaultModelSelection.modelId` and `options.reasoningLevel`. Supported
levels come from matching bundled `config.modelConfigRules.modelRules` entries'
`config.optionSpecs.reasoningLevel.values`, with matching personal
`providerModelRules` / `manualProviderModelRules` taking precedence. Model regexes
match the whole ID without case sensitivity; later matching rules override earlier
ones. No supported-level list is hardcoded. Unreadable metadata, no matching rule,
or an absent, empty or malformed final list makes effort unknown and cannot satisfy
a required effort. The reader covers the launcher's reviewed GLM-5.3 and Flash
model IDs; other selected models and unlisted levels are unknown. The base honors an inherited
`ZCODE_PERSONAL_PROVIDER_CONFIG_FILE`; named profiles override it, and read-only
launches ignore it, matching the isolated adapter. Bundled metadata follows
`ZCODE_BUILTIN_PROVIDER_CONFIG_FILE` or the `ZCODE_APP` bundle path; read-only
isolation ignores both overrides and uses the standard bundle.
`resolved.settings.effort`
records that observation, never an assumed high or an echoed request. Missing,
unreadable, malformed or unrecognized levels omit `effort` and add a fixed
`effort_warning` explaining that effort is unknown and the runtime default applies.
Missing `options.reasoningLevel` stays unknown: the bundled CLI owns the runtime
default, which is not inferred from legacy `defaultVariant` configuration. The
diagnostic names the missing field and directs operators to `zcode-cli-provision`
to record an explicit supported level.
An explicit or configured effort requirement that differs (including unknown)
refuses before launch with `zcode_effort_mismatch` and exit 5 (capability unavailable).
Follow-up turns inspect the profile again; replay retains the original observation.
Only whitelisted effort tokens and fixed diagnostics leave the reader; credentials,
config contents and raw parse/file errors are never recorded. This is a local
profile observation, not runtime/provider attestation, and cannot detect concurrent
profile edits or later native overrides. Static config compatibility does not read
ZCode profiles. `fast` service speed remains unsupported.

OpenCode recipes pass `opencode run --format json --model provider/model --`
with the exact configured model ID and positional prompt delivery. Explicit
service speed, effort, and read-only access are refused because no reviewed
recipe guarantees those settings. No auto-approval flags are added. A native
answer requires completed text from the final step, a final `step_finish` with
`part.reason: "stop"`, and exit 0. Missing/non-stop finishes fail rather than
returning partial text. Native resume and steer remain unavailable; explicit carry-forward rounds
use a fresh launch. Multica
structured delegation remains rejected under the existing authority contract.

### OpenClaw local delegation

A configured `openclaw` preference resolves to
`openclaw agent --local --agent main --model provider/model --json -m <prompt>`.
Dash-leading prompts remain the exact `-m` value without an inserted `--`.
Nonblank answer payloads retain their whitespace and are joined with two newlines;
whitespace-only payloads are skipped.
The native adapter supplies a fresh session key unless one is explicit. Only
nonempty `payloads[].text` from a successful process is result content; nonzero
exits, error envelopes, and empty answers fail. Fast service speed and read-only
access are refused; reviewed thinking levels are low, medium, and high. Coding
permission handling uses the common recipe contract with no inferred bypass flag.
A plan and version probe do not verify provider authentication. Attach, resume,
and steering have no verified route. Multica delegation remains unsupported;
explicit dispatch still requires an independently advertised Multica runtime and
retains its lifecycle-only contract. No local adapter proves remote availability.

ZCode 0.16.9 documents `--mode yolo` as its prompt permission mode. Its native
evaluator bypasses ordinary permission prompts but checks always-ask tools
first, so this existing argv reports `permissions: constrained` with or without
the coding grant. This label does not assert sandbox isolation or change argv.

## Unattended approval audit and Hermes

Codex delegate recipes add `--skip-git-repo-check` by default, including coding
without the permission grant. This skips repository admission only; it does not
grant tools or remove the read-only sandbox. Older recipes and expert `run`
classify the exact native trust failure as `codex_git_repo_check_required`.
Read-only Codex also sets `approval_policy="never"`, so denied commands return
to the native turn. See the [approval audit](unattended-approvals.md) for the
per-recipe classifications and evidence limits.

A configured `hermes` preference supports exact native model IDs, including
`{"harness":"hermes","model":"glm-5.3-flash"}`. It resolves through the same
preferred-entry allowlist as every other harness; it does not add model defaults.
The recipe is `hermes chat --format stream-json --model MODEL --query-file -`,
with optional `--reasoning` and grant-gated `--yolo`. Permissions are constrained;
read_only and explicit service speed are refused. Managed stdin closes after the
single query. Result extraction requires a successful terminal result and process
exit. `continue` drops creation overrides on `--resume`; provider-backed resume
is not verified and reported degraded. Live steering is unavailable. This native
adapter does not establish a Multica runtime or change Multica dispatch policy.
