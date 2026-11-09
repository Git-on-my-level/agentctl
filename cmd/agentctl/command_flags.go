package main

// commandValueFlags lists the flags whose next token is a value rather than a
// command token. Parsers consume those values verbatim, so a value such as
// "--help" or "--plan" is not itself a flag. Inline help and prompt repairs use
// this grammar instead of scanning raw token shapes.
var commandValueFlags = map[string]map[string]bool{
	"run": flagSet("--execution-id", "--label", "--anx-card", "--task-contract", "--adapter",
		"--timeout", "--prompt-file", "--prompt-delivery", "--cwd", "--idempotency-key",
		"--issue", "--run"),
	"delegate": flagSet("--request-file", "--prompt-file", "--cwd", "--authority", "--timeout",
		"--label", "--anx-card", "--require-result-source", "--min-result-bytes"),
	"continue": flagSet("--mode", "--request-key", "--prompt-file", "--timeout", "--label",
		"--require-result-source", "--min-result-bytes"),
	"dispatch": flagSet("--route", "--title", "--prompt-file", "--project", "--status",
		"--idempotency-key", "--label"),
	"steer":  flagSet("--prompt-file", "--idempotency-key", "--timeout"),
	"fanout": flagSet("--manifest", "--concurrency"),
	"status": flagSet("--output"),
	"events": flagSet("--after-sequence", "--limit", "--kind", "--output"),
	"recent": flagSet("--limit", "--state", "--liveness", "--adapter", "--label", "--caller",
		"--since", "--until", "--cursor", "--output"),
	"inbox": flagSet("--limit", "--stale-after", "--adapter", "--label", "--output"),
	"reconcile": flagSet("--stale-after", "--collect-older-than", "--legacy-stale-after",
		"--adapter", "--label", "--plan-digest", "--output"),
	"workspace": flagSet("--path", "--output"),
	"await":     flagSet("--timeout", "--output"),
	"await-any": flagSet("--execution", "--label-prefix", "--timeout", "--kinds", "--since", "--output"),
	"result": flagSet("--require-result-source", "--min-result-bytes", "--label", "--limit",
		"--before", "--output"),
	"promote": flagSet("--title", "--handoff-file", "--output"),
	"subscribe": flagSet("--execution", "--label-prefix", "--authority", "--destination", "--target",
		"--arg", "--allow-host", "--credential-ref", "--kind", "--ttl", "--output"),
	"supervisor":   flagSet("--output"),
	"bootstrap":    flagSet("--harness", "--home", "--target-dir", "--expected-digest", "--output"),
	"config":       flagSet("--config-bundle", "--remote", "--ref", "--bundle", "--checkout", "--output"),
	"skills":       flagSet("--expected-digest", "--output"),
	"data":         flagSet("--before", "--plan-digest", "--output"),
	"schema":       flagSet("--output"),
	"doctor":       flagSet("--adapter", "--output"),
	"capabilities": flagSet("--require", "--executable", "--output"),
	"route":        flagSet("--output"),
	"identity":     flagSet("--provider", "--native-session-id", "--output"),
	"orient":       flagSet("--limit", "--output"),
	"update":       flagSet("--output"),
	"anx":          flagSet("--output"),
	"help":         flagSet("--output"),
}

func flagSet(flags ...string) map[string]bool {
	set := make(map[string]bool, len(flags))
	for _, flag := range flags {
		set[flag] = true
	}
	return set
}

func commandFlagTakesValue(command, flag string) bool {
	return commandValueFlags[command][flag]
}
