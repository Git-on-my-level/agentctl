package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/subscription"
	"go.etcd.io/bbolt"
)

func TestChildCollectionRecheckSurvivesCoverageChanges(t *testing.T) {
	for _, mode := range []string{"expiry", "cancellation"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			j, _, now := openTestJournal(t)
			defer j.Close()
			parent, _, err := j.CreateExecution(ctx, sampleExecution(now), contracts.MutationKey{})
			if err != nil {
				t.Fatal(err)
			}
			child := sampleExecution(now)
			child.ParentExecutionID = &parent.ID
			child, _, err = j.CreateExecution(ctx, child, contracts.MutationKey{})
			if err != nil {
				t.Fatal(err)
			}
			sub := durableTestSubscription(durableTestID(ids.TypeSubscription, 1))
			sub.Filter.ExecutionIDs = []string{child.ID.String()}
			sub.ExpiresAt = now.Add(time.Hour)
			if _, _, err = j.PutSubscription(ctx, sub); err != nil {
				t.Fatal(err)
			}
			scope := ChildrenScope{Parent: &parent.ID}
			check, err := j.CheckChildCollection(ctx, scope, now)
			if err != nil || !check.Recheck || len(check.Children) != 0 {
				t.Fatalf("covered=%+v %v", check, err)
			}
			queryAt := now
			if mode == "expiry" {
				queryAt = now.Add(2 * time.Hour)
			} else {
				record, err := j.GetSubscriptionRecord(ctx, sub.ID)
				if err != nil {
					t.Fatal(err)
				}
				sub.State = subscription.StateCancelled
				if _, err = j.UpdateSubscription(ctx, sub, record.Revision); err != nil {
					t.Fatal(err)
				}
			}
			check, err = j.CheckChildCollection(ctx, scope, queryAt)
			if err != nil || !check.Recheck || len(check.Children) != 1 || check.Children[0].ID != child.ID {
				t.Fatalf("uncovered=%+v %v", check, err)
			}
			child.State, child.Liveness, child.TerminalAt = model.StateCompleted, model.LivenessExited, &now
			child, err = j.UpdateExecution(ctx, child, child.Revision)
			if err != nil {
				t.Fatal(err)
			}
			check, err = j.CheckChildCollection(ctx, scope, queryAt)
			if err != nil || !check.Recheck || len(check.Children) != 1 {
				t.Fatalf("terminal uncollected=%+v %v", check, err)
			}
			if _, _, err = j.AcknowledgeExecution(ctx, child.ID, AcknowledgementAwait); err != nil {
				t.Fatal(err)
			}
			check, err = j.CheckChildCollection(ctx, scope, queryAt)
			if err != nil || check.Recheck || len(check.Children) != 0 {
				t.Fatalf("collected=%+v %v", check, err)
			}
		})
	}
}

func TestChildExitRecheckSurvivesOldProjection(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	defer j.Close()
	e := sampleExecution(now)
	e.ChildExitRecheck = true
	e.State, e.Liveness, e.TerminalAt = model.StateCompleted, model.LivenessExited, &now
	e, _, err := j.CreateExecution(ctx, e, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	if err = j.db.Update(func(tx *bbolt.Tx) error {
		e.ChildExitRecheck = false
		raw, err := json.Marshal(e)
		if err != nil {
			return err
		}
		return tx.Bucket(bExecutions).Put([]byte(e.ID.String()), raw)
	}); err != nil {
		t.Fatal(err)
	}
	recovered, err := j.GetExecution(ctx, e.ID)
	if err != nil || !recovered.ChildExitRecheck {
		t.Fatalf("lost obligation: %+v %v", recovered, err)
	}
	// Even a stale new writer cannot clear the immutable obligation.
	e, err = j.UpdateExecution(ctx, e, e.Revision)
	if err != nil || !e.ChildExitRecheck {
		t.Fatalf("update lost obligation: %+v %v", e, err)
	}
	if err = j.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bIdempotency).Put([]byte(childExitRecheckScope+"\x00"+e.ID.String()), []byte(`{}`))
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = j.GetExecution(ctx, e.ID); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt obligation read: %v", err)
	}
	if _, err = j.PlanCleanup(ctx, now.Add(time.Hour), CleanupOptions{}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt obligation cleanup: %v", err)
	}
}
