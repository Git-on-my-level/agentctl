package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/supervisor"
)

func TestIdenticalStatusAPISnapshotSkipsRevisionUntilContactIsStale(t *testing.T) {
	ctx := context.Background()
	engine, journal, _ := testEngine(t, NewRegistry())
	config := adapter.MulticaConfig{Binary: "/opt/multica", Profile: "private", Endpoint: "https://multica.internal", Workspace: "workspace-1", Issue: "issue-contact"}
	execution := createPromotedIssueExecution(t, engine, journal, AdapterSpec{Name: "multica", Executable: config.Binary, Multica: &config})
	bridge := SupervisorExecutions{Engine: engine}
	first := fixtureNow.Add(time.Minute)
	probe := supervisor.ProbeResult{State: "attention", Liveness: "blocked", Source: "status_api", ObservedAt: first, Revision: execution.Revision}
	if err := bridge.ApplyProbe(ctx, execution.ID.String(), probe); err != nil {
		t.Fatal(err)
	}
	current, err := journal.GetExecution(ctx, execution.ID)
	if err != nil || current.Revision == execution.Revision || current.State != model.StateAttention {
		t.Fatalf("state change did not write: %#v %v", current, err)
	}
	written := current
	probe.Revision = current.Revision
	probe.ObservedAt = first.Add(time.Minute)
	if err := bridge.ApplyProbe(ctx, execution.ID.String(), probe); err != nil {
		t.Fatal(err)
	}
	current, _ = journal.GetExecution(ctx, execution.ID)
	if current.Revision != written.Revision || !current.Observation.ObservedAt.Equal(written.Observation.ObservedAt) || current.ProgressAt == nil || !current.ProgressAt.Equal(*written.ProgressAt) {
		t.Fatalf("identical snapshot rewrote the envelope: %#v", current)
	}
	probe.State, probe.Liveness = "running", "alive"
	probe.ObservedAt = first.Add(2 * time.Minute)
	if err := bridge.ApplyProbe(ctx, execution.ID.String(), probe); err != nil {
		t.Fatal(err)
	}
	current, _ = journal.GetExecution(ctx, execution.ID)
	if current.Revision == written.Revision || current.State != model.StateRunning || current.ProgressAt == nil || !current.ProgressAt.Equal(probe.ObservedAt) {
		t.Fatalf("state change did not write: %#v", current)
	}
	running := current
	probe.Revision = current.Revision
	deadline := running.Observation.ObservedAt.Add(statusAPIContactRefresh)
	var lastContact time.Time
	for at := running.Observation.ObservedAt.Add(time.Minute); at.Before(deadline); at = at.Add(time.Minute) {
		probe.ObservedAt = at
		if err := bridge.ApplyProbe(ctx, execution.ID.String(), probe); err != nil {
			t.Fatal(err)
		}
		lastContact = at
	}
	current, _ = journal.GetExecution(ctx, execution.ID)
	if current.Revision != running.Revision || !current.Observation.ObservedAt.Equal(running.Observation.ObservedAt) {
		t.Fatalf("repeated identical polls rewrote the envelope: %#v", current)
	}
	if got := journal.statusContact(execution.ID); !got.Equal(lastContact) || lastContact.Sub(current.Observation.ObservedAt) < time.Minute {
		t.Fatalf("contact=%s observed=%s", got, current.Observation.ObservedAt)
	}
	probe.ObservedAt = running.Observation.ObservedAt.Add(statusAPIContactRefresh + time.Second)
	if err := bridge.ApplyProbe(ctx, execution.ID.String(), probe); err != nil {
		t.Fatal(err)
	}
	current, _ = journal.GetExecution(ctx, execution.ID)
	if current.Revision == running.Revision || !current.Observation.ObservedAt.Equal(probe.ObservedAt) || current.ProgressAt == nil || !current.ProgressAt.Equal(*running.ProgressAt) {
		t.Fatalf("stale contact was not refreshed without a progress change: %#v", current)
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
