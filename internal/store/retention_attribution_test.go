package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"go.etcd.io/bbolt"
)

func TestCleanupHydratesLegacyChildAttributionAndPreservesGraph(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		for _, childTerminal := range []bool{false, true} {
			name := "manual/live-child"
			if automatic {
				name = "automatic/live-child"
			}
			if childTerminal {
				name += "/collected-child"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				j, _, now := openTestJournal(t)
				terminal := func() model.Execution {
					e := sampleExecution(now)
					e.State, e.TerminalAt, e.Liveness = model.StateCompleted, &now, model.LivenessExited
					return e
				}
				parent, _, err := j.CreateExecution(ctx, terminal(), contracts.MutationKey{})
				if err != nil {
					t.Fatal(err)
				}
				control, _, err := j.CreateExecution(ctx, terminal(), contracts.MutationKey{})
				if err != nil {
					t.Fatal(err)
				}
				child := sampleExecution(now)
				if childTerminal {
					child = terminal()
				}
				child.ParentExecutionID = &parent.ID
				child.CallerIdentity = &model.CallerIdentity{Provider: "codex", NativeSessionID: hash('a'), Provenance: "explicit"}
				child, _, err = j.CreateExecution(ctx, child, contracts.MutationKey{})
				if err != nil {
					t.Fatal(err)
				}
				for _, e := range []model.Execution{parent, control, child} {
					if e.State.Terminal() {
						if _, _, err := j.AcknowledgeExecution(ctx, e.ID, AcknowledgementAwait); err != nil {
							t.Fatal(err)
						}
					}
				}
				legacy := child
				legacy.ParentExecutionID, legacy.CallerIdentity = nil, nil
				raw, err := json.Marshal(legacy)
				if err != nil {
					t.Fatal(err)
				}
				if err := j.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket(bExecutions).Put([]byte(child.ID.String()), raw) }); err != nil {
					t.Fatal(err)
				}
				path := j.path
				j.Close()
				cleanup, cutoff := cleanupForPath(path), now.Add(time.Hour)
				plan, err := cleanup.Plan(ctx, cutoff, CleanupOptions{})
				if err != nil || len(plan.Eligible) != 1 || plan.Eligible[0].ExecutionID != control.ID.String() {
					t.Fatalf("legacy attribution lost graph protection: %+v %v", plan, err)
				}
				if automatic {
					progress, err := cleanup.Maintain(ctx, cutoff)
					if err != nil || progress.Executions != 1 {
						t.Fatalf("maintain: %+v %v", progress, err)
					}
				} else if _, err := cleanup.Apply(ctx, cutoff, plan.PlanDigest, CleanupOptions{}); err != nil {
					t.Fatal(err)
				}
				read, err := Open(path, Options{ReadOnly: true})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := read.GetExecution(ctx, parent.ID); err != nil {
					t.Fatalf("parent deleted: %v", err)
				}
				if _, err := read.GetExecution(ctx, control.ID); !errors.Is(err, ErrNotFound) {
					t.Fatalf("unlinked control retained: %v", err)
				}
				restored, err := read.GetExecution(ctx, child.ID)
				if err != nil || restored.ParentExecutionID == nil || *restored.ParentExecutionID != parent.ID || restored.CallerIdentity == nil {
					t.Fatalf("child binding lost: %+v %v", restored, err)
				}
				if err := read.db.View(func(tx *bbolt.Tx) error {
					if !bytes.Equal(raw, tx.Bucket(bExecutions).Get([]byte(child.ID.String()))) {
						t.Fatal("cleanup rewrote legacy projection")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				read.Close()
				got, err := cleanup.Children(ctx, ChildrenScope{Parent: &parent.ID}, now)
				want := 1
				if childTerminal {
					want = 0
				}
				if err != nil || len(got) != want {
					t.Fatalf("children after cleanup: %+v %v", got, err)
				}
			})
		}
	}
}

func TestCleanupRejectsCorruptImmutableAttribution(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	parent := sampleExecution(now)
	parent.State, parent.TerminalAt, parent.Liveness = model.StateCompleted, &now, model.LivenessExited
	parent, _, err := j.CreateExecution(ctx, parent, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := j.AcknowledgeExecution(ctx, parent.ID, AcknowledgementAwait); err != nil {
		t.Fatal(err)
	}
	child := sampleExecution(now)
	child.ParentExecutionID = &parent.ID
	child, _, err = j.CreateExecution(ctx, child, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	child.ParentExecutionID = nil
	raw, _ := json.Marshal(child)
	if err := j.db.Update(func(tx *bbolt.Tx) error {
		if err := tx.Bucket(bExecutions).Put([]byte(child.ID.String()), raw); err != nil {
			return err
		}
		return tx.Bucket(bIdempotency).Put([]byte(attributionBindingScope+"\x00"+child.ID.String()), []byte(`{"invalid":"binding"}`))
	}); err != nil {
		t.Fatal(err)
	}
	path := j.path
	j.Close()
	cleanup := cleanupForPath(path)
	if _, err := cleanup.Plan(ctx, now.Add(time.Hour), CleanupOptions{}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("plan accepted corrupt attribution: %v", err)
	}
	if _, err := cleanup.Maintain(ctx, now.Add(time.Hour)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("maintenance accepted corrupt attribution: %v", err)
	}
	read, err := Open(path, Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	if _, err := read.GetExecution(ctx, parent.ID); err != nil {
		t.Fatalf("uncertain graph parent deleted: %v", err)
	}
}
