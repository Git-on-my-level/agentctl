package main

import "testing"

func TestOwnerSubcommand(t *testing.T) {
	cases := map[string]string{
		"/Users/x/.local/bin/agentctl delegate --wait --label infra": "delegate",
		"agentctl --output json run -- codex exec":                   "run",
		"agentctl --config /tmp/c.json fanout --manifest m.json":     "fanout",
		"agentctl _delegate-worker":                                  "_delegate-worker",
		"agentctl --journal-busy-budget 10m delegate --wait":         "delegate",
		"agentctl dispatch --selector x":                             "dispatch",
		"agentctl promote exec-a":                                    "promote",
		"agentctl --explain continue exec-a":                         "continue",
		"agentctl supervisor run":                                    "",
		"agentctl _update-worker":                                    "",
		"agentctl status exec-a":                                     "",
		"agentctl --output run status":                               "",
		"/usr/bin/python3 agentctl delegate":                         "",
		"":                                                           "",
	}
	for command, want := range cases {
		if got := ownerSubcommand(command); got != want {
			t.Errorf("ownerSubcommand(%q) = %q, want %q", command, got, want)
		}
	}
}

func TestCountOwnerProcessesExcludesSelf(t *testing.T) {
	ps := []byte("  101 /Users/x/.local/bin/agentctl delegate --wait\n  102 agentctl supervisor run\n  103 agentctl _delegate-worker\n  104 agentctl delegate --wait\n  junk\n")
	if got := countOwnerProcesses(ps, 104); got != 2 {
		t.Fatalf("count = %d, want 2 (101, 103; 104 is self)", got)
	}
}
