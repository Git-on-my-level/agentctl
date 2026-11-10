package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/supervisor"
)

func TestParkedNativeRecoverySurvivesSupervisorRestart(t *testing.T) {
	ctx := context.Background()
	fake := &fakeAdapter{name: "fixture", launch: adapter.LaunchResult{Session: fixtureSession("fixture", "fixture_session", "session-1", adapter.StateRunning)}, snapshotErr: errors.New("owned process no longer exists")}
	registry := NewRegistry()
	if err := registry.Register("fixture", func(AdapterSpec) (adapter.Adapter, error) { return fake, nil }); err != nil {
		t.Fatal(err)
	}
	engine, journal, _ := testEngine(t, registry)
	execution, err := engine.Launch(ctx, LaunchOptions{Adapter: AdapterSpec{Name: "fixture", Executable: "/opt/fixture"}, Request: adapter.LaunchRequest{Argv: []string{"/opt/fixture"}}})
	if err != nil {
		t.Fatal(err)
	}
	state := "paused_network"
	execution.State, execution.Liveness, execution.SourceState = model.StateAttention, model.LivenessBlocked, &state
	execution.Observation.Integrity = model.IntegrityVerified
	execution.Observation.FreshForSeconds = nil
	execution, err = journal.UpdateExecution(ctx, execution, execution.Revision)
	if err != nil {
		t.Fatal(err)
	}
	engine.clock = func() time.Time { return fixtureNow.Add(24 * time.Hour) }
	bridge := SupervisorExecutions{Engine: engine}
	listed, err := bridge.ListNonTerminal(ctx)
	if err != nil || len(listed) != 0 {
		t.Fatalf("park scheduled for dead-owner reprobe: %v %v", listed, err)
	}
	probe, err := bridge.Reprobe(ctx, supervisor.Execution{ID: execution.ID.String()})
	if err != nil || probe.State != "attention" || probe.Liveness != "blocked" {
		t.Fatalf("pause lost: %+v %v", probe, err)
	}
	got, _ := journal.GetExecution(ctx, execution.ID)
	if got.Revision != execution.Revision || !got.NativeRecoveryParked() {
		t.Fatalf("pause rewritten: %+v", got)
	}
}

func TestSupervisorSkipsRemoteForwarding(t *testing.T) {
	engine, journal, _ := testEngine(t, NewRegistry())
	// This row deliberately has no local source binding: the remote host owns it.
	execution := model.Execution{ID: "exec-abandon-abandon-abandon-abandon-abandon-aisle", State: model.StateRunning, Delegation: &model.DelegationBinding{RemotePlan: &model.DelegationRemotePlan{Host: "remote"}}}
	journal.executions[execution.ID] = execution
	listed, err := (SupervisorExecutions{Engine: engine}).ListNonTerminal(context.Background())
	if err != nil || len(listed) != 0 {
		t.Fatalf("remote scheduled for native reprobe: %v %v", listed, err)
	}
}
