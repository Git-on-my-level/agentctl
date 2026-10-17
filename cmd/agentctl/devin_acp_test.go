package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func newDevinACPDelegateFixture(t *testing.T) delegateFixture {
	t.Helper()
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
	wrapper := "#!/bin/sh\nif [ \"$1\" = '--version' ]; then echo probe >> '" + f.probe + "'; echo fixture-1; exit 0; fi\necho launch >> '" + f.count + "'\nexec '" + python + "' '" + script + "'\n"
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
	return f
}

func TestDevinACPDelegateContinueAndReplay(t *testing.T) {
	f := newDevinACPDelegateFixture(t)
	t.Setenv("ACP_FIXTURE_MODE", "permission")
	t.Setenv("ACP_EXPECT_PERMISSION", "selected")
	t.Setenv("ACP_EXPECT_OPTION", "yes")
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

func assertDevinPreflightNoSideEffects(t *testing.T, f delegateFixture) {
	t.Helper()
	for _, path := range []string{f.count, f.probe, f.journal, filepath.Dir(f.journal)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("preflight touched %s: %v", path, err)
		}
	}
}

func TestDevinACPRejectsInvalidDeliveryBeforePlanOrLaunch(t *testing.T) {
	for _, plan := range []bool{true, false} {
		for _, tc := range []struct {
			name, delivery, prompt string
			extra                  []string
			diagnostic             string
		}{
			{name: "argv", delivery: "argv", prompt: "private task", diagnostic: "acp_prompt_delivery_required"},
			{name: "stream", delivery: "stream", prompt: "private task", diagnostic: "acp_prompt_delivery_required"},
			{name: "missing", diagnostic: "acp_prompt_delivery_required"},
			{name: "empty", delivery: "stdin", diagnostic: "prompt must not be empty"},
			{name: "no-store-override", delivery: "argv", prompt: "private task", extra: []string{"--no-store-result"}, diagnostic: "acp_prompt_delivery_required"},
			{name: "missing-result-override", delivery: "argv", prompt: "private task", extra: []string{"--allow-missing-result"}, diagnostic: "acp_prompt_delivery_required"},
		} {
			t.Run(fmt.Sprintf("%s/plan=%t", tc.name, plan), func(t *testing.T) {
				f := newDevinACPDelegateFixture(t)
				args := []string{"run", "--cwd", f.root}
				if tc.delivery != "" {
					path := filepath.Join(f.root, "prompt.txt")
					if err := os.WriteFile(path, []byte(tc.prompt), 0600); err != nil {
						t.Fatal(err)
					}
					args = append(args, "--prompt-file", path, "--prompt-delivery", tc.delivery)
				}
				args = append(args, tc.extra...)
				if plan {
					args = append(args, "--plan")
				}
				args = append(args, "--", filepath.Join(f.root, "devin"), "acp")
				code, out := f.command(t, "", args...)
				if code == 0 || !strings.Contains(out, tc.diagnostic) {
					t.Fatalf("exit=%d %s", code, out)
				}
				assertDevinPreflightNoSideEffects(t, f)
			})
		}
	}
}

func TestDevinACPRejectsInvalidUTF8BeforeAdmission(t *testing.T) {
	for _, command := range []string{"run", "delegate", "continue"} {
		for _, source := range []string{"file", "stdin"} {
			if command == "continue" && source == "file" {
				continue
			}
			for _, plan := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/%s/plan=%t", command, source, plan), func(t *testing.T) {
					f := newDevinACPDelegateFixture(t)
					prompt := string([]byte{'p', 0xff, 'q'})
					args := []string{command, "--cwd", f.root}
					if command == "delegate" {
						args = append(args, "--request-file", f.request)
					}
					if command == "continue" {
						code, out := f.invoke("first task", "--wait")
						if code != 0 {
							t.Fatalf("setup exit=%d %s", code, out)
						}
						args = []string{"continue", resultID(t, out), "--request-key", "invalid-turn"}
						for _, path := range []string{f.count, f.probe} {
							if err := os.Remove(path); err != nil {
								t.Fatal(err)
							}
						}
					}
					if source == "stdin" {
						args = append(args, "--prompt-stdin")
					} else {
						path := filepath.Join(f.root, "invalid.txt")
						if err := os.WriteFile(path, []byte(prompt), 0600); err != nil {
							t.Fatal(err)
						}
						args = append(args, "--prompt-file", path)
						prompt = ""
					}
					if plan {
						args = append(args, "--plan")
					}
					if command == "run" {
						args = append(args, "--prompt-delivery", "stdin", "--", filepath.Join(f.root, "devin"), "acp")
					}
					code, out := f.command(t, prompt, args...)
					if code == 0 || !strings.Contains(out, "acp_prompt_invalid_utf8") {
						t.Fatalf("exit=%d %s", code, out)
					}
					if command == "continue" {
						for _, path := range []string{f.count, f.probe} {
							if _, err := os.Stat(path); !os.IsNotExist(err) {
								t.Fatalf("invalid continuation probed/launched: %v", err)
							}
						}
					} else {
						assertDevinPreflightNoSideEffects(t, f)
					}
				})
			}
		}
	}
}
