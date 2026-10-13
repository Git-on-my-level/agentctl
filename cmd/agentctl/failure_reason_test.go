package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

func TestClassifyCapacityIsRetryableAndGenericProviderIsNot(t *testing.T) {
	code, kind, category, retryable := classifyOutcomeFailure("Selected model is at capacity. Please try a different model.", "", "server_overloaded")
	if code != "provider_capacity" || kind != "capacity" || category != "capacity" || !retryable {
		t.Fatalf("capacity=%s %s %s %v", code, kind, category, retryable)
	}
	code, kind, category, retryable = classifyOutcomeFailure("the turn stopped", "", "other")
	if code != "native_execution_failed" || kind != "provider" || category != "provider" || retryable {
		t.Fatalf("generic=%s %s %s %v", code, kind, category, retryable)
	}
	code, _, category, retryable = classifyOutcomeFailure("native process exited unsuccessfully", "", "")
	if code != "native_execution_failed" || category != "" || retryable {
		t.Fatalf("unstructured=%s %s %v", code, category, retryable)
	}
}

func TestCodexFailureReasonSurfacesWithoutAnswerOrSecret(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "codex")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then echo 0.0.0-test; exit 0; fi
printf '%s\n' '{"type":"item","item":{"type":"agent_message","text":"do-not-store-this-answer"}}'
printf '%s\n' '{"type":"turn.failed","thread_id":"thread-1","error":{"message":"request rejected: {\"api_key\":\"private-value\",\"prompt\":\"confidential task\"} Selected model is at capacity.","codex_error_info":"server_overloaded"}}'
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(root, "state", "journal.db")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if code := a.run(ctx, []string{"--output", "json", "--journal", journalPath, "run", "--adapter", "codex", "--", bin, "exec", "--json", "task"}); code != 0 {
		t.Fatalf("run exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "do-not-store-this-answer") || strings.Contains(stdout.String(), "private-value") || strings.Contains(stdout.String(), "confidential task") {
		t.Fatalf("run leaked answer or secret: %s", stdout.String())
	}
	id := executionIDFromOutput(t, stdout.String())
	stdout.Reset()
	if code := a.run(ctx, []string{"--output", "json", "--journal", journalPath, "status", id}); code != 0 {
		t.Fatalf("status exit=%d %s", code, stdout.String())
	}
	for _, want := range []string{`"category":"capacity"`, `"native_code":"server_overloaded"`, `"retryable":true`, failureMessageCapacity} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("status missing %s: %s", want, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "do-not-store-this-answer") || strings.Contains(stdout.String(), "private-value") || strings.Contains(stdout.String(), "confidential task") {
		t.Fatalf("status leaked: %s", stdout.String())
	}
	stdout.Reset()
	if code := a.run(ctx, []string{"--output", "json", "--journal", journalPath, "result", id}); code != 0 {
		t.Fatalf("result exit=%d %s", code, stdout.String())
	}
	for _, want := range []string{`"native_code":"server_overloaded"`, `"category":"capacity"`, `"retryable":true`} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("result missing %s: %s", want, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "do-not-store-this-answer") || strings.Contains(stdout.String(), "private-value") || strings.Contains(stdout.String(), "confidential task") || !strings.Contains(stdout.String(), failureMessageCapacity) {
		t.Fatalf("result leaked or dropped the capacity reason: %s", stdout.String())
	}
	stdout.Reset()
	if code := a.run(ctx, []string{"--output", "json", "--journal", journalPath, "events", id}); code != 0 {
		t.Fatalf("events exit=%d %s", code, stdout.String())
	}
	if strings.Contains(stdout.String(), "at capacity") || strings.Contains(stdout.String(), "do-not-store") || strings.Contains(stdout.String(), "private-value") || strings.Contains(stdout.String(), "confidential") {
		t.Fatalf("events leaked the failure message: %s", stdout.String())
	}
	parsed, err := ids.ParseExecutionID(id)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := store.Open(journalPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	execution, err := journal.GetExecution(ctx, parsed)
	journal.Close()
	if err != nil {
		t.Fatal(err)
	}
	problem := a.delegateFailedExecution(ctx, common{journalPath: journalPath}, execution)
	if problem == nil || problem.Code != output.CodeExecutionFailed || !problem.Retryable {
		t.Fatalf("delegate failure=%v", problem)
	}
	view, _ := problem.Details["failure"].(*nativeFailureView)
	if view == nil || view.NativeCode != "server_overloaded" || view.Category != "capacity" || view.Message != failureMessageCapacity || strings.Contains(view.Message, "private-value") {
		t.Fatalf("delegate details=%#v", problem.Details["failure"])
	}
	raw, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("private-value")) || bytes.Contains(raw, []byte("confidential task")) || bytes.Contains(raw, []byte("do-not-store-this-answer")) {
		t.Fatal("failure payload entered the journal")
	}
}

func TestBuildOutcomeBoundsStructuredFailure(t *testing.T) {
	now := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	journal, err := store.Open(filepath.Join(t.TempDir(), "state", "journal.db"), store.Options{Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	execution, _, err := journal.CreateExecution(context.Background(), model.Execution{
		Authority: model.AuthorityNative, Adapter: "codex", Mode: model.ModeDirect, Acquisition: model.AcquisitionLaunched,
		State: model.StateRunning, Liveness: model.LivenessAlive, SourceBindings: []model.SourceBinding{},
		Capabilities: model.CapabilitySnapshot{NegotiatedAt: now, AdapterVersion: "test", Items: []model.CapabilityItem{}},
		Observation:  model.Observation{Source: model.ObservationNativeStream, Integrity: model.IntegrityVerified, ObservedAt: now},
	}, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	secret := `request rejected: {"api_key":"private-value","prompt":"confidential task"}`
	execution.State = model.StateFailed
	outcome := buildOutcome(execution, adapter.Result{
		Success: false, State: adapter.StateFailed,
		Error: secret,
	}, now, false)
	if outcome.Failure == nil || outcome.Failure.Message != failureMessageGeneric || strings.Contains(outcome.Failure.Message, "private-value") || strings.Contains(outcome.Failure.Message, "confidential") {
		t.Fatalf("unknown failure was not allowlisted: %#v", outcome.Failure)
	}
	if err := outcome.Validate(); err != nil {
		t.Fatal(err)
	}
	view := projectNativeFailure(&model.OutcomeFailure{Code: "native_execution_failed", Kind: "unknown", Source: "codex", Message: secret})
	if view != nil && (strings.Contains(view.Message, "private-value") || strings.Contains(view.Message, "confidential")) {
		t.Fatalf("status projection kept provider text: %#v", view)
	}
	long := "server overloaded: " + strings.Repeat("x", 300) + " " + secret
	updated, err := finalizeResult(context.Background(), journal, execution, adapter.Result{
		Success: false, State: adapter.StateFailed, Error: long,
	}, now, false)
	if err != nil || !updated.State.Terminal() {
		t.Fatalf("terminal commit failed: %v %#v", err, updated.State)
	}
	stored, err := journal.GetOutcome(context.Background(), execution.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Failure == nil || stored.Failure.Category != "capacity" || !stored.Failure.Retryable || stored.Failure.Message != failureMessageCapacity || len(stored.Failure.Message) > model.NativeFailureMessageLimit {
		t.Fatalf("stored=%#v", stored.Failure)
	}
	if strings.Contains(stored.Failure.Message, "private-value") || strings.Contains(stored.Failure.Message, "xxxx") {
		t.Fatalf("stored provider text: %s", stored.Failure.Message)
	}
	inferred := buildOutcome(execution, adapter.Result{
		Success: false, State: adapter.StateFailed,
		Error: strings.Repeat("x", 500) + " at capacity",
	}, now, false)
	if inferred.Failure == nil || inferred.Failure.Category != "capacity" || inferred.Failure.NativeCode != "" || inferred.Failure.Message != failureMessageCapacity || len(inferred.Failure.Message) > model.NativeFailureMessageLimit || !inferred.Failure.Retryable || strings.Contains(inferred.Failure.Message, "xxxx") {
		t.Fatalf("inferred=%#v", inferred.Failure)
	}
	if err := inferred.Validate(); err != nil {
		t.Fatal(err)
	}
}

func executionIDFromOutput(t *testing.T, document string) string {
	t.Helper()
	const marker = `"id":"exec-`
	start := strings.Index(document, marker)
	if start < 0 {
		t.Fatalf("no execution id in %s", document)
	}
	rest := document[start+len(`"id":"`):]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		t.Fatal(document)
	}
	return rest[:end]
}
