package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/supervisor"
)

type sequenceGenerator struct {
	mu   sync.Mutex
	next map[ids.Type]uint64
}

func (g *sequenceGenerator) New(typ ids.Type) (ids.ID, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	value := g.next[typ]
	g.next[typ] = value + 1
	return ids.FromPayload(typ, value)
}

type fakeAdapter struct {
	name              string
	contextGuaranteed bool
	launch            adapter.LaunchResult
	attach            adapter.Attachment
	snapshot          adapter.Snapshot
	events            []adapter.Event
	result            adapter.Result
	probeErr          error
	launchErr         error
	snapshotErr       error
	eventsErr         error
	resultErr         error
	cancelErr         error
	launchCalls       int
	lastSnapshotRef   adapter.SourceRef
	unavailable       map[adapter.CapabilityName]bool
	constraints       map[adapter.CapabilityName]map[string]any
	eventsFn          func(adapter.EventsRequest) ([]adapter.Event, error)
	eventRequests     []adapter.EventsRequest
	eventsPageFn      func(adapter.EventsRequest) (adapter.MulticaEventPage, error)
	pageRequests      []adapter.EventsRequest
}

func (f *fakeAdapter) Name() string { return f.name }
func (f *fakeAdapter) Manifest() adapter.Manifest {
	return adapter.Manifest{SchemaVersion: 1, Adapter: f.name, AdapterVersion: "fixture-v1", ContextInjection: adapter.ContextInjection{Guaranteed: f.contextGuaranteed}}
}
func (f *fakeAdapter) Probe(context.Context, adapter.ProbeRequest) (adapter.ProbeResult, error) {
	if f.probeErr != nil {
		return adapter.ProbeResult{}, f.probeErr
	}
	capabilities := []adapter.Capability{}
	for _, name := range []adapter.CapabilityName{adapter.CapabilityLaunch, adapter.CapabilityAttach, adapter.CapabilitySnapshot, adapter.CapabilityEvents, adapter.CapabilityResult, adapter.CapabilityCancel, adapter.CapabilityContextInjection} {
		status := adapter.CapabilitySupported
		if f.unavailable[name] {
			status = adapter.CapabilityUnavailable
		}
		capabilities = append(capabilities, adapter.Capability{Name: name, Status: status, Source: "fixture", SemanticsVersion: adapter.SemanticsVersion, Constraints: cloneMap(f.constraints[name])})
	}
	return adapter.ProbeResult{AdapterVersion: "fixture-v1", BackendVersion: "backend-v1", ProbedAt: fixtureNow, Capabilities: capabilities}, nil
}
func (f *fakeAdapter) Launch(context.Context, adapter.LaunchRequest) (adapter.LaunchResult, error) {
	f.launchCalls++
	return f.launch, f.launchErr
}
func (f *fakeAdapter) Attach(context.Context, adapter.AttachRequest) (adapter.Attachment, error) {
	return f.attach, nil
}
func (f *fakeAdapter) Snapshot(_ context.Context, request adapter.SnapshotRequest) (adapter.Snapshot, error) {
	f.lastSnapshotRef = request.Ref
	return f.snapshot, f.snapshotErr
}
func (f *fakeAdapter) Wait(context.Context, adapter.SourceRef) (adapter.Result, error) {
	return f.result, f.resultErr
}
func (f *fakeAdapter) Events(_ context.Context, request adapter.EventsRequest) ([]adapter.Event, error) {
	f.eventRequests = append(f.eventRequests, request)
	if f.eventsFn != nil {
		return f.eventsFn(request)
	}
	return append([]adapter.Event(nil), f.events...), f.eventsErr
}
func (f *fakeAdapter) EventsPage(_ context.Context, request adapter.EventsRequest) (adapter.MulticaEventPage, error) {
	f.pageRequests = append(f.pageRequests, request)
	if f.eventsPageFn != nil {
		return f.eventsPageFn(request)
	}
	return adapter.MulticaEventPage{Events: append([]adapter.Event(nil), f.events...)}, f.eventsErr
}
func (f *fakeAdapter) Result(context.Context, adapter.ResultRequest) (adapter.Result, error) {
	return f.result, f.resultErr
}
func (f *fakeAdapter) Resume(context.Context, adapter.ResumeRequest) (adapter.LaunchResult, error) {
	return adapter.LaunchResult{}, &adapter.AdapterError{Code: adapter.ErrCapabilityUnavailable}
}
func (f *fakeAdapter) Cancel(context.Context, adapter.CancelRequest) error { return f.cancelErr }

var fixtureNow = time.Date(2026, 8, 10, 18, 0, 0, 0, time.UTC)

type fakeJournal struct {
	mu         sync.Mutex
	host       ids.HostID
	generator  ids.Generator
	executions map[ids.ExecutionID]model.Execution
	events     map[ids.ExecutionID][]model.Event
	dedupe     map[string]model.Event
	projection map[string]string
}

func newFakeJournal(t *testing.T, generator ids.Generator) *fakeJournal {
	t.Helper()
	hostID, err := ids.NewHostID(generator)
	if err != nil {
		t.Fatal(err)
	}
	return &fakeJournal{host: hostID, generator: generator, executions: map[ids.ExecutionID]model.Execution{}, events: map[ids.ExecutionID][]model.Event{}, dedupe: map[string]model.Event{}, projection: map[string]string{}}
}

func (j *fakeJournal) HostID(context.Context) (ids.HostID, error) { return j.host, nil }
func (j *fakeJournal) CreateExecution(_ context.Context, value model.Execution, _ contracts.MutationKey) (model.Execution, bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if value.ID.IsZero() {
		value.ID, _ = ids.NewExecutionID(j.generator)
	}
	value.SchemaVersion, value.Revision, value.OriginHostID = model.SchemaVersion, 1, j.host
	if value.CreatedAt.IsZero() {
		value.CreatedAt = fixtureNow
	}
	if value.UpdatedAt.IsZero() {
		value.UpdatedAt = value.CreatedAt
	}
	if value.Observation.ObservedAt.IsZero() {
		value.Observation.ObservedAt = value.UpdatedAt
	}
	if err := value.Validate(); err != nil {
		return model.Execution{}, false, err
	}
	j.executions[value.ID] = value
	return value, false, nil
}
func (j *fakeJournal) GetExecution(_ context.Context, id ids.ExecutionID) (model.Execution, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	value, ok := j.executions[id]
	if !ok {
		return model.Execution{}, errors.New("not found")
	}
	return value, nil
}
func (j *fakeJournal) UpdateExecution(_ context.Context, next model.Execution, revision uint64) (model.Execution, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	previous, ok := j.executions[next.ID]
	if !ok {
		return model.Execution{}, errors.New("not found")
	}
	if previous.Revision != revision {
		return model.Execution{}, errors.New("conflict")
	}
	next.Revision = revision + 1
	if err := model.ValidateTransition(previous, next); err != nil {
		return model.Execution{}, err
	}
	j.executions[next.ID] = next
	return next, nil
}
func (j *fakeJournal) ListExecutions(_ context.Context, nonterminal bool) ([]model.Execution, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	values := []model.Execution{}
	for _, value := range j.executions {
		if !nonterminal || !value.State.Terminal() {
			values = append(values, value)
		}
	}
	return values, nil
}
func (j *fakeJournal) AppendEvent(_ context.Context, event model.Event, projection []byte) (model.Event, bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if existing, ok := j.dedupe[event.DedupeKey]; ok {
		if j.projection[event.DedupeKey] != string(projection) {
			return model.Event{}, false, errors.New("dedupe conflict")
		}
		return existing, true, nil
	}
	for _, existing := range j.events[event.ExecutionID] {
		if existing.Kind == model.EventTerminal && event.Kind == model.EventTerminal {
			return model.Event{}, false, errors.New("terminal conflict")
		}
	}
	id, err := ids.NewEventID(j.generator)
	if err != nil {
		return model.Event{}, false, err
	}
	event.ID, event.SchemaVersion, event.Sequence = id, model.SchemaVersion, uint64(len(j.events[event.ExecutionID])+1)
	if err := event.Validate(); err != nil {
		return model.Event{}, false, err
	}
	j.events[event.ExecutionID] = append(j.events[event.ExecutionID], event)
	j.dedupe[event.DedupeKey], j.projection[event.DedupeKey] = event, string(projection)
	return event, false, nil
}
func (j *fakeJournal) ListEvents(_ context.Context, id ids.ExecutionID, query contracts.EventQuery) ([]model.Event, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	values := []model.Event{}
	for _, event := range j.events[id] {
		if event.Sequence > query.AfterSequence {
			values = append(values, event)
		}
	}
	return values, nil
}

func fixtureSession(name, kind, opaque string, state adapter.State) adapter.Session {
	ref := adapter.SourceRef{Adapter: name, Kind: kind, OpaqueID: opaque}
	return adapter.Session{Ref: ref, Binding: ref.Binding(), State: state, Liveness: adapter.LivenessAlive, StartedAt: fixtureNow, UpdatedAt: fixtureNow, Observation: adapter.Observation{Source: "native_stream", Integrity: "verified", ObservedAt: fixtureNow}}
}

func testEngine(t *testing.T, registry *Registry) (*Engine, *fakeJournal, *sequenceGenerator) {
	t.Helper()
	generator := &sequenceGenerator{next: map[ids.Type]uint64{ids.TypeHost: 1, ids.TypeExecution: 10, ids.TypeEvent: 20, ids.TypeSource: 30, ids.TypeProject: 40, ids.TypeIssue: 50, ids.TypeRun: 60}}
	journal := newFakeJournal(t, generator)
	engine, err := New(journal, Options{Registry: registry, Generator: generator, Clock: func() time.Time { return fixtureNow }})
	if err != nil {
		t.Fatal(err)
	}
	return engine, journal, generator
}

func TestDirectLifecycleNormalizesWithoutTranscriptPersistence(t *testing.T) {
	ref := fixtureSession("fixture", "fixture_session", "native-123", adapter.StateRunning)
	fake := &fakeAdapter{name: "fixture", launch: adapter.LaunchResult{Session: ref}}
	registry := NewRegistry()
	if err := registry.Register("fixture", func(AdapterSpec) (adapter.Adapter, error) { return fake, nil }); err != nil {
		t.Fatal(err)
	}
	engine, journal, _ := testEngine(t, registry)
	execution, err := engine.Launch(context.Background(), LaunchOptions{Adapter: AdapterSpec{Name: "fixture", Executable: "/opt/fixture"}, Request: adapter.LaunchRequest{Argv: []string{"/opt/fixture"}}})
	if err != nil {
		t.Fatal(err)
	}
	if execution.State != model.StateRunning || execution.Authority != model.AuthorityNative || len(execution.SourceBindings) != 2 {
		t.Fatalf("execution = %#v", execution)
	}
	fake.events = []adapter.Event{{Sequence: 1, Cursor: "source-1", Kind: "terminal", State: adapter.StateCompleted, SourceState: "turn.completed", ObservedAt: fixtureNow, Ordering: "source", DedupeKey: "native-event-1", Payload: map[string]any{"result_available": true}}}
	events, err := engine.Events(context.Background(), execution.ID, "", adapter.PollOptions{})
	if err != nil || len(events) != 1 || events[0].Kind != model.EventTerminal {
		t.Fatalf("events=%#v err=%v", events, err)
	}
	completed, err := journal.GetExecution(context.Background(), execution.ID)
	if err != nil || completed.State != model.StateCompleted || completed.TerminalAt == nil {
		t.Fatalf("completed=%#v err=%v", completed, err)
	}
	fake.events = []adapter.Event{{Kind: "progress", State: adapter.StateRunning, Ordering: "observation", DedupeKey: "unsafe", Payload: map[string]any{"transcript": "must never persist"}}}
	if _, err := engine.Events(context.Background(), execution.ID, "", adapter.PollOptions{}); errorCode(err) != CodeUnsafeObservation {
		t.Fatalf("unsafe event error = %v", err)
	}
	stored, err := journal.ListEvents(context.Background(), execution.ID, contracts.EventQuery{})
	if err != nil || len(stored) != 1 {
		t.Fatalf("stored events=%#v err=%v", stored, err)
	}
}

func TestExactMulticaLifecyclePersistsAuthoritySelectors(t *testing.T) {
	config := adapter.MulticaConfig{Binary: "/opt/multica", Profile: "private", Endpoint: "https://multica.internal", Workspace: "workspace-1", Issue: "issue-2", Run: "run-3"}
	session := fixtureSession("multica", "multica_run", config.Run, adapter.StateRunning)
	session.Ref.Profile, session.Ref.Endpoint, session.Ref.Workspace, session.Ref.Issue, session.Ref.Run = config.Profile, config.Endpoint, config.Workspace, config.Issue, config.Run
	fake := &fakeAdapter{name: "multica", launch: adapter.LaunchResult{Session: session}, snapshot: adapter.Snapshot{Session: session}}
	registry := NewRegistry()
	if err := registry.Register("multica", func(spec AdapterSpec) (adapter.Adapter, error) {
		if spec.Multica == nil || *spec.Multica != config {
			t.Fatalf("resolved Multica spec = %#v", spec)
		}
		return fake, nil
	}); err != nil {
		t.Fatal(err)
	}
	engine, journal, generator := testEngine(t, registry)
	execution, err := engine.Launch(context.Background(), LaunchOptions{Adapter: AdapterSpec{Name: "multica", Executable: config.Binary, Multica: &config}, Request: adapter.LaunchRequest{Argv: []string{"codex", "exec"}}})
	if err != nil {
		t.Fatal(err)
	}
	if execution.Mode != model.ModeMultica || execution.Authority != model.AuthorityMultica || len(execution.SourceBindings) != 6 {
		t.Fatalf("Multica execution = %#v", execution)
	}
	// A new Engine/Registry models restart. Reprobe must rebuild from journaled
	// selectors rather than the first adapter's process records.
	restartedAdapter := &fakeAdapter{name: "multica", snapshot: adapter.Snapshot{Session: session}}
	restartedRegistry := NewRegistry()
	_ = restartedRegistry.Register("multica", func(spec AdapterSpec) (adapter.Adapter, error) {
		if spec.Multica == nil || *spec.Multica != config {
			t.Fatalf("restart spec = %#v", spec)
		}
		return restartedAdapter, nil
	})
	restarted, err := New(journal, Options{Registry: restartedRegistry, Generator: generator, Clock: func() time.Time { return fixtureNow }})
	if err != nil {
		t.Fatal(err)
	}
	probe, err := (SupervisorExecutions{Engine: restarted}).Reprobe(context.Background(), supervisor.Execution{ID: execution.ID.String()})
	if err != nil || probe.State != "running" || restartedAdapter.lastSnapshotRef.Profile != config.Profile || restartedAdapter.lastSnapshotRef.Run != config.Run {
		t.Fatalf("probe=%#v ref=%#v err=%v", probe, restartedAdapter.lastSnapshotRef, err)
	}
}

func TestRestartMarksUnreprobeableNativeSessionUnreachable(t *testing.T) {
	session := fixtureSession("fixture", "fixture_session", "session-restart", adapter.StateRunning)
	firstAdapter := &fakeAdapter{name: "fixture", launch: adapter.LaunchResult{Session: session}}
	firstRegistry := NewRegistry()
	_ = firstRegistry.Register("fixture", func(AdapterSpec) (adapter.Adapter, error) { return firstAdapter, nil })
	engine, journal, generator := testEngine(t, firstRegistry)
	execution, err := engine.Launch(context.Background(), LaunchOptions{Adapter: AdapterSpec{Name: "fixture", Executable: "/opt/fixture"}, Request: adapter.LaunchRequest{Argv: []string{"/opt/fixture"}}})
	if err != nil {
		t.Fatal(err)
	}
	freshAdapter := &fakeAdapter{name: "fixture", snapshotErr: &adapter.AdapterError{Code: adapter.ErrNotFound, Message: "session absent"}}
	freshRegistry := NewRegistry()
	_ = freshRegistry.Register("fixture", func(AdapterSpec) (adapter.Adapter, error) { return freshAdapter, nil })
	restarted, _ := New(journal, Options{Registry: freshRegistry, Generator: generator, Clock: func() time.Time { return fixtureNow.Add(time.Minute) }})
	bridge := SupervisorExecutions{Engine: restarted}
	result, err := bridge.Reprobe(context.Background(), supervisor.Execution{ID: execution.ID.String()})
	if err != nil || result.Liveness != string(model.LivenessUnreachable) {
		t.Fatalf("reprobe=%#v err=%v", result, err)
	}
	if err := bridge.ApplyProbe(context.Background(), execution.ID.String(), result); err != nil {
		t.Fatal(err)
	}
	stored, _ := journal.GetExecution(context.Background(), execution.ID)
	if stored.State != model.StateRunning || stored.Liveness != model.LivenessUnreachable || stored.Observation.Integrity != model.IntegrityDegraded {
		t.Fatalf("stored unreachable execution = %#v", stored)
	}
}

func TestStaleNativeLeaseProbeCannotCorruptTerminalExecution(t *testing.T) {
	session := fixtureSession("fixture", "fixture_session", "session-live", adapter.StateRunning)
	fake := &fakeAdapter{name: "fixture", launch: adapter.LaunchResult{Session: session}}
	registry := NewRegistry()
	_ = registry.Register("fixture", func(AdapterSpec) (adapter.Adapter, error) { return fake, nil })
	engine, journal, _ := testEngine(t, registry)
	execution, err := engine.Launch(context.Background(), LaunchOptions{Adapter: AdapterSpec{Name: "fixture", Executable: "/opt/fixture"}, Request: adapter.LaunchRequest{Argv: []string{"/opt/fixture"}}})
	if err != nil {
		t.Fatal(err)
	}
	leaseSeconds := 5
	execution.Liveness = model.LivenessAlive
	execution.Observation = model.Observation{Source: model.ObservationNativeStream, Integrity: model.IntegrityVerified, ObservedAt: fixtureNow, FreshForSeconds: &leaseSeconds}
	execution, err = journal.UpdateExecution(context.Background(), execution, execution.Revision)
	if err != nil {
		t.Fatal(err)
	}
	bridge := SupervisorExecutions{Engine: engine}
	probe, err := bridge.Reprobe(context.Background(), supervisor.Execution{ID: execution.ID.String()})
	if err != nil || probe.State != string(model.StateRunning) || probe.Liveness != string(model.LivenessAlive) {
		t.Fatalf("lease probe=%#v err=%v", probe, err)
	}
	terminalAt := fixtureNow.Add(time.Second)
	execution.State = model.StateCompleted
	execution.Liveness = model.LivenessExited
	execution.TerminalAt = &terminalAt
	execution.UpdatedAt = terminalAt
	execution.Observation = model.Observation{Source: model.ObservationNativeStream, Integrity: model.IntegrityVerified, ObservedAt: terminalAt}
	if _, err := journal.UpdateExecution(context.Background(), execution, execution.Revision); err != nil {
		t.Fatal(err)
	}
	if err := bridge.ApplyProbe(context.Background(), execution.ID.String(), probe); err != nil {
		t.Fatal(err)
	}
	stored, err := journal.GetExecution(context.Background(), execution.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != model.StateCompleted || stored.Liveness != model.LivenessExited || stored.Observation.Integrity != model.IntegrityVerified {
		t.Fatalf("stale lease echo corrupted terminal execution: %#v", stored)
	}
}

func createPromotedIssueExecution(t *testing.T, engine *Engine, journal *fakeJournal, spec AdapterSpec) model.Execution {
	t.Helper()
	bindings, err := engine.configBindings(model.AuthorityMultica, spec)
	if err != nil {
		t.Fatal(err)
	}
	reason := "issue/run snapshots unavailable; exact workspace events are durable"
	capabilities := model.CapabilitySnapshot{
		NegotiatedAt: fixtureNow, AdapterVersion: "fixture-v1",
		Items: []model.CapabilityItem{
			{Name: string(adapter.CapabilitySnapshot), Status: model.CapabilityUnavailable, Source: "fixture", SemanticsVersion: adapter.SemanticsVersion, Reason: &reason},
			{Name: string(adapter.CapabilityEvents), Status: model.CapabilityDegraded, Source: "fixture", SemanticsVersion: adapter.SemanticsVersion, Constraints: map[string]any{"cross_restart": true, "scope": "workspace_events"}},
		},
	}
	created, _, err := journal.CreateExecution(context.Background(), model.Execution{
		Authority: model.AuthorityMultica, Adapter: "multica", Mode: model.ModeMultica,
		Acquisition: model.AcquisitionPromoted, State: model.StateStarting, Liveness: model.LivenessUnknown,
		SourceBindings: bindings, Capabilities: capabilities, Supersedes: []ids.ExecutionID{},
		Observation: model.Observation{Source: model.ObservationUnknown, Integrity: model.IntegrityUnknown, ObservedAt: fixtureNow},
	}, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func TestReprobePromotedMulticaUsesDurableEventsAndAdvancesScopedCursor(t *testing.T) {
	config := adapter.MulticaConfig{Binary: "/opt/multica", Profile: "private", Endpoint: "https://multica.internal", Workspace: "workspace-1", Issue: "issue-2"}
	spec := AdapterSpec{Name: "multica", Executable: config.Binary, Multica: &config}
	fake := &fakeAdapter{
		name:        "multica",
		unavailable: map[adapter.CapabilityName]bool{adapter.CapabilitySnapshot: true},
		constraints: map[adapter.CapabilityName]map[string]any{adapter.CapabilityEvents: {"cross_restart": true}},
	}
	fake.eventsPageFn = func(request adapter.EventsRequest) (adapter.MulticaEventPage, error) {
		switch len(fake.pageRequests) {
		case 1:
			return adapter.MulticaEventPage{Events: []adapter.Event{{Sequence: 1, Cursor: "1", SourcePosition: "api-seq-1", Kind: "progress", State: adapter.StateWaiting, SourceState: "issue.created", ObservedAt: fixtureNow.Add(time.Second), Ordering: "source", DedupeKey: "issue-created", Payload: map[string]any{"workspace_id": config.Workspace, "issue_id": config.Issue}}}, NextCursor: "evt1.1", Scanned: 1}, nil
		case 2:
			return adapter.MulticaEventPage{Events: []adapter.Event{{Sequence: 1, Cursor: "1", SourcePosition: "api-seq-2", Kind: "terminal", State: adapter.StateCompleted, SourceState: "task.completed", ObservedAt: fixtureNow.Add(2 * time.Second), Ordering: "source", DedupeKey: "related-nested-terminal", Payload: map[string]any{"workspace_id": config.Workspace, "issue_id": config.Issue, "run_id": "run-created-after-promotion", "aggregate_kind": "task"}}}, NextCursor: "evt1.2", Scanned: 1}, nil
		case 3:
			return adapter.MulticaEventPage{Events: []adapter.Event{{Sequence: 1, Cursor: "1", SourcePosition: "api-seq-3", Kind: "terminal", State: adapter.StateCompleted, SourceState: "task.completed", ObservedAt: fixtureNow.Add(3 * time.Second), Ordering: "source", DedupeKey: "unrelated-terminal", Payload: map[string]any{"workspace_id": "other-workspace", "issue_id": "other-issue", "run_id": "other-run"}}}, NextCursor: "evt1.3", Scanned: 1, Filtered: 1}, nil
		case 4:
			return adapter.MulticaEventPage{Events: []adapter.Event{{Sequence: 1, Cursor: "1", SourcePosition: "api-seq-4", Kind: "terminal", State: adapter.StateCompleted, SourceState: "issue:updated", ObservedAt: fixtureNow.Add(4 * time.Second), Ordering: "source", DedupeKey: "issue-terminal", Payload: map[string]any{"workspace_id": config.Workspace, "issue_id": config.Issue, "aggregate_kind": "issue"}}}, NextCursor: "evt1.4", Scanned: 1}, nil
		default:
			return adapter.MulticaEventPage{}, nil
		}
	}
	registry := NewRegistry()
	if err := registry.Register("multica", func(resolved AdapterSpec) (adapter.Adapter, error) {
		if resolved.Multica == nil || *resolved.Multica != config {
			t.Fatalf("resolved issue-only Multica spec = %#v", resolved)
		}
		return fake, nil
	}); err != nil {
		t.Fatal(err)
	}
	engine, journal, _ := testEngine(t, registry)
	execution := createPromotedIssueExecution(t, engine, journal, spec)
	bridge := SupervisorExecutions{Engine: engine}

	waiting, err := bridge.Reprobe(context.Background(), supervisor.Execution{ID: execution.ID.String()})
	if err != nil || waiting.State != string(model.StateWaiting) || waiting.Liveness != string(model.LivenessBlocked) || waiting.Source != string(model.ObservationDurableOutbox) {
		t.Fatalf("waiting reprobe=%#v err=%v", waiting, err)
	}
	storedWaiting, _ := journal.GetExecution(context.Background(), execution.ID)
	if storedWaiting.Observation.Source != model.ObservationDurableOutbox {
		t.Fatalf("stored waiting observation = %#v", storedWaiting.Observation)
	}
	nested, err := bridge.Reprobe(context.Background(), supervisor.Execution{ID: execution.ID.String()})
	if err != nil || nested.State != string(model.StateWaiting) || nested.Liveness != string(model.LivenessBlocked) {
		t.Fatalf("nested task terminalized issue-only execution: reprobe=%#v err=%v", nested, err)
	}
	unrelated, err := bridge.Reprobe(context.Background(), supervisor.Execution{ID: execution.ID.String()})
	if err != nil || unrelated.State != string(model.StateWaiting) || unrelated.Liveness != string(model.LivenessBlocked) {
		t.Fatalf("unrelated event changed execution: reprobe=%#v err=%v", unrelated, err)
	}
	completed, err := bridge.Reprobe(context.Background(), supervisor.Execution{ID: execution.ID.String()})
	if err != nil || completed.State != string(model.StateCompleted) || completed.Liveness != string(model.LivenessExited) {
		t.Fatalf("terminal reprobe=%#v err=%v", completed, err)
	}
	if len(fake.pageRequests) != 4 {
		t.Fatalf("event page requests = %#v", fake.pageRequests)
	}
	wantCursors := []string{"", "evt1.1", "evt1.2", "evt1.3"}
	for index, request := range fake.pageRequests {
		if request.Cursor != wantCursors[index] {
			t.Fatalf("request %d cursor=%q want=%q", index, request.Cursor, wantCursors[index])
		}
		if request.Ref.Profile != config.Profile || request.Ref.Endpoint != config.Endpoint || request.Ref.Workspace != config.Workspace || request.Ref.Issue != config.Issue || request.Ref.Run != "" {
			t.Fatalf("request %d ref=%#v", index, request.Ref)
		}
	}
	storedEvents, err := journal.ListEvents(context.Background(), execution.ID, contracts.EventQuery{})
	if err != nil || len(storedEvents) != 8 || storedEvents[2].Kind != model.EventProgress || storedEvents[2].State == nil || *storedEvents[2].State != model.StateWaiting || storedEvents[4].Kind != model.EventHealth || storedEvents[4].Payload["reason"] != "out_of_scope_event" || storedEvents[6].Kind != model.EventTerminal {
		t.Fatalf("stored events=%#v err=%v", storedEvents, err)
	}
	if storedEvents[0].SourcePosition == nil || storedEvents[0].SourcePosition.Kind != "native_sequence" || storedEvents[0].SourcePosition.Value != "api-seq-1" {
		t.Fatalf("matching event source position = %#v", storedEvents[0].SourcePosition)
	}
	for _, index := range []int{1, 3, 5, 7} {
		if storedEvents[index].SourcePosition == nil || storedEvents[index].SourcePosition.Kind != "multica_workspace_cursor" {
			t.Fatalf("event %d missing workspace cursor checkpoint: %#v", index, storedEvents[index])
		}
	}
}

func TestDecodeRestartPromotedIssueOnlyMulticaNeverGuessesRun(t *testing.T) {
	config := adapter.MulticaConfig{Binary: "/opt/multica", Profile: "private", Endpoint: "https://multica.internal", Workspace: "workspace-1", Issue: "issue-2"}
	spec := AdapterSpec{Name: "multica", Executable: config.Binary, Multica: &config}
	registry := NewRegistry()
	first := &fakeAdapter{name: "multica"}
	_ = registry.Register("multica", func(AdapterSpec) (adapter.Adapter, error) { return first, nil })
	engine, journal, generator := testEngine(t, registry)
	execution := createPromotedIssueExecution(t, engine, journal, spec)
	for _, binding := range execution.SourceBindings {
		if binding.Kind == bindingMulticaRun {
			t.Fatalf("issue-only execution unexpectedly has run binding: %#v", binding)
		}
	}
	restartedRegistry := NewRegistry()
	restartedAdapter := &fakeAdapter{name: "multica"}
	_ = restartedRegistry.Register("multica", func(resolved AdapterSpec) (adapter.Adapter, error) {
		if resolved.Multica == nil || *resolved.Multica != config || resolved.Multica.Run != "" {
			t.Fatalf("restart resolved spec = %#v", resolved)
		}
		return restartedAdapter, nil
	})
	restarted, err := New(journal, Options{Registry: restartedRegistry, Generator: generator, Clock: func() time.Time { return fixtureNow.Add(time.Minute) }})
	if err != nil {
		t.Fatal(err)
	}
	_, _, resolved, ref, err := restarted.resolveExecution(context.Background(), execution.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Multica == nil || resolved.Multica.Run != "" || ref.Kind != bindingMulticaIssue || ref.OpaqueID != config.Issue || ref.Profile != config.Profile || ref.Endpoint != config.Endpoint || ref.Workspace != config.Workspace || ref.Issue != config.Issue || ref.Run != "" {
		t.Fatalf("resolved=%#v ref=%#v", resolved, ref)
	}
}

func TestTerminalConflictPreservesFirstTerminalAndEmitsHealth(t *testing.T) {
	session := fixtureSession("fixture", "fixture_session", "terminal-session", adapter.StateCompleted)
	result := adapter.Result{Success: true, State: adapter.StateCompleted, SessionRef: session.Ref}
	fake := &fakeAdapter{name: "fixture", launch: adapter.LaunchResult{Session: session, Result: &result}}
	registry := NewRegistry()
	_ = registry.Register("fixture", func(AdapterSpec) (adapter.Adapter, error) { return fake, nil })
	engine, journal, _ := testEngine(t, registry)
	execution, err := engine.Launch(context.Background(), LaunchOptions{Adapter: AdapterSpec{Name: "fixture", Executable: "/opt/fixture"}, Request: adapter.LaunchRequest{Argv: []string{"/opt/fixture"}}})
	if err != nil {
		t.Fatal(err)
	}
	fake.events = []adapter.Event{{Sequence: 2, Cursor: "2", Kind: "terminal", State: adapter.StateFailed, SourceState: "turn.failed", ObservedAt: fixtureNow.Add(time.Second), Ordering: "source", DedupeKey: "contradiction", Payload: map[string]any{"result_available": true}}}
	events, err := engine.Events(context.Background(), execution.ID, "", adapter.PollOptions{})
	if err != nil || len(events) != 1 || events[0].Kind != model.EventHealth || events[0].Payload["reason"] != "terminal_conflict" {
		t.Fatalf("conflict events=%#v err=%v", events, err)
	}
	stored, _ := journal.GetExecution(context.Background(), execution.ID)
	if stored.State != model.StateCompleted || stored.Observation.Integrity != model.IntegrityConflicted || stored.Liveness != model.LivenessUnknown {
		t.Fatalf("conflicted execution = %#v", stored)
	}
}

func TestNoHiddenDefaultMulticaProfileAndRequiredContextFailClosed(t *testing.T) {
	registry := DefaultRegistry()
	_, err := registry.Resolve(AdapterSpec{Name: "multica"})
	if errorCode(err) != CodeInvalidConfiguration {
		t.Fatalf("unscoped Multica resolve error = %v", err)
	}
	fake := &fakeAdapter{name: "fixture", contextGuaranteed: false}
	custom := NewRegistry()
	_ = custom.Register("fixture", func(AdapterSpec) (adapter.Adapter, error) { return fake, nil })
	engine, _, _ := testEngine(t, custom)
	_, err = engine.Launch(context.Background(), LaunchOptions{Adapter: AdapterSpec{Name: "fixture", Executable: "/opt/fixture"}, Request: adapter.LaunchRequest{Argv: []string{"/opt/fixture"}, Context: &adapter.ContextInput{Path: "/context", Required: true}}})
	if errorCode(err) != CodeCapabilityUnavailable || fake.launchCalls != 0 {
		t.Fatalf("required context err=%v launchCalls=%d", err, fake.launchCalls)
	}
}

func TestUnavailableLaunchCapabilityDoesNotCreateOrInvoke(t *testing.T) {
	fake := &fakeAdapter{name: "fixture", unavailable: map[adapter.CapabilityName]bool{adapter.CapabilityLaunch: true}}
	registry := NewRegistry()
	_ = registry.Register("fixture", func(AdapterSpec) (adapter.Adapter, error) { return fake, nil })
	engine, journal, _ := testEngine(t, registry)
	_, err := engine.Launch(context.Background(), LaunchOptions{Adapter: AdapterSpec{Name: "fixture", Executable: "/opt/fixture"}, Request: adapter.LaunchRequest{Argv: []string{"/opt/fixture"}}})
	if errorCode(err) != CodeCapabilityUnavailable || fake.launchCalls != 0 {
		t.Fatalf("launch err=%v calls=%d", err, fake.launchCalls)
	}
	values, listErr := journal.ListExecutions(context.Background(), false)
	if listErr != nil || len(values) != 0 {
		t.Fatalf("executions=%#v err=%v", values, listErr)
	}
}

type fakeDurableOutbox struct {
	entries []OutboxRecord
	acked   string
}

func (f *fakeDurableOutbox) ListPending(context.Context) ([]OutboxRecord, error) {
	return f.entries, nil
}
func (f *fakeDurableOutbox) MarkAcknowledged(_ context.Context, id string) error {
	f.acked = id
	return nil
}
func (f *fakeDurableOutbox) ScheduleRetry(context.Context, string, time.Time, string) error {
	return nil
}
func (f *fakeDurableOutbox) MarkDeadLetter(context.Context, string, string) error { return nil }

type fakeTransport struct{ payload []byte }

func (f *fakeTransport) Deliver(_ context.Context, record OutboxRecord) error {
	f.payload = append([]byte(nil), record.Payload...)
	return nil
}

func TestOutboxAndDelivererBridgePreserveDurableIdentity(t *testing.T) {
	store := &fakeDurableOutbox{entries: []OutboxRecord{{ID: "delivery-1", ExecutionID: "exec-1", SubscriptionID: "sub-1", Attempts: 2, Payload: []byte(`{"bounded":true}`)}}}
	bridge := OutboxBridge{Store: store}
	entries, err := bridge.Pending(context.Background())
	if err != nil || len(entries) != 1 || entries[0].ID != "delivery-1" || entries[0].Attempts != 2 {
		t.Fatalf("entries=%#v err=%v", entries, err)
	}
	transport := &fakeTransport{}
	if err := (DelivererBridge{Transport: transport}).Deliver(context.Background(), entries[0]); err != nil {
		t.Fatal(err)
	}
	if string(transport.payload) != `{"bounded":true}` {
		t.Fatalf("payload = %s", transport.payload)
	}
	if err := bridge.Ack(context.Background(), entries[0].ID); err != nil || store.acked != "delivery-1" {
		t.Fatalf("ack=%q err=%v", store.acked, err)
	}
}

func TestStableAdapterErrorMapping(t *testing.T) {
	err := wrapError("snapshot", "fixture", &adapter.AdapterError{Code: adapter.ErrDependencyUnavailable, Message: "down", Retryable: true})
	var runtimeErr *Error
	if !errors.As(err, &runtimeErr) || runtimeErr.Code != CodeDependencyUnavailable || !runtimeErr.Retryable || runtimeErr.Adapter != "fixture" {
		t.Fatalf("mapped error = %#v", err)
	}
}
