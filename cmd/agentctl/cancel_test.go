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

func writeCancelMulticaFixture(t *testing.T, root string) (configPath, journalPath, capture string) {
	t.Helper()
	capture = filepath.Join(root, "capture")
	path := filepath.Join(root, "fake-multica")
	body := `#!/bin/sh
set -eu
mkdir -p "$AGENTCTL_TEST_CAPTURE"
case "$*" in
  *"issue cancel-task"*)
    printf '%s\n' "$*" >> "$AGENTCTL_TEST_CAPTURE/cancel.argv"
    printf '%s\n' '{"accepted":true}'
    ;;
  *"event list"*)
    printf '%s\n' '{"events":[],"next_cursor":"0","has_more":false}'
    ;;
  *)
    printf 'unexpected argv: %s\n' "$*" >&2
    exit 9
    ;;
esac
`
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTCTL_TEST_CAPTURE", capture)
	configPath = filepath.Join(root, "config", "config.json")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{
		"--output", "json", "--config", configPath,
		"config", "set-profile", "--name", "fleet",
		"--multica-executable", path,
		"--multica-profile", "desktop",
		"--workspace-id", "workspace-test",
		"--server-url", "https://multica.example.test",
		"--app-url", "https://multica.example.test",
		"--default",
	}); code != 0 {
		t.Fatalf("config exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	journalPath = filepath.Join(root, "state", "journal.db")
	return configPath, journalPath, capture
}

func multicaCancelBindings(t *testing.T, withRun bool) []model.SourceBinding {
	t.Helper()
	items := []struct {
		kind, value string
		typ         ids.Type
	}{
		{"runtime_executable", "/tmp/fake-multica", ids.TypeSource},
		{"multica_profile", "desktop", ids.TypeSource},
		{"multica_endpoint", "https://multica.example.test", ids.TypeSource},
		{"multica_workspace", "workspace-test", ids.TypeProject},
		{"multica_issue", "issue-fixture", ids.TypeIssue},
	}
	if withRun {
		items = append(items, struct {
			kind, value string
			typ         ids.Type
		}{"multica_run", "task-fixture", ids.TypeRun})
	}
	bindings := make([]model.SourceBinding, 0, len(items))
	for _, item := range items {
		alias, err := ids.New(item.typ)
		if err != nil {
			t.Fatal(err)
		}
		value := item.value
		bindings = append(bindings, model.SourceBinding{Kind: item.kind, AliasID: alias, Fingerprint: adapter.Fingerprint("multica", item.kind, item.value), OpaqueID: &value})
	}
	return bindings
}

func createMulticaCancelExecution(t *testing.T, journalPath string, withRun bool) model.Execution {
	t.Helper()
	now := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	journal, err := store.Open(journalPath, store.Options{Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	execution, _, err := journal.CreateExecution(context.Background(), model.Execution{
		Authority: model.AuthorityMultica, Adapter: "multica", Mode: model.ModeMultica,
		Acquisition: model.AcquisitionLaunched, State: model.StateWaiting, Liveness: model.LivenessBlocked,
		SourceBindings: multicaCancelBindings(t, withRun),
		Capabilities:   model.CapabilitySnapshot{NegotiatedAt: now, AdapterVersion: "0.1.0", Items: []model.CapabilityItem{}},
		Observation:    model.Observation{Source: model.ObservationDurableOutbox, Integrity: model.IntegrityVerified, ObservedAt: now},
	}, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	return execution
}

func TestCancelMulticaAuthorityForwardsWithoutTerminalizing(t *testing.T) {
	root := t.TempDir()
	configPath, journalPath, capture := writeCancelMulticaFixture(t, root)
	execution := createMulticaCancelExecution(t, journalPath, true)
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journalPath, "--config", configPath, "cancel", execution.ID.String()}); code != 0 {
		t.Fatalf("cancel exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), `"authority":"multica"`) || !strings.Contains(stdout.String(), `"state":"waiting"`) || !strings.Contains(stdout.String(), `"cancel_request_accepted"`) {
		t.Fatalf("cancel output=%s", stdout.String())
	}
	argvBytes, err := os.ReadFile(filepath.Join(capture, "cancel.argv"))
	if err != nil {
		t.Fatal(err)
	}
	joined := string(argvBytes)
	for _, expected := range []string{"issue", "cancel-task", "task-fixture", "--issue", "issue-fixture"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("cancel argv %q missing %q", joined, expected)
		}
	}
	journal, err := store.Open(journalPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	stored, err := journal.GetExecution(context.Background(), execution.ID)
	if err != nil || stored.State != model.StateWaiting {
		t.Fatalf("stored=%#v err=%v", stored, err)
	}
	if _, err := journal.GetOutcome(context.Background(), execution.ID); err == nil {
		t.Fatal("expected no terminal outcome for Multica cancel forward")
	}
}

func TestCancelMulticaMissingRunBinding(t *testing.T) {
	root := t.TempDir()
	configPath, journalPath, _ := writeCancelMulticaFixture(t, root)
	execution := createMulticaCancelExecution(t, journalPath, false)
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journalPath, "--config", configPath, "cancel", execution.ID.String()}); code != output.ExitCodeFor(output.CodeCapabilityUnavailable) {
		t.Fatalf("cancel exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "bound task") || !strings.Contains(stdout.String(), `"multica_run_bound":false`) {
		t.Fatalf("cancel output=%s", stdout.String())
	}
}

func TestCancelNativeStillTerminalizesExecution(t *testing.T) {
	now := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	journalPath := filepath.Join(t.TempDir(), "state", "journal.db")
	journal, err := store.Open(journalPath, store.Options{Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	sessionID := "native-session"
	alias, err := ids.New(ids.TypeSource)
	if err != nil {
		t.Fatal(err)
	}
	binding := model.SourceBinding{Kind: "process", AliasID: alias, Fingerprint: adapter.Fingerprint("native", "process", sessionID), OpaqueID: &sessionID}
	execution, _, err := journal.CreateExecution(context.Background(), model.Execution{
		Authority: model.AuthorityNative, Adapter: "generic-process", Mode: model.ModeDirect,
		Acquisition: model.AcquisitionLaunched, State: model.StateRunning, Liveness: model.LivenessAlive,
		SourceBindings: []model.SourceBinding{binding}, Capabilities: model.CapabilitySnapshot{NegotiatedAt: now, AdapterVersion: "test", Items: []model.CapabilityItem{}},
		Observation: model.Observation{Source: model.ObservationNativeStream, Integrity: model.IntegrityVerified, ObservedAt: now},
	}, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := commitCancellation(context.Background(), journal, execution, now.Add(time.Second))
	if err != nil || stored.State != model.StateCancelled {
		t.Fatalf("stored=%#v err=%v", stored, err)
	}
	outcome, err := journal.GetOutcome(context.Background(), execution.ID)
	if err != nil || outcome.Failure == nil || outcome.Failure.Code != "execution_cancelled" {
		t.Fatalf("outcome=%#v err=%v", outcome, err)
	}
}
