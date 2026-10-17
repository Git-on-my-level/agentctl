package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDevinACPDelegateContinueAndReplay(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 fixture unavailable")
	}
	f := newDelegateFixture(t, "")
	script, err := filepath.Abs("../../internal/adapter/testdata/devin-acp.py")
	if err != nil {
		t.Fatal(err)
	}
	native := filepath.Join(f.root, "devin")
	wrapper := "#!/bin/sh\nif [ \"$1\" = '--version' ]; then echo fixture-1; exit 0; fi\necho launch >> '" + f.count + "'\nexec '" + python + "' '" + script + "'\n"
	if err := os.WriteFile(native, []byte(wrapper), 0700); err != nil {
		t.Fatal(err)
	}
	value := map[string]any{"schema_version": 1, "default_profile": "test", "profiles": map[string]any{"test": map[string]any{
		"adapters":          map[string]any{"devin": map[string]any{"executable": native}},
		"delegation":        map[string]any{"unattended_coding_permissions": true},
		"agent_preferences": map[string]any{"mode": "advisory", "preferred": []any{map[string]any{"agent": "devin", "family": "fixture", "model": "fixture-model"}}},
	}}}
	data, _ := json.Marshal(value)
	if err := os.WriteFile(f.config, data, 0600); err != nil {
		t.Fatal(err)
	}
	f.selectRequest(t, `{"family":"fixture"}`)
	t.Setenv("ACP_FIXTURE_MODE", "permission")
	t.Setenv("ACP_EXPECT_PERMISSION", "selected")
	code, out := f.invoke("PRIVATE PROMPT", "--wait")
	if code != 0 || !strings.Contains(out, "Hello world") {
		t.Fatalf("delegate exit=%d %s", code, out)
	}
	first := resultID(t, out)
	code, plan := f.command(t, "PRIVATE FOLLOWUP", "continue", first, "--request-key", "acp-turn-2", "--prompt-stdin", "--plan")
	if code != 0 || !strings.Contains(plan, `"--agentctl-acp-session","<native-session>"`) || strings.Contains(plan, "fixture-session") {
		t.Fatalf("plan exit=%d %s", code, plan)
	}
	if f.launches(t) != 1 {
		t.Fatal("plan launched ACP")
	}
	code, out = f.command(t, "PRIVATE FOLLOWUP", "continue", first, "--request-key", "acp-turn-2", "--prompt-stdin", "--wait")
	if code != 0 || !strings.Contains(out, "Hello world") || strings.Contains(out, "REPLAY") {
		t.Fatalf("continue exit=%d %s", code, out)
	}
	second := resultID(t, out)
	code, out = f.command(t, "PRIVATE FOLLOWUP", "continue", first, "--request-key", "acp-turn-2", "--prompt-stdin", "--wait")
	if code != 0 || resultID(t, out) != second || !strings.Contains(out, `"reused":true`) || f.launches(t) != 2 {
		t.Fatalf("replay exit=%d %s", code, out)
	}
	for _, id := range []string{first, second} {
		for _, cmd := range []string{"status", "events"} {
			code, out = f.command(t, "", cmd, id)
			if code != 0 {
				t.Fatalf("%s exit=%d %s", cmd, code, out)
			}
			for _, secret := range []string{"PRIVATE", "fixture-session", "Hello world", "private tool text"} {
				if strings.Contains(out, secret) {
					t.Fatalf("%s leaked %s", cmd, secret)
				}
			}
		}
	}
}
