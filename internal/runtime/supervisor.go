package runtime

import (
	"context"
	"errors"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/supervisor"
)

// SupervisorExecutions adapts the normalized journal and stored bindings to
// both supervisor recovery contracts. It intentionally has no process table.
type SupervisorExecutions struct{ Engine *Engine }

func (b SupervisorExecutions) ListNonTerminal(ctx context.Context) ([]supervisor.Execution, error) {
	if b.Engine == nil {
		return nil, errors.New("runtime engine is required")
	}
	values, err := b.Engine.journal.ListExecutions(ctx, true)
	if err != nil {
		return nil, wrapError("list_nonterminal", "", err)
	}
	result := make([]supervisor.Execution, 0, len(values))
	for _, value := range values {
		result = append(result, supervisor.Execution{ID: value.ID.String(), State: string(value.State), Liveness: string(value.Liveness), Revision: value.Revision, UpdatedAt: value.UpdatedAt})
	}
	return result, nil
}

// Reprobe reconstructs the adapter exclusively from the journaled execution
// bindings. Native adapters without a cross-restart status route become
// explicitly unreachable; they are never recovered through stale PID memory.
func (b SupervisorExecutions) Reprobe(ctx context.Context, minimal supervisor.Execution) (supervisor.ProbeResult, error) {
	if b.Engine == nil {
		return supervisor.ProbeResult{}, errors.New("runtime engine is required")
	}
	id, err := ids.ParseExecutionID(minimal.ID)
	if err != nil {
		return supervisor.ProbeResult{}, &Error{Code: CodeUsage, Operation: "reprobe", Message: "invalid execution ID", Cause: err}
	}
	execution, value, _, ref, err := b.Engine.resolveExecution(ctx, id)
	if err != nil {
		return supervisor.ProbeResult{}, err
	}
	now := b.Engine.now()
	if activeNativeRunnerLease(execution, now) {
		return supervisor.ProbeResult{State: string(execution.State), Liveness: string(execution.Liveness), Revision: execution.Revision, ObservedAt: execution.Observation.ObservedAt, Source: string(execution.Observation.Source)}, nil
	}
	if !capabilityUsable(execution.Capabilities, adapter.CapabilitySnapshot) {
		if result, ok, eventsErr := b.reprobeFromEvents(ctx, execution, value, ref); ok {
			if eventsErr == nil {
				return result, nil
			}
			return b.unreachableOrError(execution, now, value.Name(), eventsErr)
		}
		return unreachableProbe(execution, now), nil
	}
	snapshot, err := value.Snapshot(ctx, adapter.SnapshotRequest{Ref: ref, Poll: adapter.PollOptions{Attempts: 1, Fresh: true}})
	if err != nil {
		var runtimeErr *Error
		mapped := wrapError("reprobe", value.Name(), err)
		if errors.As(mapped, &runtimeErr) {
			switch runtimeErr.Code {
			case CodeCapabilityUnavailable, CodeDependencyUnavailable, CodeAuthenticationRequired, CodeNotFound, CodeTimeout, CodeUnknownState:
				if result, ok, eventsErr := b.reprobeFromEvents(ctx, execution, value, ref); ok {
					if eventsErr == nil {
						return result, nil
					}
					return b.unreachableOrError(execution, now, value.Name(), eventsErr)
				}
				return unreachableProbe(execution, now), nil
			}
		}
		return supervisor.ProbeResult{}, mapped
	}
	observedAt := snapshot.Session.Observation.ObservedAt
	if observedAt.IsZero() {
		observedAt = now
	}
	return supervisor.ProbeResult{State: string(snapshot.Session.State), Liveness: string(snapshot.Session.Liveness), Revision: execution.Revision, ObservedAt: observedAt, Source: snapshot.Session.Observation.Source}, nil
}

func (b SupervisorExecutions) reprobeFromEvents(ctx context.Context, execution model.Execution, value adapter.Adapter, ref adapter.SourceRef) (supervisor.ProbeResult, bool, error) {
	if execution.Authority != model.AuthorityMultica || !crossRestartEventsUsable(execution.Capabilities) {
		return supervisor.ProbeResult{}, false, nil
	}
	cursor, err := b.latestSourceCursor(ctx, execution.ID)
	if err != nil {
		return supervisor.ProbeResult{}, true, err
	}
	request := adapter.EventsRequest{Ref: ref, Cursor: cursor, Poll: adapter.PollOptions{Attempts: 1, Fresh: true}}
	stateEventCount := 0
	observedCount := 0
	if paged, ok := value.(adapter.PagedEvents); ok {
		page, pageErr := paged.EventsPage(ctx, request)
		if pageErr != nil {
			return supervisor.ProbeResult{}, true, wrapError("reprobe_events", value.Name(), pageErr)
		}
		stored, ingestErr := b.Engine.ingestEvents(ctx, execution.ID, value.Name(), ref, page.Events)
		if ingestErr != nil {
			return supervisor.ProbeResult{}, true, ingestErr
		}
		stateEventCount = len(page.Events)
		observedCount = len(stored)
		if page.NextCursor != "" && page.NextCursor != cursor {
			checkpoint, checkpointErr := b.Engine.recordWorkspaceCursor(ctx, execution.ID, page.NextCursor, page.Scanned, page.Filtered)
			if checkpointErr != nil {
				return supervisor.ProbeResult{}, true, checkpointErr
			}
			if checkpoint != nil {
				observedCount++
			}
		}
	} else {
		stored, eventsErr := b.Engine.Events(ctx, execution.ID, cursor, request.Poll)
		if eventsErr != nil {
			return supervisor.ProbeResult{}, true, eventsErr
		}
		stateEventCount, observedCount = len(stored), len(stored)
	}
	refreshed, err := b.Engine.journal.GetExecution(ctx, execution.ID)
	if err != nil {
		return supervisor.ProbeResult{}, true, wrapError("get_execution", execution.Adapter, err)
	}
	observedAt := refreshed.Observation.ObservedAt
	if stateEventCount == 0 || observedCount == 0 || observedAt.IsZero() {
		observedAt = b.Engine.now()
	}
	return supervisor.ProbeResult{
		State: string(refreshed.State), Liveness: string(refreshed.Liveness), Revision: refreshed.Revision,
		ObservedAt: observedAt, Source: string(model.ObservationDurableOutbox),
	}, true, nil
}

func crossRestartEventsUsable(snapshot model.CapabilitySnapshot) bool {
	if !capabilityUsable(snapshot, adapter.CapabilityEvents) {
		return false
	}
	for _, item := range snapshot.Items {
		if item.Name != string(adapter.CapabilityEvents) {
			continue
		}
		value, ok := item.Constraints["cross_restart"].(bool)
		return ok && value
	}
	return false
}

func (b SupervisorExecutions) latestSourceCursor(ctx context.Context, executionID ids.ExecutionID) (string, error) {
	const pageSize = 1000
	var after uint64
	var cursor string
	for {
		events, err := b.Engine.journal.ListEvents(ctx, executionID, contracts.EventQuery{AfterSequence: after, Limit: pageSize})
		if err != nil {
			return "", wrapError("list_events", "", err)
		}
		if len(events) == 0 {
			return cursor, nil
		}
		for _, event := range events {
			if event.SourcePosition == nil {
				continue
			}
			switch event.SourcePosition.Kind {
			case "multica_workspace_cursor":
				cursor = event.SourcePosition.Value
			}
		}
		next := events[len(events)-1].Sequence
		if next <= after || len(events) < pageSize {
			return cursor, nil
		}
		after = next
	}
}

func (b SupervisorExecutions) unreachableOrError(execution model.Execution, now time.Time, adapterName string, err error) (supervisor.ProbeResult, error) {
	mapped := wrapError("reprobe_events", adapterName, err)
	var runtimeErr *Error
	if errors.As(mapped, &runtimeErr) {
		switch runtimeErr.Code {
		case CodeCapabilityUnavailable, CodeDependencyUnavailable, CodeAuthenticationRequired, CodeNotFound, CodeTimeout, CodeUnknownState:
			return unreachableProbe(execution, now), nil
		}
	}
	return supervisor.ProbeResult{}, mapped
}

func unreachableProbe(execution model.Execution, now time.Time) supervisor.ProbeResult {
	return supervisor.ProbeResult{State: string(execution.State), Liveness: string(model.LivenessUnreachable), Revision: execution.Revision, ObservedAt: now, Source: string(model.ObservationUnknown)}
}

func activeNativeRunnerLease(execution model.Execution, now time.Time) bool {
	if execution.Authority != model.AuthorityNative || execution.State.Terminal() || (execution.Liveness != model.LivenessAlive && execution.Liveness != model.LivenessBlocked) || execution.Observation.Source != model.ObservationNativeStream || execution.Observation.Integrity != model.IntegrityVerified || execution.Observation.FreshForSeconds == nil || *execution.Observation.FreshForSeconds <= 0 {
		return false
	}
	expiresAt := execution.Observation.ObservedAt.Add(time.Duration(*execution.Observation.FreshForSeconds) * time.Second)
	return !now.After(expiresAt)
}

func (b SupervisorExecutions) ApplyProbe(ctx context.Context, id string, result supervisor.ProbeResult) error {
	if b.Engine == nil {
		return errors.New("runtime engine is required")
	}
	executionID, err := ids.ParseExecutionID(id)
	if err != nil {
		return &Error{Code: CodeUsage, Operation: "apply_probe", Message: "invalid execution ID", Cause: err}
	}
	execution, err := b.Engine.journal.GetExecution(ctx, executionID)
	if err != nil {
		return wrapError("get_execution", "", err)
	}
	sourceRevision := result.Revision
	if sourceRevision == 0 {
		// Runtime reprobes always carry a revision. Preserve the same CAS-race
		// safety for callers that omit it by binding this observation to the
		// envelope ApplyProbe actually read.
		sourceRevision = execution.Revision
	}
	// Reprobe and ApplyProbe are separated by adapter work and a journal read.
	// Once a newer revision has terminalized, a probe computed from the older
	// envelope cannot add current authority evidence. In particular, never let
	// an old restart-recovery failure replace verified terminal liveness and
	// integrity with unreachable/unknown uncertainty.
	staleProbe := result.Revision != 0 && result.Revision < execution.Revision
	if staleProbe && execution.State.Terminal() {
		return nil
	}
	// A live native runner may renew its lease in the same gap. Never let an
	// unreachable/unknown conclusion computed from an older revision overwrite
	// the newer verified runner lease.
	staleNativeProbe := execution.Authority == model.AuthorityNative && staleProbe && activeNativeRunnerLease(execution, b.Engine.now())
	if staleNativeProbe {
		return nil
	}
	nativeRunnerEcho := result.Source == string(model.ObservationNativeStream) && (result.Liveness == string(model.LivenessAlive) || result.Liveness == string(model.LivenessBlocked))
	if nativeRunnerEcho && (execution.State.Terminal() || activeNativeRunnerLease(execution, b.Engine.now())) {
		// The runner is the live native authority and refreshes this bounded
		// lease itself. Applying the supervisor's echo would erase the lease and
		// make the next cycle falsely classify the child as unreachable. If the
		// runner terminalized between Reprobe and ApplyProbe, the echo is stale
		// and must never mutate that terminal envelope.
		return nil
	}
	state := execution.State
	if result.State != "" {
		candidate := mapState(adapter.State(result.State))
		if !validNormalizedState(candidate) {
			return &Error{Code: CodeUnsafeObservation, Operation: "apply_probe", Adapter: execution.Adapter, Message: "reprobe returned an unknown state"}
		}
		state = candidate
	}
	observedAt := result.ObservedAt
	if observedAt.IsZero() {
		observedAt = b.Engine.now()
	}
	source := model.ObservationSource(result.Source)
	if !validObservationSource(source) {
		source = model.ObservationUnknown
	}
	integrity := model.IntegrityVerified
	if result.Liveness == string(model.LivenessUnreachable) || source == model.ObservationUnknown {
		integrity = model.IntegrityDegraded
	}
	targetLiveness := normalizeLiveness(result.Liveness, execution.Liveness)
	targetSourceState := sourceState(result.State)
	if unchangedUnreachableUnknown(execution, state, targetLiveness, targetSourceState, source, integrity) {
		// Reprobing an authority that cannot be observed after restart adds no new
		// evidence once the normalized envelope already records that uncertainty.
		// In particular, a later observation timestamp alone must not churn the
		// execution revision on every supervisor cycle.
		return nil
	}
	execution.Liveness = targetLiveness
	updated, err := b.Engine.applyObservedStateFromRevision(ctx, execution, state, sourceState(result.State), model.Observation{Source: source, Integrity: integrity, ObservedAt: observedAt}, sourceRevision)
	if err != nil {
		return err
	}
	// applyObservedState derives ordinary liveness; preserve explicit
	// unreachable/unknown evidence from restart recovery.
	if result.Liveness == string(model.LivenessUnreachable) || result.Liveness == string(model.LivenessUnknown) {
		updated.Liveness = normalizeLiveness(result.Liveness, updated.Liveness)
		updated.Observation.Integrity = integrity
		_, err = b.Engine.updateCASFromRevision(ctx, updated, sourceRevision)
	}
	return err
}

func unchangedUnreachableUnknown(execution model.Execution, state model.State, liveness model.Liveness, sourceStateValue *string, source model.ObservationSource, integrity model.Integrity) bool {
	return !state.Terminal() &&
		execution.State == state &&
		execution.Liveness == model.LivenessUnreachable &&
		liveness == model.LivenessUnreachable &&
		execution.Observation.Source == model.ObservationUnknown &&
		source == model.ObservationUnknown &&
		execution.Observation.Integrity == model.IntegrityDegraded &&
		integrity == model.IntegrityDegraded &&
		execution.Observation.FreshForSeconds == nil &&
		equalOptionalString(execution.SourceState, sourceStateValue)
}

func equalOptionalString(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func normalizeLiveness(value string, fallback model.Liveness) model.Liveness {
	switch model.Liveness(value) {
	case model.LivenessUnknown, model.LivenessAlive, model.LivenessBlocked, model.LivenessExited, model.LivenessUnreachable:
		return model.Liveness(value)
	default:
		return fallback
	}
}

func validNormalizedState(value model.State) bool {
	switch value {
	case model.StateCreated, model.StateStarting, model.StateRunning, model.StateWaiting, model.StateAttention, model.StateCompleted, model.StateFailed, model.StateCancelled, model.StateOrphaned:
		return true
	default:
		return false
	}
}

func validObservationSource(value model.ObservationSource) bool {
	switch value {
	case model.ObservationDurableOutbox, model.ObservationNativeStream, model.ObservationStatusAPI, model.ObservationStateFile, model.ObservationPoll, model.ObservationProcess, model.ObservationReconciled, model.ObservationUnknown:
		return true
	default:
		return false
	}
}

// OutboxRecord is the storage-neutral durable record consumed by the bridge.
type OutboxRecord struct {
	ID              string
	ExecutionID     string
	SubscriptionID  string
	Attempts        int
	AttemptInFlight bool
	NextAttemptAt   time.Time
	ExpiresAt       time.Time
	Payload         []byte
}

// DurableOutbox is intentionally separate from supervisor.Outbox so a store
// can expose domain names and atomically persist receipts later.
type DurableOutbox interface {
	ListPending(context.Context) ([]OutboxRecord, error)
	MarkAcknowledged(context.Context, string) error
	ScheduleRetry(context.Context, string, time.Time, string) error
	MarkDeadLetter(context.Context, string, string) error
}

// AttemptStartingOutbox is the optional durable pre-send boundary. Stores
// return the same in-flight payload after a crash and mint fresh metadata only
// after a scheduled retry has been classified.
type AttemptStartingOutbox interface {
	BeginAttempt(context.Context, string) (OutboxRecord, error)
}

type OutboxBridge struct{ Store DurableOutbox }

func (b OutboxBridge) Pending(ctx context.Context) ([]supervisor.Delivery, error) {
	if b.Store == nil {
		return nil, errors.New("durable outbox is required")
	}
	entries, err := b.Store.ListPending(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]supervisor.Delivery, 0, len(entries))
	for _, entry := range entries {
		result = append(result, supervisor.Delivery{ID: entry.ID, ExecutionID: entry.ExecutionID, SubscriptionID: entry.SubscriptionID, Attempts: entry.Attempts, AttemptInFlight: entry.AttemptInFlight, NextAttemptAt: entry.NextAttemptAt, ExpiresAt: entry.ExpiresAt, Payload: append([]byte(nil), entry.Payload...)})
	}
	return result, nil
}
func (b OutboxBridge) Ack(ctx context.Context, id string) error {
	return b.Store.MarkAcknowledged(ctx, id)
}
func (b OutboxBridge) Retry(ctx context.Context, id string, next time.Time, reason string) error {
	return b.Store.ScheduleRetry(ctx, id, next, bounded(reason, 256))
}
func (b OutboxBridge) DeadLetter(ctx context.Context, id, reason string) error {
	return b.Store.MarkDeadLetter(ctx, id, bounded(reason, 256))
}

func (b OutboxBridge) BeginAttempt(ctx context.Context, id string) (supervisor.Delivery, error) {
	starter, ok := b.Store.(AttemptStartingOutbox)
	if !ok {
		return supervisor.Delivery{}, supervisor.ErrAttemptStartUnsupported
	}
	entry, err := starter.BeginAttempt(ctx, id)
	if err != nil {
		return supervisor.Delivery{}, err
	}
	return supervisor.Delivery{ID: entry.ID, ExecutionID: entry.ExecutionID, SubscriptionID: entry.SubscriptionID, Attempts: entry.Attempts, AttemptInFlight: entry.AttemptInFlight, NextAttemptAt: entry.NextAttemptAt, ExpiresAt: entry.ExpiresAt, Payload: append([]byte(nil), entry.Payload...)}, nil
}

// DeliveryTransport sends one bounded record and does not mutate its outbox.
type DeliveryTransport interface {
	Deliver(context.Context, OutboxRecord) error
}

type DelivererBridge struct{ Transport DeliveryTransport }

func (b DelivererBridge) Deliver(ctx context.Context, value supervisor.Delivery) error {
	if b.Transport == nil {
		return &supervisor.PermanentDeliveryError{Err: errors.New("delivery transport is required")}
	}
	err := b.Transport.Deliver(ctx, OutboxRecord{ID: value.ID, ExecutionID: value.ExecutionID, SubscriptionID: value.SubscriptionID, Attempts: value.Attempts, AttemptInFlight: value.AttemptInFlight, NextAttemptAt: value.NextAttemptAt, ExpiresAt: value.ExpiresAt, Payload: append([]byte(nil), value.Payload...)})
	if err == nil {
		return nil
	}
	var runtimeErr *Error
	if errors.As(err, &runtimeErr) {
		if runtimeErr.Retryable {
			return &supervisor.RetryableDeliveryError{Err: runtimeErr}
		}
		return &supervisor.PermanentDeliveryError{Err: runtimeErr}
	}
	return err
}

var _ supervisor.ExecutionRepository = SupervisorExecutions{}
var _ supervisor.Reprober = SupervisorExecutions{}
var _ supervisor.Outbox = OutboxBridge{}
var _ supervisor.AttemptStarter = OutboxBridge{}
var _ supervisor.Deliverer = DelivererBridge{}
