package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/supervisor"
)

func TestMulticaEventProbeProgress(t *testing.T) {
	ctx := context.Background()
	config := adapter.MulticaConfig{Binary: "/opt/multica", Profile: "private", Endpoint: "https://multica.internal", Workspace: "workspace-1", Issue: "issue-2"}
	fake := &fakeAdapter{name: "multica"}
	registry := NewRegistry()
	if err := registry.Register("multica", func(AdapterSpec) (adapter.Adapter, error) { return fake, nil }); err != nil {
		t.Fatal(err)
	}
	engine, journal, _ := testEngine(t, registry)
	now := fixtureNow.Add(time.Hour)
	engine.clock = func() time.Time { return now }
	execution := createPromotedIssueExecution(t, engine, journal, AdapterSpec{Name: "multica", Executable: config.Binary, Multica: &config})
	bridge := SupervisorExecutions{Engine: engine}
	probe := func() supervisor.ProbeResult {
		t.Helper()
		result, err := bridge.Reprobe(ctx, supervisor.Execution{ID: execution.ID.String()})
		if err != nil {
			t.Fatal(err)
		}
		if !result.ProbedAt.Equal(now) {
			t.Fatalf("attempt time = %s, want %s", result.ProbedAt, now)
		}
		if err := bridge.ApplyProbe(ctx, execution.ID.String(), result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	assertUnchanged := func(want model.Execution) {
		t.Helper()
		got, err := journal.GetExecution(ctx, execution.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Revision != want.Revision || !got.UpdatedAt.Equal(want.UpdatedAt) || !got.Observation.ObservedAt.Equal(want.Observation.ObservedAt) {
			t.Fatalf("empty/duplicate poll changed execution: before=%#v after=%#v", want, got)
		}
		if (got.ProgressAt == nil) != (want.ProgressAt == nil) || (got.ProgressAt != nil && !got.ProgressAt.Equal(*want.ProgressAt)) {
			t.Fatalf("poll manufactured progress: %#v", got)
		}
	}
	// An empty page can echo a cursor even though no workspace event was scanned.
	fake.eventsPageFn = func(adapter.EventsRequest) (adapter.MulticaEventPage, error) {
		return adapter.MulticaEventPage{NextCursor: "evt1.empty"}, nil
	}
	// Unknown legacy history is not inferred from creation or an attempted poll.
	for range 3 {
		probe()
		assertUnchanged(execution)
		now = now.Add(time.Minute)
	}
	events, _ := journal.ListEvents(ctx, execution.ID, contracts.EventQuery{})
	if len(events) != 0 {
		t.Fatalf("empty polls journaled events: %#v", events)
	}

	// An empty workspace response after a failed observation is not evidence
	// that this bound issue is live. Keep that uncertainty rather than healing
	// cached liveness from workspace API reachability (also v0.15.0 behavior).
	fake.eventsPageFn = nil
	fake.eventsErr = &adapter.AdapterError{Code: adapter.ErrTimeout, Message: "fixture timeout"}
	probe()
	execution, _ = journal.GetExecution(ctx, execution.ID)
	if execution.Liveness != model.LivenessUnreachable {
		t.Fatalf("failure did not retain uncertainty: %#v", execution)
	}
	fake.eventsErr = nil
	for range 3 {
		probe()
		assertUnchanged(execution)
		now = now.Add(time.Minute)
	}

	// Pagination through unrelated workspace work needs a cursor checkpoint,
	// but neither a revision nor task progress.
	fake.eventsPageFn = func(adapter.EventsRequest) (adapter.MulticaEventPage, error) {
		return adapter.MulticaEventPage{NextCursor: "evt1.1", Scanned: 1, Filtered: 1}, nil
	}
	probe()
	assertUnchanged(execution)
	fake.eventsPageFn = nil
	at := now
	fake.events = []adapter.Event{{Kind: "progress", State: adapter.StateRunning, SourceState: "issue.started", ObservedAt: at, Ordering: "source", DedupeKey: "progress-1", Payload: map[string]any{"workspace_id": config.Workspace, "issue_id": config.Issue}}}
	probe()
	progressed, _ := journal.GetExecution(ctx, execution.ID)
	if progressed.ProgressAt == nil || !progressed.ProgressAt.Equal(at) || progressed.Revision != execution.Revision+1 {
		t.Fatalf("real event did not record exactly one progress update: %#v", progressed)
	}
	// Replay must preserve the full envelope, not just progress_at.
	now = now.Add(time.Minute)
	probe()
	assertUnchanged(progressed)
	replayed, _ := journal.GetExecution(ctx, execution.ID)
	if replayed.ProgressAt == nil || !replayed.ProgressAt.Equal(at) {
		t.Fatalf("duplicate event manufactured progress: %#v", replayed)
	}
	// A timeout after that event cannot be healed by its duplicate delivery.
	failure := unreachableProbe(replayed, now)
	if err := bridge.ApplyProbe(ctx, execution.ID.String(), failure); err != nil {
		t.Fatal(err)
	}
	failed, _ := journal.GetExecution(ctx, execution.ID)
	probe()
	assertUnchanged(failed)
	stillFailed, _ := journal.GetExecution(ctx, execution.ID)
	if stillFailed.Liveness != model.LivenessUnreachable || stillFailed.Observation.Integrity != model.IntegrityDegraded {
		t.Fatalf("replay healed failed probe: %#v", stillFailed)
	}
	progressed = failed
	fake.events = nil
	for range 3 {
		probe()
		assertUnchanged(progressed)
		now = now.Add(time.Minute)
	}

	// A terminal event can arrive after an empty probe and before ApplyProbe.
	result, err := bridge.Reprobe(ctx, supervisor.Execution{ID: execution.ID.String()})
	if err != nil {
		t.Fatal(err)
	}
	fake.events = []adapter.Event{{Kind: "terminal", State: adapter.StateCompleted, SourceState: "issue.completed", ObservedAt: now, Ordering: "source", DedupeKey: "terminal-1", Payload: map[string]any{"workspace_id": config.Workspace, "issue_id": config.Issue, "aggregate_kind": "issue"}}}
	if _, err := engine.Events(ctx, execution.ID, "", adapter.PollOptions{}); err != nil {
		t.Fatal(err)
	}
	terminal, _ := journal.GetExecution(ctx, execution.ID)
	if err := bridge.ApplyProbe(ctx, execution.ID.String(), result); err != nil {
		t.Fatal(err)
	}
	assertUnchanged(terminal)
	got, _ := journal.GetExecution(ctx, execution.ID)
	if got.State != model.StateCompleted || got.Observation.Integrity != model.IntegrityVerified || got.ProgressAt == nil || !got.ProgressAt.Equal(now) {
		t.Fatalf("terminal race lost authority evidence: %#v", got)
	}
}

func TestSnapshotProbeProgressOnlyStateChanges(t *testing.T) {
	ctx := context.Background()
	engine, journal, _ := testEngine(t, NewRegistry())
	config := adapter.MulticaConfig{Binary: "/opt/multica", Profile: "private", Endpoint: "https://multica.internal", Workspace: "workspace-1", Issue: "issue-2"}
	execution := createPromotedIssueExecution(t, engine, journal, AdapterSpec{Name: "multica", Executable: config.Binary, Multica: &config})
	bridge := SupervisorExecutions{Engine: engine}
	first := fixtureNow.Add(time.Minute)
	result := supervisor.ProbeResult{State: "running", Liveness: "alive", Source: "status_api", ObservedAt: first, Revision: execution.Revision}
	if err := bridge.ApplyProbe(ctx, execution.ID.String(), result); err != nil {
		t.Fatal(err)
	}
	current, _ := journal.GetExecution(ctx, execution.ID)
	if current.ProgressAt == nil || !current.ProgressAt.Equal(first) {
		t.Fatalf("snapshot state change did not record progress: %#v", current)
	}
	result.Revision = current.Revision
	result.ObservedAt = first.Add(time.Hour)
	if err := bridge.ApplyProbe(ctx, execution.ID.String(), result); err != nil {
		t.Fatal(err)
	}
	current, _ = journal.GetExecution(ctx, execution.ID)
	if current.ProgressAt == nil || !current.ProgressAt.Equal(first) {
		t.Fatalf("snapshot poll manufactured progress: %#v", current)
	}
	result.Revision = current.Revision
	result.State, result.Liveness = "completed", "exited"
	result.ObservedAt = first.Add(2 * time.Hour)
	if err := bridge.ApplyProbe(ctx, execution.ID.String(), result); err != nil {
		t.Fatal(err)
	}
	current, _ = journal.GetExecution(ctx, execution.ID)
	if current.State != model.StateCompleted || current.ProgressAt == nil || !current.ProgressAt.Equal(result.ObservedAt) {
		t.Fatalf("terminal snapshot did not advance progress: %#v", current)
	}
}

func TestRuntimeCASPreservesNewerProgress(t *testing.T) {
	ctx := context.Background()
	engine, journal, _ := testEngine(t, NewRegistry())
	config := adapter.MulticaConfig{Binary: "/opt/multica", Profile: "private", Endpoint: "https://multica.internal", Workspace: "workspace-1", Issue: "issue-2"}
	stale := createPromotedIssueExecution(t, engine, journal, AdapterSpec{Name: "multica", Executable: config.Binary, Multica: &config})
	at := fixtureNow.Add(time.Minute)
	newer := stale
	newer.ProgressAt = &at
	if _, err := journal.UpdateExecution(ctx, newer, stale.Revision); err != nil {
		t.Fatal(err)
	}
	// An unbound observation update may retry against newer coordination state,
	// but cannot clear its task-progress evidence with an older envelope copy.
	got, err := engine.updateCAS(ctx, stale)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProgressAt == nil || !got.ProgressAt.Equal(at) {
		t.Fatalf("CAS lost newer progress: %#v", got)
	}
}

func TestMulticaEventCASRaceDoesNotLosePage(t *testing.T) {
	for _, scenario := range []string{"progress", "terminal", "terminal-wins", "exhausted-retry", "newer-attention", "older-terminal", "newer-unreachable"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			config := adapter.MulticaConfig{Binary: "/opt/multica", Profile: "private", Endpoint: "https://multica.internal", Workspace: "workspace-1", Issue: "issue-2"}
			fake := &fakeAdapter{name: "multica"}
			registry := NewRegistry()
			if err := registry.Register("multica", func(AdapterSpec) (adapter.Adapter, error) { return fake, nil }); err != nil {
				t.Fatal(err)
			}
			engine, journal, _ := testEngine(t, registry)
			execution := createPromotedIssueExecution(t, engine, journal, AdapterSpec{Name: "multica", Executable: config.Binary, Multica: &config})
			at := fixtureNow.Add(time.Minute)
			kind, state, source := "progress", adapter.StateRunning, "issue.started"
			if scenario == "terminal" || scenario == "exhausted-retry" || scenario == "older-terminal" {
				kind, state, source = "terminal", adapter.StateCompleted, "issue.completed"
			}
			fake.eventsPageFn = func(adapter.EventsRequest) (adapter.MulticaEventPage, error) {
				return adapter.MulticaEventPage{Events: []adapter.Event{{Kind: kind, State: state, SourceState: source, ObservedAt: at, Ordering: "source", DedupeKey: "raced-event", Payload: map[string]any{"workspace_id": config.Workspace, "issue_id": config.Issue, "aggregate_kind": "issue"}}}, Scanned: 1, NextCursor: "evt1.1"}, nil
			}
			writes := 0
			journal.beforeUpdate = func(j *fakeJournal, id ids.ExecutionID) {
				writes++
				if writes > 1 && scenario != "exhausted-retry" {
					return
				}
				current := j.executions[id]
				current.Revision++
				current.Labels = []string{"concurrent"}
				if scenario == "terminal-wins" {
					current.State, current.Liveness = model.StateCompleted, model.LivenessExited
					current.TerminalAt, current.ProgressAt = &at, &at
					current.Observation = model.Observation{Source: model.ObservationDurableOutbox, Integrity: model.IntegrityVerified, ObservedAt: at}
				}
				if scenario == "newer-attention" || scenario == "older-terminal" {
					newer := at.Add(time.Minute)
					current.State, current.Liveness = model.StateAttention, model.LivenessBlocked
					current.SourceState = sourceState("attention")
					current.ProgressAt, current.UpdatedAt = &newer, newer
					current.Observation = model.Observation{Source: model.ObservationStatusAPI, Integrity: model.IntegrityVerified, ObservedAt: newer}
				}
				if scenario == "newer-unreachable" {
					newer := at.Add(time.Minute)
					current.State, current.SourceState = model.StateRunning, sourceState(source)
					current.Liveness, current.UpdatedAt = model.LivenessUnreachable, newer
					current.Observation = model.Observation{Source: model.ObservationUnknown, Integrity: model.IntegrityDegraded, ObservedAt: newer}
				}
				j.executions[id] = current
			}
			bridge := SupervisorExecutions{Engine: engine}
			result, err := bridge.Reprobe(ctx, supervisor.Execution{ID: execution.ID.String()})
			if scenario == "exhausted-retry" {
				if errorCode(err) != CodeConflict {
					t.Fatalf("exhausted retry error=%v", err)
				}
				cursor, cursorErr := bridge.latestSourceCursor(ctx, execution.ID)
				if cursorErr != nil || cursor != "" {
					t.Fatalf("failed apply advanced cursor: %q %v", cursor, cursorErr)
				}
				journal.beforeUpdate = nil
				result, err = bridge.Reprobe(ctx, supervisor.Execution{ID: execution.ID.String()})
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := bridge.ApplyProbe(ctx, execution.ID.String(), result); err != nil {
				t.Fatal(err)
			}
			current, _ := journal.GetExecution(ctx, execution.ID)
			want := model.State(state)
			if scenario == "terminal-wins" {
				want = model.StateCompleted
			}
			wantProgress := at
			if scenario == "newer-attention" || scenario == "older-terminal" {
				wantProgress = at.Add(time.Minute)
			}
			if scenario == "newer-attention" {
				want = model.StateAttention
				if current.Revision != execution.Revision+1 || current.Liveness != model.LivenessBlocked || current.Observation.Source != model.ObservationStatusAPI || !current.Observation.ObservedAt.Equal(wantProgress) || !current.UpdatedAt.Equal(wantProgress) {
					t.Fatalf("older event overwrote newer snapshot: %#v", current)
				}
			}
			wantIntegrity := model.IntegrityVerified
			if scenario == "newer-unreachable" {
				wantIntegrity = model.IntegrityDegraded
				newer := at.Add(time.Minute)
				if current.Liveness != model.LivenessUnreachable || current.Observation.Source != model.ObservationUnknown || !current.Observation.ObservedAt.Equal(newer) || !current.UpdatedAt.Equal(newer) {
					t.Fatalf("older event healed newer failure: %#v", current)
				}
			}
			if current.State != want || current.ProgressAt == nil || !current.ProgressAt.Equal(wantProgress) || len(current.Labels) != 1 || current.Labels[0] != "concurrent" || current.Observation.Integrity != wantIntegrity {
				t.Fatalf("race lost evidence or coordination: %#v", current)
			}
			if !current.State.Terminal() {
				cursor, _ := bridge.latestSourceCursor(ctx, execution.ID)
				if cursor != "evt1.1" {
					t.Fatalf("successful apply did not checkpoint page: %q", cursor)
				}
			}
		})
	}
}

func TestRuntimeLaunchAndResultRecordProgress(t *testing.T) {
	ctx := context.Background()
	session := fixtureSession("fixture", "fixture_session", "session-1", adapter.StateRunning)
	fake := &fakeAdapter{name: "fixture", launch: adapter.LaunchResult{Session: session}}
	registry := NewRegistry()
	if err := registry.Register("fixture", func(AdapterSpec) (adapter.Adapter, error) { return fake, nil }); err != nil {
		t.Fatal(err)
	}
	engine, _, _ := testEngine(t, registry)
	execution, err := engine.Launch(ctx, LaunchOptions{Adapter: AdapterSpec{Name: "fixture"}, Request: adapter.LaunchRequest{Argv: []string{"fixture"}}})
	if err != nil {
		t.Fatal(err)
	}
	if execution.ProgressAt == nil || !execution.ProgressAt.Equal(fixtureNow) {
		t.Fatalf("launch has unknown progress: %#v", execution)
	}
	completed := fixtureNow.Add(time.Minute)
	engine.clock = func() time.Time { return completed }
	fake.result = adapter.Result{State: adapter.StateCompleted, Success: true, SessionRef: session.Ref}
	execution, err = engine.Result(ctx, execution.ID, adapter.PollOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if execution.State != model.StateCompleted || execution.ProgressAt == nil || !execution.ProgressAt.Equal(completed) {
		t.Fatalf("result did not record terminal progress: %#v", execution)
	}
	engine.clock = func() time.Time { return completed.Add(time.Hour) }
	execution, err = engine.Result(ctx, execution.ID, adapter.PollOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if execution.ProgressAt == nil || !execution.ProgressAt.Equal(completed) {
		t.Fatalf("identical result poll manufactured progress: %#v", execution)
	}
}

func TestUnchangedSnapshotSourceNormalizationDoesNotAdvanceProgress(t *testing.T) {
	for _, path := range []string{"supervisor", "session"} {
		t.Run(path, func(t *testing.T) {
			ctx := context.Background()
			engine, journal, _ := testEngine(t, NewRegistry())
			config := adapter.MulticaConfig{Binary: "/opt/multica", Profile: "private", Endpoint: "https://multica.internal", Workspace: "workspace-1", Issue: "issue-2"}
			spec := AdapterSpec{Name: "multica", Executable: config.Binary, Multica: &config}
			execution := createPromotedIssueExecution(t, engine, journal, spec)
			execution.State, execution.SourceState = model.StateRunning, sourceState("issue.started")
			execution.ProgressAt = &fixtureNow
			execution, err := journal.UpdateExecution(ctx, execution, execution.Revision)
			if err != nil {
				t.Fatal(err)
			}
			later := fixtureNow.Add(time.Hour)
			if path == "supervisor" {
				err = (SupervisorExecutions{Engine: engine}).ApplyProbe(ctx, execution.ID.String(), supervisor.ProbeResult{State: "running", Liveness: "alive", Source: "status_api", ObservedAt: later, Revision: execution.Revision})
			} else {
				session := fixtureSession("multica", "multica_issue", config.Issue, adapter.StateRunning)
				session.Observation.Source, session.Observation.ObservedAt = "status_api", later
				session.Ref.Profile, session.Ref.Endpoint, session.Ref.Workspace, session.Ref.Issue = config.Profile, config.Endpoint, config.Workspace, config.Issue
				_, err = engine.applySession(ctx, execution, session, spec)
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := journal.GetExecution(ctx, execution.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.ProgressAt == nil || !got.ProgressAt.Equal(fixtureNow) {
				t.Fatalf("normalization manufactured progress: %#v", got)
			}
		})
	}
}
