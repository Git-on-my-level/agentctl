package model

import (
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/ids"
)

func fixtureExecution(t *testing.T) Execution {
	t.Helper()
	exec, _ := ids.FromPayload(ids.TypeExecution, 1)
	host, _ := ids.FromPayload(ids.TypeHost, 2)
	execID, _ := ids.ParseExecutionID(exec.String())
	hostID, _ := ids.ParseHostID(host.String())
	now := time.Date(2026, 8, 10, 18, 0, 0, 0, time.UTC)
	return Execution{SchemaVersion: 1, ID: execID, OriginHostID: hostID, Revision: 1, Authority: AuthorityNative, Adapter: "codex", Mode: ModeDirect, Acquisition: AcquisitionLaunched, State: StateRunning, Liveness: LivenessAlive, SourceBindings: []SourceBinding{}, Capabilities: CapabilitySnapshot{NegotiatedAt: now, AdapterVersion: "1.0.0", Items: []CapabilityItem{}}, Supersedes: []ids.ExecutionID{}, CreatedAt: now, UpdatedAt: now, Observation: Observation{Source: ObservationNativeStream, Integrity: IntegrityVerified, ObservedAt: now}}
}

func TestExecutionSchemaInvariants(t *testing.T) {
	value := fixtureExecution(t)
	if err := value.Validate(); err != nil {
		t.Fatal(err)
	}
	value.Mode = ModeMultica
	if err := value.Validate(); err == nil {
		t.Fatal("mode/authority disagreement accepted")
	}
	value = fixtureExecution(t)
	value.State = StateCompleted
	if err := value.Validate(); err == nil {
		t.Fatal("terminal state without terminal_at accepted")
	}
}

func TestExecutionLabelsAreBoundedUniqueMetadata(t *testing.T) {
	value := fixtureExecution(t)
	value.Labels = []string{"review", "model.grok"}
	if err := value.Validate(); err != nil {
		t.Fatal(err)
	}
	value.Labels = []string{"review", "review"}
	if err := value.Validate(); err == nil {
		t.Fatal("duplicate labels accepted")
	}
	value.Labels = []string{"Contains Secret Spaces"}
	if err := value.Validate(); err == nil {
		t.Fatal("invalid label accepted")
	}
	previous := fixtureExecution(t)
	previous.Labels = []string{"review"}
	next := previous
	next.Revision++
	next.Labels = []string{"changed"}
	if err := ValidateTransition(previous, next); err == nil {
		t.Fatal("label mutation accepted")
	}
}

func TestTerminalTransitionCannotRegress(t *testing.T) {
	previous := fixtureExecution(t)
	terminal := previous.UpdatedAt.Add(time.Second)
	previous.State = StateCompleted
	previous.TerminalAt = &terminal
	previous.UpdatedAt = terminal
	previous.Observation.ObservedAt = terminal
	next := previous
	next.Revision = 2
	next.State = StateRunning
	next.TerminalAt = nil
	if err := ValidateTransition(previous, next); err == nil {
		t.Fatal("terminal regression accepted")
	}
}

func TestEventRejectsRawContentAndAcceptsReferences(t *testing.T) {
	execution := fixtureExecution(t)
	eventID, _ := ids.FromPayload(ids.TypeEvent, 3)
	parsedEvent, _ := ids.ParseEventID(eventID.String())
	now := execution.UpdatedAt
	event := Event{SchemaVersion: 1, ID: parsedEvent, OriginHostID: execution.OriginHostID, ExecutionID: execution.ID, Sequence: 1, Ordering: OrderingObservation, Kind: EventProgress, Authority: AuthorityNative, Adapter: "codex", ObservedAt: now, DedupeKey: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", DedupeVersion: 1, Payload: map[string]any{"artifact_ids": []any{}}}
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}
	event.Payload = map[string]any{"nested": map[string]any{"transcript": "secret"}}
	if err := event.Validate(); err == nil {
		t.Fatal("raw transcript payload accepted")
	}
}
