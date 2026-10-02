package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"go.etcd.io/bbolt"
)

func rewriteCallerProjection(t *testing.T, journal *Journal, execution model.Execution, caller *model.ExecutionCaller) {
	t.Helper()
	execution.Caller = caller
	body, err := json.Marshal(execution)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bExecutions).Put([]byte(execution.ID.String()), body)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCallerSurvivesLegacyProjectionRewrite(t *testing.T) {
	for _, authority := range []model.Authority{model.AuthorityNative, model.AuthorityMultica} {
		t.Run(string(authority), func(t *testing.T) {
			ctx := context.Background()
			journal, _, now := openTestJournal(t)
			initial := sampleExecution(now)
			initial.Caller = &model.ExecutionCaller{Harness: model.CallerHermes, Provenance: model.CallerDeclared}
			if authority == model.AuthorityMultica {
				initial.Authority, initial.Mode, initial.Adapter = authority, model.ModeMultica, "multica"
			}
			mutation := contracts.MutationKey{Scope: "execution:test-caller", Key: "request-1", InputDigest: hash('a')}
			created, _, err := journal.CreateExecution(ctx, initial, mutation)
			if err != nil {
				t.Fatal(err)
			}
			// An old supervisor drops the field when marshaling its older struct.
			rewriteCallerProjection(t, journal, created, nil)
			recovered, err := journal.GetExecution(ctx, created.ID)
			if err != nil || !reflect.DeepEqual(recovered.Caller, created.Caller) {
				t.Fatalf("recovered caller=%#v err=%v", recovered.Caller, err)
			}
			listed, err := journal.ListExecutions(ctx, false)
			if err != nil || len(listed) != 1 || !reflect.DeepEqual(listed[0].Caller, created.Caller) {
				t.Fatalf("listed=%#v err=%v", listed, err)
			}
			replay, found, err := journal.GetExecutionByMutation(ctx, mutation)
			if err != nil || !found || !reflect.DeepEqual(replay.Caller, created.Caller) {
				t.Fatalf("mutation replay caller=%#v found=%v err=%v", replay.Caller, found, err)
			}
			initial.Caller = &model.ExecutionCaller{Harness: model.CallerCodex, Provenance: model.CallerDeclared}
			reused, wasReused, err := journal.CreateExecution(ctx, initial, mutation)
			if err != nil || !wasReused || !reflect.DeepEqual(reused.Caller, created.Caller) {
				t.Fatalf("create replay caller=%#v reused=%v err=%v", reused.Caller, wasReused, err)
			}
			// Hydration also supplies the prior value to transition validation.
			recovered.UpdatedAt = now.Add(time.Second)
			recovered.Observation.ObservedAt = recovered.UpdatedAt
			updated, err := journal.UpdateExecution(ctx, recovered, recovered.Revision)
			if err != nil || !reflect.DeepEqual(updated.Caller, created.Caller) {
				t.Fatalf("new observer transition caller=%#v err=%v", updated.Caller, err)
			}
			updated.Caller = nil
			if _, err := journal.UpdateExecution(ctx, updated, updated.Revision); !errors.Is(err, ErrConflict) {
				t.Fatalf("caller erasure update accepted: %v", err)
			}
			rewriteCallerProjection(t, journal, created, initial.Caller)
			if _, err := journal.GetExecution(ctx, created.ID); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("contradictory caller projection accepted: %v", err)
			}
		})
	}
}

func TestCallerSnapshotRestoresReadOnlyWithoutProjectionWrites(t *testing.T) {
	ctx := context.Background()
	journal, _, now := openTestJournal(t)
	path := journal.Path()
	initial := sampleExecution(now)
	initial.Caller = &model.ExecutionCaller{Harness: model.CallerCursor, Provenance: model.CallerDeclared}
	created, _, err := journal.CreateExecution(ctx, initial, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	rewriteCallerProjection(t, journal, created, nil)
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := Open(path, Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	recovered, getErr := reader.GetExecution(ctx, created.ID)
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if getErr != nil || !reflect.DeepEqual(recovered.Caller, created.Caller) {
		t.Fatalf("read-only caller=%#v err=%v", recovered.Caller, getErr)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("read-only caller hydration changed journal: %v", err)
	}
}

func TestCallerSnapshotAbsentForUndeclaredExecutions(t *testing.T) {
	ctx := context.Background()
	journal, _, now := openTestJournal(t)
	created, _, err := journal.CreateExecution(ctx, sampleExecution(now), contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.db.View(func(tx *bbolt.Tx) error {
		if tx.Bucket(bIdempotency).Get([]byte(callerBindingScope+"\x00"+created.ID.String())) != nil {
			t.Error("undeclared caller acquired a snapshot")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	loaded, err := journal.GetExecution(ctx, created.ID)
	if err != nil || loaded.Caller != nil {
		t.Fatalf("historical caller changed: %#v err=%v", loaded.Caller, err)
	}
	loaded.UpdatedAt = now.Add(time.Second)
	loaded.Caller = &model.ExecutionCaller{Harness: model.CallerCodex, Provenance: model.CallerDeclared}
	if _, err := journal.UpdateExecution(ctx, loaded, loaded.Revision); !errors.Is(err, ErrConflict) {
		t.Fatalf("historical caller backfill accepted: %v", err)
	}
}

func TestCallerSnapshotRejectsMalformedBindings(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*mutationRecord)
	}{
		{"scope", func(r *mutationRecord) { r.Scope = "other" }},
		{"key", func(r *mutationRecord) { r.Key = "other" }},
		{"object_type", func(r *mutationRecord) { r.ObjectType = "other" }},
		{"object_id", func(r *mutationRecord) { r.ObjectID = "other" }},
		{"digest", func(r *mutationRecord) { r.InputDigest = hash('b') }},
		{"missing_caller", func(r *mutationRecord) { r.Caller = nil }},
		{"harness", func(r *mutationRecord) { r.Caller.Harness = "inferred" }},
		{"provenance", func(r *mutationRecord) { r.Caller.Provenance = "inferred" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			journal, _, now := openTestJournal(t)
			initial := sampleExecution(now)
			initial.Caller = &model.ExecutionCaller{Harness: model.CallerHermes, Provenance: model.CallerDeclared}
			created, _, err := journal.CreateExecution(ctx, initial, contracts.MutationKey{})
			if err != nil {
				t.Fatal(err)
			}
			if err := journal.db.Update(func(tx *bbolt.Tx) error {
				bucket := tx.Bucket(bIdempotency)
				key := []byte(callerBindingScope + "\x00" + created.ID.String())
				var record mutationRecord
				if err := json.Unmarshal(bucket.Get(key), &record); err != nil {
					return err
				}
				tc.mutate(&record)
				body, err := json.Marshal(record)
				if err != nil {
					return err
				}
				return bucket.Put(key, body)
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := journal.GetExecution(ctx, created.ID); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("malformed binding accepted: %v", err)
			}
		})
	}
}

func TestCleanupRemovesCallerSnapshot(t *testing.T) {
	ctx := context.Background()
	journal, _, now := openTestJournal(t)
	initial := sampleExecution(now)
	initial.Caller = &model.ExecutionCaller{Harness: model.CallerHermes, Provenance: model.CallerDeclared}
	terminal := now.Add(time.Second)
	initial.State, initial.Liveness, initial.TerminalAt = model.StateCompleted, model.LivenessExited, &terminal
	created, _, err := journal.CreateExecution(ctx, initial, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	cutoff, opts := now.Add(time.Hour), CleanupOptions{IncludeUnreconciled: true}
	plan, err := journal.PlanCleanup(ctx, cutoff, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Eligible) != 1 || plan.Records.IdempotencyKeys != 1 {
		t.Fatalf("caller snapshot omitted from cleanup graph: %#v", plan)
	}
	if _, err := journal.ApplyCleanup(ctx, cutoff, plan.PlanDigest, opts); err != nil {
		t.Fatal(err)
	}
	if err := journal.db.View(func(tx *bbolt.Tx) error {
		if tx.Bucket(bIdempotency).Get([]byte(callerBindingScope+"\x00"+created.ID.String())) != nil {
			t.Error("caller snapshot retained after cleanup")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
