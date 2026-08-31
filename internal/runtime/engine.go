package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
)

// Journal is the small operational subset needed by the integration layer.
// *store.Journal satisfies it without giving adapters persistence access.
type Journal interface {
	HostID(context.Context) (ids.HostID, error)
	CreateExecution(context.Context, model.Execution, contracts.MutationKey) (model.Execution, bool, error)
	GetExecution(context.Context, ids.ExecutionID) (model.Execution, error)
	UpdateExecution(context.Context, model.Execution, uint64) (model.Execution, error)
	ListExecutions(context.Context, bool) ([]model.Execution, error)
	AppendEvent(context.Context, model.Event, []byte) (model.Event, bool, error)
	ListEvents(context.Context, ids.ExecutionID, contracts.EventQuery) ([]model.Event, error)
}

type Clock func() time.Time

type Options struct {
	Registry  *Registry
	Generator ids.Generator
	Clock     Clock
}

type Engine struct {
	journal   Journal
	registry  *Registry
	generator ids.Generator
	clock     Clock
}

func New(journal Journal, options Options) (*Engine, error) {
	if journal == nil {
		return nil, errors.New("runtime journal is required")
	}
	if options.Registry == nil {
		options.Registry = DefaultRegistry()
	}
	if options.Generator == nil {
		options.Generator = ids.CryptoGenerator{}
	}
	if options.Clock == nil {
		options.Clock = time.Now
	}
	return &Engine{journal: journal, registry: options.Registry, generator: options.Generator, clock: options.Clock}, nil
}

type LaunchOptions struct {
	Adapter      AdapterSpec
	Request      adapter.LaunchRequest
	Probe        adapter.ProbeRequest
	Mutation     contracts.MutationKey
	TaskContract *model.TaskContract
}

type AttachOptions struct {
	Adapter      AdapterSpec
	Request      adapter.AttachRequest
	Probe        adapter.ProbeRequest
	Mutation     contracts.MutationKey
	TaskContract *model.TaskContract
}

// Probe returns the immutable normalized capability snapshot used by an
// execution revision. It never falls back from Multica to a native profile.
func (e *Engine) Probe(ctx context.Context, spec AdapterSpec, request adapter.ProbeRequest) (model.CapabilitySnapshot, error) {
	value, err := e.registry.Resolve(spec)
	if err != nil {
		return model.CapabilitySnapshot{}, err
	}
	request = completeProbeRequest(spec, request)
	result, err := value.Probe(ctx, request)
	if err != nil {
		return model.CapabilitySnapshot{}, wrapError("probe", value.Name(), err)
	}
	snapshot := normalizeCapabilities(result)
	if snapshot.NegotiatedAt.IsZero() {
		snapshot.NegotiatedAt = e.now()
	}
	if snapshot.AdapterVersion == "" {
		snapshot.AdapterVersion = value.Manifest().AdapterVersion
	}
	return snapshot, nil
}

func (e *Engine) Launch(ctx context.Context, options LaunchOptions) (model.Execution, error) {
	if options.Adapter.Executable == "" && len(options.Request.Argv) > 0 {
		options.Adapter.Executable = options.Request.Argv[0]
	}
	value, err := e.registry.Resolve(options.Adapter)
	if err != nil {
		return model.Execution{}, err
	}
	capabilities, err := e.probeResolved(ctx, value, options.Adapter, options.Probe)
	if err != nil {
		return model.Execution{}, err
	}
	if err := requireCapability(capabilities, adapter.CapabilityLaunch); err != nil {
		return model.Execution{}, err
	}
	if err := requireContextGuarantee(value, capabilities, options.Request.Context); err != nil {
		return model.Execution{}, err
	}
	execution, reused, err := e.createStarting(ctx, value.Name(), options.Adapter, model.AcquisitionLaunched, capabilities, options.Request.Cwd, options.TaskContract, options.Mutation)
	if err != nil {
		return model.Execution{}, wrapError("create_execution", value.Name(), err)
	}
	if reused {
		return execution, nil
	}
	launched, launchErr := value.Launch(ctx, options.Request)
	if launchErr != nil {
		failed, updateErr := e.recordOperationFailure(ctx, execution, "launch", launchErr)
		if updateErr != nil {
			return execution, updateErr
		}
		return failed, wrapError("launch", value.Name(), launchErr)
	}
	updated, err := e.applySession(ctx, execution, launched.Session, options.Adapter)
	if err != nil {
		return execution, err
	}
	if launched.Result != nil {
		updated, err = e.applyResult(ctx, updated, *launched.Result)
	}
	return updated, err
}

func (e *Engine) Attach(ctx context.Context, options AttachOptions) (model.Execution, error) {
	if options.Adapter.Executable == "" {
		options.Adapter.Executable = options.Probe.Executable
	}
	value, err := e.registry.Resolve(options.Adapter)
	if err != nil {
		return model.Execution{}, err
	}
	capabilities, err := e.probeResolved(ctx, value, options.Adapter, options.Probe)
	if err != nil {
		return model.Execution{}, err
	}
	if err := requireCapability(capabilities, adapter.CapabilityAttach); err != nil {
		return model.Execution{}, err
	}
	attached, err := value.Attach(ctx, options.Request)
	if err != nil {
		return model.Execution{}, wrapError("attach", value.Name(), err)
	}
	execution, reused, err := e.createStarting(ctx, value.Name(), options.Adapter, model.AcquisitionAttached, capabilities, "", options.TaskContract, options.Mutation)
	if err != nil {
		return model.Execution{}, wrapError("create_execution", value.Name(), err)
	}
	if reused {
		return execution, nil
	}
	return e.applySession(ctx, execution, attached.Session, options.Adapter)
}

func (e *Engine) Snapshot(ctx context.Context, executionID ids.ExecutionID, poll adapter.PollOptions) (model.Execution, error) {
	execution, value, spec, ref, err := e.resolveExecution(ctx, executionID)
	if err != nil {
		return model.Execution{}, err
	}
	if err := requireCapability(execution.Capabilities, adapter.CapabilitySnapshot); err != nil {
		return model.Execution{}, err
	}
	snapshot, err := value.Snapshot(ctx, adapter.SnapshotRequest{Ref: ref, Poll: poll})
	if err != nil {
		return model.Execution{}, wrapError("snapshot", value.Name(), err)
	}
	return e.applySession(ctx, execution, snapshot.Session, spec)
}

func (e *Engine) Events(ctx context.Context, executionID ids.ExecutionID, cursor string, poll adapter.PollOptions) ([]model.Event, error) {
	execution, value, _, ref, err := e.resolveExecution(ctx, executionID)
	if err != nil {
		return nil, err
	}
	if err := requireCapability(execution.Capabilities, adapter.CapabilityEvents); err != nil {
		return nil, err
	}
	observed, err := value.Events(ctx, adapter.EventsRequest{Ref: ref, Cursor: cursor, Poll: poll})
	if err != nil {
		return nil, wrapError("events", value.Name(), err)
	}
	return e.ingestEvents(ctx, executionID, value.Name(), ref, observed)
}

func (e *Engine) ingestEvents(ctx context.Context, executionID ids.ExecutionID, adapterName string, ref adapter.SourceRef, observed []adapter.Event) ([]model.Event, error) {
	stored := make([]model.Event, 0, len(observed))
	for _, event := range observed {
		current, getErr := e.journal.GetExecution(ctx, executionID)
		if getErr != nil {
			return stored, wrapError("get_execution", adapterName, getErr)
		}
		if current.Authority == model.AuthorityMultica && !multicaEventMatchesRef(event, ref) {
			checkpoint, checkpointErr := e.recordScopeCheckpoint(ctx, current, event)
			if checkpointErr != nil {
				return stored, checkpointErr
			}
			if checkpoint != nil {
				stored = append(stored, *checkpoint)
			}
			continue
		}
		if current.Authority == model.AuthorityMultica {
			event = constrainMulticaEventAuthority(event, ref)
		}
		normalized, projection, normalizeErr := normalizeEvent(current, event, e.now())
		if normalizeErr != nil {
			return stored, normalizeErr
		}
		if event.Kind == string(model.EventTerminal) {
			conflict, existingErr := e.terminalConflict(ctx, current, normalized)
			if existingErr != nil {
				return stored, existingErr
			}
			if conflict {
				health, healthErr := e.recordTerminalConflict(ctx, current, normalized)
				if healthErr != nil {
					return stored, healthErr
				}
				stored = append(stored, health)
				continue
			}
			if current.State.Terminal() {
				existing, findErr := e.existingTerminal(ctx, current.ID)
				if findErr != nil {
					return stored, findErr
				}
				if existing != nil {
					stored = append(stored, *existing)
					continue
				}
			}
		}
		appended, _, appendErr := e.journal.AppendEvent(ctx, normalized, projection)
		if appendErr != nil {
			return stored, &Error{Code: CodeConflict, Operation: "append_event", Adapter: adapterName, Message: "journal rejected normalized event", Cause: appendErr}
		}
		stored = append(stored, appended)
		if event.State != "" {
			observation := observationFromEvent(event, e.now())
			if current.Authority == model.AuthorityMultica && crossRestartEventsUsable(current.Capabilities) {
				observation.Source = model.ObservationDurableOutbox
			}
			_, applyErr := e.applyObservedState(ctx, current, mapState(event.State), sourceState(event.SourceState), observation)
			if applyErr != nil {
				return stored, applyErr
			}
		}
	}
	return stored, nil
}

func constrainMulticaEventAuthority(event adapter.Event, ref adapter.SourceRef) adapter.Event {
	if ref.Run != "" || event.Kind != string(model.EventTerminal) {
		return event
	}
	aggregate, _ := payloadString(event.Payload, "aggregate_kind", "aggregate_type")
	isIssueEvent := strings.EqualFold(aggregate, "issue") || strings.HasPrefix(strings.ToLower(event.SourceState), "issue.") || strings.HasPrefix(strings.ToLower(event.SourceState), "issue:")
	if isIssueEvent {
		return event
	}
	if event.State == adapter.StateFailed || event.State == adapter.StateCancelled || event.State == adapter.StateOrphaned {
		event.Kind = string(model.EventAttention)
	} else {
		event.Kind = string(model.EventProgress)
	}
	event.State = adapter.StateWaiting
	return event
}

// multicaEventMatchesRef is defense in depth around the adapter's exact page
// filtering. Missing scope fields are accepted because older Multica events do
// not carry every selector; an explicit conflicting selector is never applied.
func multicaEventMatchesRef(event adapter.Event, ref adapter.SourceRef) bool {
	for _, check := range []struct {
		want string
		keys []string
	}{
		{ref.Workspace, []string{"workspace_id", "workspace"}},
		{ref.Issue, []string{"issue_id", "issue"}},
		{ref.Run, []string{"run_id", "run"}},
	} {
		if check.want == "" {
			continue
		}
		if got, present := payloadString(event.Payload, check.keys...); present && got != check.want {
			return false
		}
	}
	return true
}

func payloadString(payload map[string]any, keys ...string) (string, bool) {
	for _, key := range keys {
		value, ok := payload[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case string:
			return typed, typed != ""
		case json.Number:
			return typed.String(), true
		case float64:
			return strconv.FormatFloat(typed, 'f', -1, 64), true
		case int:
			return strconv.Itoa(typed), true
		case int64:
			return strconv.FormatInt(typed, 10), true
		}
	}
	return "", false
}

// recordScopeCheckpoint persists only the source position, never identifiers
// or payload from the unrelated event. This prevents a durable workspace
// cursor from replaying the same ignored page forever after restart.
func (e *Engine) recordScopeCheckpoint(ctx context.Context, execution model.Execution, observed adapter.Event) (*model.Event, error) {
	if observed.Cursor == "" && observed.Sequence <= 0 {
		return nil, nil
	}
	checkpoint := adapter.Event{
		Sequence: observed.Sequence, Cursor: observed.Cursor,
		Kind: string(model.EventHealth), SourceState: "scope_event_ignored",
		ObservedAt: observed.ObservedAt, Ordering: observed.Ordering,
		DedupeKey: adapter.Fingerprint(observed.DedupeKey, observed.Cursor, "scope_event_ignored"),
		Payload:   map[string]any{"reason": "out_of_scope_event"},
	}
	normalized, projection, err := normalizeEvent(execution, checkpoint, e.now())
	if err != nil {
		return nil, err
	}
	stored, _, err := e.journal.AppendEvent(ctx, normalized, projection)
	if err != nil {
		return nil, &Error{Code: CodeConflict, Operation: "events", Adapter: execution.Adapter, Message: "journal rejected scope checkpoint", Cause: err}
	}
	return &stored, nil
}

func (e *Engine) recordWorkspaceCursor(ctx context.Context, executionID ids.ExecutionID, nextCursor string, scanned, filtered int) (*model.Event, error) {
	if nextCursor == "" {
		return nil, nil
	}
	execution, err := e.journal.GetExecution(ctx, executionID)
	if err != nil {
		return nil, wrapError("get_execution", "multica", err)
	}
	checkpoint := adapter.Event{
		Cursor: nextCursor, Kind: string(model.EventHealth), SourceState: "workspace_cursor_advanced",
		ObservedAt: e.now(), Ordering: string(model.OrderingSource),
		DedupeKey: adapter.Fingerprint(execution.ID.String(), "multica_workspace_cursor", nextCursor),
		Payload: map[string]any{
			"cursor_advanced": true, "scanned": scanned, "filtered": filtered,
		},
	}
	normalized, projection, err := normalizeEvent(execution, checkpoint, e.now())
	if err != nil {
		return nil, err
	}
	stored, _, err := e.journal.AppendEvent(ctx, normalized, projection)
	if err != nil {
		return nil, &Error{Code: CodeConflict, Operation: "events", Adapter: execution.Adapter, Message: "journal rejected Multica workspace cursor", Cause: err}
	}
	return &stored, nil
}

// Result obtains the adapter's bounded result projection, journals a terminal
// event when authoritative, and returns the updated normalized execution.
func (e *Engine) Result(ctx context.Context, executionID ids.ExecutionID, poll adapter.PollOptions) (model.Execution, error) {
	execution, value, _, ref, err := e.resolveExecution(ctx, executionID)
	if err != nil {
		return model.Execution{}, err
	}
	if err := requireCapability(execution.Capabilities, adapter.CapabilityResult); err != nil {
		return model.Execution{}, err
	}
	result, err := value.Result(ctx, adapter.ResultRequest{Ref: ref, Poll: poll})
	if err != nil {
		return model.Execution{}, wrapError("result", value.Name(), err)
	}
	return e.applyResult(ctx, execution, result)
}

// Cancel records a cancel request but does not infer cancellation from command
// acceptance. A fresh snapshot is applied only when the adapter can provide it.
func (e *Engine) Cancel(ctx context.Context, executionID ids.ExecutionID, signal string, grace time.Duration) (model.Execution, error) {
	execution, value, spec, ref, err := e.resolveExecution(ctx, executionID)
	if err != nil {
		return model.Execution{}, err
	}
	capabilities, err := e.probeResolved(ctx, value, spec, adapter.ProbeRequest{Fresh: true})
	if err != nil {
		return model.Execution{}, err
	}
	if err := requireCapability(capabilities, adapter.CapabilityCancel); err != nil {
		return model.Execution{}, err
	}
	execution.Capabilities = capabilities
	execution.UpdatedAt = e.now()
	execution, err = e.updateCAS(ctx, execution)
	if err != nil {
		return model.Execution{}, err
	}
	if err := value.Cancel(ctx, adapter.CancelRequest{Ref: ref, Signal: signal, Grace: grace}); err != nil {
		return model.Execution{}, wrapError("cancel", value.Name(), err)
	}
	requested := adapter.Event{Kind: string(model.EventProgress), SourceState: "cancel_requested", State: adapter.State(execution.State), ObservedAt: e.now(), Ordering: string(model.OrderingObservation), DedupeKey: adapter.Fingerprint(ref.Binding().Fingerprint, "cancel_requested", signal), Payload: map[string]any{"cancel_requested": true, "signal": bounded(signal, 32)}}
	normalized, projection, err := normalizeEvent(execution, requested, e.now())
	if err != nil {
		return model.Execution{}, err
	}
	if _, _, err := e.journal.AppendEvent(ctx, normalized, projection); err != nil {
		return model.Execution{}, &Error{Code: CodeConflict, Operation: "cancel", Adapter: value.Name(), Message: "cancel succeeded but its journal event failed", Cause: err}
	}
	if capabilityUsable(execution.Capabilities, adapter.CapabilitySnapshot) {
		snapshot, snapshotErr := value.Snapshot(ctx, adapter.SnapshotRequest{Ref: ref, Poll: adapter.PollOptions{Attempts: 1, Fresh: true}})
		if snapshotErr == nil {
			return e.applySession(ctx, execution, snapshot.Session, spec)
		}
	}
	return e.journal.GetExecution(ctx, executionID)
}

func (e *Engine) probeResolved(ctx context.Context, value adapter.Adapter, spec AdapterSpec, request adapter.ProbeRequest) (model.CapabilitySnapshot, error) {
	request = completeProbeRequest(spec, request)
	result, err := value.Probe(ctx, request)
	if err != nil {
		return model.CapabilitySnapshot{}, wrapError("probe", value.Name(), err)
	}
	snapshot := normalizeCapabilities(result)
	if snapshot.NegotiatedAt.IsZero() {
		snapshot.NegotiatedAt = e.now()
	}
	if snapshot.AdapterVersion == "" {
		snapshot.AdapterVersion = value.Manifest().AdapterVersion
	}
	return snapshot, nil
}

func completeProbeRequest(spec AdapterSpec, request adapter.ProbeRequest) adapter.ProbeRequest {
	if request.Executable == "" {
		request.Executable = spec.Executable
		if request.Executable == "" && spec.Multica != nil {
			request.Executable = spec.Multica.Binary
		}
	}
	if spec.Multica != nil {
		request.Profile = spec.Multica.Profile
		request.Endpoint = spec.Multica.Endpoint
		request.Workspace = spec.Multica.Workspace
	}
	return request
}

func (e *Engine) createStarting(ctx context.Context, adapterName string, spec AdapterSpec, acquisition model.Acquisition, capabilities model.CapabilitySnapshot, cwd string, task *model.TaskContract, mutation contracts.MutationKey) (model.Execution, bool, error) {
	now := e.now()
	mode, authority := model.ModeDirect, model.AuthorityNative
	if canonicalAdapterName(spec.Name) == "multica" {
		mode, authority = model.ModeMultica, model.AuthorityMultica
	}
	bindings, err := e.configBindings(authority, spec)
	if err != nil {
		return model.Execution{}, false, err
	}
	var cwdPtr *string
	if cwd != "" {
		cwdCopy := cwd
		cwdPtr = &cwdCopy
	}
	execution := model.Execution{Authority: authority, Adapter: adapterName, Mode: mode, Acquisition: acquisition, State: model.StateStarting, Liveness: model.LivenessUnknown, SourceBindings: bindings, Capabilities: capabilities, CWD: cwdPtr, Supersedes: []ids.ExecutionID{}, TaskContract: task, Observation: model.Observation{Source: model.ObservationUnknown, Integrity: model.IntegrityUnknown, ObservedAt: now}}
	created, reused, err := e.journal.CreateExecution(ctx, execution, mutation)
	return created, reused, err
}

func (e *Engine) applySession(ctx context.Context, execution model.Execution, session adapter.Session, spec AdapterSpec) (model.Execution, error) {
	bindings, err := e.sessionBindings(execution, spec, session)
	if err != nil {
		return model.Execution{}, err
	}
	execution.SourceBindings = bindings
	state := mapState(session.State)
	if !validNormalizedState(state) {
		return model.Execution{}, &Error{Code: CodeUnsafeObservation, Operation: "normalize_session", Adapter: execution.Adapter, Message: "adapter session returned an unknown state"}
	}
	updated, err := e.applyObservedState(ctx, execution, state, sourceState(string(session.State)), normalizeObservation(session.Observation, e.now()))
	if err != nil {
		return model.Execution{}, err
	}
	observedLiveness := mapLiveness(session.Liveness)
	if state.Terminal() && observedLiveness != model.LivenessUnreachable {
		observedLiveness = model.LivenessExited
	}
	if observedLiveness != "" && observedLiveness != updated.Liveness {
		updated.Liveness = observedLiveness
		return e.updateCAS(ctx, updated)
	}
	return updated, nil
}

func (e *Engine) applyResult(ctx context.Context, execution model.Execution, result adapter.Result) (model.Execution, error) {
	state := mapState(result.State)
	if !validNormalizedState(state) {
		return model.Execution{}, &Error{Code: CodeUnsafeObservation, Operation: "result", Adapter: execution.Adapter, Message: "adapter result returned an unknown state"}
	}
	if !state.Terminal() {
		return e.applyObservedState(ctx, execution, state, sourceState(string(result.State)), model.Observation{Source: model.ObservationStatusAPI, Integrity: model.IntegrityDegraded, ObservedAt: e.now()})
	}
	event := adapter.Event{Kind: string(model.EventTerminal), State: result.State, SourceState: string(result.State), ObservedAt: e.now(), Ordering: string(model.OrderingObservation), DedupeKey: adapter.Fingerprint(result.SessionRef.Binding().Fingerprint, "result", string(result.State)), Payload: map[string]any{"result_available": true, "success": result.Success}}
	if result.ExitCode != nil {
		event.Payload["exit_code"] = *result.ExitCode
	}
	normalized, projection, err := normalizeEvent(execution, event, e.now())
	if err != nil {
		return model.Execution{}, err
	}
	conflict, err := e.terminalConflict(ctx, execution, normalized)
	if err != nil {
		return model.Execution{}, err
	}
	if conflict {
		_, err = e.recordTerminalConflict(ctx, execution, normalized)
		if err != nil {
			return model.Execution{}, err
		}
		current, getErr := e.journal.GetExecution(ctx, execution.ID)
		if getErr != nil {
			return model.Execution{}, getErr
		}
		return current, &Error{Code: CodeUnknownState, Operation: "result", Adapter: execution.Adapter, Message: "authority returned contradictory terminal states"}
	}
	if execution.State.Terminal() {
		existing, findErr := e.existingTerminal(ctx, execution.ID)
		if findErr != nil {
			return model.Execution{}, findErr
		}
		if existing != nil {
			return e.applyObservedState(ctx, execution, state, sourceState(string(result.State)), model.Observation{Source: model.ObservationStatusAPI, Integrity: model.IntegrityVerified, ObservedAt: e.now()})
		}
	}
	if _, _, err := e.journal.AppendEvent(ctx, normalized, projection); err != nil {
		return model.Execution{}, &Error{Code: CodeConflict, Operation: "result", Adapter: execution.Adapter, Message: "journal rejected terminal result", Cause: err}
	}
	return e.applyObservedState(ctx, execution, state, sourceState(string(result.State)), model.Observation{Source: model.ObservationStatusAPI, Integrity: model.IntegrityVerified, ObservedAt: e.now()})
}

func (e *Engine) recordOperationFailure(ctx context.Context, execution model.Execution, operation string, cause error) (model.Execution, error) {
	now := e.now()
	failed := model.StateFailed
	event := adapter.Event{Kind: string(model.EventTerminal), State: adapter.StateFailed, SourceState: operation + "_failed", ObservedAt: now, Ordering: string(model.OrderingObservation), DedupeKey: adapter.Fingerprint(execution.ID.String(), operation, "failed"), Payload: map[string]any{"operation": operation, "failure_code": string(errorCode(cause))}}
	normalized, projection, err := normalizeEvent(execution, event, now)
	if err != nil {
		return execution, err
	}
	if _, _, err := e.journal.AppendEvent(ctx, normalized, projection); err != nil {
		return execution, err
	}
	return e.applyObservedState(ctx, execution, failed, sourceState(operation+"_failed"), model.Observation{Source: model.ObservationProcess, Integrity: model.IntegrityVerified, ObservedAt: now})
}

func errorCode(err error) ErrorCode {
	var runtimeErr *Error
	if errors.As(wrapError("", "", err), &runtimeErr) {
		return runtimeErr.Code
	}
	return CodeInternal
}

func (e *Engine) applyObservedState(ctx context.Context, execution model.Execution, state model.State, source *string, observation model.Observation) (model.Execution, error) {
	return e.applyObservedStateFromRevision(ctx, execution, state, source, observation, 0)
}

// applyObservedStateFromRevision applies evidence derived from a specific
// execution revision. If a newer revision terminalizes before the CAS read,
// that stale observation becomes a no-op instead of conflicting with or
// downgrading verified terminal evidence.
func (e *Engine) applyObservedStateFromRevision(ctx context.Context, execution model.Execution, state model.State, source *string, observation model.Observation, sourceRevision uint64) (model.Execution, error) {
	if execution.State.Terminal() && execution.State != state {
		execution.Liveness = model.LivenessUnknown
		execution.Observation = observation
		execution.Observation.Integrity = model.IntegrityConflicted
		return e.updateCASFromRevision(ctx, execution, sourceRevision)
	}
	execution.State = state
	execution.SourceState = source
	execution.Observation = observation
	execution.Liveness = livenessForState(state, execution.Liveness)
	if state.Terminal() {
		at := observation.ObservedAt
		execution.TerminalAt = &at
		if execution.Liveness != model.LivenessUnreachable {
			execution.Liveness = model.LivenessExited
		}
	}
	execution.UpdatedAt = observation.ObservedAt
	return e.updateCASFromRevision(ctx, execution, sourceRevision)
}

func (e *Engine) updateCAS(ctx context.Context, desired model.Execution) (model.Execution, error) {
	return e.updateCASFromRevision(ctx, desired, 0)
}

func (e *Engine) updateCASFromRevision(ctx context.Context, desired model.Execution, sourceRevision uint64) (model.Execution, error) {
	for range 3 {
		current, err := e.journal.GetExecution(ctx, desired.ID)
		if err != nil {
			return model.Execution{}, wrapError("get_execution", desired.Adapter, err)
		}
		if sourceRevision != 0 && current.State.Terminal() && current.Revision > sourceRevision {
			return current, nil
		}
		desired.Revision = current.Revision
		desired.CreatedAt = current.CreatedAt
		// Runtime observation updates never own coordination or acceptance
		// relationships. Preserve concurrent edits to those fields across a CAS
		// retry instead of replaying a stale whole-envelope copy.
		desired.Repository = current.Repository
		desired.ParentExecutionID = current.ParentExecutionID
		desired.Supersedes = current.Supersedes
		desired.SupersededBy = current.SupersededBy
		desired.Promotion = current.Promotion
		desired.TaskContract = current.TaskContract
		if current.State.Terminal() && current.State != desired.State {
			desired.State = current.State
			desired.TerminalAt = current.TerminalAt
			desired.Observation.Integrity = model.IntegrityConflicted
			desired.Liveness = model.LivenessUnknown
		}
		updated, updateErr := e.journal.UpdateExecution(ctx, desired, current.Revision)
		if updateErr == nil {
			return updated, nil
		}
	}
	return model.Execution{}, &Error{Code: CodeConflict, Operation: "update_execution", Adapter: desired.Adapter, Message: "execution changed during repeated journal updates"}
}

func (e *Engine) resolveExecution(ctx context.Context, executionID ids.ExecutionID) (model.Execution, adapter.Adapter, AdapterSpec, adapter.SourceRef, error) {
	execution, err := e.journal.GetExecution(ctx, executionID)
	if err != nil {
		return model.Execution{}, nil, AdapterSpec{}, adapter.SourceRef{}, wrapError("get_execution", "", err)
	}
	spec, ref, err := decodeBindings(execution)
	if err != nil {
		return model.Execution{}, nil, AdapterSpec{}, adapter.SourceRef{}, err
	}
	value, err := e.registry.Resolve(spec)
	if err != nil {
		return model.Execution{}, nil, AdapterSpec{}, adapter.SourceRef{}, err
	}
	return execution, value, spec, ref, nil
}

func (e *Engine) now() time.Time { return e.clock().UTC() }

func sourceState(value string) *string {
	if value == "" {
		return nil
	}
	value = bounded(value, 128)
	return &value
}

func livenessForState(state model.State, current model.Liveness) model.Liveness {
	switch state {
	case model.StateRunning, model.StateStarting:
		return model.LivenessAlive
	case model.StateWaiting, model.StateAttention:
		return model.LivenessBlocked
	case model.StateCompleted, model.StateFailed, model.StateCancelled, model.StateOrphaned:
		return model.LivenessExited
	default:
		return current
	}
}

func requireContextGuarantee(value adapter.Adapter, capabilities model.CapabilitySnapshot, input *adapter.ContextInput) error {
	if input == nil || !input.Required {
		return nil
	}
	if err := requireCapability(capabilities, adapter.CapabilityContextInjection); err != nil {
		return err
	}
	if !value.Manifest().ContextInjection.Guaranteed {
		return &Error{Code: CodeCapabilityUnavailable, Operation: "launch", Adapter: value.Name(), Message: "required context delivery is not guaranteed by this adapter", Details: map[string]any{"capability": string(adapter.CapabilityContextInjection)}}
	}
	return nil
}

func requireCapability(snapshot model.CapabilitySnapshot, name adapter.CapabilityName) error {
	for _, item := range snapshot.Items {
		if item.Name == string(name) {
			if item.SemanticsVersion != adapter.SemanticsVersion {
				return &Error{Code: CodeCapabilityUnavailable, Message: "capability semantics version is unsupported", Details: map[string]any{"capability": string(name)}}
			}
			switch item.Status {
			case model.CapabilitySupported, model.CapabilityDegraded:
				return nil
			case model.CapabilityUnavailable:
				message := "adapter capability is unavailable"
				if item.Reason != nil && *item.Reason != "" {
					message = *item.Reason
				}
				return &Error{Code: CodeCapabilityUnavailable, Message: message, Details: map[string]any{"capability": string(name)}}
			default:
				return &Error{Code: CodeCapabilityUnavailable, Message: "adapter returned an unknown capability status", Details: map[string]any{"capability": string(name)}}
			}
		}
	}
	return &Error{Code: CodeCapabilityUnavailable, Message: "adapter did not negotiate the requested capability", Details: map[string]any{"capability": string(name)}}
}

func capabilityUsable(snapshot model.CapabilitySnapshot, name adapter.CapabilityName) bool {
	return requireCapability(snapshot, name) == nil
}

func normalizeCapabilities(result adapter.ProbeResult) model.CapabilitySnapshot {
	items := make([]model.CapabilityItem, 0, len(result.Capabilities))
	for _, capability := range result.Capabilities {
		status := model.CapabilityStatus(capability.Status)
		if capability.Status == adapter.CapabilityConditional {
			status = model.CapabilityDegraded
		}
		var reason *string
		if capability.Reason != "" {
			copy := bounded(capability.Reason, 512)
			reason = &copy
		}
		items = append(items, model.CapabilityItem{Name: string(capability.Name), Status: status, Source: capability.Source, SemanticsVersion: capability.SemanticsVersion, Constraints: cloneMap(capability.Constraints), Reason: reason})
	}
	var backend *string
	if result.BackendVersion != "" {
		copy := bounded(result.BackendVersion, 128)
		backend = &copy
	}
	return model.CapabilitySnapshot{NegotiatedAt: result.ProbedAt, AdapterVersion: result.AdapterVersion, BackendVersion: backend, Items: items}
}

func cloneMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

func mapState(state adapter.State) model.State {
	switch state {
	case adapter.StateCreated:
		return model.StateCreated
	case adapter.StateStarting:
		return model.StateStarting
	case adapter.StateRunning:
		return model.StateRunning
	case adapter.StateWaiting:
		return model.StateWaiting
	case adapter.StateAttention:
		return model.StateAttention
	case adapter.StateCompleted:
		return model.StateCompleted
	case adapter.StateFailed:
		return model.StateFailed
	case adapter.StateCancelled:
		return model.StateCancelled
	case adapter.StateOrphaned:
		return model.StateOrphaned
	default:
		return ""
	}
}

func mapLiveness(value adapter.Liveness) model.Liveness {
	switch value {
	case adapter.LivenessUnknown:
		return model.LivenessUnknown
	case adapter.LivenessAlive:
		return model.LivenessAlive
	case adapter.LivenessBlocked:
		return model.LivenessBlocked
	case adapter.LivenessExited:
		return model.LivenessExited
	case adapter.LivenessUnreachable:
		return model.LivenessUnreachable
	default:
		return ""
	}
}
