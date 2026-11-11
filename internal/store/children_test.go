package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"go.etcd.io/bbolt"
)

func TestChildrenIsolationCollectionAndWakeCoverage(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	parent, _, err := j.CreateExecution(ctx, sampleExecution(now), contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	caller := &model.CallerIdentity{Provider: "claude", NativeSessionID: hash('a'), Provenance: "explicit"}
	makeChild := func(terminal, ack, other, subscribed bool) model.Execution {
		e := sampleExecution(now)
		e.ParentExecutionID = &parent.ID
		e.CallerIdentity = caller
		if other {
			otherID, _ := ids.ParseExecutionID(durableTestID(ids.TypeExecution, 900))
			e.ParentExecutionID = &otherID
			e.CallerIdentity = &model.CallerIdentity{Provider: "claude", NativeSessionID: hash('b'), Provenance: "explicit"}
		}
		if terminal {
			e.State, e.TerminalAt, e.Liveness = model.StateCompleted, &now, model.LivenessExited
		}
		e, _, err = j.CreateExecution(ctx, e, contracts.MutationKey{})
		if err != nil {
			t.Fatal(err)
		}
		if ack {
			if _, _, err := j.AcknowledgeExecution(ctx, e.ID, AcknowledgementAwait); err != nil {
				t.Fatal(err)
			}
		}
		if subscribed {
			sub := durableTestSubscription(durableTestID(ids.TypeSubscription, 1))
			sub.Filter.ExecutionIDs = []string{e.ID.String()}
			sub.Filter.Kinds = []string{"terminal"}
			sub.ExpiresAt = now.Add(time.Hour)
			if _, _, err := j.PutSubscription(ctx, sub); err != nil {
				t.Fatal(err)
			}
		}
		return e
	}
	active := makeChild(false, false, false, false)
	terminal := makeChild(true, false, false, false)
	makeChild(true, true, false, false)
	makeChild(false, false, true, false)
	covered := makeChild(false, false, false, true)
	path := j.path
	j.Close()
	reader := Cleanup{Open: func(ctx context.Context, ro bool) (*Journal, error) {
		if !ro {
			t.Fatal("query requested write lock")
		}
		return Open(path, Options{ReadOnly: true, LockTimeout: time.Millisecond})
	}}
	for _, scope := range []ChildrenScope{{Parent: &parent.ID}, {Provider: caller.Provider, NativeSessionID: caller.NativeSessionID}} {
		got, err := reader.Children(ctx, scope, now)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].ID != active.ID || got[1].ID != terminal.ID || got[0].Reason != "running_without_known_waiter" || got[1].Reason != "terminal_uncollected" {
			t.Fatalf("children=%+v", got)
		}
	}
	// Expiration is evaluated at query time, without mutating subscription state.
	got, err := reader.Children(ctx, ChildrenScope{Parent: &parent.ID}, now.Add(2*time.Hour))
	if err != nil || len(got) != 3 || got[2].ID != covered.ID {
		t.Fatalf("expired coverage: %+v %v", got, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if got, err := reader.Children(cancelled, ChildrenScope{Parent: &parent.ID}, now); !errors.Is(err, context.Canceled) || got != nil {
		t.Fatalf("cancelled scan returned %+v %v", got, err)
	}
}

func TestAttributionSurvivesOldProjectionAndReplay(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	parent, _, _ := j.CreateExecution(ctx, sampleExecution(now), contracts.MutationKey{})
	e := sampleExecution(now)
	e.ParentExecutionID = &parent.ID
	e.CallerIdentity = &model.CallerIdentity{Provider: "codex", NativeSessionID: hash('a'), Provenance: "native_stream"}
	mutation := contracts.MutationKey{Scope: "test:attribution", Key: "same", InputDigest: hash('c')}
	created, _, err := j.CreateExecution(ctx, e, mutation)
	if err != nil {
		t.Fatal(err)
	}
	e.ParentExecutionID = nil
	e.CallerIdentity = nil
	replay, reused, err := j.CreateExecution(ctx, e, mutation)
	if err != nil || !reused || !reflect.DeepEqual(replay.CallerIdentity, created.CallerIdentity) {
		t.Fatalf("replay=%+v %v", replay, err)
	}
	// A legacy observer rewrites a projection without the new metadata.
	legacy := created
	legacy.ParentExecutionID = nil
	legacy.CallerIdentity = nil
	raw, _ := json.Marshal(legacy)
	if err := j.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket(bExecutions).Put([]byte(created.ID.String()), raw) }); err != nil {
		t.Fatal(err)
	}
	restored, err := j.GetExecution(ctx, created.ID)
	if err != nil || !reflect.DeepEqual(restored.CallerIdentity, created.CallerIdentity) || restored.ParentExecutionID == nil || *restored.ParentExecutionID != parent.ID {
		t.Fatalf("restored=%+v %v", restored, err)
	}
	restored.CallerIdentity = nil
	if _, err := j.UpdateExecution(ctx, restored, restored.Revision); err == nil {
		t.Fatal("erased attribution")
	}
	path := j.path
	j.Close()
	got, err := cleanupForPath(path).Children(ctx, ChildrenScope{Parent: &parent.ID}, now)
	if err != nil || len(got) != 1 || got[0].ID != created.ID {
		t.Fatalf("query missed restored attribution: %+v %v", got, err)
	}
}

func TestChildrenBoundReturnsNoPartialSuccess(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	caller := &model.CallerIdentity{Provider: "codex", NativeSessionID: hash('a'), Provenance: "native_environment"}
	for i := 0; i < 201; i++ {
		e := sampleExecution(now)
		e.CallerIdentity = caller
		if _, _, err := j.CreateExecution(ctx, e, contracts.MutationKey{}); err != nil {
			t.Fatal(err)
		}
	}
	path := j.path
	j.Close()
	got, err := cleanupForPath(path).Children(ctx, ChildrenScope{Provider: caller.Provider, NativeSessionID: caller.NativeSessionID}, now)
	if !errors.Is(err, ErrChildrenBounded) || got != nil {
		t.Fatalf("partial query: %+v %v", got, err)
	}
}

func TestChildrenNeverScansUnrelatedProjectionOrAcknowledgements(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	e := sampleExecution(now)
	e.CallerIdentity = &model.CallerIdentity{Provider: "codex", NativeSessionID: hash('a'), Provenance: "native_environment"}
	e, _, err := j.CreateExecution(ctx, e, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	// Unrelated broken rows would fail a scan or global acknowledgement load.
	if err := j.db.Update(func(tx *bbolt.Tx) error {
		if err := tx.Bucket(bExecutions).Put([]byte(durableTestID(ids.TypeExecution, 900)), []byte("invalid unrelated projection")); err != nil {
			return err
		}
		return tx.Bucket(bAcknowledgements).Put([]byte(durableTestID(ids.TypeExecution, 901)), []byte("invalid unrelated acknowledgement"))
	}); err != nil {
		t.Fatal(err)
	}
	path := j.path
	j.Close()
	got, err := cleanupForPath(path).Children(ctx, ChildrenScope{Provider: "codex", NativeSessionID: hash('a')}, now)
	if err != nil || len(got) != 1 || got[0].ID != e.ID {
		t.Fatalf("indexed query scanned unrelated data: %+v %v", got, err)
	}
}

func TestCleanupRemovesAttributionSnapshotAndIndexes(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	e := sampleExecution(now)
	e.CallerIdentity = &model.CallerIdentity{Provider: "claude", NativeSessionID: hash('a'), Provenance: "explicit"}
	terminal := now.Add(time.Second)
	e.State, e.Liveness, e.TerminalAt = model.StateCompleted, model.LivenessExited, &terminal
	created, _, err := j.CreateExecution(ctx, e, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	cutoff, opts := now.Add(time.Hour), CleanupOptions{IncludeUnreconciled: true}
	plan, err := j.PlanCleanup(ctx, cutoff, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Eligible) != 1 || plan.Records.IdempotencyKeys != 2 {
		t.Fatalf("attribution graph: %+v", plan)
	}
	if _, err := j.ApplyCleanup(ctx, cutoff, plan.PlanDigest, opts); err != nil {
		t.Fatal(err)
	}
	if err := j.db.View(func(tx *bbolt.Tx) error {
		if tx.Bucket(bIdempotency).Get([]byte(attributionBindingScope+"\x00"+created.ID.String())) != nil {
			t.Fatal("snapshot survived cleanup")
		}
		prefix := childrenScopePrefix(ChildrenScope{Provider: "claude", NativeSessionID: hash('a')})
		if tx.Bucket(bIdempotency).Get(append(prefix, []byte(created.ID.String())...)) != nil {
			t.Fatal("session index survived cleanup")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
