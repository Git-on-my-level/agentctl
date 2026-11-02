package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/store"
	"github.com/Git-on-my-level/agentctl/internal/supervisor"
)

func TestSupervisorOrphansOnlyProvenGoneStaleNativeLaunch(t *testing.T) {
	for _, scenario := range []string{"gone", "alive", "unproven", "lease", "legacy", "recent", "renewed", "same_boot", "boot_unknown", "owner_alive", "owner_gone", "owner_unproven", "pending"} {
		t.Run(scenario, func(t *testing.T) {
			world := seedReconcileWorld(t)
			row := world.gone
			j, err := store.Open(world.path, store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "pending":
				row.State = model.StateStarting
				row.SourceBindings = nil
				row.Launch = nil
			case "same_boot":
				row.Launch.StartedAt = world.now.Add(-10 * time.Minute)
			case "owner_alive", "owner_gone", "owner_unproven":
				row.Launch.StartedAt = world.now.Add(-10 * time.Minute)
				row.OwnerLaunch = &model.LaunchIdentity{PID: 9999, StartedAt: world.now.Add(-30 * time.Minute)}
			case "legacy":
				row.Launch = nil
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
			a.bootTime = func() time.Time {
				if scenario == "boot_unknown" {
					return time.Time{}
				}
				return world.now.Add(-time.Hour)
			}
			proofCalls := 0
			a.processProof = func(identity processIdentity) processProof {
				proofCalls++
				if identity.PID == 9999 {
					return processProof{Gone: scenario == "owner_gone", Alive: scenario == "owner_alive", Proof: "owner_proof"}
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
				gone := scenario == "gone" || scenario == "renewed" || scenario == "same_boot" || scenario == "boot_unknown" || scenario == "owner_alive" || scenario == "owner_gone" || scenario == "owner_unproven"
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
			if (scenario == "lease" || scenario == "legacy" || scenario == "recent" || scenario == "pending") && proofCalls != 0 {
				t.Fatalf("unnecessary kernel probe for %s", scenario)
			}
		})
	}
}
