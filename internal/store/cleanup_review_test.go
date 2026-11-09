package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/subscription"
	"go.etcd.io/bbolt"
)

func TestAutomaticRetentionPreservesRequestKeyReplay(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	mutation := contracts.MutationKey{Scope: "execution:run", Key: "durable-retry", InputDigest: hash('b')}
	e := seedTerminalForRetention(t, j, now.Add(-30*24*time.Hour), mutation)
	if _, _, err := j.AcknowledgeExecution(ctx, e.ID, AcknowledgementResult); err != nil {
		t.Fatal(err)
	}
	path := j.path
	j.Close()
	if p, err := cleanupForPath(path).Maintain(ctx, now.Add(-14*24*time.Hour)); err != nil || p.Batches != 0 {
		t.Fatalf("retained replay deleted: %+v %v", p, err)
	}
	j, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	replay, found, err := j.GetExecutionByMutation(ctx, mutation)
	if err != nil || !found || replay.ID != e.ID {
		t.Fatalf("replay=%+v found=%v err=%v", replay, found, err)
	}
	replay, reused, err := j.CreateExecution(ctx, sampleExecution(now), mutation)
	if err != nil || !reused || replay.ID != e.ID {
		t.Fatalf("retry launched new work: %+v %v %v", replay, reused, err)
	}
	if _, err := j.GetOutcome(ctx, e.ID); err != nil {
		t.Fatalf("replayed result lost: %v", err)
	}
	changed := mutation
	changed.InputDigest = hash('c')
	if _, _, err = j.GetExecutionByMutation(ctx, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed inputs no longer conflict: %v", err)
	}
}

func TestCleanupANXStampIncludedInPagedProof(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	e := seedTerminalForRetention(t, j, now.Add(-48*time.Hour), contracts.MutationKey{})
	path := j.path
	j.Close()
	cleanup := cleanupForPath(path)
	plan, err := cleanup.Plan(ctx, now.Add(-24*time.Hour), CleanupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	j, err = Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = j.ProtectANXRun(ctx, e.ID); err != nil {
			t.Fatal(err)
		}
	}
	j.Close()
	if _, err = cleanup.Apply(ctx, plan.Before, plan.PlanDigest, CleanupOptions{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale plan deleted linked run: %v", err)
	}
	plan, err = cleanup.Plan(ctx, plan.Before, CleanupOptions{IncludeUnreconciled: true})
	if err != nil || len(plan.Eligible) != 0 || len(planReasons(plan, e.ID)) != 1 || planReasons(plan, e.ID)[0] != "anx_run_link" {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
}

func TestCleanupProtectsPausedSubscriptionCursor(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	e := seedTerminalForRetention(t, j, now.Add(-48*time.Hour), contracts.MutationKey{})
	events, err := j.ListEvents(ctx, e.ID, contracts.EventQuery{})
	if err != nil || len(events) == 0 {
		t.Fatalf("events=%v %v", events, err)
	}
	sub := durableTestSubscription(durableTestID(ids.TypeSubscription, 999))
	sub.Filter.ExecutionIDs = nil
	sub.Filter.LabelPrefix = "unrelated"
	sub.State = subscription.StatePaused
	sub.Cursor.EventID = events[0].ID.String()
	if _, _, err = j.PutSubscription(ctx, sub); err != nil {
		t.Fatal(err)
	}
	path := j.path
	j.Close()
	plan, err := cleanupForPath(path).Plan(ctx, now.Add(-24*time.Hour), CleanupOptions{})
	if err != nil || planIsEligible(plan, e.ID) || len(planReasons(plan, e.ID)) != 1 || planReasons(plan, e.ID)[0] != "active_subscription_cursor" {
		t.Fatalf("paused cursor lost: %+v %v", plan, err)
	}
}

func TestMaintenanceTimeoutResumesCompletedPages(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	e := seedTerminalForRetention(t, j, now.Add(-48*time.Hour), contracts.MutationKey{})
	if _, _, err := j.AcknowledgeExecution(ctx, e.ID, AcknowledgementResult); err != nil {
		t.Fatal(err)
	}
	// Enough unrelated metadata to span multiple pages without event scanning.
	if err := j.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bIdempotency)
		for i := 0; i < cleanupPageSize*3; i++ {
			key := []byte(durableTestID(ids.TypeExecution, uint64(i+10000)))
			raw, _ := json.Marshal(mutationRecord{ObjectType: "other"})
			if err := b.Put(key, raw); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	path := j.path
	j.Close()
	cursor := &MaintenanceCursor{}
	// Expire each pass after a fixed number of opens: deterministic, no sleeps.
	for pass := 0; pass < 30; pass++ {
		passCtx, cancel := context.WithCancel(ctx)
		opens := 0
		cleanup := Cleanup{Open: func(ctx context.Context, ro bool) (*Journal, error) {
			opens++
			if opens == 4 && cursor.bucket < len(cleanupProtectionBuckets) {
				cancel()
				return nil, context.DeadlineExceeded
			}
			return Open(path, Options{ReadOnly: ro})
		}}
		progress, err := cleanup.MaintainNext(passCtx, now.Add(-24*time.Hour), cursor)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			continue
		}
		if err != nil || progress.Executions != 1 {
			t.Fatalf("progress=%+v err=%v", progress, err)
		}
		if pass == 0 {
			t.Fatal("test never interrupted a scan")
		}
		return
	}
	t.Fatal("short resumed passes never converged")
}

func TestMaintenanceResumedProofRejectsInterveningProtection(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	e := seedTerminalForRetention(t, j, now.Add(-48*time.Hour), contracts.MutationKey{})
	if _, _, err := j.AcknowledgeExecution(ctx, e.ID, AcknowledgementResult); err != nil {
		t.Fatal(err)
	}
	path := j.path
	j.Close()
	cursor := &MaintenanceCursor{}
	opens := 0
	cleanup := Cleanup{Open: func(ctx context.Context, ro bool) (*Journal, error) {
		opens++
		if opens == 3 {
			return nil, context.DeadlineExceeded
		}
		return Open(path, Options{ReadOnly: ro})
	}}
	if _, err := cleanup.MaintainNext(ctx, now.Add(-24*time.Hour), cursor); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	j, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err = j.ProtectANXRun(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	j.Close()
	cleanup = cleanupForPath(path)
	if p, err := cleanup.MaintainNext(ctx, now.Add(-24*time.Hour), cursor); !errors.Is(err, ErrConflict) || p.Batches != 0 || cursor.snapshot != nil {
		t.Fatalf("stale proof used: %+v %v", p, err)
	}
	if p, err := cleanup.MaintainNext(ctx, now.Add(-24*time.Hour), cursor); err != nil || p.Batches != 0 {
		t.Fatalf("linked target deleted: %+v %v", p, err)
	}
	plan, err := cleanup.Plan(ctx, now.Add(-24*time.Hour), CleanupOptions{IncludeUnreconciled: true})
	if err != nil || len(planReasons(plan, e.ID)) == 0 {
		t.Fatalf("plan=%+v %v", plan, err)
	}
}

func TestCleanupProtectsLegacyANXSpoolReference(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	e := seedTerminalForRetention(t, j, now.Add(-48*time.Hour), contracts.MutationKey{})
	path := j.path
	j.Close()
	plan, err := cleanupForPath(path).Plan(ctx, now.Add(-24*time.Hour), CleanupOptions{ANXRunLinks: []ids.ExecutionID{e.ID}, IncludeUnreconciled: true})
	if err != nil || len(plan.Eligible) != 0 || len(planReasons(plan, e.ID)) != 1 || planReasons(plan, e.ID)[0] != "anx_run_link" {
		t.Fatalf("legacy link erased: %+v %v", plan, err)
	}
}
