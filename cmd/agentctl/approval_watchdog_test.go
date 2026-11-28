package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/config"
	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
)

func TestNativeApprovalWaitBoundaryAndReset(t *testing.T) {
	start := time.Date(2026, 11, 25, 15, 0, 0, 0, time.UTC)
	since := updateNativeApprovalWait(nil, []adapter.Event{{
		Kind: "attention", State: adapter.StateAttention, SourceState: "permission.required",
		ObservedAt: start, Payload: map[string]any{"attention_kind": "permission"},
	}}, start)
	if since == nil || !since.Equal(start) {
		t.Fatalf("approval wait started at %v, want %v", since, start)
	}
	if nativeApprovalWaitExpired(since, start.Add(nativeApprovalWaitTimeout-time.Nanosecond)) {
		t.Fatal("approval wait expired before the boundary")
	}
	if !nativeApprovalWaitExpired(since, start.Add(nativeApprovalWaitTimeout)) {
		t.Fatal("approval wait did not expire at the 10-minute boundary")
	}
	for _, event := range []adapter.Event{
		{Kind: "health", State: adapter.StateRunning, SourceState: "stderr"},
		{Kind: "health", State: adapter.StateRunning, SourceState: "malformed_output"},
	} {
		got := updateNativeApprovalWait(since, []adapter.Event{event}, start.Add(time.Minute))
		if got == nil || !got.Equal(start) {
			t.Fatalf("unrelated running event %q reset approval timer: %v", event.SourceState, got)
		}
		since = got
	}
	if nativeApprovalWaitExpired(since, start.Add(nativeApprovalWaitTimeout-time.Nanosecond)) {
		t.Fatal("unrelated health events expired approval wait early")
	}
	if !nativeApprovalWaitExpired(since, start.Add(nativeApprovalWaitTimeout)) {
		t.Fatal("unrelated health events moved the exact 10-minute boundary")
	}
	if got := updateNativeApprovalWait(since, []adapter.Event{{Kind: "started", State: adapter.StateRunning, SourceState: "item.started"}}, start.Add(time.Minute)); got != nil {
		t.Fatalf("resolved approval retained timer: %v", got)
	}
}

func TestSyntheticDelegateApprovalWatchdog(t *testing.T) {
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	initRepo := exec.Command("git", "init", "--quiet", repo)
	if out, err := initRepo.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	baseCodexHome := filepath.Join(root, "codex-home")
	if err := os.Mkdir(baseCodexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCodexCheckoutTrust(t, baseCodexHome, repo)
	launcher := filepath.Join(root, "codex")
	allowedMarker := filepath.Join(root, "allowed-work")
	approvalMarker := filepath.Join(root, "approval-wait")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then
  printf 'codex-cli 0.170.0\n'
  exit 0
fi
rules=$(find "$CODEX_HOME/rules" -name 'agentctl-delegate-generated-*.rules' -print -quit)
[ -n "$rules" ] || exit 71
grep -Fq 'pattern=["git", "status"]' "$rules" || exit 71
if grep -Fq 'pattern=["git", "push"]' "$rules"; then exit 72; fi
git -C "$SYNTHETIC_REPO" status --short >/dev/null || exit 73
printf 'allowed command completed\n' > "$SYNTHETIC_ALLOWED_MARKER"
printf '%s\n' '{"type":"permission.required","thread_id":"synthetic","message":"approval required"}'
printf 'unrelated background warning\n' >&2
printf '%s\n' "$AGENTCTL_EXECUTION_ID" > "$SYNTHETIC_APPROVAL_MARKER"
while :; do sleep 1; done
`
	if err := os.WriteFile(launcher, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", baseCodexHome)
	t.Setenv("SYNTHETIC_REPO", repo)
	t.Setenv("SYNTHETIC_ALLOWED_MARKER", allowedMarker)
	t.Setenv("SYNTHETIC_APPROVAL_MARKER", approvalMarker)
	configPath := filepath.Join(root, "config.json")
	value := config.Config{
		SchemaVersion: 1, DefaultProfile: "test", Profiles: map[string]config.Profile{"test": {
			Adapters:         map[string]config.Adapter{"codex": {Executable: launcher}},
			Delegation:       &config.DelegationPolicy{UnattendedCodingPermissions: true},
			AgentPreferences: &config.AgentPreferences{Mode: "advisory", Preferred: []config.AgentPreference{{Agent: "codex", Family: "synthetic", Model: "synthetic-model", Default: true}}},
		}},
	}
	configBytes, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, configBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	requestPath := filepath.Join(root, "request.json")
	if err := os.WriteFile(requestPath, []byte(`{"schema_version":1,"request_key":"approval-watchdog","selector":{"model":"synthetic-model"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(root, "state", "journal.db")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	var fakeNow atomic.Int64
	a.now = func() time.Time {
		if value := fakeNow.Load(); value != 0 {
			return time.Unix(0, value).UTC()
		}
		return time.Now().UTC()
	}
	a.stdin = strings.NewReader("synthetic delegated task")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	done := make(chan int, 1)
	go func() {
		done <- a.run(ctx, []string{"--output", "json", "--config", configPath, "--journal", journalPath, "delegate", "--cwd", repo, "--request-file", requestPath, "--prompt-stdin", "--wait"})
	}()
	waitForPathOrExit(t, allowedMarker, done)
	if got, err := os.ReadFile(allowedMarker); err != nil || string(got) != "allowed command completed\n" {
		t.Fatalf("allowlisted work result=%q err=%v", got, err)
	}
	waitForPathOrExit(t, approvalMarker, done)
	identifier, err := os.ReadFile(approvalMarker)
	if err != nil {
		t.Fatal(err)
	}
	id, err := ids.ParseExecutionID(strings.TrimSpace(string(identifier)))
	if err != nil {
		t.Fatalf("synthetic execution id: %q: %v", identifier, err)
	}
	attentionAt := waitForAttentionObservation(t, a, journalPath, id)
	waitForSourceEvent(t, a, journalPath, id, "stderr")
	fakeNow.Store(attentionAt.Add(nativeApprovalWaitTimeout - time.Nanosecond).UnixNano())
	select {
	case code := <-done:
		t.Fatalf("delegate exited before the approval boundary: code=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	case <-time.After(50 * time.Millisecond):
	}
	fakeNow.Store(attentionAt.Add(nativeApprovalWaitTimeout).UnixNano())
	var code int
	select {
	case code = <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("delegate did not terminate at the approval boundary: stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	if code != output.ExitCodeFor(output.CodeExecutionFailed) {
		t.Fatalf("delegate exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), nativeApprovalWaitDiagnostic) || !strings.Contains(stdout.String(), "10 minutes") {
		t.Fatalf("delegate did not return the stable timeout failure: %s", stdout.String())
	}

	stdout.Reset()
	if awaitCode := a.run(ctx, []string{"--output", "json", "--journal", journalPath, "await", id.String()}); awaitCode != output.ExitCodeFor(output.CodeExecutionFailed) || !strings.Contains(stdout.String(), nativeApprovalWaitDiagnostic) {
		t.Fatalf("await did not report parent-visible failure: exit=%d output=%s stderr=%s", awaitCode, stdout.String(), stderr.String())
	}
	stdout.Reset()
	if resultCode := a.run(ctx, []string{"--output", "json", "--journal", journalPath, "result", id.String()}); resultCode != 0 || !strings.Contains(stdout.String(), nativeApprovalWaitDiagnostic) {
		t.Fatalf("result collection lost failure: exit=%d output=%s", resultCode, stdout.String())
	}
	journal, problem := a.openRead(common{journalPath: journalPath})
	if problem != nil {
		t.Fatal(problem)
	}
	defer journal.Close()
	stored, err := journal.GetOutcome(ctx, id)
	if err != nil || stored.State != model.StateFailed || stored.Failure == nil || stored.Failure.Code != nativeApprovalWaitDiagnostic {
		t.Fatalf("stored failure=%+v err=%v", stored, err)
	}
}

func waitForAttentionObservation(t *testing.T, a *app, journalPath string, id ids.ExecutionID) time.Time {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		journal, problem := a.openRead(common{journalPath: journalPath})
		if problem == nil {
			events, err := journal.ListEvents(context.Background(), id, contracts.EventQuery{})
			_ = journal.Close()
			if err == nil {
				for _, event := range events {
					if event.State != nil && *event.State == model.StateAttention && !event.ObservedAt.IsZero() {
						return event.ObservedAt
					}
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("native approval attention was not journaled")
	return time.Time{}
}

func waitForSourceEvent(t *testing.T, a *app, journalPath string, id ids.ExecutionID, sourceState string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		journal, problem := a.openRead(common{journalPath: journalPath})
		if problem == nil {
			events, err := journal.ListEvents(context.Background(), id, contracts.EventQuery{})
			_ = journal.Close()
			if err == nil {
				for _, event := range events {
					if event.SourceState != nil && *event.SourceState == sourceState {
						return
					}
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("native event %q was not journaled", sourceState)
}

func TestSyntheticDelegatePreExecutionDenialFailsAndCancels(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "--quiet", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	codexHome := filepath.Join(root, "codex-home")
	if err := os.Mkdir(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCodexCheckoutTrust(t, codexHome, repo)
	launcher := filepath.Join(root, "codex")
	startedMarker := filepath.Join(root, "started")
	stoppedMarker := filepath.Join(root, "stopped")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then
  printf 'codex-cli 0.170.0\n'
  exit 0
fi
trap 'printf stopped > "$SYNTHETIC_STOPPED_MARKER"; exit 0' TERM
thread_id='550e8400-e29b-41d4-a716-446655440004'
mkdir -p "$CODEX_HOME/sessions/2026/11/25"
rollout="$CODEX_HOME/sessions/2026/11/25/rollout-2026-11-25T00-00-00-00000000-$thread_id.jsonl"
printf '%s\n' "{\"type\":\"session_meta\",\"payload\":{\"id\":\"$thread_id\"}}" > "$rollout"
printf '%s\n' "{\"type\":\"thread.started\",\"thread_id\":\"$thread_id\"}"
printf '%s\n' "$AGENTCTL_EXECUTION_ID" > "$SYNTHETIC_STARTED_MARKER"
sleep 0.05
printf '%s\n' '{"type":"response_item","payload":{"type":"function_call","call_id":"call-denied","name":"exec_command","arguments":"{\"cmd\":\"make test\",\"sandbox_permissions\":\"require_escalated\"}"}}' >> "$rollout"
printf '%s\n' '{"type":"response_item","payload":{"type":"function_call_output","call_id":"call-denied","output":"approval policy is Never; reject command — you cannot ask for escalated permissions if the approval policy is Never"}}' >> "$rollout"
printf '%s\n' "{\"type\":\"item.completed\",\"thread_id\":\"$thread_id\",\"item\":{\"id\":\"message-1\",\"type\":\"agent_message\",\"text\":\"I could not run the command: approval policy is Never; reject command.\"}}"
printf '%s\n' "{\"type\":\"turn.completed\",\"thread_id\":\"$thread_id\"}"
while :; do sleep 1; done
`
	if err := os.WriteFile(launcher, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("SYNTHETIC_STARTED_MARKER", startedMarker)
	t.Setenv("SYNTHETIC_STOPPED_MARKER", stoppedMarker)
	configPath := filepath.Join(root, "config.json")
	value := config.Config{
		SchemaVersion: 1, DefaultProfile: "test", Profiles: map[string]config.Profile{"test": {
			Adapters:         map[string]config.Adapter{"codex": {Executable: launcher}},
			Delegation:       &config.DelegationPolicy{UnattendedCodingPermissions: true},
			AgentPreferences: &config.AgentPreferences{Mode: "advisory", Preferred: []config.AgentPreference{{Agent: "codex", Family: "synthetic", Model: "synthetic-model", Default: true}}},
		}},
	}
	configBytes, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, configBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	requestPath := filepath.Join(root, "request.json")
	if err := os.WriteFile(requestPath, []byte(`{"schema_version":1,"request_key":"pre-execution-denial","selector":{"model":"synthetic-model"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(root, "state", "journal.db")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	a.stdin = strings.NewReader("synthetic delegated task")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	done := make(chan int, 1)
	go func() {
		done <- a.run(ctx, []string{"--output", "json", "--config", configPath, "--journal", journalPath, "delegate", "--cwd", repo, "--request-file", requestPath, "--prompt-stdin", "--wait"})
	}()
	waitForPathOrExit(t, startedMarker, done)
	identifier, err := os.ReadFile(startedMarker)
	if err != nil {
		t.Fatal(err)
	}
	id, err := ids.ParseExecutionID(strings.TrimSpace(string(identifier)))
	if err != nil {
		t.Fatalf("synthetic execution id %q: %v", identifier, err)
	}
	select {
	case code := <-done:
		if code != output.ExitCodeFor(output.CodeExecutionFailed) || !strings.Contains(stdout.String(), nativeCommandDeclinedDiagnostic) {
			t.Fatalf("pre-execution denial exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("pre-execution denial did not cancel promptly: stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	if _, err := os.Stat(stoppedMarker); err != nil {
		t.Fatalf("native process was not cancelled: %v", err)
	}
	stdout.Reset()
	if code := a.run(ctx, []string{"--output", "json", "--journal", journalPath, "await", id.String()}); code != output.ExitCodeFor(output.CodeExecutionFailed) || !strings.Contains(stdout.String(), nativeCommandDeclinedDiagnostic) {
		t.Fatalf("await did not return pre-execution denial: exit=%d output=%s", code, stdout.String())
	}
	stdout.Reset()
	if code := a.run(ctx, []string{"--output", "json", "--journal", journalPath, "result", id.String()}); code != 0 || !strings.Contains(stdout.String(), nativeCommandDeclinedDiagnostic) {
		t.Fatalf("result did not preserve pre-execution denial: exit=%d output=%s", code, stdout.String())
	}
	journal, problem := a.openRead(common{journalPath: journalPath})
	if problem != nil {
		t.Fatal(problem)
	}
	defer journal.Close()
	stored, err := journal.GetOutcome(ctx, id)
	if err != nil || stored.State != model.StateFailed || stored.Failure == nil || stored.Failure.Code != nativeCommandDeclinedDiagnostic || stored.Failure.Category != "permission" {
		t.Fatalf("stored pre-execution denial=%+v err=%v", stored, err)
	}
}

func writeCodexCheckoutTrust(t *testing.T, codexHome, repo string) {
	t.Helper()
	root, err := exec.Command("git", "-C", repo, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("resolve synthetic Git root: %v", err)
	}
	config := "[projects." + strconv.Quote(strings.TrimSpace(string(root))) + "]\ntrust_level = \"trusted\"\n"
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
}

func waitForPathOrExit(t *testing.T, path string, done <-chan int) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case code := <-done:
			t.Fatalf("delegate exited before %s: code=%d", path, code)
		case <-deadline:
			t.Fatalf("timed out waiting for %s", path)
		case <-time.After(10 * time.Millisecond):
		}
	}
}
