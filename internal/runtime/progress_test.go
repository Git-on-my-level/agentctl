package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/contracts"
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
	// Replayed source evidence cannot manufacture new progress.
	now = now.Add(time.Minute)
	probe()
	replayed, _ := journal.GetExecution(ctx, execution.ID)
	if replayed.ProgressAt == nil || !replayed.ProgressAt.Equal(at) {
		t.Fatalf("duplicate event manufactured progress: %#v", replayed)
	}
	progressed = replayed
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
