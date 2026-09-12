package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"go.etcd.io/bbolt"
)

func storedDelegationFixture() *model.DelegationBinding {
	return &model.DelegationBinding{RequestSHA256: hash('a'), ConfigurationSHA256: hash('b'), Requested: json.RawMessage(`{"family":"grok"}`), Resolved: model.DelegationTarget{Harness: "cursor", Family: "grok", Model: "cursor-grok-4.6-high", Host: "local", Authority: model.AuthorityNative}, Defaulted: []string{"model", "host"}}
}

func TestDelegationSurvivesLegacyProjectionRewrite(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	initial := sampleExecution(now)
	initial.Delegation = storedDelegationFixture()
	initial.Delegation.NativePlan = &model.DelegationNativePlan{Argv: []string{"cursor-agent", "--print"}, PromptDelivery: "argv"}
	mutation := contracts.MutationKey{Scope: "execution:delegate", Key: "request-01", InputDigest: hash('a')}
	created, _, err := j.CreateExecution(ctx, initial, mutation)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an already-running older supervisor: unknown JSON fields vanish
	// when its older Execution struct is marshaled back to the same record.
	err = j.db.Update(func(tx *bbolt.Tx) error {
		legacy := created
		legacy.Delegation = nil
		body, e := json.Marshal(legacy)
		if e != nil {
			return e
		}
		return tx.Bucket(bExecutions).Put([]byte(created.ID.String()), body)
	})
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := j.GetExecution(ctx, created.ID)
	if err != nil || !reflect.DeepEqual(recovered.Delegation, created.Delegation) {
		t.Fatalf("recovered=%#v error=%v", recovered.Delegation, err)
	}
	replay, found, err := j.GetExecutionByMutation(ctx, mutation)
	if err != nil || !found || replay.Delegation == nil {
		t.Fatalf("replay=%#v found=%v err=%v", replay, found, err)
	}
	reused, wasReused, err := j.CreateExecution(ctx, initial, mutation)
	if err != nil || !wasReused || reused.Delegation == nil {
		t.Fatalf("reuse=%#v reused=%v err=%v", reused, wasReused, err)
	}
	// A new writer must still be able to finalize after the old writer dropped
	// metadata; the prior state used in transition validation is hydrated too.
	recovered.UpdatedAt = now.Add(time.Second)
	recovered.Observation.ObservedAt = recovered.UpdatedAt
	if _, err := j.UpdateExecution(ctx, recovered, recovered.Revision); err != nil {
		t.Fatal(err)
	}
	// A contradictory copy is corruption, never a mutable-default selection.
	err = j.db.Update(func(tx *bbolt.Tx) error {
		bad := created
		copy := *bad.Delegation
		copy.Resolved.Model = "other"
		bad.Delegation = &copy
		body, e := json.Marshal(bad)
		if e != nil {
			return e
		}
		return tx.Bucket(bExecutions).Put([]byte(created.ID.String()), body)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.GetExecution(ctx, created.ID); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("contradiction error=%v", err)
	}
}

func TestCleanupRemovesDelegationIdempotencySnapshot(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	initial := sampleExecution(now)
	initial.Delegation = storedDelegationFixture()
	terminal := now.Add(time.Second)
	initial.State = model.StateCompleted
	initial.Liveness = model.LivenessExited
	initial.TerminalAt = &terminal
	created, _, err := j.CreateExecution(ctx, initial, contracts.MutationKey{Scope: "execution:delegate", Key: "request-01", InputDigest: hash('a')})
	if err != nil {
		t.Fatal(err)
	}
	cutoff := now.Add(time.Hour)
	opts := CleanupOptions{IncludeUnreconciled: true}
	plan, err := j.PlanCleanup(ctx, cutoff, opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.ApplyCleanup(ctx, cutoff, plan.PlanDigest, opts); err != nil {
		t.Fatal(err)
	}
	err = j.db.View(func(tx *bbolt.Tx) error {
		if tx.Bucket(bIdempotency).Get([]byte(delegationBindingScope+"\x00"+created.ID.String())) != nil {
			t.Fatal("delegation snapshot retained after cleanup")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
