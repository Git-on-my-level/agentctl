package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

func writeDispatchFixture(t *testing.T, root, agents, runtimes string) (string, string) {
	t.Helper()
	capture := filepath.Join(root, "capture")
	path := filepath.Join(root, "fake-multica")
	body := `#!/bin/sh
set -eu
case "$*" in
  *"agent list --output json"*)
    printf '%s\n' "$AGENTCTL_TEST_AGENTS"
    ;;
  *"runtime list --output json"*)
    printf '%s\n' "$AGENTCTL_TEST_RUNTIMES"
    ;;
  *"event list"*)
	if [ -n "${AGENTCTL_TEST_EVENT_DELAY:-}" ]; then
	  : > "$AGENTCTL_TEST_CAPTURE.event.started"
	  if [ "${AGENTCTL_TEST_EVENT_IGNORE_TERM:-}" = "1" ]; then
	    trap '' TERM
	  fi
	  sleep "$AGENTCTL_TEST_EVENT_DELAY"
	fi
	if [ -n "${AGENTCTL_TEST_EVENT_FAILURES:-}" ]; then
	  count=0
	  if [ -f "$AGENTCTL_TEST_CAPTURE.event.failures" ]; then
	    count=$(wc -l < "$AGENTCTL_TEST_CAPTURE.event.failures" | tr -d ' ')
	  fi
	  if [ "$count" -lt "$AGENTCTL_TEST_EVENT_FAILURES" ]; then
	    printf '%s\n' failure >> "$AGENTCTL_TEST_CAPTURE.event.failures"
	    printf '%s\n' 'transient event-list failure' >&2
	    exit 19
	  fi
	fi
    printf '%s\n' "$AGENTCTL_TEST_EVENTS"
    ;;
  *"issue create"*)
    printf '%s\n' "$*" >> "$AGENTCTL_TEST_CAPTURE.argv"
    cat > "$AGENTCTL_TEST_CAPTURE.stdin"
	if [ -f "$AGENTCTL_TEST_CAPTURE.updated" ]; then
	  printf '%s\n' '{"id":"opaque-multica-issue","identifier":"SCA-932","status":"todo"}'
	else
	  printf '%s\n' '{"id":"opaque-multica-issue","identifier":"SCA-932","status":"backlog"}'
	fi
	;;
  *"issue update"*)
	printf '%s\n' "$*" >> "$AGENTCTL_TEST_CAPTURE.update.argv"
	if [ "${AGENTCTL_TEST_LOSE_UPDATE_RESPONSE_ONCE:-}" = "1" ] && [ ! -f "$AGENTCTL_TEST_CAPTURE.update.response-lost" ]; then
	  : > "$AGENTCTL_TEST_CAPTURE.updated"
	  : > "$AGENTCTL_TEST_CAPTURE.update.response-lost"
	  printf '%s\n' 'response lost after committed update' >&2
	  exit 18
	fi
	if [ "${AGENTCTL_TEST_FAIL_UPDATE_ONCE:-}" = "1" ] && [ ! -f "$AGENTCTL_TEST_CAPTURE.update.failed" ]; then
	  : > "$AGENTCTL_TEST_CAPTURE.update.failed"
	  printf '%s\n' 'transient update failure' >&2
	  exit 17
	fi
	: > "$AGENTCTL_TEST_CAPTURE.updated"
	printf '%s\n' '{"id":"opaque-multica-issue","identifier":"SCA-932","status":"todo"}'
    ;;
  *"issue get"*)
	if [ -f "$AGENTCTL_TEST_CAPTURE.updated" ]; then
	  printf '%s\n' '{"id":"opaque-multica-issue","identifier":"SCA-932","status":"todo"}'
	else
	  printf '%s\n' '{"id":"opaque-multica-issue","identifier":"SCA-932","status":"backlog"}'
	fi
	;;
  *)
    printf '%s\n' "unexpected argv: $*" >&2
    exit 9
    ;;
esac
`
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTCTL_TEST_CAPTURE", capture)
	t.Setenv("AGENTCTL_TEST_AGENTS", agents)
	t.Setenv("AGENTCTL_TEST_RUNTIMES", runtimes)
	t.Setenv("AGENTCTL_TEST_EVENTS", `{"events":[],"next_cursor":"0","has_more":false}`)
	return path, capture
}

func writeDispatchConfig(t *testing.T, path, multica string, preferred, hosts any) {
	t.Helper()
	value := map[string]any{
		"schema_version":  1,
		"default_profile": "fleet",
		"profiles": map[string]any{"fleet": map[string]any{
			"multica":           map[string]any{"executable": multica, "profile": "desktop", "workspace_id": "workspace-test", "server_url": "https://multica.example.test", "app_url": "https://multica.example.test"},
			"agent_preferences": map[string]any{"mode": "advisory", "preferred": preferred},
			"route":             map[string]any{"hosts": hosts, "placement": map[string]any{"kind": "multica"}},
		}},
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDispatchPlanResolvesLiveTargetWithoutWriting(t *testing.T) {
	root := t.TempDir()
	agents := `[{
      "id":"agent-sol","name":"M5 MBP Codex (Sol)","model":"gpt-5.6-sol",
      "runtime_id":"runtime-m5-codex","status":"working","archived_at":null
    }]`
	runtimes := `[{"id":"runtime-m5-codex","custom_name":"M5 MBP","provider":"codex","status":"online"}]`
	fake, capture := writeDispatchFixture(t, root, agents, runtimes)
	configPath := filepath.Join(root, "config", "config.json")
	journalPath := filepath.Join(root, "state", "journal.db")
	writeDispatchConfig(t, configPath, fake,
		[]any{map[string]any{"agent": "codex", "model": "gpt-5.6-sol", "speed": "regular", "use_for": "alias:sol"}},
		map[string]any{"m5": "m5-mbp"})
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	a.stdin = strings.NewReader("Review the exact diff.")
	a.stdinIsTerminal = func() bool { return false }
	code := a.run(context.Background(), []string{"--config", configPath, "--journal", journalPath, "dispatch", "--route", "m5 sol", "--title", "Review release", "--prompt-stdin", "--idempotency-key", "review-v1", "--plan"})
	if code != 0 {
		t.Fatalf("dispatch plan exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), `"runtime_verified":true`) || !strings.Contains(stdout.String(), `"tracked_execution":false`) || !strings.Contains(stdout.String(), `"side_effect_class":"read_only"`) || !strings.Contains(stdout.String(), `"agent_name":"M5 MBP Codex (Sol)"`) {
		t.Fatalf("plan output=%s", stdout.String())
	}
	if _, err := os.Stat(journalPath); !os.IsNotExist(err) {
		t.Fatalf("plan created journal: %v", err)
	}
	if _, err := os.Stat(capture + ".argv"); !os.IsNotExist(err) {
		t.Fatalf("plan created Multica issue: %v", err)
	}
}

func TestDispatchExecuteReplayTracksOneExecutionWithoutPromptRetention(t *testing.T) {
	root := t.TempDir()
	prompt := "Review the private implementation detail, but never retain this sentence."
	agents := `[{"id":"agent-sol","name":"M5 MBP Codex (Sol)","model":"gpt-5.6-sol","runtime_id":"runtime-m5-codex","status":"idle","archived_at":null}]`
	runtimes := `[{"id":"runtime-m5-codex","custom_name":"M5 MBP","provider":"codex","status":"online"}]`
	fake, capture := writeDispatchFixture(t, root, agents, runtimes)
	configPath := filepath.Join(root, "config", "config.json")
	journalPath := filepath.Join(root, "state", "journal.db")
	writeDispatchConfig(t, configPath, fake,
		[]any{map[string]any{"agent": "codex", "model": "gpt-5.6-sol", "speed": "regular", "use_for": "alias:sol"}},
		map[string]any{"m5": "m5-mbp"})
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	a.stdinIsTerminal = func() bool { return false }
	args := []string{"--config", configPath, "--journal", journalPath, "dispatch", "--route", "m5 sol", "--title", "Review release", "--prompt-stdin", "--idempotency-key", "review-v1", "--label", "release-review"}
	type dispatchResult struct {
		Execution model.Execution `json:"execution"`
		Reused    bool            `json:"reused"`
	}
	var first, second struct {
		Result dispatchResult `json:"result"`
	}
	for index, target := range []*struct {
		Result dispatchResult `json:"result"`
	}{&first, &second} {
		if index == 1 {
			t.Setenv("AGENTCTL_TEST_AGENTS", `[]`)
			t.Setenv("AGENTCTL_TEST_RUNTIMES", `[]`)
		}
		a.stdin = strings.NewReader(prompt)
		stdout.Reset()
		if code := a.run(context.Background(), args); code != 0 {
			t.Fatalf("dispatch %d exit=%d output=%s stderr=%s", index+1, code, stdout.String(), stderr.String())
		}
		if strings.Contains(stdout.String(), prompt) {
			t.Fatalf("dispatch output retained prompt: %s", stdout.String())
		}
		if err := json.Unmarshal(stdout.Bytes(), target); err != nil {
			t.Fatal(err)
		}
	}
	if first.Result.Execution.ID != second.Result.Execution.ID || first.Result.Reused || !second.Result.Reused {
		t.Fatalf("unexpected replay: first=%#v second=%#v", first.Result, second.Result)
	}
	if first.Result.Execution.Authority != model.AuthorityMultica || first.Result.Execution.Mode != model.ModeMultica || first.Result.Execution.Acquisition != model.AcquisitionLaunched || first.Result.Execution.State != model.StateWaiting {
		t.Fatalf("unexpected execution=%#v", first.Result.Execution)
	}
	for _, binding := range first.Result.Execution.SourceBindings {
		if binding.OpaqueID != nil {
			t.Fatalf("normal output exposed opaque binding: %#v", binding)
		}
	}
	capturedPrompt, err := os.ReadFile(capture + ".stdin")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(capturedPrompt, []byte(prompt)) || !bytes.Contains(capturedPrompt, []byte("prompt_digest: sha256:")) || !bytes.Contains(capturedPrompt, []byte("dispatch_status: todo")) {
		t.Fatalf("Multica stdin missing prompt or provenance: %q", capturedPrompt)
	}
	capturedArgv, err := os.ReadFile(capture + ".argv")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(capturedArgv, []byte(prompt)) || !bytes.Contains(capturedArgv, []byte("--assignee-id agent-sol")) || !bytes.Contains(capturedArgv, []byte("--client-key sha256:")) {
		t.Fatalf("unexpected Multica argv: %q", capturedArgv)
	}
	if !bytes.Contains(capturedArgv, []byte("--status backlog")) {
		t.Fatalf("idempotent creation did not use backlog: %q", capturedArgv)
	}
	updateArgv, err := os.ReadFile(capture + ".update.argv")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(strings.TrimSpace(string(updateArgv)), "\n") != 0 || !bytes.Contains(updateArgv, []byte("issue update opaque-multica-issue --status todo --output json")) {
		t.Fatalf("dispatch activation was not replay-safe: %q", updateArgv)
	}
	rawJournal, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(rawJournal, []byte(prompt)) {
		t.Fatal("journal retained raw dispatch prompt")
	}
	journal, err := store.Open(journalPath, store.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	executions, err := journal.ListExecutions(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(executions) != 1 || executions[0].ID != first.Result.Execution.ID {
		t.Fatalf("dispatch replay created extra executions: %#v", executions)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTCTL_TEST_EVENTS", `{"events":[{"type":"issue.updated","aggregate_kind":"issue","aggregate_id":"opaque-multica-issue","sequence":2,"payload":{"status":"done"}}],"next_cursor":"2","has_more":false}`)
	if problem := a.reprobeAwaitedMultica(context.Background(), common{journalPath: journalPath}, executions[0]); problem != nil {
		t.Fatalf("reprobe dispatched execution: %v", problem)
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journalPath, "recent", "--unreconciled"}); code != 0 || !strings.Contains(stdout.String(), first.Result.Execution.ID.String()) {
		t.Fatalf("terminal dispatch missing from unreconciled exit=%d output=%s", code, stdout.String())
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journalPath, "await", first.Result.Execution.ID.String(), "--timeout", "2s"}); code != 0 {
		t.Fatalf("await terminal dispatch exit=%d output=%s", code, stdout.String())
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journalPath, "result", first.Result.Execution.ID.String(), "--allow-empty"}); code != 0 || !strings.Contains(stdout.String(), `"availability":"unavailable_at_source"`) {
		t.Fatalf("empty Multica result exit=%d output=%s", code, stdout.String())
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journalPath, "recent", "--unreconciled"}); code != 0 || strings.Contains(stdout.String(), first.Result.Execution.ID.String()) {
		t.Fatalf("await did not acknowledge dispatch exit=%d output=%s", code, stdout.String())
	}
}

func TestAwaitRetriesTransientMulticaRefreshFailures(t *testing.T) {
	root := t.TempDir()
	agents := `[{"id":"agent-sol","name":"M5 MBP Codex (Sol)","model":"gpt-5.6-sol","runtime_id":"runtime-m5-codex","status":"idle","archived_at":null}]`
	runtimes := `[{"id":"runtime-m5-codex","custom_name":"M5 MBP","provider":"codex","status":"online"}]`
	fake, capture := writeDispatchFixture(t, root, agents, runtimes)
	configPath := filepath.Join(root, "config", "config.json")
	journalPath := filepath.Join(root, "state", "journal.db")
	writeDispatchConfig(t, configPath, fake,
		[]any{map[string]any{"agent": "codex", "model": "gpt-5.6-sol", "speed": "regular", "use_for": "alias:sol"}},
		map[string]any{"m5": "m5-mbp"})
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	a.stdin = strings.NewReader("Review the release candidate.")
	a.stdinIsTerminal = func() bool { return false }
	if code := a.run(context.Background(), []string{"--config", configPath, "--journal", journalPath, "dispatch", "--route", "m5 sol", "--title", "Review release", "--prompt-stdin", "--idempotency-key", "await-retry-v1"}); code != 0 {
		t.Fatalf("dispatch exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var dispatched struct {
		Result struct {
			Execution model.Execution `json:"execution"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &dispatched); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTCTL_TEST_EVENT_FAILURES", "2")
	t.Setenv("AGENTCTL_TEST_EVENTS", `{"events":[{"type":"issue.updated","aggregate_kind":"issue","aggregate_id":"opaque-multica-issue","sequence":2,"payload":{"status":"in_review"}}],"next_cursor":"2","has_more":false}`)
	now := time.Now().UTC()
	a.now = func() time.Time {
		now = now.Add(awaitMulticaReprobeInterval)
		return now
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journalPath, "await", dispatched.Result.Execution.ID.String(), "--no-timeout"}); code != output.ExitCodeFor(output.CodeAttentionRequired) {
		t.Fatalf("await exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	failures, err := os.ReadFile(capture + ".event.failures")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(strings.TrimSpace(string(failures)), "\n") + 1; got != 2 {
		t.Fatalf("transient failure count=%d log=%q", got, failures)
	}
}

func TestAwaitReturnsThirdConsecutiveMulticaRefreshFailure(t *testing.T) {
	root := t.TempDir()
	agents := `[{"id":"agent-sol","name":"M5 MBP Codex (Sol)","model":"gpt-5.6-sol","runtime_id":"runtime-m5-codex","status":"idle","archived_at":null}]`
	runtimes := `[{"id":"runtime-m5-codex","custom_name":"M5 MBP","provider":"codex","status":"online"}]`
	fake, capture := writeDispatchFixture(t, root, agents, runtimes)
	configPath := filepath.Join(root, "config", "config.json")
	journalPath := filepath.Join(root, "state", "journal.db")
	writeDispatchConfig(t, configPath, fake,
		[]any{map[string]any{"agent": "codex", "model": "gpt-5.6-sol", "speed": "regular", "use_for": "alias:sol"}},
		map[string]any{"m5": "m5-mbp"})
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	a.stdin = strings.NewReader("Review the release candidate.")
	a.stdinIsTerminal = func() bool { return false }
	if code := a.run(context.Background(), []string{"--config", configPath, "--journal", journalPath, "dispatch", "--route", "m5 sol", "--title", "Review release", "--prompt-stdin", "--idempotency-key", "await-failure-v1"}); code != 0 {
		t.Fatalf("dispatch exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var dispatched struct {
		Result struct {
			Execution model.Execution `json:"execution"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &dispatched); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTCTL_TEST_EVENT_FAILURES", "99")
	now := time.Now().UTC()
	a.now = func() time.Time {
		now = now.Add(awaitMulticaReprobeInterval)
		return now
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journalPath, "await", dispatched.Result.Execution.ID.String(), "--no-timeout"}); code != output.ExitCodeFor(output.CodeRemoteFailure) {
		t.Fatalf("await exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), `"consecutive_failures":3`) || !strings.Contains(stdout.String(), `"diagnostic_message":"Multica event list did not return a structured page"`) {
		t.Fatalf("await lost bounded failure diagnostic: %s", stdout.String())
	}
	failures, err := os.ReadFile(capture + ".event.failures")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(strings.TrimSpace(string(failures)), "\n") + 1; got != awaitMulticaMaxConsecutiveErrors {
		t.Fatalf("persistent failure count=%d log=%q", got, failures)
	}
}

func TestMulticaAwaitCancellationWinsOverRetry(t *testing.T) {
	root := t.TempDir()
	agents := `[{"id":"agent-sol","name":"M5 MBP Codex (Sol)","model":"gpt-5.6-sol","runtime_id":"runtime-m5-codex","status":"idle","archived_at":null}]`
	runtimes := `[{"id":"runtime-m5-codex","custom_name":"M5 MBP","provider":"codex","status":"online"}]`
	fake, capture := writeDispatchFixture(t, root, agents, runtimes)
	configPath := filepath.Join(root, "config", "config.json")
	journalPath := filepath.Join(root, "state", "journal.db")
	writeDispatchConfig(t, configPath, fake,
		[]any{map[string]any{"agent": "codex", "model": "gpt-5.6-sol", "speed": "regular", "use_for": "alias:sol"}},
		map[string]any{"m5": "m5-mbp"})
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	a.stdin = strings.NewReader("Review the release candidate.")
	a.stdinIsTerminal = func() bool { return false }
	if code := a.run(context.Background(), []string{"--config", configPath, "--journal", journalPath, "dispatch", "--route", "m5 sol", "--title", "Review release", "--prompt-stdin", "--idempotency-key", "await-cancel-v1"}); code != 0 {
		t.Fatalf("dispatch exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var dispatched struct {
		Result struct {
			Execution model.Execution `json:"execution"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &dispatched); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTCTL_TEST_EVENT_DELAY", "5")
	t.Setenv("AGENTCTL_TEST_EVENT_IGNORE_TERM", "1")
	stdout.Reset()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- a.run(ctx, []string{"--journal", journalPath, "await", dispatched.Result.Execution.ID.String(), "--no-timeout"})
	}()
	started := capture + ".event.started"
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Multica event reprobe did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case code := <-done:
		if code != output.ExitCodeFor(output.CodeExecutionCancelled) || !strings.Contains(stdout.String(), `"code":"execution_cancelled"`) {
			t.Fatalf("cancelled await exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled Multica await did not return")
	}
}

func TestMulticaAwaitTimeoutBoundsInflightReprobe(t *testing.T) {
	root := t.TempDir()
	agents := `[{"id":"agent-sol","name":"M5 MBP Codex (Sol)","model":"gpt-5.6-sol","runtime_id":"runtime-m5-codex","status":"idle","archived_at":null}]`
	runtimes := `[{"id":"runtime-m5-codex","custom_name":"M5 MBP","provider":"codex","status":"online"}]`
	fake, capture := writeDispatchFixture(t, root, agents, runtimes)
	configPath := filepath.Join(root, "config", "config.json")
	journalPath := filepath.Join(root, "state", "journal.db")
	writeDispatchConfig(t, configPath, fake,
		[]any{map[string]any{"agent": "codex", "model": "gpt-5.6-sol", "speed": "regular", "use_for": "alias:sol"}},
		map[string]any{"m5": "m5-mbp"})
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	a.stdin = strings.NewReader("Review the release candidate.")
	a.stdinIsTerminal = func() bool { return false }
	if code := a.run(context.Background(), []string{"--config", configPath, "--journal", journalPath, "dispatch", "--route", "m5 sol", "--title", "Review release", "--prompt-stdin", "--idempotency-key", "await-timeout-v1"}); code != 0 {
		t.Fatalf("dispatch exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var dispatched struct {
		Result struct {
			Execution model.Execution `json:"execution"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &dispatched); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTCTL_TEST_EVENT_DELAY", "5")
	t.Setenv("AGENTCTL_TEST_EVENT_IGNORE_TERM", "1")
	stdout.Reset()
	startedAt := time.Now()
	if code := a.run(context.Background(), []string{"--journal", journalPath, "await", dispatched.Result.Execution.ID.String(), "--timeout", "1s"}); code != output.ExitCodeFor(output.CodeTimeout) {
		t.Fatalf("await exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if elapsed := time.Since(startedAt); elapsed >= 2*time.Second {
		t.Fatalf("in-flight Multica reprobe overran await timeout: %s", elapsed)
	}
	if !strings.Contains(stdout.String(), `"code":"timeout"`) || !strings.Contains(stdout.String(), `"state":"waiting"`) {
		t.Fatalf("await timeout lost execution outcome: %s", stdout.String())
	}
	if _, err := os.Stat(capture + ".event.started"); err != nil {
		t.Fatalf("Multica event reprobe did not start: %v", err)
	}
}

func TestDispatchUsesRuntimePlacementNotMisleadingAgentName(t *testing.T) {
	root := t.TempDir()
	agents := `[{"id":"agent-grok","name":"M1 Studio Cursor (Grok 4.6)","model":"grok-4.6","runtime_id":"runtime-m4-cursor","status":"idle","archived_at":null}]`
	runtimes := `[{"id":"runtime-m4-cursor","custom_name":"M4 Mini","provider":"cursor","status":"online"}]`
	fake, capture := writeDispatchFixture(t, root, agents, runtimes)
	configPath := filepath.Join(root, "config", "config.json")
	writeDispatchConfig(t, configPath, fake,
		[]any{map[string]any{"agent": "cursor", "model": "cursor-grok-4.6-high", "speed": "regular", "use_for": "alias:grok,grok-4.6"}},
		map[string]any{"m1": "m1-mac-studio", "mini": "m4-mac-mini"})
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	a.stdin = strings.NewReader("Do the work.")
	a.stdinIsTerminal = func() bool { return false }
	code := a.run(context.Background(), []string{"--config", configPath, "dispatch", "--route", "m1 grok", "--title", "Wrong host guard", "--prompt-stdin", "--idempotency-key", "wrong-host", "--plan"})
	if code == 0 || !strings.Contains(stdout.String(), `"code":"capability_unavailable"`) || !strings.Contains(stdout.String(), `"host":"m1-mac-studio"`) {
		t.Fatalf("misleading target exit=%d output=%s", code, stdout.String())
	}
	if _, err := os.Stat(capture + ".argv"); !os.IsNotExist(err) {
		t.Fatalf("mismatched placement created issue: %v", err)
	}
}

func TestDispatchRecoversCommittedActivationWithoutMutableFleetResolution(t *testing.T) {
	root := t.TempDir()
	prompt := "Review the recovery boundary."
	agents := `[{"id":"agent-sol","name":"M5 MBP Codex (Sol)","model":"gpt-5.6-sol","runtime_id":"runtime-m5-codex","status":"idle","archived_at":null}]`
	runtimes := `[{"id":"runtime-m5-codex","custom_name":"M5 MBP","provider":"codex","status":"online"}]`
	fake, capture := writeDispatchFixture(t, root, agents, runtimes)
	t.Setenv("AGENTCTL_TEST_LOSE_UPDATE_RESPONSE_ONCE", "1")
	configPath := filepath.Join(root, "config", "config.json")
	journalPath := filepath.Join(root, "state", "journal.db")
	writeDispatchConfig(t, configPath, fake,
		[]any{map[string]any{"agent": "codex", "model": "gpt-5.6-sol", "speed": "regular", "use_for": "alias:sol"}},
		map[string]any{"m5": "m5-mbp"})
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	a.stdinIsTerminal = func() bool { return false }
	args := []string{"--config", configPath, "--journal", journalPath, "dispatch", "--route", "m5 sol", "--title", "Recovery review", "--prompt-stdin", "--idempotency-key", "recovery-v1"}
	a.stdin = strings.NewReader(prompt)
	if code := a.run(context.Background(), args); code != 11 || !strings.Contains(stdout.String(), `"execution_id":"exec-`) {
		t.Fatalf("first recovery dispatch exit=%d output=%s", code, stdout.String())
	}
	journal, err := store.Open(journalPath, store.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	executions, err := journal.ListExecutions(context.Background(), false)
	if err != nil || len(executions) != 1 || executions[0].State != model.StateStarting {
		t.Fatalf("prepared executions=%#v err=%v", executions, err)
	}
	preparedID := executions[0].ID
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTCTL_TEST_AGENTS", `[]`)
	t.Setenv("AGENTCTL_TEST_RUNTIMES", `[]`)
	stdout.Reset()
	a.stdin = strings.NewReader(prompt)
	if code := a.run(context.Background(), args); code != 0 {
		t.Fatalf("recovery retry exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), preparedID.String()) || !strings.Contains(stdout.String(), `"reused":true`) {
		t.Fatalf("recovery did not reuse prepared execution: %s", stdout.String())
	}
	createArgv, err := os.ReadFile(capture + ".argv")
	if err != nil || strings.Count(strings.TrimSpace(string(createArgv)), "\n") != 0 {
		t.Fatalf("authority create was not retried exactly: %q err=%v", createArgv, err)
	}
	updateArgv, err := os.ReadFile(capture + ".update.argv")
	if err != nil || strings.Count(strings.TrimSpace(string(updateArgv)), "\n") != 0 {
		t.Fatalf("authority activation was not recovered exactly: %q err=%v", updateArgv, err)
	}
}

func TestDispatchRejectsUnavailableAgentBeforeIssueCreation(t *testing.T) {
	root := t.TempDir()
	agents := `[{"id":"agent-sol","name":"M5 MBP Codex (Sol)","model":"gpt-5.6-sol","runtime_id":"runtime-m5-codex","status":"offline","archived_at":null}]`
	runtimes := `[{"id":"runtime-m5-codex","custom_name":"M5 MBP","provider":"codex","status":"online"}]`
	fake, capture := writeDispatchFixture(t, root, agents, runtimes)
	configPath := filepath.Join(root, "config", "config.json")
	writeDispatchConfig(t, configPath, fake,
		[]any{map[string]any{"agent": "codex", "model": "gpt-5.6-sol", "speed": "regular", "use_for": "alias:sol"}},
		map[string]any{"m5": "m5-mbp"})
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	a.stdin = strings.NewReader("Review the change.")
	a.stdinIsTerminal = func() bool { return false }
	code := a.run(context.Background(), []string{"--config", configPath, "dispatch", "--route", "m5 sol", "--title", "Unavailable guard", "--prompt-stdin", "--idempotency-key", "unavailable-agent", "--plan"})
	if code == 0 || !strings.Contains(stdout.String(), `"code":"capability_unavailable"`) {
		t.Fatalf("unavailable target exit=%d output=%s", code, stdout.String())
	}
	if _, err := os.Stat(capture + ".argv"); !os.IsNotExist(err) {
		t.Fatalf("unavailable agent created issue: %v", err)
	}
}

func TestDispatchReplayDoesNotRegressAdvancedIssue(t *testing.T) {
	root := t.TempDir()
	agents := `[{"id":"agent-sol","name":"M5 MBP Codex (Sol)","model":"gpt-5.6-sol","runtime_id":"runtime-m5-codex","status":"working","archived_at":null}]`
	runtimes := `[{"id":"runtime-m5-codex","custom_name":"M5 MBP","provider":"codex","status":"online"}]`
	fake, capture := writeDispatchFixture(t, root, agents, runtimes)
	if err := os.WriteFile(capture+".updated", []byte("already advanced"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config", "config.json")
	writeDispatchConfig(t, configPath, fake,
		[]any{map[string]any{"agent": "codex", "model": "gpt-5.6-sol", "speed": "regular", "use_for": "alias:sol"}},
		map[string]any{"m5": "m5-mbp"})
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	a.stdin = strings.NewReader("Review the change.")
	a.stdinIsTerminal = func() bool { return false }
	code := a.run(context.Background(), []string{"--config", configPath, "--journal", filepath.Join(root, "state", "journal.db"), "dispatch", "--route", "m5 sol", "--title", "Replay guard", "--prompt-stdin", "--idempotency-key", "advanced-replay"})
	if code != 0 {
		t.Fatalf("advanced replay exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(capture + ".update.argv"); !os.IsNotExist(err) {
		t.Fatalf("advanced issue status was mutated: %v", err)
	}
}

func TestDispatchRequiresReplayKeyAndExactPromptSource(t *testing.T) {
	for _, args := range [][]string{
		{"dispatch", "--route", "m5 sol", "--title", "Review", "--prompt-stdin"},
		{"dispatch", "--route", "m5 sol", "--title", "Review", "--idempotency-key", "key"},
		{"dispatch", "--route", "m5 sol", "--title", "Review", "--prompt-stdin", "--prompt-file", "task.md", "--idempotency-key", "key"},
		{"dispatch", "--route", "m5 sol", "--title", "Review", "--prompt-stdin", "--idempotency-key", "key", "--status", "tood"},
	} {
		var stdout, stderr bytes.Buffer
		a := testApp(&stdout, &stderr)
		if code := a.run(context.Background(), args); code != 2 || !strings.Contains(stdout.String(), `"code":"usage"`) {
			t.Fatalf("args=%v exit=%d output=%s", args, code, stdout.String())
		}
	}
}
