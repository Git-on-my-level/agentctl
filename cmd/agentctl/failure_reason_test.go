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
printf '%s\n' '{"type":"turn.failed","thread_id":"thread-1","error":{"message":"Selected model is at capacity. Please try a different model. token=secret","codex_error_info":"server_overloaded"}}'
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
	if strings.Contains(stdout.String(), "do-not-store-this-answer") || strings.Contains(stdout.String(), "token=secret") {
		t.Fatalf("run leaked answer or secret: %s", stdout.String())
	}
	id := executionIDFromOutput(t, stdout.String())
	stdout.Reset()
	if code := a.run(ctx, []string{"--output", "json", "--journal", journalPath, "status", id}); code != 0 {
		t.Fatalf("status exit=%d %s", code, stdout.String())
	}
	for _, want := range []string{`"category":"capacity"`, `"native_code":"server_overloaded"`, `"retryable":true`, "at capacity", "[REDACTED]"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("status missing %s: %s", want, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "do-not-store-this-answer") || strings.Contains(stdout.String(), "token=secret") {
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
	if strings.Contains(stdout.String(), "do-not-store-this-answer") || strings.Contains(stdout.String(), "token=secret") {
		t.Fatalf("result leaked: %s", stdout.String())
	}
	stdout.Reset()
	if code := a.run(ctx, []string{"--output", "json", "--journal", journalPath, "events", id}); code != 0 {
		t.Fatalf("events exit=%d %s", code, stdout.String())
	}
	if strings.Contains(stdout.String(), "at capacity") || strings.Contains(stdout.String(), "do-not-store") || strings.Contains(stdout.String(), "secret") {
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
	if view == nil || view.NativeCode != "server_overloaded" || view.Category != "capacity" || strings.Contains(view.Message, "secret") {
		t.Fatalf("delegate details=%#v", problem.Details["failure"])
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
	execution.State = model.StateFailed
	outcome := buildOutcome(execution, adapter.Result{
		Success: false, State: adapter.StateFailed,
		Error: strings.Repeat("x", 500) + " at capacity",
		Data:  map[string]any{"failure_structured": true, "native_code": "server_overloaded"},
	}, now, false)
	if outcome.Failure == nil || len(outcome.Failure.Message) > model.NativeFailureMessageLimit || outcome.Failure.NativeCode != "server_overloaded" || !outcome.Failure.Retryable {
		t.Fatalf("outcome=%#v", outcome.Failure)
	}
	if err := outcome.Validate(); err != nil {
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
