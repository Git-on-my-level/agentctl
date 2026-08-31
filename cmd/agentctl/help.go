package main

import (
	"sort"
	"strings"

	"github.com/Git-on-my-level/agentctl/internal/output"
)

type commandHelp struct {
	Name            string                 `json:"name"`
	Summary         string                 `json:"summary"`
	Usage           string                 `json:"usage"`
	SideEffectClass output.SideEffectClass `json:"side_effect_class"`
	Defaults        []string               `json:"defaults"`
	Examples        [][]string             `json:"examples"`
	Related         []string               `json:"related_topics"`
}

func helpTopics() map[string]commandHelp {
	return map[string]commandHelp{
		"doctor": {
			Name: "doctor", Summary: "Answer whether detected agents can launch, be observed, and return results.", Usage: "agentctl doctor [--adapter name] [--static] [--full]",
			SideEffectClass: output.ReadOnly, Defaults: []string{"checks bootstrap, journal, configuration, supervisor, and detected adapters", "live capability probes are enabled"},
			Examples: [][]string{{"agentctl", "doctor"}, {"agentctl", "doctor", "--adapter", "cursor"}}, Related: []string{"bootstrap", "capabilities", "run"},
		},
		"bootstrap": {
			Name: "bootstrap", Summary: "Inspect or reconcile agentctl across detected local harnesses.", Usage: "agentctl bootstrap status|update [flags]",
			SideEffectClass: output.LocalOperationalWrite, Defaults: []string{"status is read-only and reports instruction pointer path, state, digest, and revision per harness", "update detects supported harnesses and canonical roots", "update reconciles pointers only in existing documented user-global instruction files", "update refuses drifted or duplicate managed assets"},
			Examples: [][]string{{"agentctl", "bootstrap", "status"}, {"agentctl", "bootstrap", "update", "--dry-run"}, {"agentctl", "bootstrap", "update"}}, Related: []string{"bootstrap update", "doctor"},
		},
		"bootstrap update": {
			Name: "bootstrap update", Summary: "Reconcile the embedded portable skill and delegation pointers for detected harnesses.", Usage: "agentctl bootstrap update [--dry-run] [--harness names] [--home path] [--target-dir path]",
			SideEffectClass: output.LocalOperationalWrite, Defaults: []string{"all detected harnesses", "canonical skill roots", "managed skill upgrades and missing installs", "instruction pointers update only marked blocks in existing documented user-global files", "missing instruction files are reported but never created", "no legacy-copy deletion", "no new supervisor service"},
			Examples: [][]string{{"agentctl", "bootstrap", "update", "--dry-run"}, {"agentctl", "bootstrap", "update"}, {"agentctl", "bootstrap", "update", "--harness", "cursor"}}, Related: []string{"bootstrap", "doctor"},
		},
		"capabilities": {
			Name: "capabilities", Summary: "Return a concise live viability projection for one adapter.", Usage: "agentctl capabilities <adapter> [--require names] [--full] [--static] [--executable path]",
			SideEffectClass: output.ReadOnly, Defaults: []string{"concise summary", "live probe", "all capabilities when --require is omitted"},
			Examples: [][]string{{"agentctl", "capabilities", "codex"}, {"agentctl", "capabilities", "cursor", "--require", "launch,result_content"}}, Related: []string{"doctor", "run"},
		},
		"orient": {
			Name: "orient", Summary: "Describe the current worktree, configured authorities, static adapter health, and workspace-scoped executions in one read-only snapshot.", Usage: "agentctl orient [--limit n]",
			SideEffectClass: output.ReadOnly, Defaults: []string{"current working directory", "at most 10 workspace-matched execution records total", "newest active executions first, then newest terminal executions within the remaining limit", "no Git fetch", "no adapter launch or remote authentication probe", "journal opens read-only and an absent journal is reported as unconfigured", "raw prompts, transcripts, results, and per-execution paths omitted", "static adapter health proves only local executable availability"},
			Examples: [][]string{{"agentctl", "orient"}, {"agentctl", "orient", "--limit", "25"}}, Related: []string{"doctor", "recent", "route"},
		},
		"run": {
			Name: "run", Summary: "Launch an exact native argv and persist its normalized execution and terminal result.", Usage: "agentctl run [--execution-id exec-...] [--label name ...] [--task-contract json-file] [--background] [--adapter name] [--timeout duration|--no-timeout] [--prompt-file path|--prompt-stdin] [--prompt-delivery argv|stdin] [flags] -- <native argv>",
			SideEffectClass: output.ExternalSideEffect, Defaults: []string{"adapter inferred from known executable names", "execution ID generated unless preallocated", "foreground ownership unless --background is explicit", "background launches use a detached noninteractive host-local worker and return after the execution is journaled", "background workers survive the caller but not a host restart", "direct background execution has no cross-process cancel path unless the adapter advertises one", "background prompt stdin is materialized through a one-shot parent-to-worker pipe", "background cannot use idempotency keys", "up to 16 exact metadata labels", "task contracts are bounded strict JSON metadata retained by status and result", "native completion never implies external acceptance", "Multica issues remain authoritative for Multica task contracts", "no wall-clock timeout", "prompt delivery argv when a prompt source is selected", "prompt files are regular non-symlinks within --cwd and at most 8 MiB", "invocation-scoped launch and result_content preflight", "bounded final result storage", "Cursor plan mode rejected because its one-shot completion is unreliable"},
			Examples: [][]string{{"agentctl", "run", "--", "codex", "exec", "--json", "review this change"}, {"agentctl", "run", "--task-contract", "task-contract.json", "--", "codex", "exec", "--json", "perform the bounded task"}, {"agentctl", "run", "--background", "--label", "review", "--prompt-file", "task.md", "--prompt-delivery", "argv", "--", "cursor-agent", "--print", "--output-format", "stream-json", "--trust"}, {"agentctl", "run", "--prompt-stdin", "--prompt-delivery", "stdin", "--", "codex", "exec", "--json", "-"}}, Related: []string{"recent", "fanout", "capabilities", "await", "result", "subscribe"},
		},
		"dispatch": {
			Name: "dispatch", Summary: "Create or recover routed Multica work and persist a tracked execution handle.", Usage: "agentctl dispatch --route selector --title title (--prompt-file path|--prompt-stdin) --idempotency-key key [--project ID] [--status status] [--label name ...] [--plan]",
			SideEffectClass: output.RemoteCoordinationWrite, Defaults: []string{"live agent and runtime resolution from the configured Multica workspace", "exact routed host, adapter, and concrete model required", "unarchived idle or working agent and online runtime required", "exact assignee and runtime reserved in a local starting execution before the remote call", "replay recovers the prepared target without mutable fleet resolution", "assigned backlog creation is bound locally before todo activation unless --status is explicit", "status must be one of Multica's seven lifecycle statuses", "caller idempotency key required", "prompt delivered only on Multica stdin and never journaled", "returns a Multica-authority exec ID", "no local fallback", "--plan performs live read-only resolution without creating an issue or journal record"},
			Examples: [][]string{{"agentctl", "dispatch", "--route", "m5 sol", "--title", "Review the release", "--prompt-file", "task.md", "--idempotency-key", "release-review-v1", "--plan"}, {"agentctl", "dispatch", "--route", "m5 sol", "--title", "Review the release", "--prompt-stdin", "--idempotency-key", "release-review-v1"}}, Related: []string{"route", "await", "subscribe", "recent", "promote"},
		},
		"recent":    {Name: "recent", Summary: "Discover recent host-local executions without reading prompt or result records.", Usage: "agentctl recent [--limit n] [--state state|terminal|nonterminal] [--liveness value] [--unreconciled] [--adapter name] [--label name ...]", SideEffectClass: output.ReadOnly, Defaults: []string{"newest 20 executions", "host-local journal only", "state and liveness are independent exact filters", "repeated exact label filters use AND semantics", "--unreconciled is terminal work whose result has never been acknowledged", "terminals that predate acknowledgement tracking are not unreconciled", "paths, prompts, transcripts, and results omitted"}, Examples: [][]string{{"agentctl", "recent"}, {"agentctl", "recent", "--state", "nonterminal", "--liveness", "alive", "--label", "review"}, {"agentctl", "recent", "--liveness", "unreachable"}, {"agentctl", "recent", "--unreconciled"}}, Related: []string{"status", "await", "result"}},
		"inbox":     {Name: "inbox", Summary: "Explain host-local executions that need collection, intervention, integrity reconciliation, or bounded stale-work review.", Usage: "agentctl inbox [--limit n] [--stale-after duration] [--adapter name] [--label name ...]", SideEffectClass: output.ReadOnly, Defaults: []string{"newest 20 actionable executions", "one hour stale-observation bound", "host-local journal only", "terminal failures leave the inbox when result or terminal await acknowledges collection", "conflicted evidence remains actionable after collection", "work health and tool liveness are reported separately", "no adapter probe, native status refresh, result read, or acknowledgement write", "paths, prompts, transcripts, and results omitted"}, Examples: [][]string{{"agentctl", "inbox"}, {"agentctl", "inbox", "--stale-after", "2h", "--label", "review"}}, Related: []string{"recent", "status", "await", "result"}},
		"workspace": {Name: "workspace", Summary: "Report nonterminal executions launched from Git worktrees.", Usage: "agentctl workspace owners [--path directory]", SideEffectClass: output.ReadOnly, Defaults: []string{"host-local journal only", "all attributed nonterminal executions when --path is omitted", "exact Git worktree match when --path is supplied", "launch-time provenance, never an exclusive lock", "older nonterminal rows without workspace metadata make evidence_complete false"}, Examples: [][]string{{"agentctl", "workspace", "owners"}, {"agentctl", "workspace", "owners", "--path", "/absolute/worktree"}}, Related: []string{"recent", "status", "schema"}},
		"fanout": {
			Name: "fanout", Summary: "Run one prompt through several explicit native argv vectors with bounded concurrency.", Usage: "agentctl fanout --manifest path [--concurrency n] [--fail-fast] [--plan]",
			SideEffectClass: output.ExternalSideEffect, Defaults: []string{"manifest v1 requires schema_version, prompt_file, and children[].argv", "prompt_file and relative children[].cwd resolve from the manifest directory", "a child without cwd inherits the invoking process directory", "prompt_delivery is argv or stdin globally or per child", "foreground same-process coordination", "concurrency 2", "one prompt file read once", "per-child execution IDs and results", "no automatic synthesis or detached ownership"},
			Examples: [][]string{{"agentctl", "schema", "list"}, {"agentctl", "fanout", "--plan", "--manifest", "fanout.json"}, {"agentctl", "fanout", "--manifest", "fanout.json"}}, Related: []string{"schema", "run", "await", "result"},
		},
		"await": {
			Name: "await", Summary: "Wait for terminal state or attention without polling native session files.", Usage: "agentctl await <execution-id> [--timeout duration|--no-timeout|--through-execution-deadline] [--ignore-attention]",
			SideEffectClass: output.LocalOperationalWrite, Defaults: []string{"10 minute timeout", "returns immediately when attention is required", "Multica-authority executions are refreshed from durable workspace events while awaiting", "two consecutive retryable Multica refresh failures are tolerated; the third returns its diagnostic", "a terminal return acknowledges the execution so it is no longer unreconciled"},
			Examples: [][]string{{"agentctl", "await", "exec-..."}, {"agentctl", "await", "exec-...", "--through-execution-deadline"}, {"agentctl", "await", "exec-...", "--no-timeout", "--ignore-attention"}}, Related: []string{"status", "events", "result"},
		},
		"result": {
			Name: "result", Summary: "Dereference and optionally assert guarantees on a bounded terminal result.", Usage: "agentctl result <execution-id> [--content|--summary] [--allow-empty] [--require-result-source source] [--min-result-bytes n]",
			SideEffectClass: output.LocalOperationalWrite, Defaults: []string{"stored content or a structured failure is required", "--content writes exact stored UTF-8 text with no envelope and no added newline", "integrity conflicts fail closed", "a successful dereference acknowledges the execution so it is no longer unreconciled"},
			Examples: [][]string{{"agentctl", "result", "exec-..."}, {"agentctl", "result", "exec-...", "--content"}, {"agentctl", "result", "exec-...", "--require-result-source", "assistant", "--min-result-bytes", "200"}}, Related: []string{"run", "await", "events", "recent"},
		},
		"subscribe": {
			Name: "subscribe", Summary: "Create durable at-least-once callback delivery for execution events.", Usage: "agentctl subscribe create --execution ID --destination file|webhook --target target [--authority direct|multica] [--kind kind] [--ttl duration] [--keep-after-terminal] | list | show ID | cancel ID",
			SideEffectClass: output.LocalOperationalWrite, Defaults: []string{"create filters terminal, attention, and artifact events", "24 hour TTL", "expires after acknowledged terminal delivery"},
			Examples: [][]string{{"agentctl", "subscribe", "create", "--execution", "exec-...", "--destination", "file", "--target", "/absolute/events.ndjson"}}, Related: []string{"run", "events", "supervisor"},
		},
		"status":        {Name: "status", Summary: "Read one normalized execution envelope.", Usage: "agentctl status <execution-id>", SideEffectClass: output.ReadOnly, Defaults: []string{"native paths, prompts, and transcripts remain redacted"}, Examples: [][]string{{"agentctl", "status", "exec-..."}}, Related: []string{"events", "await", "result"}},
		"events":        {Name: "events", Summary: "Read bounded normalized event history.", Usage: "agentctl events <execution-id> [--after-sequence n] [--limit n] [--kind kind]", SideEffectClass: output.ReadOnly, Defaults: []string{"after sequence 0", "limit 100", "all semantic event kinds"}, Examples: [][]string{{"agentctl", "events", "exec-..."}, {"agentctl", "events", "exec-...", "--kind", "terminal"}}, Related: []string{"status", "subscribe", "await"}},
		"promote":       {Name: "promote", Summary: "Explicitly create or recover a Multica issue for a direct execution.", Usage: "agentctl promote <execution-id> --title title [--handoff-file path] [--plan]", SideEffectClass: output.RemoteCoordinationWrite, Defaults: []string{"performs the remote mutation", "idempotent for identical semantics"}, Examples: [][]string{{"agentctl", "promote", "exec-...", "--title", "Durable follow-up", "--plan"}}, Related: []string{"route", "config"}},
		"route":         {Name: "route", Summary: "Explain ranked host/model matches from a short selector; never launch or dispatch work.", Usage: "agentctl route explain <selector...>", SideEffectClass: output.ReadOnly, Defaults: []string{"a non-empty short selector ranks reviewed host/model aliases", "task prose and lifecycle intent are not inferred", "remote advice does not verify a Multica assignee/runtime or create an exec id", "this_host must be configured before local versus remote placement is claimed"}, Examples: [][]string{{"agentctl", "route", "explain", "--", "studio", "omp"}}, Related: []string{"run", "dispatch", "promote"}},
		"supervisor":    {Name: "supervisor", Summary: "Run, inspect, or plan the host-local callback supervisor.", Usage: "agentctl supervisor run|status|plan ...", SideEffectClass: output.LocalOperationalWrite, Defaults: []string{"Unix socket only", "new service installation remains explicit"}, Examples: [][]string{{"agentctl", "supervisor", "status"}}, Related: []string{"subscribe", "bootstrap"}},
		"update":        {Name: "update", Summary: "Inspect or control invocation-triggered agentctl release updates.", Usage: "agentctl update status|now|policy auto|notify|off", SideEffectClass: output.LocalOperationalWrite, Defaults: []string{"auto policy", "first work-creating invocation per UTC day starts a detached short-lived worker", "read-only commands never trigger automatic maintenance", "managed packaged installations only", "status is read-only", "notify emits one daily update warning on eligible invocations", "off performs no release check"}, Examples: [][]string{{"agentctl", "update", "status"}, {"agentctl", "update", "policy", "notify"}, {"agentctl", "update", "now"}}, Related: []string{"bootstrap", "doctor"}},
		"config":        {Name: "config", Summary: "Manage executable provenance expectations, advisory agent preferences, optional authority, and explicit local or Git-backed configuration.", Usage: "agentctl config set-profile|show|validate|doctor|bundle|source ...", SideEffectClass: output.LocalOperationalWrite, Defaults: []string{"agent preferences are advisory", "no implicit unauthenticated Multica profile", "Git sources are explicit and fast-forward only", "native run argv remains caller-authoritative"}, Examples: [][]string{{"agentctl", "config", "show"}, {"agentctl", "config", "source", "status"}, {"agentctl", "--config-bundle", "/local/config-bundle.json", "config", "bundle", "plan"}}, Related: []string{"config source", "doctor", "promote"}},
		"config source": {Name: "config source", Summary: "Materialize a reviewed Git config bundle into the owner-only live config.", Usage: "agentctl config source init --remote URL [--ref main] [--bundle config-bundle.json] [--checkout path] [--plan] | status | update [--plan] | restore [--plan]", SideEffectClass: output.LocalOperationalWrite, Defaults: []string{"status and --plan perform no fetch", "first init may add missing fields but never replace existing values", "update fetches noninteractively and accepts fast-forwards only", "restore repairs only the live config from the pinned clean checkout", "dirty checkouts and unreviewed history fail closed", "Git and SSH own credentials"}, Examples: [][]string{{"agentctl", "config", "source", "init", "--remote", "git@github.com:owner/agentctl-config.git", "--plan"}, {"agentctl", "config", "source", "update"}, {"agentctl", "config", "source", "status"}}, Related: []string{"config", "doctor"}},
		"skills":        {Name: "skills", Summary: "Fetch a config-selected Skill Hub pack, reconcile clean managed copies, and preserve local drift for review.", Usage: "agentctl skills plan|status|doctor|update|reconcile|diff|restore|propose ...", SideEffectClass: output.LocalOperationalWrite, Defaults: []string{"plan, status, doctor, update --plan, diff, restore plan, and propose plan are read-only", "Skill Hub updates are fast-forward only", "auto-clean replaces only unchanged marker-owned copies", "drift and unmanaged collisions are preserved", "agentctl-portable is bootstrap-owned and reserved", "publishing a proposal is always separate"}, Examples: [][]string{{"agentctl", "skills", "update", "--plan"}, {"agentctl", "skills", "update"}, {"agentctl", "skills", "diff", "example-skill"}, {"agentctl", "skills", "propose", "example-skill"}}, Related: []string{"config source", "bootstrap update", "update", "doctor"}},
		"data":          {Name: "data", Summary: "Inventory local journal data and explicitly plan retention cleanup.", Usage: "agentctl data inventory | agentctl data cleanup --before timestamp --plan | --apply --plan-digest digest", SideEffectClass: output.LocalOperationalWrite, Defaults: []string{"inventory and cleanup planning are read-only", "no automatic retention", "cleanup apply requires the reviewed plan digest"}, Examples: [][]string{{"agentctl", "data", "inventory"}, {"agentctl", "data", "cleanup", "--before", "2026-07-01T00:00:00Z", "--plan"}}, Related: []string{"data cleanup", "result"}},
		"data cleanup":  {Name: "data cleanup", Summary: "Plan or atomically remove unreferenced terminal execution graphs before a cutoff.", Usage: "agentctl data cleanup --before RFC3339 --plan | agentctl data cleanup --before RFC3339 --apply --plan-digest sha256:...", SideEffectClass: output.LocalOperationalWrite, Defaults: []string{"no deletion without --apply and a matching reviewed plan digest", "nonterminal and referenced graphs are protected", "physical journal compaction is not automatic"}, Examples: [][]string{{"agentctl", "data", "cleanup", "--before", "2026-07-01T00:00:00Z", "--plan"}, {"agentctl", "data", "cleanup", "--before", "2026-07-01T00:00:00Z", "--apply", "--plan-digest", "sha256:..."}}, Related: []string{"data", "result"}},
		"knowledge":     {Name: "knowledge", Summary: "Validate, synchronize, compile, or verify deterministic knowledge bundles.", Usage: "agentctl knowledge validate|sync|compile|verify ...", SideEffectClass: output.LocalOperationalWrite, Defaults: []string{"validation and sync planning do not fetch Git", "credentials and harness state are forbidden"}, Examples: [][]string{{"agentctl", "knowledge", "validate", "--source", "source.json"}}, Related: []string{"context"}},
		"context":       {Name: "context", Summary: "Select deterministic task context from a verified bundle.", Usage: "agentctl context --bundle path --repository path --task-kind kind --query query [--render path]", SideEffectClass: output.LocalOperationalWrite, Defaults: []string{"read-only unless --render is supplied"}, Examples: [][]string{{"agentctl", "context", "--bundle", "/bundle", "--query", "deployment"}}, Related: []string{"knowledge"}},
		"attach":        {Name: "attach", Summary: "Resolve a portable URI through reviewed local bindings.", Usage: "agentctl attach <portable-uri>", SideEffectClass: output.ReadOnly, Defaults: []string{"foreign hosts fail closed", "authority state is never guessed"}, Related: []string{"status"}},
		"cancel":        {Name: "cancel", Summary: "Request cancellation only when the adapter advertises durable cancel authority.", Usage: "agentctl cancel <execution-id>", SideEffectClass: output.ExternalSideEffect, Defaults: []string{"never guesses from a PID"}, Related: []string{"status", "capabilities"}},
		"id":            {Name: "id", Summary: "Generate or inspect typed six-word identifiers.", Usage: "agentctl id generate <type> | agentctl id inspect <id>", SideEffectClass: output.ReadOnly, Defaults: []string{"full typed IDs are mutation targets"}, Examples: [][]string{{"agentctl", "id", "generate", "exec"}}},
		"schema":        {Name: "schema", Summary: "List normative JSON schema files.", Usage: "agentctl schema list", SideEffectClass: output.ReadOnly, Defaults: []string{}, Examples: [][]string{{"agentctl", "schema", "list"}}},
	}
}

func (a *app) help(renderer output.Renderer, rawTopic string) int {
	topic := strings.ToLower(strings.Join(strings.Fields(rawTopic), " "))
	topics := helpTopics()
	if topic != "" {
		aliases := map[string]string{
			"list":             "recent",
			"workspace owners": "workspace",
			"bootstrap status": "bootstrap", "route explain": "route",
			"subscribe create": "subscribe", "subscribe list": "subscribe", "subscribe show": "subscribe", "subscribe cancel": "subscribe",
			"supervisor run": "supervisor", "supervisor status": "supervisor", "supervisor plan": "supervisor",
			"update status": "update", "update now": "update", "update policy": "update",
			"config set-profile": "config", "config show": "config", "config validate": "config", "config doctor": "config", "config bundle": "config", "config bundle validate": "config", "config bundle show": "config", "config bundle plan": "config", "config source init": "config source", "config source status": "config source", "config source update": "config source", "config source restore": "config source", "skills plan": "skills", "skills status": "skills", "skills doctor": "skills", "skills update": "skills", "skills reconcile": "skills", "skills diff": "skills", "skills restore": "skills", "skills propose": "skills",
			"data inventory":     "data",
			"knowledge validate": "knowledge", "knowledge sync": "knowledge", "knowledge compile": "knowledge", "knowledge verify": "knowledge",
		}
		if canonical, ok := aliases[topic]; ok {
			topic = canonical
		}
		item, ok := topics[topic]
		if !ok {
			return a.fail(renderer, output.NewError(output.CodeNotFound, "help topic not found", false).WithDetail("topic", topic))
		}
		lines := []output.Line{{Lead: "help", Fields: []output.Field{{Name: "topic", Value: item.Name}, {Name: "summary", Value: item.Summary}}}, {Lead: "usage", Fields: []output.Field{{Name: "argv", Value: item.Usage}}}}
		for _, value := range item.Defaults {
			lines = append(lines, output.Line{Lead: "default", Fields: []output.Field{{Name: "behavior", Value: value}}})
		}
		for _, argv := range item.Examples {
			lines = append(lines, output.Line{Lead: "example", Fields: []output.Field{{Name: "argv", Value: argv}}})
		}
		actions := make([]output.NextAction, 0, len(item.Related))
		for _, related := range item.Related {
			actions = append(actions, output.NextAction{Label: "Discover " + related, Argv: []string{"agentctl", "help", related}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{}})
		}
		_ = renderer.Success(output.Success{Result: item, Lines: lines, NextActions: actions})
		return 0
	}

	names := make([]string, 0, len(topics))
	for name := range topics {
		names = append(names, name)
	}
	sort.Strings(names)
	commands := make([]map[string]any, 0, len(names))
	lines := []output.Line{{Lead: "agentctl", Fields: []output.Field{{Name: "version", Value: version}}}, {Lead: "usage", Fields: []output.Field{{Name: "argv", Value: "agentctl help <topic>"}}}}
	for _, name := range names {
		item := topics[name]
		commands = append(commands, map[string]any{"name": item.Name, "summary": item.Summary, "side_effect_class": item.SideEffectClass})
		lines = append(lines, output.Line{Lead: "topic", Fields: []output.Field{{Name: "name", Value: item.Name}, {Name: "summary", Value: item.Summary}}})
	}
	result := map[string]any{"name": "agentctl", "version": version, "commands": commands, "global_flags": []string{"--output json|text (default json)", "--profile <name>", "--context-file <path>", "--explain", "--config <path>", "--config-bundle <explicit-local-path>", "--journal <path>"}, "discovery": []string{"agentctl help <topic>", "agentctl doctor"}}
	_ = renderer.Success(output.Success{Result: result, Lines: lines, NextActions: []output.NextAction{{Label: "Check readiness", Argv: []string{"agentctl", "doctor"}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{}}}})
	return 0
}
