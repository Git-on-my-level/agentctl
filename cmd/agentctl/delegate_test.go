package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/config"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
)

type delegateFixture struct{ root, config, journal, request, count, probe string }

func newDelegateFixture(t *testing.T, body string) delegateFixture {
	t.Helper()
	root := t.TempDir()
	f := delegateFixture{root: root, config: filepath.Join(root, "config.json"), journal: filepath.Join(root, "state", "journal.db"), request: filepath.Join(root, "request.json"), count: filepath.Join(root, "launches"), probe: filepath.Join(root, "probes")}
	// Paths are fixture-generated and shell-quoted, never caller-controlled prompts.
	script := "#!/bin/sh\nif [ \"$1\" = '--version' ]; then\n echo probe >> '" + f.probe + "'\n echo 2026.09.10\n exit 0\nfi\necho launch >> '" + f.count + "'\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(root, "cursor-agent"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	f.configure(t, "cursor-grok-4.6-high")
	f.selectRequest(t, `{"family":"grok"}`)
	return f
}
func (f delegateFixture) configure(t *testing.T, model string) {
	t.Helper()
	value := map[string]any{"schema_version": 1, "default_profile": "test", "profiles": map[string]any{"test": map[string]any{
		"adapters":          map[string]any{"cursor": map[string]any{"executable": filepath.Join(f.root, "cursor-agent")}},
		"route":             map[string]any{"this_host": "workstation", "hosts": map[string]string{"remote": "other-workstation"}},
		"agent_preferences": map[string]any{"mode": "advisory", "preferred": []any{map[string]any{"agent": "cursor", "family": "grok", "version": "4.6", "model": model, "speed": "regular", "default": true}}},
	}}}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.config, data, 0600); err != nil {
		t.Fatal(err)
	}
}
func (f delegateFixture) selectRequest(t *testing.T, selector string) {
	t.Helper()
	if err := os.WriteFile(f.request, []byte(`{"schema_version":1,"request_key":"test-work-1","selector":`+selector+`}`), 0600); err != nil {
		t.Fatal(err)
	}
}
func (f delegateFixture) invoke(prompt string, extra ...string) (int, string) {
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	a.stdin = strings.NewReader(prompt)
	args := []string{"--config", f.config, "--journal", f.journal, "delegate", "--cwd", f.root, "--request-file", f.request, "--prompt-stdin"}
	args = append(args, extra...)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	code := a.run(ctx, args)
	return code, stdout.String()
}

const delegateSuccessScript = `printf '%s\n' '{"type":"system","subtype":"init","session_id":"delegate-test"}' '{"type":"result","subtype":"success","is_error":false,"result":"verified answer"}'`

func TestDelegatePlanNoMutationOrPromptLeak(t *testing.T) {
	f := newDelegateFixture(t, delegateSuccessScript)
	code, out := f.invoke("private-task-token", "--plan")
	if code != 0 {
		t.Fatalf("exit=%d %s", code, out)
	}
	if strings.Contains(out, "private-task-token") || !strings.Contains(out, `"model":"cursor-grok-4.6-high"`) || !strings.Contains(out, `"side_effect_class":"read_only"`) {
		t.Fatal(out)
	}
	for _, p := range []string{f.journal, filepath.Dir(f.journal), f.count} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("plan created %s: %v", p, err)
		}
	}
}
func TestDelegateWaitReplayAndConflict(t *testing.T) {
	f := newDelegateFixture(t, delegateSuccessScript)
	code, out := f.invoke("private-task-token", "--wait")
	if code != 0 || !strings.Contains(out, `"text":"verified answer"`) || !strings.Contains(out, `"reused":false`) {
		t.Fatalf("exit=%d %s", code, out)
	}
	if strings.Contains(out, "private-task-token") {
		t.Fatal("prompt leaked")
	}
	f.configure(t, "changed-default")
	code, out = f.invoke("private-task-token", "--wait")
	if code != 0 || !strings.Contains(out, `"reused":true`) || !strings.Contains(out, `"model":"cursor-grok-4.6-high"`) {
		t.Fatalf("replay exit=%d %s", code, out)
	}
	code, out = f.invoke("private-task-token", "--wait", "--content")
	if code != 0 || out != "verified answer" {
		t.Fatalf("content exit=%d %s", code, out)
	}
	code, out = f.invoke("different-task", "--wait")
	if code != output.ExitCodeFor(output.CodeConflict) {
		t.Fatalf("conflict exit=%d %s", code, out)
	}
	launches, _ := os.ReadFile(f.count)
	if string(launches) != "launch\n" {
		t.Fatalf("launches=%q", launches)
	}
	raw, _ := os.ReadFile(f.journal)
	if bytes.Contains(raw, []byte("private-task-token")) {
		t.Fatal("prompt in journal")
	}
}
func TestDelegateConcurrentSameKeyLaunchesOnce(t *testing.T) {
	f := newDelegateFixture(t, "sleep 1\n"+delegateSuccessScript)
	var wg sync.WaitGroup
	codes := make([]int, 2)
	outs := make([]string, 2)
	for i := range codes {
		wg.Add(1)
		go func(i int) { defer wg.Done(); codes[i], outs[i] = f.invoke("same task", "--wait") }(i)
	}
	wg.Wait()
	for i, code := range codes {
		if code != 0 {
			t.Fatalf("call %d exit=%d %s", i, code, outs[i])
		}
	}
	launches, _ := os.ReadFile(f.count)
	if string(launches) != "launch\n" {
		t.Fatalf("launches=%q", launches)
	}
	if strings.Count(strings.Join(outs, ""), `"reused":true`) != 1 {
		t.Fatalf("replay receipts=%v", outs)
	}
}
func TestDelegateRejectsConstraintsBeforeProbe(t *testing.T) {
	for _, selector := range []string{`{"family":"grok","version":"4.5"}`, `{"family":"grok","harness":"codex"}`, `{"family":"grok","settings":{"speed":"fast"}}`, `{"family":"grok","host":"remote"}`, `{"family":"grok","unknown":"x"}`, `{"family":"grok","family":"grok"}`, `{"Family":"grok"}`} {
		t.Run(selector, func(t *testing.T) {
			f := newDelegateFixture(t, delegateSuccessScript)
			f.selectRequest(t, selector)
			code, out := f.invoke("task", "--wait")
			if code == 0 {
				t.Fatal(out)
			}
			for _, p := range []string{f.count, f.probe, f.journal} {
				if _, err := os.Stat(p); !os.IsNotExist(err) {
					t.Fatalf("rejection touched %s", p)
				}
			}
		})
	}
}
func TestDelegateWaitRequiresSuccessfulNonemptyAnswer(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"failed", `printf '%s\n' '{"type":"result","subtype":"error","is_error":true,"result":"failed"}'; exit 1`, output.ExitCodeFor(output.CodeExecutionFailed)},
		{"empty", `printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":""}'`, output.ExitCodeFor(output.CodeNotFound)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDelegateFixture(t, tc.body)
			code, out := f.invoke("task", "--wait")
			if code != tc.want || !strings.Contains(out, `"execution_id":"exec-`) {
				t.Fatalf("exit=%d want=%d %s", code, tc.want, out)
			}
		})
	}
}

func TestDelegateDefaultsToLocalWithoutHostSetup(t *testing.T) {
	for _, selector := range []string{"", "local", " LOCAL "} {
		host, local, err := resolveDelegateHost(selector, config.Profile{})
		if err != nil || host != "local" || !local {
			t.Fatalf("selector=%q host=%q local=%v err=%v", selector, host, local, err)
		}
	}
}

func TestDelegateExpiredOwnerNeverRelaunches(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	seconds := 30
	e := model.Execution{State: model.StateRunning, Liveness: model.LivenessAlive, CreatedAt: time.Now(), Observation: model.Observation{ObservedAt: time.Now().Add(-time.Minute), FreshForSeconds: &seconds}}
	problem := a.collectDelegation(context.Background(), output.Renderer{Writer: &stdout}, common{}, e, delegateOptions{wait: true}, true)
	if problem == nil || problem.Code != output.CodeExecutionUnknown {
		t.Fatalf("error=%v", problem)
	}
}

func TestDelegateEquivalentSpellingReplaysOriginal(t *testing.T) {
	f := newDelegateFixture(t, delegateSuccessScript)
	code, out := f.invoke("task", "--wait")
	if code != 0 {
		t.Fatalf("exit=%d %s", code, out)
	}
	f.selectRequest(t, `{"family":" GROK "}`)
	code, out = f.invoke("task", "--wait")
	if code != 0 || !strings.Contains(out, `"reused":true`) {
		t.Fatalf("exit=%d %s", code, out)
	}
}

func TestDelegateReplayPlanKeepsFrozenRecipePrivate(t *testing.T) {
	f := newDelegateFixture(t, delegateSuccessScript)
	code, out := f.invoke("private-task-token", "--wait")
	if code != 0 {
		t.Fatal(out)
	}
	f.configure(t, "changed-default")
	code, out = f.invoke("private-task-token", "--plan")
	if code != 0 || !strings.Contains(out, `"source":"frozen_admission"`) || !strings.Contains(out, `"argv":[`) || strings.Contains(out, "changed-default") || strings.Contains(out, "private-task-token") {
		t.Fatalf("exit=%d %s", code, out)
	}
	var receipt struct {
		Result struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &receipt); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--journal", f.journal, "status", receipt.Result.ID}); code != 0 {
		t.Fatal(stdout.String())
	}
	if strings.Contains(stdout.String(), f.root) || strings.Contains(stdout.String(), `"native_plan"`) {
		t.Fatalf("private recipe in normal status: %s", stdout.String())
	}
}

func TestDelegatePlanRejectsCollectionFlags(t *testing.T) {
	for _, extra := range [][]string{{"--plan", "--wait"}, {"--plan", "--wait", "--content"}, {"--plan", "--wait", "--require-result-source", "assistant"}} {
		args := append([]string{"--request-file", "request.json", "--prompt-stdin"}, extra...)
		if _, err := parseDelegate(args); err == nil {
			t.Fatalf("accepted %v", extra)
		}
	}
}

func TestDelegateHarnessAliasesUseCanonicalExecutable(t *testing.T) {
	f := newDelegateFixture(t, delegateSuccessScript)
	f.selectRequest(t, `{"harness":"cursor-agent","family":"grok"}`)
	code, out := f.invoke("task", "--wait")
	if code != 0 || !strings.Contains(out, `"harness":"cursor"`) {
		t.Fatalf("exit=%d %s", code, out)
	}
}

func TestDelegateConcurrentFailedProbeRecoversAdmission(t *testing.T) {
	f := newDelegateFixture(t, "sleep 1\n"+delegateSuccessScript)
	barrier := filepath.Join(f.root, "probe-started")
	failing := filepath.Join(f.root, "cursor-failing-probe")
	script := "#!/bin/sh\nif [ \"$1\" = '--version' ]; then\n touch '" + barrier + "'\n while [ ! -f '" + f.count + "' ]; do sleep 0.02; done\n exit 1\nfi\nexit 99\n"
	if err := os.WriteFile(failing, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	other := f
	other.config = filepath.Join(f.root, "other-config.json")
	raw, err := os.ReadFile(f.config)
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.ReplaceAll(raw, []byte(filepath.Join(f.root, "cursor-agent")), []byte(failing))
	if err := os.WriteFile(other.config, raw, 0600); err != nil {
		t.Fatal(err)
	}
	type answer struct {
		code int
		out  string
	}
	done := make(chan answer, 1)
	go func() { code, out := other.invoke("same task", "--wait"); done <- answer{code, out} }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(barrier); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("probe did not begin")
		}
		time.Sleep(10 * time.Millisecond)
	}
	code, out := f.invoke("same task", "--wait")
	if code != 0 {
		t.Fatalf("winner exit=%d %s", code, out)
	}
	reply := <-done
	if reply.code != 0 || !strings.Contains(reply.out, `"reused":true`) {
		t.Fatalf("loser exit=%d %s", reply.code, reply.out)
	}
	launches, _ := os.ReadFile(f.count)
	if string(launches) != "launch\n" {
		t.Fatalf("launches=%q", launches)
	}
}
