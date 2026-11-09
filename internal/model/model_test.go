package model

import (
	"strings"
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

func TestNativeUsageAndRateLimitAreNumericMetadata(t *testing.T) {
	value := fixtureExecution(t)
	next := value.UpdatedAt.Add(time.Minute)
	value.TokenUsage = &TokenUsage{Input: 3, Output: 2, CacheRead: 1, CacheWrite: 1}
	value.RateLimit = &NativeRateLimit{Attempt: 2, RetryAfterMS: 26000, NextRetryAt: &next, ProviderErrorType: "provider.rate-limit", HTTPStatus: 429}
	if err := value.Validate(); err != nil {
		t.Fatal(err)
	}
	value.TokenUsage.Input = -1
	if err := value.Validate(); err == nil {
		t.Fatal("negative usage accepted")
	}
	value = fixtureExecution(t)
	value.RateLimit = &NativeRateLimit{Attempt: 1, HTTPStatus: 500}
	if err := value.Validate(); err == nil {
		t.Fatal("non-429 rate-limit metadata accepted")
	}
	value.RateLimit = &NativeRateLimit{Attempt: 0, HTTPStatus: 429}
	if err := value.Validate(); err == nil {
		t.Fatal("zero retry attempt accepted")
	}
	for _, providerErrorType := range []string{"sk-live-51H8xQ2eZ9", "provider.", "provider.rate limit", "provider." + strings.Repeat("a", 60)} {
		value.RateLimit = &NativeRateLimit{Attempt: 2, HTTPStatus: 429, ProviderErrorType: providerErrorType}
		if err := value.Validate(); err == nil {
			t.Fatalf("unsafe provider error type %q accepted", providerErrorType)
		}
	}
	value.RateLimit = &NativeRateLimit{Attempt: 2, HTTPStatus: 429, ProviderErrorType: "unclassified"}
	if err := value.Validate(); err != nil {
		t.Fatalf("content-free marker rejected: %v", err)
	}
}

func TestTokenUsageAggregationSaturatesWithoutSchemaOverflow(t *testing.T) {
	usage := TokenUsage{Input: maxTokenUsageCount}
	usage.Add(TokenUsage{Input: 1, Output: maxTokenUsageCount})
	if usage.Input != maxTokenUsageCount || usage.Output != maxTokenUsageCount || !usage.Saturated {
		t.Fatalf("saturated usage = %#v", usage)
	}
	if err := usage.Validate(); err != nil {
		t.Fatal(err)
	}
	usage.Input++
	if err := usage.Validate(); err == nil {
		t.Fatal("out-of-range aggregate accepted")
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

func TestExecutionLabelLengthBoundary(t *testing.T) {
	value := fixtureExecution(t)
	value.Labels = []string{"a" + strings.Repeat("b", 127)}
	if err := value.Validate(); err != nil {
		t.Fatalf("128-character label rejected: %v", err)
	}
	value.Labels = []string{"a" + strings.Repeat("b", 128)}
	if err := value.Validate(); err == nil {
		t.Fatal("129-character label accepted")
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
