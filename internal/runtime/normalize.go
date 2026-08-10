package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/callback"
	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
)

func normalizeObservation(observation adapter.Observation, fallback time.Time) model.Observation {
	observedAt := observation.ObservedAt
	if observedAt.IsZero() {
		observedAt = fallback
	}
	var fresh *int
	if observation.FreshFor >= 0 {
		seconds := int(observation.FreshFor / time.Second)
		fresh = &seconds
	}
	source := model.ObservationSource(observation.Source)
	switch source {
	case model.ObservationDurableOutbox, model.ObservationNativeStream, model.ObservationStatusAPI, model.ObservationStateFile, model.ObservationPoll, model.ObservationProcess, model.ObservationReconciled:
	default:
		source = model.ObservationUnknown
	}
	integrity := model.Integrity(observation.Integrity)
	switch integrity {
	case model.IntegrityVerified, model.IntegrityDegraded, model.IntegrityConflicted:
	default:
		integrity = model.IntegrityUnknown
	}
	return model.Observation{Source: source, Integrity: integrity, ObservedAt: observedAt, FreshForSeconds: fresh}
}

func observationFromEvent(event adapter.Event, fallback time.Time) model.Observation {
	observedAt := event.ObservedAt
	if observedAt.IsZero() {
		observedAt = fallback
	}
	source := model.ObservationNativeStream
	integrity := model.IntegrityVerified
	if event.Ordering == string(model.OrderingObservation) {
		source, integrity = model.ObservationPoll, model.IntegrityDegraded
	}
	if event.Ordering == string(model.OrderingReconciled) {
		source = model.ObservationReconciled
	}
	return model.Observation{Source: source, Integrity: integrity, ObservedAt: observedAt}
}

func normalizeEvent(execution model.Execution, event adapter.Event, fallback time.Time) (model.Event, []byte, error) {
	payload, err := safePayload(event.Payload)
	if err != nil {
		return model.Event{}, nil, err
	}
	observedAt := event.ObservedAt
	if observedAt.IsZero() {
		observedAt = fallback
	}
	ordering := model.Ordering(event.Ordering)
	if ordering != model.OrderingSource && ordering != model.OrderingReconciled {
		ordering = model.OrderingObservation
	}
	var position *model.SourcePosition
	if event.SourceState == "workspace_cursor_advanced" && event.Cursor != "" {
		position = &model.SourcePosition{Kind: "multica_workspace_cursor", Value: bounded(event.Cursor, 512)}
	} else if ordering == model.OrderingSource || ordering == model.OrderingReconciled {
		value := event.SourcePosition
		positionKind := "native_sequence"
		if value == "" {
			value = event.Cursor
			positionKind = "adapter_cursor"
		}
		if value == "" && event.Sequence > 0 {
			value = fmt.Sprintf("%d", event.Sequence)
			positionKind = "adapter_sequence"
		}
		if value == "" {
			ordering = model.OrderingObservation
		} else {
			if ordering == model.OrderingReconciled {
				positionKind = "reconciled_" + positionKind
			}
			position = &model.SourcePosition{Kind: positionKind, Value: bounded(value, 512)}
		}
	}
	kind := model.EventKind(event.Kind)
	switch kind {
	case model.EventStarted, model.EventProgress, model.EventAttention, model.EventArtifact, model.EventHealth, model.EventTerminal, model.EventPromoted, model.EventSuperseded:
	default:
		return model.Event{}, nil, &Error{Code: CodeUnsafeObservation, Operation: "normalize_event", Adapter: execution.Adapter, Message: "adapter returned an unknown event kind"}
	}
	var state *model.State
	if event.State != "" {
		mapped := mapState(event.State)
		if !validNormalizedState(mapped) {
			return model.Event{}, nil, &Error{Code: CodeUnsafeObservation, Operation: "normalize_event", Adapter: execution.Adapter, Message: "adapter event returned an unknown state"}
		}
		state = &mapped
	}
	projectionValue := map[string]any{
		"authority": string(execution.Authority), "adapter": execution.Adapter,
		"source_fingerprints": bindingFingerprints(execution), "source_dedupe_key": event.DedupeKey,
		"kind": string(kind), "state": state, "source_state": event.SourceState,
		"ordering": string(ordering), "source_position": position, "payload": payload,
	}
	producer, err := callback.NewProducer(execution.Adapter, 1)
	if err != nil {
		return model.Event{}, nil, &Error{Code: CodeUnsafeObservation, Operation: "normalize_event", Adapter: execution.Adapter, Message: "invalid semantic event producer", Cause: err}
	}
	semantic, err := producer.Produce(projectionValue)
	if err != nil {
		return model.Event{}, nil, &Error{Code: CodeUnsafeObservation, Operation: "normalize_event", Adapter: execution.Adapter, Message: "event projection is not JSON encodable", Cause: err}
	}
	dedupeKey, projection := semantic.DedupeKey, semantic.Projection
	normalized := model.Event{ExecutionID: execution.ID, OriginHostID: execution.OriginHostID, Ordering: ordering, SourcePosition: position, Kind: kind, State: state, SourceState: sourceState(event.SourceState), Authority: execution.Authority, Adapter: execution.Adapter, OccurredAt: event.OccurredAt, ObservedAt: observedAt, DedupeKey: dedupeKey, DedupeVersion: 1, Payload: payload}
	return normalized, projection, nil
}

func bindingFingerprints(execution model.Execution) []string {
	values := make([]string, 0, len(execution.SourceBindings))
	for _, binding := range execution.SourceBindings {
		values = append(values, binding.Fingerprint)
	}
	return values
}

func safePayload(payload map[string]any) (map[string]any, error) {
	if payload == nil {
		return map[string]any{}, nil
	}
	copy, err := copySafeValue(payload)
	if err != nil {
		return nil, err
	}
	result, ok := copy.(map[string]any)
	if !ok {
		return nil, &Error{Code: CodeUnsafeObservation, Operation: "normalize_event", Message: "event payload is not an object"}
	}
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) > 64*1024 {
		return nil, &Error{Code: CodeUnsafeObservation, Operation: "normalize_event", Message: "event payload is invalid or exceeds 64 KiB", Cause: err}
	}
	return result, nil
}

func copySafeValue(value any) (any, error) {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			if sourceIdentifierKey(key) {
				continue
			}
			if forbiddenPayloadKey(key) {
				return nil, &Error{Code: CodeUnsafeObservation, Operation: "normalize_event", Message: "adapter event contained transcript-like payload material", Details: map[string]any{"field": strings.ToLower(key)}}
			}
			copy, err := copySafeValue(item)
			if err != nil {
				return nil, err
			}
			out[key] = copy
		}
		return out, nil
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			copy, err := copySafeValue(item)
			if err != nil {
				return nil, err
			}
			out[index] = copy
		}
		return out, nil
	case nil, bool, string, float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, json.Number:
		return typed, nil
	default:
		return nil, &Error{Code: CodeUnsafeObservation, Operation: "normalize_event", Message: "adapter event payload contains an unsupported value"}
	}
}

func sourceIdentifierKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "opaque_id", "session_id", "thread_id", "run_id", "issue_id", "workspace_id", "project_id", "aggregate_id", "entity_id":
		return true
	default:
		return false
	}
}

func forbiddenPayloadKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "prompt", "raw_prompt", "transcript", "reasoning", "chain_of_thought", "stdout", "stderr", "raw_output", "token_delta", "tokens":
		return true
	default:
		return false
	}
}

func (e *Engine) terminalConflict(ctx context.Context, execution model.Execution, incoming model.Event) (bool, error) {
	if !execution.State.Terminal() {
		return false, nil
	}
	if incoming.State == nil {
		return false, nil
	}
	return execution.State != *incoming.State, nil
}

func (e *Engine) existingTerminal(ctx context.Context, executionID ids.ExecutionID) (*model.Event, error) {
	events, err := e.journal.ListEvents(ctx, executionID, contracts.EventQuery{Kinds: []model.EventKind{model.EventTerminal}, Limit: 2})
	if err != nil {
		return nil, &Error{Code: CodeInternal, Operation: "list_terminal_events", Message: "failed to inspect terminal journal state", Cause: err}
	}
	if len(events) == 0 {
		return nil, nil
	}
	return &events[0], nil
}

func (e *Engine) recordTerminalConflict(ctx context.Context, execution model.Execution, incoming model.Event) (model.Event, error) {
	execution.Observation.Integrity = model.IntegrityConflicted
	execution.Liveness = model.LivenessUnknown
	execution.Observation.ObservedAt = e.now()
	execution.UpdatedAt = execution.Observation.ObservedAt
	if _, err := e.updateCAS(ctx, execution); err != nil {
		return model.Event{}, err
	}
	payload := map[string]any{"reason": "terminal_conflict", "retained_state": string(execution.State)}
	if incoming.State != nil {
		payload["observed_state"] = string(*incoming.State)
	}
	health := adapter.Event{Kind: string(model.EventHealth), SourceState: "terminal_conflict", ObservedAt: e.now(), Ordering: string(model.OrderingObservation), DedupeKey: adapter.Fingerprint(execution.ID.String(), "terminal_conflict", fmt.Sprint(payload["observed_state"])), Payload: payload}
	normalized, projection, err := normalizeEvent(execution, health, e.now())
	if err != nil {
		return model.Event{}, err
	}
	stored, _, err := e.journal.AppendEvent(ctx, normalized, projection)
	if err != nil {
		return model.Event{}, &Error{Code: CodeConflict, Operation: "terminal_conflict", Adapter: execution.Adapter, Message: "failed to journal terminal conflict", Cause: err}
	}
	return stored, nil
}
