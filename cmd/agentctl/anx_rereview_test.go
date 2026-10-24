package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/testutil"
)

func TestReReviewMulticaDispatchQueuesRegistration(t *testing.T) {
	root := testutil.TempDir(t)
	agents := `[{"id":"agent-sol","name":"M5 MBP Codex (Sol)","model":"gpt-5.6-sol","runtime_id":"runtime-m5-codex","status":"idle","archived_at":null}]`
	runtimes := `[{"id":"runtime-m5-codex","custom_name":"M5 MBP","provider":"codex","status":"online","archived_at":null}]`
	fake, _ := writeDispatchFixture(t, root, agents, runtimes)
	configPath, journal := filepath.Join(root, "config", "config.json"), filepath.Join(root, "state", "journal.db")
	writeDispatchConfig(t, configPath, fake, []any{map[string]any{"agent": "codex", "model": "gpt-5.6-sol", "speed": "regular", "use_for": "alias:sol"}}, map[string]any{"m5": "m5-mbp"})
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	for range 2 {
		a.stdin = strings.NewReader("Review the release candidate.")
		a.stdinIsTerminal = func() bool { return false }
		if code := a.run(context.Background(), []string{"--config", configPath, "--journal", journal, "dispatch", "--route", "m5 sol", "--title", "Review release", "--prompt-stdin", "--idempotency-key", "anx-rereview", "--label", "anx.card.dogfooding"}); code != 0 {
			t.Fatalf("dispatch exit=%d out=%s err=%s", code, stdout.String(), stderr.String())
		}
	}
	pending, err := openSpoolForTest(t, journal).ListPending()
	if err != nil || len(pending) != 1 {
		t.Fatalf("Multica dispatch did not enqueue exactly one start record: %v %v", pending, err)
	}
	if pending[0].Registration.NativeSession != nil {
		t.Fatal("Multica invented native session")
	}
	t.Setenv("AGENTCTL_TEST_ISSUE_STATUS", "done")
	if code := a.run(context.Background(), []string{"--config", configPath, "--journal", journal, "await", pending[0].ExecutionID, "--timeout", "5s"}); code != 0 {
		t.Fatalf("Multica await: %d %s", code, stdout.String())
	}
	pending, err = openSpoolForTest(t, journal).ListPending()
	if err != nil || len(pending) != 2 {
		t.Fatalf("Multica terminal not queued: %+v %v", pending, err)
	}
	foundTerminal := false
	for _, entry := range pending {
		if entry.Phase == "terminal" {
			foundTerminal = true
			if entry.Registration.State != "completed" || entry.Registration.Outcome == nil || entry.Registration.Outcome.Availability != "unavailable_at_source" || entry.Registration.NativeSession != nil || entry.Registration.ResultCollected {
				t.Fatalf("dishonest Multica projection: %+v", entry.Registration)
			}
		}
	}
	if !foundTerminal {
		t.Fatal("terminal phase missing")
	}
}

// This is the review's cross-process sequence: start delivery holds the lock,
// the execution finishes and starts its terminal worker, then start is released.
// No later invocation or explicit flush is allowed to rescue the terminal.
func TestReReviewDetachedWorkersDoNotLoseTerminal(t *testing.T) {
	root := testutil.TempDir(t)
	binary := filepath.Join(root, "agentctl")
	if out, err := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	anx := filepath.Join(root, "fake-anx")
	held, release, delivered := filepath.Join(root, "held"), filepath.Join(root, "release"), filepath.Join(root, "delivered")
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0600) })
	script := `#!/usr/bin/env python3
import json,sys,time,pathlib,os
root=pathlib.Path(os.environ['AGENTCTL_REREVIEW_FIXTURE'])
cmd='auth.whoami' if 'whoami' in sys.argv else sys.argv[sys.argv.index('api')+1]
body=json.load(sys.stdin) if '--json-body' in sys.argv else {}
result={}
if cmd=='sessions.register': result={'session':{'id':'session-test'}}
elif cmd=='auth.whoami': result={'agent':{'actor_id':'actor-test'}}
elif cmd=='cards.list': result={'cards':[{'ref':'card:dogfooding','thread_id':'thread-test'}]}
elif cmd=='cards.get': result={'card':{'thread_id':'thread-test'}}
elif cmd=='events.create':
 phase=json.loads(body['event']['payload']['text'])['phase']
 if phase=='start':
  (root/'held').touch()
  deadline=time.monotonic()+15
  while not (root/'release').exists() and time.monotonic()<deadline: time.sleep(.01)
 with (root/'delivered').open('a') as f: f.write(phase+'\n')
print(json.dumps({'ok':True,'result':result}))
`
	if err := os.WriteFile(anx, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	configPath := writeAnxConfig(t, anx)
	journal := filepath.Join(root, "state", "journal.db")
	native := filepath.Join(root, "native")
	// Readiness is explicit: the result cannot race ahead of start's snapshot.
	nativeScript := `#!/usr/bin/env python3
import os,pathlib,time,json
root=pathlib.Path(os.environ['AGENTCTL_REREVIEW_FIXTURE'])
deadline=time.monotonic()+10
while not (root/'held').exists() and time.monotonic()<deadline: time.sleep(.01)
print(json.dumps({'type':'result','session_id':'fixture-session','status':'completed','result':'done'}))
`
	if err := os.WriteFile(native, []byte(nativeScript), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "--config", configPath, "--journal", journal, "run", "--adapter", "generic-process", "--anx-card", "dogfooding", "--", native)
	command.Env = append(os.Environ(), "AGENTCTL_UPDATE_MODE=off", "AGENTCTL_REREVIEW_FIXTURE="+root)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("run: %v %s", err, out)
	}
	if _, err := os.Stat(held); err != nil {
		t.Fatal("start not held")
	}
	spool := openSpoolForTest(t, journal)
	pending, err := spool.ListPending()
	if err != nil {
		t.Fatal(err)
	}
	terminal := false
	for _, entry := range pending {
		if entry.Phase == "terminal" {
			terminal = true
		}
	}
	if !terminal {
		raw, _ := json.Marshal(pending)
		t.Fatalf("terminal not queued: %s", raw)
	}
	// Allow the detached terminal process to reach the occupied flush lock.
	time.Sleep(300 * time.Millisecond)
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		raw, _ := os.ReadFile(delivered)
		pending, err := spool.ListPending()
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) == "start\nterminal\n" && len(pending) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	raw, _ := os.ReadFile(delivered)
	t.Fatalf("terminal wakeup lost, delivered=%q", raw)
}

func TestReReviewStatusOmitsCredentialSlug(t *testing.T) {
	root := testutil.TempDir(t)
	journal := filepath.Join(root, "state", "journal.db")
	configPath := filepath.Join(root, "missing.json")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	secret := "glpat-" + "abcdefghijklmnopqrst"
	id := seedTerminalLabeledExecution(t, a, journal, []string{"anx.card." + secret})
	a.anxRegisterTerminal(common{configPath: configPath, journalPath: journal}, mustParseExecID(t, id))
	if code := a.run(context.Background(), []string{"--config", configPath, "--journal", journal, "anx", "status"}); code != 0 {
		t.Fatal(code)
	}
	if strings.Contains(stdout.String(), secret) || strings.Contains(stderr.String(), secret) {
		t.Fatal("credential slug escaped status")
	}
	if !strings.Contains(stdout.String(), `"egress_error_code":1001`) {
		t.Fatalf("missing numeric rejection: %s", stdout.String())
	}
}
