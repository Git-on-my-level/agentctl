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

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/launchrecipe"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

const steerSecret = "STEER-MESSAGE-TEXT-7731"

// steerLiveAgent acknowledges each stdin message the way Claude Code's
// streaming input does, and will not finish until it has been steered.
const steerLiveAgent = `#!/bin/sh
case "$1" in --version) echo "fake 1.0"; exit 0;; esac
IFS= read -r first
printf '%s\n' '{"type":"system","subtype":"init","session_id":"live-1"}'
printf '%s\n' '{"type":"user","isReplay":true,"session_id":"live-1"}'
IFS= read -r second
printf '%s\n' '{"type":"result","result":"turn-1","is_error":false,"session_id":"live-1"}'
printf '%s\n' '{"type":"user","isReplay":true,"session_id":"live-1"}'
printf '%s\n' '{"type":"result","result":"turn-2","is_error":false,"session_id":"live-1"}'
while IFS= read -r extra; do :; done
`

// steerResumableAgent is one-shot: only its `resume` form ever finishes.
const steerResumableAgent = `#!/bin/sh
case "$1" in --version) echo "fake 1.0"; exit 0;; esac
case " $* " in
*" resume "*)
  IFS= read -r message
  printf '%s\n' '{"type":"thread.started","thread_id":"thread-1"}'
  printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"resumed"}}'
  printf '%s\n' '{"type":"turn.completed"}'
  ;;
*)
  printf '%s\n' '{"type":"thread.started","thread_id":"thread-1"}'
  sleep 60
  ;;
esac
`

type steerFixture struct {
	t       *testing.T
	journal string
	done    chan struct{}
	code    int
	id      string
}

func startSteerFixture(t *testing.T, script, adapterName, delivery string, nativeArgs ...string) *steerFixture {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "fake-agent")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	fixture := &steerFixture{t: t, journal: filepath.Join(root, "state", "journal.db"), done: make(chan struct{})}
	var stdout, stderr bytes.Buffer
	owner := testApp(&stdout, &stderr)
	owner.stdin = strings.NewReader("launch task\n")
	owner.stdinIsTerminal = func() bool { return false }
	args := append([]string{"--output", "json", "--journal", fixture.journal, "run", "--label", "steer-fixture", "--timeout", "30s", "--adapter", adapterName, "--prompt-stdin", "--prompt-delivery", delivery, "--", path}, nativeArgs...)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		fixture.code = owner.run(ctx, args)
		close(fixture.done)
	}()
	// The owner reaps its native child on every return path, so cancelling it
	// leaves no fixture process behind.
	t.Cleanup(func() {
		cancel()
		<-fixture.done
	})
	deadline := time.Now().Add(10 * time.Second)
	for fixture.id == "" && time.Now().Before(deadline) {
		code, out := fixture.command("", "recent", "--label", "steer-fixture")
		var doc struct {
			Result struct {
				Executions []struct {
					ID    string      `json:"id"`
					State model.State `json:"state"`
				} `json:"executions"`
			} `json:"result"`
		}
		// The inbox opens only once the launch is recorded as running.
		if code == 0 && json.Unmarshal([]byte(out), &doc) == nil && len(doc.Result.Executions) == 1 && doc.Result.Executions[0].State == model.StateRunning {
			fixture.id = doc.Result.Executions[0].ID
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if fixture.id == "" {
		t.Fatalf("execution never became running: %s %s", stdout.String(), stderr.String())
	}
	return fixture
}

func (f *steerFixture) command(stdin string, args ...string) (int, string) {
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if stdin != "" {
		a.stdin = strings.NewReader(stdin)
		a.stdinIsTerminal = func() bool { return false }
	}
	code := a.run(context.Background(), append([]string{"--output", "json", "--journal", f.journal}, args...))
	return code, stdout.String()
}

func (f *steerFixture) waitOwner() int {
	select {
	case <-f.done:
		return f.code
	case <-time.After(20 * time.Second):
		f.t.Fatal("owning run did not return")
		return -1
	}
}

func (f *steerFixture) assertMessageNeverPersisted() {
	f.t.Helper()
	if _, events := f.command("", "events", f.id); strings.Contains(events, steerSecret) {
		f.t.Fatalf("events leaked the steering message: %s", events)
	}
	raw, err := os.ReadFile(f.journal)
	if err != nil {
		f.t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(steerSecret)) {
		f.t.Fatal("journal stored the steering message")
	}
	if entries, err := os.ReadDir(filepath.Join(filepath.Dir(f.journal), "steer")); err == nil && len(entries) != 0 {
		f.t.Fatalf("steer spool was not cleaned up: %v", entries)
	}
}

func TestSteerLiveInputFromAnotherInvocation(t *testing.T) {
	fixture := startSteerFixture(t, steerLiveAgent, "claude-code", "stream", "--print", "--output-format", "stream-json", "--input-format", "stream-json", "--replay-user-messages")
	code, plan := fixture.command(steerSecret, "steer", fixture.id, "--prompt-stdin", "--plan")
	if code != 0 || !strings.Contains(plan, `"delivery":"live_input"`) || !strings.Contains(plan, `"requires_allow_interrupt":false`) || strings.Contains(plan, steerSecret) {
		t.Fatalf("plan exit=%d output=%s", code, plan)
	}
	code, out := fixture.command(steerSecret, "steer", fixture.id, "--prompt-stdin", "--idempotency-key", "redirect-1")
	if code != 0 || !strings.Contains(out, `"status":"delivered"`) || !strings.Contains(out, `"delivery":"live_input"`) || strings.Contains(out, steerSecret) {
		t.Fatalf("steer exit=%d output=%s", code, out)
	}
	if code := fixture.waitOwner(); code != 0 {
		t.Fatalf("owner exit=%d", code)
	}
	if code, result := fixture.command("", "result", fixture.id); code != 0 || !strings.Contains(result, "turn-2") {
		t.Fatalf("the answer to the steering message must be the final result: exit=%d %s", code, result)
	}
	_, events := fixture.command("", "events", fixture.id)
	for _, want := range []string{`"source_state":"steer_delivered"`, `"source_state":"turn_completed"`, `"message_sha256":"sha256:`, `"message_bytes":23`} {
		if !strings.Contains(events, want) {
			t.Fatalf("events missing %s: %s", want, events)
		}
	}
	fixture.assertMessageNeverPersisted()
	// Replaying the same key and message reports the recorded delivery; a
	// new request against the terminal execution is refused.
	code, replay := fixture.command(steerSecret, "steer", fixture.id, "--prompt-stdin", "--idempotency-key", "redirect-1")
	if code != 0 || !strings.Contains(replay, `"replayed":true`) || !strings.Contains(replay, `"status":"delivered"`) {
		t.Fatalf("replayed steer exit=%d output=%s", code, replay)
	}
	code, late := fixture.command(steerSecret, "steer", fixture.id, "--prompt-stdin")
	if code == 0 || !strings.Contains(late, `"code":"invalid_state"`) {
		t.Fatalf("steer after terminal exit=%d output=%s", code, late)
	}
}

func TestSteerInterruptResumeRequiresExplicitPermission(t *testing.T) {
	fixture := startSteerFixture(t, steerResumableAgent, "codex", "stdin", "exec", "--json", "-")
	code, refused := fixture.command(steerSecret, "steer", fixture.id, "--prompt-stdin")
	if code == 0 || !strings.Contains(refused, `"code":"capability_unavailable"`) || !strings.Contains(refused, "steer_interrupt_not_permitted") {
		t.Fatalf("steer without permission exit=%d output=%s", code, refused)
	}
	if _, status := fixture.command("", "status", fixture.id); !strings.Contains(status, `"state":"running"`) {
		t.Fatalf("a refused steer must not disturb the execution: %s", status)
	}
	code, out := fixture.command(steerSecret, "steer", fixture.id, "--prompt-stdin", "--allow-interrupt")
	if code != 0 || !strings.Contains(out, `"delivery":"interrupt_resume"`) || !strings.Contains(out, `"status":"delivered"`) {
		t.Fatalf("steer exit=%d output=%s", code, out)
	}
	if code := fixture.waitOwner(); code != 0 {
		t.Fatalf("owner exit=%d", code)
	}
	if code, result := fixture.command("", "result", fixture.id); code != 0 || !strings.Contains(result, "resumed") {
		t.Fatalf("result exit=%d %s", code, result)
	}
	fixture.assertMessageNeverPersisted()
}

func TestSteerRejectsExecutionsWithoutARoute(t *testing.T) {
	fixture := startSteerFixture(t, "#!/bin/sh\nIFS= read -r prompt\nsleep 60\n", "generic-process", "stdin")
	code, out := fixture.command(steerSecret, "steer", fixture.id, "--prompt-stdin", "--allow-interrupt")
	if code == 0 || !strings.Contains(out, `"code":"capability_unavailable"`) || !strings.Contains(out, "no verified steering route") {
		t.Fatalf("steer exit=%d output=%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(fixture.journal), "steer")); !os.IsNotExist(err) {
		t.Fatalf("a route-less execution must not open a steer inbox: %v", err)
	}
}

func TestSteerWithdrawsARequestTheOwnerNeverTakes(t *testing.T) {
	now := time.Now().UTC()
	path := filepath.Join(t.TempDir(), "state", "journal.db")
	journal, err := store.Open(path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	fresh := 30
	execution, _, err := journal.CreateExecution(context.Background(), model.Execution{Authority: model.AuthorityNative, Adapter: "claude-code", Mode: model.ModeDirect, Acquisition: model.AcquisitionLaunched, State: model.StateRunning, Liveness: model.LivenessAlive, SourceBindings: []model.SourceBinding{},
		Capabilities: model.CapabilitySnapshot{NegotiatedAt: now, AdapterVersion: "test", Items: []model.CapabilityItem{{Name: "steer", Status: "supported", Source: "manifest", SemanticsVersion: 1, Constraints: map[string]any{"delivery": "live_input"}}}},
		Observation:  model.Observation{Source: model.ObservationNativeStream, Integrity: model.IntegrityVerified, ObservedAt: now, FreshForSeconds: &fresh}}, contracts.MutationKey{})
	journal.Close()
	if err != nil {
		t.Fatal(err)
	}
	fixture := &steerFixture{t: t, journal: path, id: execution.ID.String()}
	// No owner has opened an inbox: nothing is queued.
	code, out := fixture.command(steerSecret, "steer", fixture.id, "--prompt-stdin", "--timeout", "300ms")
	if code == 0 || !strings.Contains(out, "steer_inbox_missing") {
		t.Fatalf("steer without an inbox exit=%d output=%s", code, out)
	}
	spool := steerSpoolDir(path, execution.ID)
	if err := os.MkdirAll(spool, 0o700); err != nil {
		t.Fatal(err)
	}
	code, out = fixture.command(steerSecret, "steer", fixture.id, "--prompt-stdin", "--timeout", "300ms")
	if code == 0 || !strings.Contains(out, `"code":"timeout"`) || !strings.Contains(out, "nothing was delivered") {
		t.Fatalf("unserviced steer exit=%d output=%s", code, out)
	}
	if entries, _ := os.ReadDir(spool); len(entries) != 0 {
		t.Fatalf("withdrawn request left files behind: %v", entries)
	}
}

func TestSteerFlagsFailClosed(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"exec-a"},
		{"exec-a", "--prompt-file", "a.md", "--prompt-stdin"},
		{"exec-a", "exec-b", "--prompt-stdin"},
		{"exec-a", "--prompt-stdin", "--timeout", "0s"},
		{"exec-a", "--prompt-stdin", "--force"},
	} {
		if _, _, problem := parseSteer(args); problem == nil {
			t.Fatalf("args %v must be rejected", args)
		}
	}
}

// Delegate builds the native argv, so its recipes decide which steering route
// a delegated agent gets. This guards against a recipe flag that silently
// drops a harness off its verified route.
func TestDelegateRecipesNegotiateTheirSteeringRoute(t *testing.T) {
	for _, test := range []struct {
		input  launchrecipe.Input
		status adapter.CapabilityStatus
		route  string
	}{
		{input: launchrecipe.Input{Harness: "claude-code", Model: "m"}, status: adapter.CapabilitySupported, route: "live_input"},
		{input: launchrecipe.Input{Harness: "codex", Model: "m", Speed: "regular", Effort: "high", UnattendedCodingPermissions: true}, status: adapter.CapabilityDegraded, route: "interrupt_resume"},
		{input: launchrecipe.Input{Harness: "codex", Model: "m", Access: launchrecipe.AccessReadOnly}, status: adapter.CapabilityDegraded, route: "interrupt_resume"},
		{input: launchrecipe.Input{Harness: "cursor", Model: "m", CursorWorkspaceTrust: true}, status: adapter.CapabilityUnavailable},
		{input: launchrecipe.Input{Harness: "omp", Model: "m"}, status: adapter.CapabilityUnavailable},
		{input: launchrecipe.Input{Harness: "devin", Model: "swe-2-high"}, status: adapter.CapabilityUnavailable},
		{input: launchrecipe.Input{Harness: "zcode", Model: "zai/glm-5.3"}, status: adapter.CapabilityUnavailable},
	} {
		recipe, err := launchrecipe.Build(test.input)
		if err != nil {
			t.Fatalf("%s: %v", test.input.Harness, err)
		}
		runtime, _, problem := testApp(&bytes.Buffer{}, &bytes.Buffer{}).runtimeAdapter(common{}, test.input.Harness, "", "")
		if problem != nil {
			t.Fatalf("%s: %v", test.input.Harness, problem)
		}
		argv, promptArgs := recipe.Argv, 0
		if recipe.PromptDelivery == launchrecipe.PromptDeliveryArgv {
			argv, promptArgs = append(append([]string(nil), argv...), "task"), 1
		}
		got := adapter.NegotiateSteer(runtime.Manifest(), argv, recipe.PromptDelivery, promptArgs)
		if route, _ := got.Constraints["delivery"].(string); got.Status != test.status || route != test.route {
			t.Fatalf("%s %v: steer=%s/%q (%s), want %s/%q", test.input.Harness, recipe.Argv, got.Status, route, got.Reason, test.status, test.route)
		}
	}
}

func TestSteerConcurrentRetriesOfOneRequestDeliverOnce(t *testing.T) {
	fixture := startSteerFixture(t, steerLiveAgent, "claude-code", "stream", "--print", "--output-format", "stream-json", "--input-format", "stream-json", "--replay-user-messages")
	codes := make([]int, 3)
	outputs := make([]string, 3)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], outputs[i] = fixture.command(steerSecret, "steer", fixture.id, "--prompt-stdin", "--idempotency-key", "same-request")
		}(i)
	}
	wg.Wait()
	for i := range codes {
		if codes[i] != 0 || !strings.Contains(outputs[i], `"status":"delivered"`) {
			t.Fatalf("retry %d exit=%d output=%s", i, codes[i], outputs[i])
		}
	}
	if code := fixture.waitOwner(); code != 0 {
		t.Fatalf("owner exit=%d", code)
	}
	_, events := fixture.command("", "events", fixture.id)
	if delivered := strings.Count(events, `"source_state":"steer_delivered"`); delivered != 1 {
		t.Fatalf("the message was delivered %d times: %s", delivered, events)
	}
}
