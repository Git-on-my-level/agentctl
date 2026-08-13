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
			SideEffectClass: output.LocalOperationalWrite, Defaults: []string{"status is read-only", "update detects supported harnesses and canonical roots", "update refuses drifted or duplicate managed assets"},
			Examples: [][]string{{"agentctl", "bootstrap", "status"}, {"agentctl", "bootstrap", "update", "--dry-run"}, {"agentctl", "bootstrap", "update"}}, Related: []string{"bootstrap update", "doctor"},
		},
		"bootstrap update": {
			Name: "bootstrap update", Summary: "Install or upgrade the embedded portable skill in every detected canonical harness root.", Usage: "agentctl bootstrap update [--dry-run] [--harness names] [--home path] [--target-dir path]",
			SideEffectClass: output.LocalOperationalWrite, Defaults: []string{"all detected harnesses", "canonical roots", "managed upgrades and missing installs", "no legacy-copy deletion", "no new supervisor service"},
			Examples: [][]string{{"agentctl", "bootstrap", "update", "--dry-run"}, {"agentctl", "bootstrap", "update"}, {"agentctl", "bootstrap", "update", "--harness", "cursor"}}, Related: []string{"bootstrap", "doctor"},
		},
		"capabilities": {
			Name: "capabilities", Summary: "Return a concise live viability projection for one adapter.", Usage: "agentctl capabilities <adapter> [--require names] [--full] [--static] [--executable path]",
			SideEffectClass: output.ReadOnly, Defaults: []string{"concise summary", "live probe", "all capabilities when --require is omitted"},
			Examples: [][]string{{"agentctl", "capabilities", "codex"}, {"agentctl", "capabilities", "cursor", "--require", "launch,result_content"}}, Related: []string{"doctor", "run"},
		},
		"run": {
			Name: "run", Summary: "Launch an exact native argv and persist its normalized execution and final result.", Usage: "agentctl run [--execution-id exec-...] [--adapter name] [--timeout duration|--no-timeout] [flags] -- <native argv>",
			SideEffectClass: output.ExternalSideEffect, Defaults: []string{"adapter inferred from known executable names", "execution ID generated unless preallocated", "30 minute timeout", "launch and result_content preflight", "bounded final result storage"},
			Examples: [][]string{{"agentctl", "run", "--", "codex", "exec", "--json", "review this change"}, {"agentctl", "run", "--", "cursor-agent", "--print", "--output-format", "stream-json", "--trust", "review this change"}}, Related: []string{"capabilities", "await", "result", "subscribe"},
		},
		"await": {
			Name: "await", Summary: "Wait for terminal state or attention without polling native session files.", Usage: "agentctl await <execution-id> [--timeout duration] [--ignore-attention]",
			SideEffectClass: output.ReadOnly, Defaults: []string{"10 minute timeout", "returns immediately when attention is required"},
			Examples: [][]string{{"agentctl", "await", "exec-..."}}, Related: []string{"status", "events", "result"},
		},
		"result": {
			Name: "result", Summary: "Dereference the bounded final result for a terminal execution.", Usage: "agentctl result <execution-id> [--summary] [--allow-empty]",
			SideEffectClass: output.ReadOnly, Defaults: []string{"stored content or a structured failure is required", "integrity conflicts fail closed"},
			Examples: [][]string{{"agentctl", "result", "exec-..."}, {"agentctl", "result", "exec-...", "--summary"}}, Related: []string{"run", "await", "events"},
		},
		"subscribe": {
			Name: "subscribe", Summary: "Create durable at-least-once callback delivery for execution events.", Usage: "agentctl subscribe create --execution ID --destination file|webhook --target target [--authority direct|multica] [--kind kind] [--ttl duration] [--keep-after-terminal] | list | show ID | cancel ID",
			SideEffectClass: output.LocalOperationalWrite, Defaults: []string{"create filters terminal, attention, and artifact events", "24 hour TTL", "expires after acknowledged terminal delivery"},
			Examples: [][]string{{"agentctl", "subscribe", "create", "--execution", "exec-...", "--destination", "file", "--target", "/absolute/events.ndjson"}}, Related: []string{"run", "events", "supervisor"},
		},
		"status":        {Name: "status", Summary: "Read one normalized execution envelope.", Usage: "agentctl status <execution-id>", SideEffectClass: output.ReadOnly, Defaults: []string{"native paths, prompts, and transcripts remain redacted"}, Examples: [][]string{{"agentctl", "status", "exec-..."}}, Related: []string{"events", "await", "result"}},
		"events":        {Name: "events", Summary: "Read bounded normalized event history.", Usage: "agentctl events <execution-id> [--after-sequence n] [--limit n]", SideEffectClass: output.ReadOnly, Defaults: []string{"after sequence 0", "limit 100"}, Examples: [][]string{{"agentctl", "events", "exec-..."}}, Related: []string{"status", "subscribe", "await"}},
		"promote":       {Name: "promote", Summary: "Explicitly create or recover a Multica issue for a direct execution.", Usage: "agentctl promote <execution-id> --title title [--handoff-file path] [--plan]", SideEffectClass: output.RemoteCoordinationWrite, Defaults: []string{"performs the remote mutation", "idempotent for identical semantics"}, Examples: [][]string{{"agentctl", "promote", "exec-...", "--title", "Durable follow-up", "--plan"}}, Related: []string{"route", "config"}},
		"route":         {Name: "route", Summary: "Explain the deterministic direct-versus-Multica lifecycle recommendation.", Usage: "agentctl route explain [flags]", SideEffectClass: output.ReadOnly, Defaults: []string{"bounded work routes direct"}, Examples: [][]string{{"agentctl", "route", "explain", "--model-family", "gpt"}}, Related: []string{"run", "promote"}},
		"supervisor":    {Name: "supervisor", Summary: "Run, inspect, or plan the host-local callback supervisor.", Usage: "agentctl supervisor run|status|plan ...", SideEffectClass: output.LocalOperationalWrite, Defaults: []string{"Unix socket only", "new service installation remains explicit"}, Examples: [][]string{{"agentctl", "supervisor", "status"}}, Related: []string{"subscribe", "bootstrap"}},
		"config":        {Name: "config", Summary: "Manage executable provenance expectations, advisory agent preferences, optional authority, and explicit local or Git-backed configuration.", Usage: "agentctl config set-profile|show|validate|doctor|bundle|source ...", SideEffectClass: output.LocalOperationalWrite, Defaults: []string{"agent preferences are advisory", "no implicit unauthenticated Multica profile", "Git sources are explicit and fast-forward only", "native run argv remains caller-authoritative"}, Examples: [][]string{{"agentctl", "config", "show"}, {"agentctl", "config", "source", "status"}, {"agentctl", "--config-bundle", "/local/config-bundle.json", "config", "bundle", "plan"}}, Related: []string{"config source", "doctor", "promote"}},
		"config source": {Name: "config source", Summary: "Materialize a reviewed Git config bundle into the owner-only live config.", Usage: "agentctl config source init --remote URL [--ref main] [--bundle config-bundle.json] [--checkout path] [--plan] | status | update [--plan] | restore [--plan]", SideEffectClass: output.LocalOperationalWrite, Defaults: []string{"status and --plan perform no fetch", "update fetches noninteractively and accepts fast-forwards only", "restore repairs only the live config from the pinned clean checkout", "dirty checkouts and unreviewed history fail closed", "Git and SSH own credentials"}, Examples: [][]string{{"agentctl", "config", "source", "init", "--remote", "git@github.com:owner/agentctl-config.git", "--plan"}, {"agentctl", "config", "source", "update"}, {"agentctl", "config", "source", "status"}}, Related: []string{"config", "doctor"}},
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
			"bootstrap status": "bootstrap", "route explain": "route",
			"subscribe create": "subscribe", "subscribe list": "subscribe", "subscribe show": "subscribe", "subscribe cancel": "subscribe",
			"supervisor run": "supervisor", "supervisor status": "supervisor", "supervisor plan": "supervisor",
			"config set-profile": "config", "config show": "config", "config validate": "config", "config doctor": "config", "config bundle": "config", "config bundle validate": "config", "config bundle show": "config", "config bundle plan": "config", "config source init": "config source", "config source status": "config source", "config source update": "config source", "config source restore": "config source",
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
