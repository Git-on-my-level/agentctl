package main

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
	"github.com/Git-on-my-level/agentctl/internal/supervisor"
)

func TestSupervisorOrphansOnlyProvenGoneStaleNativeLaunch(t *testing.T) {
	for _, scenario := range []string{"gone", "alive", "unproven", "lease", "legacy", "recent", "renewed", "owner_missing", "clock_shift_legacy", "owner_alive", "owner_gone", "owner_unproven", "pending", "pending_owner_gone", "pending_owner_alive"} {
		t.Run(scenario, func(t *testing.T) {
			world := seedReconcileWorld(t)
			row := world.gone
			row.OwnerLaunch = &model.LaunchIdentity{PID: 9999, StartedAt: world.now.Add(-30 * time.Minute)}
			j, err := store.Open(world.path, store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "pending", "pending_owner_gone", "pending_owner_alive":
				row.State = model.StateStarting
				row.SourceBindings = nil
				row.Launch = nil
				if scenario == "pending" {
					row.OwnerLaunch = nil
				}
			case "owner_missing", "clock_shift_legacy":
				// Even an apparently pre-boot wall-clock launch must not
				// substitute for a missing collector identity.
				row.Launch.StartedAt = world.now.Add(-24 * time.Hour)
				row.OwnerLaunch = nil
			case "owner_alive", "owner_gone", "owner_unproven":
				row.Launch.StartedAt = world.now.Add(-10 * time.Minute)
				row.OwnerLaunch = &model.LaunchIdentity{PID: 9999, StartedAt: world.now.Add(-30 * time.Minute)}
			case "legacy":
				row.Launch = nil
				row.OwnerLaunch = nil
			case "recent":
				row.Observation.ObservedAt = world.now.Add(-time.Minute)
				row.UpdatedAt = row.Observation.ObservedAt
			case "lease":
				seconds := 30
				row.Liveness = model.LivenessAlive
				row.Observation = model.Observation{Source: model.ObservationNativeStream, Integrity: model.IntegrityVerified, ObservedAt: world.now, FreshForSeconds: &seconds}
			}
			row, err = j.UpdateExecution(context.Background(), row, row.Revision)
			j.Close()
			if err != nil {
				t.Fatal(err)
			}
			var out, stderr bytes.Buffer
			a := testApp(&out, &stderr)
			a.now = func() time.Time { return world.now }
			proofCalls := 0
			a.processProof = func(identity processIdentity) processProof {
				proofCalls++
				if identity.PID == 9999 {
					return processProof{Gone: scenario == "gone" || scenario == "renewed" || scenario == "owner_gone" || scenario == "pending_owner_gone", Alive: scenario == "owner_alive" || scenario == "pending_owner_alive", Proof: "owner_proof"}
				}
				if scenario == "renewed" {
					// The proof must run outside the journal lock, and progress
					// committed during it must prevent orphaning the old revision.
					writer, err := store.Open(world.path, store.Options{LockTimeout: time.Millisecond})
					if err != nil {
						t.Fatal(err)
					}
					row.UpdatedAt = world.now
					row.Observation.ObservedAt = world.now
					row, err = writer.UpdateExecution(context.Background(), row, row.Revision)
					writer.Close()
					if err != nil {
						t.Fatal(err)
					}
				}
				gone := scenario == "gone" || scenario == "renewed" || scenario == "owner_alive" || scenario == "owner_gone" || scenario == "owner_unproven"
				return processProof{Gone: gone, Alive: scenario == "alive", Proof: "pid_absent"}
			}
			bridge := pathSupervisorExecutions{path: world.path, app: a}
			if scenario == "pending" {
				var result supervisor.ProbeResult
				result, err = bridge.Reprobe(context.Background(), supervisor.Execution{ID: row.ID.String()})
				if !result.AlreadyApplied {
					t.Fatal("pending admission was sent to an unavailable native status route")
				}
			} else {
				_, _, err = bridge.probeNativeOwner(context.Background(), row.ID.String())
			}
			if err != nil {
				t.Fatal(err)
			}
			reader, err := store.Open(world.path, store.Options{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			current, err := reader.GetExecution(context.Background(), row.ID)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "gone" || scenario == "owner_gone" {
				if current.State != model.StateOrphaned {
					t.Fatalf("state=%s", current.State)
				}
				outcome, err := reader.GetOutcome(context.Background(), row.ID)
				if err != nil || outcome.Failure == nil || outcome.Failure.Code != "owner_lost" {
					t.Fatalf("outcome=%+v err=%v", outcome, err)
				}
			} else if current.Revision != row.Revision || current.State != row.State {
				t.Fatalf("mutated %s: %+v", scenario, current)
			}
			if (scenario == "lease" || scenario == "legacy" || scenario == "recent" || scenario == "pending" || scenario == "owner_missing" || scenario == "clock_shift_legacy") && proofCalls != 0 {
				t.Fatalf("unnecessary kernel probe for %s", scenario)
			}
		})
	}
}

func TestSupervisorPendingOutboxSharesReadHandle(t *testing.T) {
	path, holder := holdJournal(t)
	holder.Close()
	reader, err := store.Open(path, store.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := (journalSupervisorOutbox{path: path}).ListPending(ctx); err != nil {
		t.Fatalf("pending scan tried to acquire exclusive lock: %v", err)
	}
}

func TestSupervisorStartingLiveChildWithGoneOwnerNeedsReconcile(t *testing.T) {
	world := seedReconcileWorld(t)
	row := world.gone
	row.State = model.StateStarting
	row.SourceBindings = nil
	row.Launch = nil
	row.OwnerLaunch = &model.LaunchIdentity{PID: 9999, StartedAt: world.now.Add(-time.Hour)}
	j, err := store.Open(world.path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	row, err = j.UpdateExecution(context.Background(), row, row.Revision)
	j.Close()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command("/bin/sleep", "60")
	prepareDetachedCommand(child)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	var out, stderr bytes.Buffer
	a := testApp(&out, &stderr)
	a.now = func() time.Time { return world.now }
	a.processProof = func(identity processIdentity) processProof {
		if identity.PID != 9999 {
			t.Fatalf("no child identity was recorded: %+v", identity)
		}
		return processProof{Gone: true, Proof: "pid_absent"}
	}
	bridge := pathSupervisorExecutions{path: world.path, app: a}
	result, err := bridge.Reprobe(context.Background(), supervisor.Execution{ID: row.ID.String()})
	if err != nil || !result.AlreadyApplied || result.State != "starting" || result.Liveness != "unknown" {
		t.Fatalf("probe=%+v err=%v", result, err)
	}
	if err := child.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("child not live: %v", err)
	}
	j, err = store.Open(world.path, store.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	current, err := j.GetExecution(context.Background(), row.ID)
	j.Close()
	if err != nil || current.State != model.StateStarting || current.Revision != row.Revision {
		t.Fatalf("starting row orphaned: %+v %v", current, err)
	}
	out.Reset()
	if problem := a.doctorReadiness(context.Background(), output.Renderer{Mode: output.JSON, Writer: &out}, common{journalPath: world.path, configPath: filepath.Join(t.TempDir(), "absent.json")}, []string{"--static"}); problem != nil {
		t.Fatal(problem)
	}
	if !strings.Contains(out.String(), "journal_native_child_unknown") || !strings.Contains(out.String(), row.ID.String()) || !strings.Contains(out.String(), "needs_reconcile") {
		t.Fatalf("doctor omitted uncertain child: %s", out.String())
	}
}
