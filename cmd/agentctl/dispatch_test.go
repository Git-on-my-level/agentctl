package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
	"github.com/Git-on-my-level/agentctl/internal/supervisor"
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
    if [ -n "${AGENTCTL_TEST_PROBE_BINARY:-}" ]; then
      "$AGENTCTL_TEST_PROBE_BINARY" -test.run=^TestJournalProcessProbe$ >&2
    fi
    if [ "${AGENTCTL_TEST_CREATE_BARRIER:-}" = "1" ]; then
      : > "$AGENTCTL_TEST_CAPTURE.create.$$"
      while [ ! -f "$AGENTCTL_TEST_CAPTURE.release" ]; do sleep 0.02; done
    fi
 if [ -n "${AGENTCTL_TEST_CREATE_JSON:-}" ]; then printf '%s\n' "$AGENTCTL_TEST_CREATE_JSON"; exit 0; fi
 if [ -n "${AGENTCTL_TEST_CREATE_ERROR:-}" ]; then printf '%s\n' "$AGENTCTL_TEST_CREATE_ERROR" >&2; exit 2; fi
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
    if [ -n "${AGENTCTL_TEST_ISSUE_GET_DELAY:-}" ]; then
      : > "$AGENTCTL_TEST_CAPTURE.issue.get.started"
      if [ "${AGENTCTL_TEST_ISSUE_GET_IGNORE_TERM:-}" = "1" ]; then
        trap '' TERM
      fi
      sleep "$AGENTCTL_TEST_ISSUE_GET_DELAY"
    fi
    if [ -n "${AGENTCTL_TEST_ISSUE_GET_FAILURES:-}" ]; then
      count=0
      if [ -f "$AGENTCTL_TEST_CAPTURE.issue.get.failures" ]; then
        count=$(wc -l < "$AGENTCTL_TEST_CAPTURE.issue.get.failures" | tr -d ' ')
      fi
      if [ "$count" -lt "$AGENTCTL_TEST_ISSUE_GET_FAILURES" ]; then
        printf '%s\n' failure >> "$AGENTCTL_TEST_CAPTURE.issue.get.failures"
        printf '%s\n' 'transient issue get failure' >&2
        exit 19
      fi
    fi
    if [ -n "${AGENTCTL_TEST_READ_ERROR:-}" ]; then printf '%s\n' "$AGENTCTL_TEST_READ_ERROR" >&2; exit 23; fi
	issue_id="${AGENTCTL_TEST_ISSUE_GET_ID:-opaque-multica-issue}"
	issue_ws="${AGENTCTL_TEST_ISSUE_GET_WORKSPACE:-workspace-test}"
	if [ -n "${AGENTCTL_TEST_ISSUE_STATUS:-}" ]; then
	  printf '%s\n' "{\"id\":\"${issue_id}\",\"identifier\":\"SCA-932\",\"workspace_id\":\"${issue_ws}\",\"status\":\"${AGENTCTL_TEST_ISSUE_STATUS}\"}"
	elif [ -f "$AGENTCTL_TEST_CAPTURE.updated" ]; then
	  printf '%s\n' "{\"id\":\"${issue_id}\",\"identifier\":\"SCA-932\",\"workspace_id\":\"${issue_ws}\",\"status\":\"todo\"}"
	else
	  printf '%s\n' "{\"id\":\"${issue_id}\",\"identifier\":\"SCA-932\",\"workspace_id\":\"${issue_ws}\",\"status\":\"backlog\"}"
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
	t.Setenv("AGENTCTL_TEST_ISSUE_STATUS", "done")
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

func TestDispatchRejectsMismatchedIssueReadWithoutActivation(t *testing.T) {
	for _, env := range []struct{ key, value string }{
		{"AGENTCTL_TEST_ISSUE_GET_ID", "issue-foreign"},
		{"AGENTCTL_TEST_ISSUE_GET_WORKSPACE", "workspace-foreign"},
	} {
		t.Run(env.key, func(t *testing.T) {
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
			plan := a.run(context.Background(), []string{"--config", configPath, "--journal", journalPath, "dispatch", "--route", "m5 sol", "--title", "Review release", "--prompt-stdin", "--idempotency-key", "mismatch-v1", "--plan"})
			if plan != 0 {
				t.Fatalf("plan exit=%d output=%s stderr=%s", plan, stdout.String(), stderr.String())
			}
			if _, err := os.Stat(capture + ".argv"); !os.IsNotExist(err) {
				t.Fatalf("plan created Multica issue: %v", err)
			}
			t.Setenv(env.key, env.value)
			args := []string{"--config", configPath, "--journal", journalPath, "dispatch", "--route", "m5 sol", "--title", "Review release", "--prompt-stdin", "--idempotency-key", "mismatch-v1"}
			for attempt := 0; attempt < 2; attempt++ {
				a.stdin = strings.NewReader("Review the release candidate.")
				stdout.Reset()
				if code := a.run(context.Background(), args); code != output.ExitCodeFor(output.CodeRemoteFailure) {
					t.Fatalf("attempt %d exit=%d output=%s stderr=%s", attempt, code, stdout.String(), stderr.String())
				}
				if !strings.Contains(stdout.String(), `"identity_mismatch"`) {
					t.Fatalf("attempt %d output=%s", attempt, stdout.String())
				}
			}
			created, err := os.ReadFile(capture + ".argv")
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Count(strings.TrimSpace(string(created)), "\n") + 1; got != 1 {
				t.Fatalf("issue create ran %d times: %q", got, created)
			}
			if _, err := os.Stat(capture + ".update.argv"); !os.IsNotExist(err) {
				t.Fatalf("mismatched issue was activated: %v", err)
			}
			journal, err := store.Open(journalPath, store.Options{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			executions, err := journal.ListExecutions(context.Background(), false)
			if err != nil {
				t.Fatal(err)
			}
			if len(executions) != 1 {
				t.Fatalf("mismatch created %d executions", len(executions))
			}
			for _, item := range executions[0].Capabilities.Items {
				if item.Name == "snapshot" && item.Status == model.CapabilitySupported {
					t.Fatalf("unverified issue read claimed supported snapshot: %#v", item)
				}
			}
			if err := journal.Close(); err != nil {
				t.Fatal(err)
			}
		})
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
	t.Setenv("AGENTCTL_TEST_ISSUE_GET_FAILURES", "2")
	t.Setenv("AGENTCTL_TEST_ISSUE_STATUS", "in_review")
	now := time.Now().UTC()
	a.now = func() time.Time {
		now = now.Add(awaitMulticaReprobeInterval)
		return now
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journalPath, "await", dispatched.Result.Execution.ID.String(), "--no-timeout"}); code != output.ExitCodeFor(output.CodeAttentionRequired) {
		t.Fatalf("await exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	failures, err := os.ReadFile(capture + ".issue.get.failures")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(strings.TrimSpace(string(failures)), "\n") + 1; got != 2 {
		t.Fatalf("transient failure count=%d log=%q", got, failures)
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journalPath, "result", dispatched.Result.Execution.ID.String()}); code != output.ExitCodeFor(output.CodeInvalidState) || !strings.Contains(stdout.String(), `"events"`) || !strings.Contains(stdout.String(), `"--ignore-attention"`) {
		t.Fatalf("attention result action exit=%d output=%s", code, stdout.String())
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
	t.Setenv("AGENTCTL_TEST_ISSUE_GET_FAILURES", "99")
	now := time.Now().UTC()
	a.now = func() time.Time {
		now = now.Add(awaitMulticaReprobeInterval)
		return now
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--journal", journalPath, "await", dispatched.Result.Execution.ID.String(), "--no-timeout"}); code != output.ExitCodeFor(output.CodeRemoteFailure) {
		t.Fatalf("await exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), `"consecutive_failures":3`) || !strings.Contains(stdout.String(), `"diagnostic_message":"Multica issue get was rejected"`) {
		t.Fatalf("await lost bounded failure diagnostic: %s", stdout.String())
	}
	failures, err := os.ReadFile(capture + ".issue.get.failures")
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
	t.Setenv("AGENTCTL_TEST_ISSUE_GET_DELAY", "5")
	t.Setenv("AGENTCTL_TEST_ISSUE_GET_IGNORE_TERM", "1")
	stdout.Reset()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- a.run(ctx, []string{"--journal", journalPath, "await", dispatched.Result.Execution.ID.String(), "--no-timeout"})
	}()
	started := capture + ".issue.get.started"
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Multica issue reprobe did not start")
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
	t.Setenv("AGENTCTL_TEST_ISSUE_GET_DELAY", "5")
	t.Setenv("AGENTCTL_TEST_ISSUE_GET_IGNORE_TERM", "1")
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
	if _, err := os.Stat(capture + ".issue.get.started"); err != nil {
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
	if code == 0 || !strings.Contains(stdout.String(), `"code":"capability_unavailable"`) || !strings.Contains(stdout.String(), `"agent_status_unavailable":1`) {
		t.Fatalf("unavailable target exit=%d output=%s", code, stdout.String())
	}
	if _, err := os.Stat(capture + ".argv"); !os.IsNotExist(err) {
		t.Fatalf("unavailable agent created issue: %v", err)
	}
}

func TestDispatchRejectsOffPolicyModel(t *testing.T) {
	root := t.TempDir()
	agents := `[{"id":"agent-grok","name":"M5 Cursor Grok","model":"cursor-grok-4.6-high","runtime_id":"runtime-m5-cursor","status":"idle","archived_at":null}]`
	runtimes := `[{"id":"runtime-m5-cursor","custom_name":"M5 MBP","provider":"cursor","status":"online"}]`
	fake, capture := writeDispatchFixture(t, root, agents, runtimes)
	configPath := filepath.Join(root, "config", "config.json")
	writeDispatchConfig(t, configPath, fake,
		[]any{map[string]any{"agent": "codex", "model": "gpt-5.6-sol", "speed": "regular", "use_for": "alias:sol"}},
		map[string]any{"m5": "m5-mbp"})
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	a.stdin = strings.NewReader("Review the change.")
	a.stdinIsTerminal = func() bool { return false }
	code := a.run(context.Background(), []string{"--config", configPath, "dispatch", "--route", "m5 grok", "--title", "Off-policy guard", "--prompt-stdin", "--idempotency-key", "off-policy", "--plan"})
	if code == 0 || !strings.Contains(stdout.String(), `"code":"usage"`) || !strings.Contains(stdout.String(), `agent_preferences`) || strings.Contains(stdout.String(), `"runtime_verified":true`) {
		t.Fatalf("off-policy dispatch exit=%d output=%s", code, stdout.String())
	}
	if _, err := os.Stat(capture + ".argv"); !os.IsNotExist(err) {
		t.Fatalf("off-policy dispatch created issue: %v", err)
	}
}

func TestDispatchPlanAcceptsInTableGrok(t *testing.T) {
	root := t.TempDir()
	agents := `[{"id":"agent-grok","name":"M4 Mini Cursor (Grok)","model":"cursor-grok-4.6-high","runtime_id":"runtime-m4-cursor","status":"idle","archived_at":null}]`
	runtimes := `[{"id":"runtime-m4-cursor","custom_name":"M4 Mini","provider":"cursor","status":"online"}]`
	fake, capture := writeDispatchFixture(t, root, agents, runtimes)
	configPath := filepath.Join(root, "config", "config.json")
	writeDispatchConfig(t, configPath, fake,
		[]any{map[string]any{"agent": "cursor", "model": "cursor-grok-4.6-high", "speed": "regular", "use_for": "alias:grok"}},
		map[string]any{"mini": "m4-mini"})
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	a.stdin = strings.NewReader("Do the reviewed grok work.")
	a.stdinIsTerminal = func() bool { return false }
	code := a.run(context.Background(), []string{"--config", configPath, "dispatch", "--route", "mini grok", "--title", "In-table grok", "--prompt-stdin", "--idempotency-key", "in-table-grok", "--plan"})
	if code != 0 || !strings.Contains(stdout.String(), `"runtime_verified":true`) || !strings.Contains(stdout.String(), `"model":"cursor-grok-4.6-high"`) || !strings.Contains(stdout.String(), `"adapter":"cursor"`) {
		t.Fatalf("in-table grok dispatch exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(capture + ".argv"); !os.IsNotExist(err) {
		t.Fatalf("plan created Multica issue: %v", err)
	}
}

func TestDispatchPlanExactModelSelectsOnlyReviewedMatch(t *testing.T) {
	root := t.TempDir()
	agents := `[
      {"id":"agent-fusion","name":"M5 Devin (Fusion)","model":"fusion-gpt-6-sol-high-sidekick-swe-2-high","runtime_id":"runtime-m5-devin","status":"idle","archived_at":null},
      {"id":"agent-swe2","name":"M5 Devin (SWE-2)","model":"swe-2-high","runtime_id":"runtime-m5-devin","status":"idle","archived_at":null}
    ]`
	runtimes := `[{"id":"runtime-m5-devin","custom_name":"M5 MBP","provider":"devin","status":"online"}]`
	fake, capture := writeDispatchFixture(t, root, agents, runtimes)
	configPath := filepath.Join(root, "config", "config.json")
	writeDispatchConfig(t, configPath, fake,
		[]any{map[string]any{"agent": "devin", "model": "fusion-gpt-6-sol-high-sidekick-swe-2-high", "speed": "regular", "use_for": "alias:fusion"}},
		map[string]any{"m5": "m5-mbp"})
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	a.stdin = strings.NewReader("Review the exact diff.")
	a.stdinIsTerminal = func() bool { return false }
	code := a.run(context.Background(), []string{"--config", configPath, "dispatch", "--route", "m5 fusion", "--title", "Exact model", "--prompt-stdin", "--idempotency-key", "exact-model-v1", "--plan"})
	if code != 0 || !strings.Contains(stdout.String(), `"agent_name":"M5 Devin (Fusion)"`) || strings.Contains(stdout.String(), "SWE-2") {
		t.Fatalf("exact-model plan exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(capture + ".argv"); !os.IsNotExist(err) {
		t.Fatalf("plan created Multica issue: %v", err)
	}

	t.Setenv("AGENTCTL_TEST_AGENTS", `[{"id":"agent-swe2","name":"M5 Devin (SWE-2)","model":"swe-2-high","runtime_id":"runtime-m5-devin","status":"idle","archived_at":null}]`)
	stdout.Reset()
	stderr.Reset()
	a.stdin = strings.NewReader("Review the exact diff.")
	code = a.run(context.Background(), []string{"--config", configPath, "dispatch", "--route", "m5 fusion", "--title", "Exact model", "--prompt-stdin", "--idempotency-key", "exact-model-v2", "--plan"})
	if code == 0 || !strings.Contains(stdout.String(), `"code":"capability_unavailable"`) {
		t.Fatalf("substring-only plan exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(capture + ".argv"); !os.IsNotExist(err) {
		t.Fatalf("substring-only plan created Multica issue: %v", err)
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

func TestSupervisorCycleDoesNotBlockAnotherProcessJournal(t *testing.T) {
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
	t.Setenv("AGENTCTL_TEST_ISSUE_GET_DELAY", "2")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	done := make(chan string, 1)
	go func() {
		var out, errout bytes.Buffer
		service := testApp(&out, &errout)
		code := service.run(ctx, []string{"--journal", journalPath, "supervisor", "run", "--once", "--state-dir", filepath.Join(root, "supervisor")})
		if code != 0 {
			done <- out.String() + errout.String()
			return
		}
		done <- ""
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(capture + ".issue.get.started"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("probe did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestJournalProcessProbe$")
	command.Env = append(os.Environ(), "AGENTCTL_LOCK_TEST_PATH="+journalPath)
	if data, err := command.CombinedOutput(); err != nil {
		t.Fatalf("independent writer blocked during adapter I/O: %v %s", err, data)
	}
	if failure := <-done; failure != "" {
		t.Fatal(failure)
	}
	// Only the supervisor opts into a short per-probe deadline; the shared
	// bridge used by explicit await preserves its caller's deadline.
	bounded := pathSupervisorExecutions{path: journalPath, probeTimeout: 20 * time.Millisecond}
	if _, err := bounded.Reprobe(context.Background(), supervisor.Execution{ID: dispatched.Result.Execution.ID.String()}); err == nil {
		t.Fatal("supervisor probe ignored its deadline")
	}
}
func TestJournalProcessProbe(t *testing.T) {
	path := os.Getenv("AGENTCTL_LOCK_TEST_PATH")
	if path == "" {
		t.Skip("subprocess helper")
	}
	journal, err := store.Open(path, store.Options{LockTimeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	if _, err := journal.ListExecutions(context.Background(), false); err != nil {
		t.Fatal(err)
	}
}

func TestDispatchFailureMetadataAndSameKeyRecovery(t *testing.T) {
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

	_ = capture
	t.Setenv("AGENTCTL_TEST_CREATE_ERROR", "unknown flag: --description-stdin token=do-not-record")
	args := []string{"--config", configPath, "--journal", journalPath, "dispatch", "--route", "m5 sol", "--title", "Review", "--prompt-stdin", "--idempotency-key", "recover-v1"}
	var out, stderr bytes.Buffer
	a := testApp(&out, &stderr)
	a.stdinIsTerminal = func() bool { return false }
	var first string
	for attempt := 0; attempt < 2; attempt++ {
		out.Reset()
		a.stdin = strings.NewReader("private prompt")
		if code := a.run(context.Background(), args); code != 11 {
			t.Fatalf("exit=%d %s", code, out.String())
		}
		var doc struct {
			Error struct {
				Details map[string]any `json:"details"`
			} `json:"error"`
		}
		if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		id, _ := doc.Error.Details["execution_id"].(string)
		if attempt == 0 {
			first = id
		} else if id != first {
			t.Fatal("retry allocated another execution")
		}
		if !strings.Contains(out.String(), "invalid_arguments") || strings.Contains(out.String(), "do-not-record") || strings.Contains(out.String(), "private prompt") {
			t.Fatalf("unsafe diagnostic %s", out.String())
		}
	}
	out.Reset()
	if code := a.run(context.Background(), []string{"--journal", journalPath, "status", first}); code != 0 || !strings.Contains(out.String(), "last_operation_failure") {
		t.Fatalf("status %d %s", code, out.String())
	}
	// Hold the journal beyond one open attempt, but within the recording budget.
	id, err := ids.ParseExecutionID(first)
	if err != nil {
		t.Fatal(err)
	}
	locked, err := store.Open(journalPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() { time.Sleep(550 * time.Millisecond); _ = locked.Close(); close(released) }()
	problem := a.dispatchFailure(context.Background(), common{journalPath: journalPath}, id, "recover-v1", "default", "issue_create", errors.New("test upstream failure"))
	<-released
	if problem.Details["diagnostic_recorded"] != true {
		t.Fatalf("did not retry busy diagnostic: %#v", problem.Details)
	}
	t.Setenv("AGENTCTL_TEST_CREATE_ERROR", "")
	out.Reset()
	a.stdin = strings.NewReader("private prompt")
	if code := a.run(context.Background(), args); code != 0 || !strings.Contains(out.String(), first) {
		t.Fatalf("recovery %d %s", code, out.String())
	}
	t.Setenv("AGENTCTL_TEST_AGENTS", strings.Replace(agents, `"working"`, `"offline"`, 1))
	out.Reset()
	a.stdin = strings.NewReader("private prompt")
	if code := a.run(context.Background(), append(args, "--plan")); code != output.ExitCodeFor(output.CodeCapabilityUnavailable) || !strings.Contains(out.String(), `"agent_status_unavailable":1`) {
		t.Fatalf("status rejection %d %s", code, out.String())
	}
}

func TestExternalMutationsReleaseJournal(t *testing.T) {
	for _, operation := range []string{"dispatch", "promote", "cancel"} {
		t.Run(operation, func(t *testing.T) {
			root := t.TempDir()
			var configPath, journalPath string
			var args []string
			var stdout, stderr bytes.Buffer
			a := testApp(&stdout, &stderr)
			if operation == "cancel" {
				configPath, journalPath, _ = writeCancelMulticaFixture(t, root)
				execution := createMulticaCancelExecution(t, journalPath, true)
				args = []string{"cancel", execution.ID.String()}
			} else {
				fake, _ := writeDispatchFixture(t, root,
					`[{"id":"agent-sol","name":"M5 Codex","model":"gpt-5.6-sol","runtime_id":"runtime-m5","status":"idle"}]`,
					`[{"id":"runtime-m5","custom_name":"M5 MBP","provider":"codex","status":"online"}]`)
				configPath, journalPath = filepath.Join(root, "config.json"), filepath.Join(root, "state", "journal.db")
				writeDispatchConfig(t, configPath, fake, []any{map[string]any{"agent": "codex", "model": "gpt-5.6-sol", "speed": "regular", "use_for": "alias:sol"}}, map[string]any{"m5": "m5-mbp"})
				args = []string{"dispatch", "--route", "m5 sol", "--title", "Review", "--prompt-stdin", "--idempotency-key", "lock-test"}
				if operation == "promote" {
					if code := a.run(context.Background(), []string{"--journal", journalPath, "run", "--adapter", "generic-process", "--", "/bin/echo", `{"type":"result","status":"completed","result":{"summary":"done"}}`}); code != 0 {
						t.Fatalf("seed: %d %s", code, stdout.String())
					}
					var doc struct {
						Result model.Execution `json:"result"`
					}
					if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
						t.Fatal(err)
					}
					args = []string{"promote", doc.Result.ID.String(), "--title", "Review"}
				}
			}
			t.Setenv("AGENTCTL_TEST_PROBE_BINARY", os.Args[0])
			t.Setenv("AGENTCTL_LOCK_TEST_PATH", journalPath)
			stdout.Reset()
			a.stdin = strings.NewReader("Review the change.")
			a.stdinIsTerminal = func() bool { return false }
			args = append([]string{"--journal", journalPath, "--config", configPath}, args...)
			if code := a.run(context.Background(), args); code != 0 {
				t.Fatalf("external %s could not open journal: %d %s %s", operation, code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestConcurrentDispatchReplaysPreserveOneExecutionAndBinding(t *testing.T) {
	root := t.TempDir()
	fake, capture := writeDispatchFixture(t, root,
		`[{"id":"agent-sol","name":"M5 Codex","model":"gpt-5.6-sol","runtime_id":"runtime-m5","status":"idle"}]`,
		`[{"id":"runtime-m5","custom_name":"M5 MBP","provider":"codex","status":"online"}]`)
	configPath, journalPath := filepath.Join(root, "config.json"), filepath.Join(root, "state", "journal.db")
	writeDispatchConfig(t, configPath, fake, []any{map[string]any{"agent": "codex", "model": "gpt-5.6-sol", "speed": "regular", "use_for": "alias:sol"}}, map[string]any{"m5": "m5-mbp"})
	t.Setenv("AGENTCTL_TEST_CREATE_BARRIER", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type response struct {
		code int
		text string
	}
	done := make(chan response, 2)
	for range 2 {
		go func() {
			var stdout, stderr bytes.Buffer
			a := testApp(&stdout, &stderr)
			a.stdin = strings.NewReader("Review")
			a.stdinIsTerminal = func() bool { return false }
			code := a.run(ctx, []string{"--journal", journalPath, "--config", configPath, "dispatch", "--route", "m5 sol", "--title", "Review", "--prompt-stdin", "--idempotency-key", "same-key"})
			done <- response{code, stdout.String()}
		}()
	}
	defer os.WriteFile(capture+".release", nil, 0o600)
	for {
		files, _ := filepath.Glob(capture + ".create.*")
		if len(files) == 2 {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("concurrent creates never reached barrier")
		}
		time.Sleep(10 * time.Millisecond)
	}
	journal, err := store.Open(journalPath, store.Options{LockTimeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	items, err := journal.ListExecutions(ctx, false)
	if err != nil || len(items) != 1 {
		journal.Close()
		t.Fatalf("reserved executions: %v %v", items, err)
	}
	current := items[0]
	current.LastOperationFailure = &model.OperationFailure{Stage: "issue_create", Category: "timeout", UpstreamExitCode: -1, Retryable: true, RemoteCreationUncertain: true, RecordedAt: time.Now().UTC()}
	_, err = journal.UpdateExecution(ctx, current, current.Revision)
	journal.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(capture+".release", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		result := <-done
		if result.code != 0 || !strings.Contains(result.text, current.ID.String()) {
			t.Fatalf("replay failed: %+v", result)
		}
	}
	journal, err = store.Open(journalPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	items, err = journal.ListExecutions(ctx, false)
	if err != nil || len(items) != 1 {
		t.Fatalf("replay executions: %v %v", items, err)
	}
	bindings := 0
	for _, binding := range items[0].SourceBindings {
		if binding.Kind == "multica_issue" {
			bindings++
		}
	}
	if bindings != 1 || items[0].LastOperationFailure == nil || items[0].State != model.StateWaiting {
		t.Fatalf("lost concurrent metadata or duplicated binding: %#v", items[0])
	}
}

func TestDispatchFailureStagesRecoverWithOriginalKey(t *testing.T) {
	for _, tc := range []struct {
		variable, value, stage string
		exit                   int
		uncertain              bool
	}{
		{"AGENTCTL_TEST_CREATE_JSON", `{}`, "issue_create", 0, true},
		{"AGENTCTL_TEST_READ_ERROR", "unauthorized secret-do-not-retain", "issue_read", 23, false},
		{"AGENTCTL_TEST_FAIL_UPDATE_ONCE", "1", "issue_activate", 17, false},
	} {
		t.Run(tc.stage, func(t *testing.T) {
			root := t.TempDir()
			fake, _ := writeDispatchFixture(t, root,
				`[{"id":"agent-sol","name":"M5 Codex","model":"gpt-5.6-sol","runtime_id":"runtime-m5","status":"idle"}]`,
				`[{"id":"runtime-m5","custom_name":"M5 MBP","provider":"codex","status":"online"}]`)
			configPath, journalPath := filepath.Join(root, "config.json"), filepath.Join(root, "state", "journal.db")
			writeDispatchConfig(t, configPath, fake, []any{map[string]any{"agent": "codex", "model": "gpt-5.6-sol", "speed": "regular", "use_for": "alias:sol"}}, map[string]any{"m5": "m5-mbp"})
			t.Setenv(tc.variable, tc.value)
			var stdout, stderr bytes.Buffer
			a := testApp(&stdout, &stderr)
			a.stdinIsTerminal = func() bool { return false }
			a.stdin = strings.NewReader("private task")
			args := []string{"--journal", journalPath, "--config", configPath, "dispatch", "--route", "m5 sol", "--title", "Review", "--prompt-stdin", "--idempotency-key", "stage-test"}
			if code := a.run(context.Background(), args); code != output.ExitCodeFor(output.CodeRemoteFailure) {
				t.Fatalf("exit=%d %s", code, stdout.String())
			}
			if strings.Contains(stdout.String(), "secret-do-not-retain") || strings.Contains(stdout.String(), "private task") {
				t.Fatal("raw content in diagnostic")
			}
			journal := scopedJournal{path: journalPath}
			items, err := journal.ListExecutions(context.Background(), false)
			if err != nil || len(items) != 1 {
				t.Fatalf("executions=%v err=%v", items, err)
			}
			failure := items[0].LastOperationFailure
			if failure == nil || failure.Stage != tc.stage || failure.UpstreamExitCode != tc.exit || failure.RemoteCreationUncertain != tc.uncertain {
				t.Fatalf("failure=%#v", failure)
			}
			t.Setenv(tc.variable, "")
			stdout.Reset()
			a.stdin = strings.NewReader("private task")
			if code := a.run(context.Background(), args); code != 0 || !strings.Contains(stdout.String(), items[0].ID.String()) {
				t.Fatalf("recovery=%d %s", code, stdout.String())
			}
		})
	}
}
