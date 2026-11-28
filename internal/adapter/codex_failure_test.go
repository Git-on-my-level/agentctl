package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodexStructuredFailureKeepsReasonAndDropsAnswer(t *testing.T) {
	record := &processRecord{parser: codexParser{}, maxOutput: 1 << 20, ref: SourceRef{Kind: "codex_thread"}}
	record.ingest([]byte(`{"type":"item","item":{"type":"agent_message","text":"do-not-store-this-answer"}}`), false)
	record.ingest([]byte(`{"type":"turn.failed","thread_id":"thread-1","error":{"message":"Selected model is at capacity. Please try a different model. token=secret","codex_error_info":"server_overloaded"}}`), false)
	if record.result == nil || record.result.Success || record.result.State != StateFailed {
		t.Fatalf("result=%#v", record.result)
	}
	if record.result.Content != "" || record.result.Summary != "" || strings.Contains(record.result.Error, "secret") || strings.Contains(record.result.Error, "do-not-store") {
		t.Fatalf("failure kept answer or secret: %#v", record.result)
	}
	if record.result.Data["native_code"] != "server_overloaded" || !strings.Contains(record.result.Error, "at capacity") || !strings.Contains(record.result.Error, "[REDACTED]") {
		t.Fatalf("result=%#v", record.result)
	}
	if record.result.SessionRef.OpaqueID != "thread-1" {
		t.Fatalf("session=%#v", record.result.SessionRef)
	}
}

func TestCodexTaskCompleteFailureAndGenericCode(t *testing.T) {
	overloaded := codexParser{}.Parse([]byte(`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"do-not-store-this-answer","error":{"message":"Selected model is at capacity. Please try a different model.","codex_error_info":"server_overloaded"}}}`), false)
	if !overloaded.Terminal || overloaded.Content != "" || overloaded.Error == "" || overloaded.Data["native_code"] != "server_overloaded" || strings.Contains(overloaded.Error, "do-not-store") {
		t.Fatalf("task_complete=%#v", overloaded)
	}
	generic := codexParser{}.Parse([]byte(`{"type":"turn.failed","error":{"message":"the turn stopped","codex_error_info":"other"}}`), false)
	if !generic.Terminal || generic.Data["native_code"] != "other" || generic.Error != "the turn stopped" || generic.Data["failure_structured"] != true {
		t.Fatalf("generic=%#v", generic)
	}
	freeText := codexParser{}.Parse([]byte(`{"type":"turn.failed","error":{"message":"boom","codex_error_info":"Not A Code"}}`), false)
	if _, ok := freeText.Data["native_code"]; ok || freeText.Error != "boom" {
		t.Fatalf("free text became a code: %#v", freeText)
	}
}

func TestCodexDeclinedCommandStaysFailedAfterTurnCompletes(t *testing.T) {
	record := &processRecord{parser: codexParser{}, maxOutput: 1 << 20, ref: SourceRef{Kind: "codex_thread", Adapter: "codex"}}
	record.ingest([]byte(`{"type":"item.completed","thread_id":"thread-1","item":{"id":"cmd-1","type":"command_execution","status":"declined","aggregated_output":"approval policy is never","exit_code":-1}}`), false)
	record.ingest([]byte(`{"type":"turn.completed","thread_id":"thread-1"}`), false)
	if record.result == nil || record.result.Success || record.result.State != StateFailed || record.result.Data["diagnostic_code"] != "native_command_declined" {
		t.Fatalf("declined command was not latched as failure: %#v", record.result)
	}
	if record.result.Content != "" || strings.Contains(record.result.Error, "approval policy is never") || strings.Contains(record.result.Error, "cmd-1") {
		t.Fatalf("declined command kept native output: %#v", record.result)
	}
	progress := codexParser{}.Parse([]byte(`{"type":"item.completed","item":{"type":"command_execution","status":"completed","exit_code":0}}`), false)
	if progress.Terminal || progress.State == StateFailed {
		t.Fatalf("successful command was misclassified: %#v", progress)
	}
}

func TestCodexFAQRefusalProseDoesNotImplyNativeDenial(t *testing.T) {
	threadID := "550e8400-e29b-41d4-a716-446655440000"
	record, rollout := newCodexRolloutRecord(t, threadID)
	answer := "The FAQ explains why you cannot run the command without approval."
	appendCodexRollout(t, rollout, map[string]any{
		"type":    "response_item",
		"payload": map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": answer}}},
	})
	record.ingest([]byte(`{"type":"item.completed","thread_id":"`+threadID+`","item":{"type":"agent_message","text":"`+answer+`"}}`), false)
	record.ingest([]byte(`{"type":"turn.completed","thread_id":"`+threadID+`"}`), false)
	if record.result == nil || !record.result.Success || record.result.Content != answer {
		t.Fatalf("FAQ explanation should remain a successful answer: %#v", record.result)
	}
}

func TestCodexRolloutDenialStaysFailedAfterTurnCompletes(t *testing.T) {
	threadID := "550e8400-e29b-41d4-a716-446655440001"
	record, rollout := newCodexRolloutRecord(t, threadID)
	appendCodexRollout(t, rollout,
		map[string]any{"type": "response_item", "payload": map[string]any{
			"type": "function_call", "call_id": "call-denied", "name": "exec_command",
			"arguments": `{"cmd":"make test","sandbox_permissions":"require_escalated"}`,
		}},
		map[string]any{"type": "response_item", "payload": map[string]any{
			"type": "function_call_output", "call_id": "call-denied", "output": codexNeverPolicyError,
		}},
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "assistant", "content": []any{}}},
	)
	record.ingest([]byte(`{"type":"item.completed","thread_id":"`+threadID+`","item":{"type":"agent_message","text":"The command was rejected by native policy."}}`), false)
	record.ingest([]byte(`{"type":"turn.completed","thread_id":"`+threadID+`"}`), false)
	if record.result == nil || record.result.Success || record.result.State != StateFailed || record.result.Data["diagnostic_code"] != "native_command_declined" {
		t.Fatalf("typed rollout denial was reported as success: %#v", record.result)
	}
	if record.result.Content != "" || strings.Contains(record.result.Error, codexNeverPolicyError) {
		t.Fatalf("native denial retained rollout or assistant text: %#v", record.result)
	}
}

func TestCodexRolloutDenialWrittenBeforeThreadStartedIsDetected(t *testing.T) {
	threadID := "550e8400-e29b-41d4-a716-446655440016"
	record, rollout := newCodexRolloutRecordBeforeThreadStarted(t, threadID)
	appendCodexRollout(t, rollout,
		map[string]any{"type": "session_meta", "payload": map[string]any{"id": threadID}},
		map[string]any{"type": "response_item", "payload": map[string]any{
			"type": "function_call", "call_id": "call-early", "name": "exec_command",
			"arguments": `{"cmd":"make test","sandbox_permissions":"require_escalated"}`,
		}},
		map[string]any{"type": "response_item", "payload": map[string]any{
			"type": "function_call_output", "call_id": "call-early", "output": codexNeverPolicyError,
		}},
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "assistant", "content": []any{}}},
	)
	// The native rollout is already complete before stdout reveals the thread
	// ID. The prelaunch fresh-session baseline must still inspect these bytes.
	record.ingest([]byte(`{"type":"thread.started","thread_id":"`+threadID+`"}`), false)
	record.ingest([]byte(`{"type":"item.completed","thread_id":"`+threadID+`","item":{"type":"agent_message","text":"The native policy rejected the command."}}`), false)
	record.ingest([]byte(`{"type":"turn.completed","thread_id":"`+threadID+`"}`), false)
	if record.result == nil || record.result.Success || record.result.Data["diagnostic_code"] != "native_command_declined" {
		t.Fatalf("pre-thread-start rollout denial was missed: %#v", record.result)
	}
}

func TestCodexSuccessfulCurrentTurnWrittenBeforeThreadStartedIsAccepted(t *testing.T) {
	threadID := "550e8400-e29b-41d4-a716-446655440017"
	record, rollout := newCodexRolloutRecordBeforeThreadStarted(t, threadID)
	appendCodexRollout(t, rollout,
		map[string]any{"type": "session_meta", "payload": map[string]any{"id": threadID}},
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "assistant", "content": []any{}}},
	)
	record.ingest([]byte(`{"type":"thread.started","thread_id":"`+threadID+`"}`), false)
	record.ingest([]byte(`{"type":"item.completed","thread_id":"`+threadID+`","item":{"type":"agent_message","text":"current answer"}}`), false)
	record.ingest([]byte(`{"type":"turn.completed","thread_id":"`+threadID+`"}`), false)
	if record.result == nil || !record.result.Success || record.result.Content != "current answer" {
		t.Fatalf("pre-thread-start current rollout was rejected: %#v", record.result)
	}
}

func TestCodexArrayToolOutputDoesNotFailSuccessfulTurn(t *testing.T) {
	threadID := "550e8400-e29b-41d4-a716-446655440018"
	record, rollout := newCodexRolloutRecord(t, threadID)
	appendCodexRollout(t, rollout,
		map[string]any{"type": "response_item", "payload": map[string]any{
			"type": "function_call", "call_id": "view-image", "name": "view_image",
			"arguments": `{"path":"diagram.png"}`,
		}},
		map[string]any{"type": "response_item", "payload": map[string]any{
			"type": "function_call_output", "call_id": "view-image", "output": []any{map[string]any{"type": "image", "data": "encoded-image", "mime_type": "image/png"}},
		}},
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "assistant", "content": []any{}}},
	)
	record.ingest([]byte(`{"type":"item.completed","thread_id":"`+threadID+`","item":{"type":"agent_message","text":"image reviewed"}}`), false)
	record.ingest([]byte(`{"type":"turn.completed","thread_id":"`+threadID+`"}`), false)
	if record.result == nil || !record.result.Success || record.result.Content != "image reviewed" {
		t.Fatalf("array-shaped image output corrupted the turn: %#v", record.result)
	}
}

func TestCodexRecoveredInvalidExecCallsDoNotFailSuccessfulTurn(t *testing.T) {
	threadID := "550e8400-e29b-41d4-a716-446655440019"
	record, rollout := newCodexRolloutRecord(t, threadID)
	appendCodexRollout(t, rollout,
		map[string]any{"type": "response_item", "payload": map[string]any{
			"type": "function_call", "call_id": "bad-json", "name": "exec_command", "arguments": "not-json",
		}},
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call_output", "call_id": "bad-json", "output": "invalid tool arguments"}},
		map[string]any{"type": "response_item", "payload": map[string]any{
			"type": "function_call", "call_id": "missing-command", "name": "exec_command", "arguments": `{"sandbox_permissions":"require_escalated"}`,
		}},
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call_output", "call_id": "missing-command", "output": "command field is required"}},
		map[string]any{"type": "response_item", "payload": map[string]any{
			"type": "function_call", "call_id": "corrected", "name": "exec_command", "arguments": `{"cmd":"go test ./..."}`,
		}},
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call_output", "call_id": "corrected", "output": "tests passed"}},
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "assistant", "content": []any{}}},
	)
	record.ingest([]byte(`{"type":"item.completed","thread_id":"`+threadID+`","item":{"type":"agent_message","text":"I corrected the command arguments and completed the task."}}`), false)
	record.ingest([]byte(`{"type":"turn.completed","thread_id":"`+threadID+`"}`), false)
	if record.result == nil || !record.result.Success || record.result.Content != "I corrected the command arguments and completed the task." {
		t.Fatalf("recovered invalid tool calls failed the turn: %#v", record.result)
	}
}

func TestCodexAdditionalPermissionDenialRequiresValidRequestShape(t *testing.T) {
	threadID := "550e8400-e29b-41d4-a716-446655440013"
	record, rollout := newCodexRolloutRecord(t, threadID)
	appendCodexRollout(t, rollout,
		map[string]any{"type": "response_item", "payload": map[string]any{
			"type": "function_call", "call_id": "call-network", "name": "exec_command",
			"arguments": `{"cmd":"go test ./...","sandbox_permissions":"with_additional_permissions","additional_permissions":{"network":{"enabled":true}}}`,
		}},
		map[string]any{"type": "response_item", "payload": map[string]any{
			"type": "function_call_output", "call_id": "call-network", "output": codexNeverPolicyError,
		}},
	)
	record.ingest([]byte(`{"type":"turn.completed","thread_id":"`+threadID+`"}`), false)
	if record.result == nil || record.result.Success || record.result.Data["diagnostic_code"] != "native_command_declined" {
		t.Fatalf("valid additional-permission denial was not latched: %#v", record.result)
	}
}

func TestCodexOnlyChecksCurrentTurnRolloutDelta(t *testing.T) {
	threadID := "550e8400-e29b-41d4-a716-446655440014"
	home := t.TempDir()
	rollout := filepath.Join(home, "sessions", "2026", "11", "25", "rollout-2026-11-25T00-00-00-00000000-"+threadID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(rollout), 0o700); err != nil {
		t.Fatal(err)
	}
	appendCodexRollout(t, rollout,
		map[string]any{"type": "session_meta", "payload": map[string]any{"id": threadID}},
		map[string]any{"type": "response_item", "payload": map[string]any{
			"type": "function_call", "call_id": "old-call", "name": "exec_command",
			"arguments": `{"cmd":"make test","sandbox_permissions":"require_escalated"}`,
		}},
		map[string]any{"type": "response_item", "payload": map[string]any{
			"type": "function_call_output", "call_id": "old-call", "output": codexNeverPolicyError,
		}},
	)
	record := &processRecord{parser: codexParser{}, maxOutput: 1 << 20, codexHome: home, codexRolloutRequired: true,
		codexRolloutCheckpoint: codexRolloutCheckpointForLaunch(home, threadID), ref: SourceRef{Kind: "codex_thread", Adapter: "codex"}}
	appendCodexRollout(t, rollout, map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "assistant", "content": []any{}}})
	record.ingest([]byte(`{"type":"thread.started","thread_id":"`+threadID+`"}`), false)
	record.ingest([]byte(`{"type":"turn.completed","thread_id":"`+threadID+`"}`), false)
	if record.result == nil || !record.result.Success {
		t.Fatalf("prior-turn denial affected current turn: %#v", record.result)
	}
}

func TestCodexUnrelatedToolOutputQuotingDenialDoesNotFail(t *testing.T) {
	threadID := "550e8400-e29b-41d4-a716-446655440002"
	record, rollout := newCodexRolloutRecord(t, threadID)
	appendCodexRollout(t, rollout,
		map[string]any{"type": "response_item", "payload": map[string]any{
			"type": "function_call", "call_id": "call-search", "name": "web_search",
			"arguments": `{"query":"approval policy"}`,
		}},
		map[string]any{"type": "response_item", "payload": map[string]any{
			"type": "function_call_output", "call_id": "call-search", "output": codexNeverPolicyError,
		}},
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "assistant", "content": []any{}}},
	)
	record.ingest([]byte(`{"type":"turn.completed","thread_id":"`+threadID+`"}`), false)
	if record.result == nil || !record.result.Success {
		t.Fatalf("unrelated quoted tool output should not fail the turn: %#v", record.result)
	}
}

func TestCodexMissingOrMalformedRolloutFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name      string
		malformed bool
	}{
		{name: "missing"},
		{name: "malformed", malformed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			threadID := "550e8400-e29b-41d4-a716-446655440003"
			record, rollout := newCodexRolloutRecord(t, threadID)
			if test.malformed {
				if err := os.WriteFile(rollout, []byte("not-json\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Remove(rollout); err != nil {
				t.Fatal(err)
			}
			record.ingest([]byte(`{"type":"item.completed","thread_id":"`+threadID+`","item":{"type":"agent_message","text":"A successful explanatory answer."}}`), false)
			record.ingest([]byte(`{"type":"turn.completed","thread_id":"`+threadID+`"}`), false)
			if record.result == nil || record.result.Success || record.result.Data["diagnostic_code"] != codexRolloutFailure || record.result.Content != "" {
				t.Fatalf("missing or malformed rollout did not fail closed: %#v", record.result)
			}
		})
	}
}

func TestCodexEphemeralStructuredCompletionNeedsNoRollout(t *testing.T) {
	threadID := "550e8400-e29b-41d4-a716-446655440020"
	path := fixtureExecutable(t, `printf '%s\n' '{"type":"thread.started","thread_id":"`+threadID+`"}'
printf '%s\n' '{"type":"item.completed","thread_id":"`+threadID+`","item":{"type":"agent_message","text":"ephemeral answer"}}'
printf '%s\n' '{"type":"turn.completed","thread_id":"`+threadID+`"}'`)
	launched, err := NewCodex().Launch(context.Background(), LaunchRequest{
		Argv: []string{path, "exec", "--json", "--ephemeral", "-"}, Stdin: []byte("task\n"),
		PromptDelivery: PromptDeliveryStdin, DiscoveryWindow: time.Second,
		Env: []string{"CODEX_HOME=" + filepath.Join(t.TempDir(), "missing-home")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if launched.Result == nil || !launched.Result.Success || launched.Result.Content != "ephemeral answer" {
		t.Fatalf("valid ephemeral completion was rejected without a rollout: %#v", launched.Result)
	}
}

func TestCodexEphemeralDelegatedLaunchIsRejectedBeforeStart(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	path := fixtureExecutable(t, `printf started > "$MARKER"`)
	_, err := NewCodex().Launch(context.Background(), LaunchRequest{
		Argv: []string{path, "exec", "--json", "--ephemeral", "-"}, Stdin: []byte("task\n"),
		PromptDelivery:   PromptDeliveryStdin,
		Env:              []string{"MARKER=" + marker},
		ExecutionContext: &ExecutionContext{ExecutionID: "delegated-test", Adapter: "codex", Authority: "native", Delegated: true},
	})
	var adapterErr *AdapterError
	if !errors.As(err, &adapterErr) || adapterErr.Code != ErrCapabilityUnavailable || adapterErr.Details["diagnostic_code"] != "codex_ephemeral_delegation_unsupported" {
		t.Fatalf("delegated ephemeral launch error = %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("rejected delegated ephemeral child was started: %v", err)
	}
}

func TestCodexRelativeHomeUsesChildWorkingDirectory(t *testing.T) {
	threadID := "550e8400-e29b-41d4-a716-446655440021"
	childDir := filepath.Join(t.TempDir(), "child")
	if err := os.MkdirAll(childDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := fixtureExecutable(t, `thread_id='`+threadID+`'
mkdir -p "$CODEX_HOME/sessions/2026/11/25"
rollout="$CODEX_HOME/sessions/2026/11/25/rollout-2026-11-25T00-00-00-00000000-$thread_id.jsonl"
printf '%s\n' "{\"type\":\"session_meta\",\"payload\":{\"id\":\"$thread_id\"}}" > "$rollout"
printf '%s\n' '{"type":"response_item","payload":{"type":"message","role":"assistant","content":[]}}' >> "$rollout"
printf '%s\n' "{\"type\":\"thread.started\",\"thread_id\":\"$thread_id\"}"
printf '%s\n' "{\"type\":\"item.completed\",\"thread_id\":\"$thread_id\",\"item\":{\"type\":\"agent_message\",\"text\":\"relative home answer\"}}"
printf '%s\n' "{\"type\":\"turn.completed\",\"thread_id\":\"$thread_id\"}"`)
	launched, err := NewCodex().Launch(context.Background(), LaunchRequest{
		Argv: []string{path, "exec", "--json", "-"}, Cwd: childDir,
		Stdin: []byte("task\n"), PromptDelivery: PromptDeliveryStdin, DiscoveryWindow: time.Second,
		Env: []string{"CODEX_HOME=relative-codex-home"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if launched.Result == nil || !launched.Result.Success || launched.Result.Content != "relative home answer" {
		t.Fatalf("child-relative rollout was not verified: %#v", launched.Result)
	}
	wantRollout := filepath.Join(childDir, "relative-codex-home", "sessions", "2026", "11", "25", "rollout-2026-11-25T00-00-00-00000000-"+threadID+".jsonl")
	if _, err := os.Stat(wantRollout); err != nil {
		t.Fatalf("fake Codex wrote no rollout at child-resolved CODEX_HOME %s: %v", wantRollout, err)
	}
}

func newCodexRolloutRecord(t *testing.T, threadID string) (*processRecord, string) {
	t.Helper()
	home := t.TempDir()
	rollout := filepath.Join(home, "sessions", "2026", "11", "25", "rollout-2026-11-25T00-00-00-00000000-"+threadID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(rollout), 0o700); err != nil {
		t.Fatal(err)
	}
	appendCodexRollout(t, rollout, map[string]any{"type": "session_meta", "payload": map[string]any{"id": threadID}})
	record := &processRecord{parser: codexParser{}, maxOutput: 1 << 20, ref: SourceRef{Kind: "codex_thread", Adapter: "codex"}, codexHome: home,
		codexRolloutRequired: true, codexRolloutCheckpoint: codexRolloutCheckpoint{Captured: true}}
	record.ingest([]byte(`{"type":"thread.started","thread_id":"`+threadID+`"}`), false)
	return record, rollout
}

func newCodexRolloutRecordBeforeThreadStarted(t *testing.T, threadID string) (*processRecord, string) {
	t.Helper()
	home := t.TempDir()
	rollout := filepath.Join(home, "sessions", "2026", "11", "25", "rollout-2026-11-25T00-00-00-00000000-"+threadID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(rollout), 0o700); err != nil {
		t.Fatal(err)
	}
	record := &processRecord{parser: codexParser{}, maxOutput: 1 << 20, ref: SourceRef{Kind: "codex_thread", Adapter: "codex"}, codexHome: home,
		codexRolloutRequired: true, codexRolloutCheckpoint: codexRolloutCheckpoint{Captured: true}}
	return record, rollout
}

func appendCodexRollout(t *testing.T, path string, records ...map[string]any) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		data, err := json.Marshal(record)
		if err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		if _, err := file.Write(append(data, '\n')); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
