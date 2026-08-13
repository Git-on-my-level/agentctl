package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

func TestRunnerLeasePreservesBlockedLiveness(t *testing.T) {
	for _, state := range []model.State{model.StateWaiting, model.StateAttention} {
		if got := runnerLeaseLiveness(state); got != model.LivenessBlocked {
			t.Fatalf("state %s liveness = %s", state, got)
		}
	}
	for _, state := range []model.State{model.StateCreated, model.StateStarting, model.StateRunning} {
		if got := runnerLeaseLiveness(state); got != model.LivenessAlive {
			t.Fatalf("state %s liveness = %s", state, got)
		}
	}
}

func TestCommitCancellationWritesOutcomeAndTerminalEventAtomically(t *testing.T) {
	now := time.Date(2026, 8, 13, 3, 0, 0, 0, time.UTC)
	journal, err := store.Open(filepath.Join(t.TempDir(), "state", "journal.db"), store.Options{Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	execution, _, err := journal.CreateExecution(context.Background(), model.Execution{Authority: model.AuthorityNative, Adapter: "generic-process", Mode: model.ModeDirect, Acquisition: model.AcquisitionLaunched, State: model.StateRunning, Liveness: model.LivenessAlive, SourceBindings: []model.SourceBinding{}, Capabilities: model.CapabilitySnapshot{NegotiatedAt: now, AdapterVersion: "test", Items: []model.CapabilityItem{}}, Observation: model.Observation{Source: model.ObservationNativeStream, Integrity: model.IntegrityVerified, ObservedAt: now}}, contracts.MutationKey{})
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
	events, err := journal.ListEvents(context.Background(), execution.ID, contracts.EventQuery{})
	if err != nil || len(events) != 1 || events[0].Kind != model.EventTerminal {
		t.Fatalf("events=%#v err=%v", events, err)
	}
}

func TestFinalizeResultConvergesOnExistingTerminalOutcome(t *testing.T) {
	now := time.Date(2026, 8, 13, 3, 0, 0, 0, time.UTC)
	journal, err := store.Open(filepath.Join(t.TempDir(), "state", "journal.db"), store.Options{Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	execution, _, err := journal.CreateExecution(context.Background(), model.Execution{Authority: model.AuthorityNative, Adapter: "generic-process", Mode: model.ModeDirect, Acquisition: model.AcquisitionLaunched, State: model.StateRunning, Liveness: model.LivenessAlive, SourceBindings: []model.SourceBinding{}, Capabilities: model.CapabilitySnapshot{NegotiatedAt: now, AdapterVersion: "test", Items: []model.CapabilityItem{}}, Observation: model.Observation{Source: model.ObservationNativeStream, Integrity: model.IntegrityVerified, ObservedAt: now}}, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := commitCancellation(context.Background(), journal, execution, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	completed := adapter.Result{Success: true, State: adapter.StateCompleted, Content: "late result", ContentType: "text/plain"}
	stored, err := finalizeResultConverging(context.Background(), journal, execution, completed, now.Add(2*time.Second), false)
	if err != nil || stored.ID != cancelled.ID || stored.State != model.StateCancelled {
		t.Fatalf("stored=%#v err=%v", stored, err)
	}
}
