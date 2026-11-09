package store

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/subscription"
	"go.etcd.io/bbolt"
)

func TestManualCleanupCompactsReplayAndRefusesRelaunch(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	key := contracts.MutationKey{Scope: "execution:run", Key: "do-once", InputDigest: hash('b')}
	e := seedTerminalForRetention(t, j, now.Add(-48*time.Hour), key, cleanupEventBatch+1)
	bucketKey := []byte(key.Scope + "\x00" + key.Key)
	if err := j.db.Update(func(tx *bbolt.Tx) error {
		var value map[string]any
		if err := json.Unmarshal(tx.Bucket(bIdempotency).Get(bucketKey), &value); err != nil {
			return err
		}
		value["legacy_extra_metadata"] = strings.Repeat("x", 64<<10)
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		return tx.Bucket(bIdempotency).Put(bucketKey, raw)
	}); err != nil {
		t.Fatal(err)
	}
	path := j.path
	j.Close()
	cleanup := cleanupForPath(path)
	cutoff := now.Add(-24 * time.Hour)
	for batch := 0; batch < 3; batch++ {
		plan, err := cleanup.Plan(ctx, cutoff, CleanupOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Eligible) == 0 {
			break
		}
		if plan.Eligible[0].Partial && plan.Records.ReplayTombstones != 0 {
			t.Fatal("partial claims a tombstone")
		}
		if _, err = cleanup.Apply(ctx, cutoff, plan.PlanDigest, CleanupOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	j, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if _, err = j.GetExecution(ctx, e.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("execution not reclaimed: %v", err)
	}
	if _, err = j.GetOutcome(ctx, e.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("answer not reclaimed: %v", err)
	}
	if err = j.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bIdempotency).Get(bucketKey)
		if len(raw) == 0 || len(raw) > 300 || strings.Contains(string(raw), "legacy_extra") {
			t.Fatalf("tombstone is not compact: %d bytes", len(raw))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, create := range []bool{false, true} {
		var err error
		if create {
			_, _, err = j.CreateExecution(ctx, sampleExecution(now), key)
		} else {
			_, _, err = j.GetExecutionByMutation(ctx, key)
		}
		var expired *ReplayExpired
		if !errors.As(err, &expired) || expired.ExecutionID != e.ID.String() {
			t.Fatalf("create=%v retry not refused: %v", create, err)
		}
	}
	changed := key
	changed.InputDigest = hash('c')
	if _, _, err = j.CreateExecution(ctx, sampleExecution(now), changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed digest lost conflict: %v", err)
	}
	if err = j.ClearMutation(ctx, key); !errors.Is(err, ErrReplayExpired) {
		t.Fatalf("clear removed durable tombstone: %v", err)
	}
	changed.Key = "new-intent"
	if _, reused, err := j.CreateExecution(ctx, sampleExecution(now), changed); err != nil || reused {
		t.Fatalf("distinct request refused: %v %v", reused, err)
	}
}

func TestCleanupHasMoreRequiresSelectableTail(t *testing.T) {
	for _, selectable := range []bool{false, true} {
		t.Run(map[bool]string{false: "deferred-only", true: "removable-tail"}[selectable], func(t *testing.T) {
			ctx := context.Background()
			j, _, now := openTestJournal(t)
			values := []model.Execution{}
			for i := 0; i < 10; i++ {
				values = append(values, seedTerminalForRetention(t, j, now.Add(-48*time.Hour), contracts.MutationKey{}))
			}
			sort.Slice(values, func(a, b int) bool { return values[a].ID.String() < values[b].ID.String() })
			addRetentionArtifacts(t, j, values[8], cleanupEventBatch+1)
			if !selectable {
				addRetentionArtifacts(t, j, values[9], cleanupEventBatch+1)
			}
			plan, err := j.PlanCleanup(ctx, now.Add(-24*time.Hour), CleanupOptions{})
			if err != nil || len(plan.Eligible) != 8 || plan.HasMore != selectable || !containsReason(planReasons(plan, values[8].ID), "cleanup_final_batch_too_large") {
				t.Fatalf("plan=%+v err=%v", plan, err)
			}
			if _, err = j.ApplyCleanup(ctx, plan.Before, plan.PlanDigest, CleanupOptions{}); err != nil {
				t.Fatal(err)
			}
			next, err := j.PlanCleanup(ctx, plan.Before, CleanupOptions{})
			if err != nil || (len(next.Eligible) > 0) != selectable {
				t.Fatalf("next=%+v err=%v", next, err)
			}
		})
	}
}

func TestLabelPrefixPagesReleaseLockAndUseBoundedAcknowledgements(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	for i := 0; i < cleanupPageSize+3; i++ {
		e := sampleExecution(now)
		e.Labels = []string{"watch.lane"}
		if _, _, err := j.CreateExecution(ctx, e, contracts.MutationKey{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := j.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bAcknowledgements).Put([]byte(durableTestID(ids.TypeExecution, 999999)), []byte("unrelated-invalid-json"))
	}); err != nil {
		t.Fatal(err)
	}
	path := j.path
	j.Close()
	opens, seen := 0, 0
	reader := Reader{Open: func(ctx context.Context, ro bool) (*Journal, error) {
		contender, err := Open(path, Options{LockTimeout: time.Millisecond})
		if err != nil {
			t.Fatal("label scan retained flock", err)
		}
		contender.Close()
		opens++
		return Open(path, Options{ReadOnly: ro})
	}}
	err := reader.WalkExecutionsByLabelPrefix(ctx, "watch.", func(j *Journal, page []model.Execution) error {
		if len(page) > cleanupPageSize {
			t.Fatal("unbounded page")
		}
		seen += len(page)
		index, err := j.AcknowledgementIndexFor(ctx, page)
		if err != nil || len(index.ByID) != 0 {
			t.Fatalf("index=%+v %v", index, err)
		}
		return nil
	})
	if err != nil || seen != cleanupPageSize+3 || opens < 3 {
		t.Fatalf("seen=%d opens=%d err=%v", seen, opens, err)
	}
}

func TestMaintenanceUnchangedGenerationFastPathAndCutoff(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	e := seedTerminalForRetention(t, j, now.Add(-10*24*time.Hour), contracts.MutationKey{})
	if _, _, err := j.AcknowledgeExecution(ctx, e.ID, AcknowledgementResult); err != nil {
		t.Fatal(err)
	}
	path := j.path
	j.Close()
	opens := 0
	cleanup := Cleanup{Open: func(ctx context.Context, ro bool) (*Journal, error) {
		opens++
		return Open(path, Options{ReadOnly: ro})
	}}
	cursor := &MaintenanceCursor{}
	before := now.Add(-14 * 24 * time.Hour)
	if p, err := cleanup.MaintainNext(ctx, before, cursor); err != nil || p.Batches != 0 {
		t.Fatalf("first=%+v %v", p, err)
	}
	opens = 0
	if p, err := cleanup.MaintainNext(ctx, before.Add(time.Hour), cursor); err != nil || p.Batches != 0 || opens != 1 {
		t.Fatalf("quiet pass scanned: %+v %v opens=%d", p, err, opens)
	}
	opens = 0
	// A shorter cutoff must not skip a newly aged-in terminal despite no writes.
	if p, err := cleanup.MaintainNext(ctx, now.Add(-7*24*time.Hour), cursor); err != nil || p.Executions != 1 || opens <= 1 {
		t.Fatalf("aged work skipped: %+v %v opens=%d", p, err, opens)
	}
	opens = 0
	if _, err := cleanup.MaintainNext(ctx, now, cursor); err != nil || opens <= 1 {
		t.Fatalf("own deletion generation was incorrectly cached: %v %d", err, opens)
	}
}

func TestMaintenanceChangedExternalLinksInvalidateFastPath(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	e := seedTerminalForRetention(t, j, now.Add(-48*time.Hour), contracts.MutationKey{})
	if _, _, err := j.AcknowledgeExecution(ctx, e.ID, AcknowledgementResult); err != nil {
		t.Fatal(err)
	}
	path := j.path
	j.Close()
	cleanup := cleanupForPath(path)
	cursor := &MaintenanceCursor{}
	if p, err := cleanup.MaintainNext(ctx, now.Add(-24*time.Hour), cursor, []ids.ExecutionID{e.ID}...); err != nil || p.Batches != 0 {
		t.Fatalf("protected=%+v %v", p, err)
	}
	if p, err := cleanup.MaintainNext(ctx, now.Add(-24*time.Hour), cursor); err != nil || p.Executions != 1 {
		t.Fatalf("removed legacy link still protected: %+v %v", p, err)
	}
}

func TestMaintenanceFastPathExpiresWithSubscriptionProtection(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	e := seedTerminalForRetention(t, j, now.Add(-48*time.Hour), contracts.MutationKey{})
	if _, _, err := j.AcknowledgeExecution(ctx, e.ID, AcknowledgementResult); err != nil {
		t.Fatal(err)
	}
	sub := durableTestSubscription(durableTestID(ids.TypeSubscription, 999123))
	sub.State = subscription.StatePaused
	sub.Filter.ExecutionIDs = []string{e.ID.String()}
	sub.Filter.Kinds = []string{string(model.EventAttention)}
	sub.ExpiresAt = now.Add(time.Hour)
	if _, _, err := j.PutSubscription(ctx, sub); err != nil {
		t.Fatal(err)
	}
	snapshot, err := j.cleanupReader(ctx).snapshot(true)
	if err != nil {
		t.Fatal(err)
	}
	before := now.Add(-24 * time.Hour)
	_, eligible, err := snapshot.selectCleanup(before, now, CleanupOptions{automatic: true})
	if err != nil || len(eligible) != 0 || !snapshot.nextCheckAt.Equal(sub.ExpiresAt) {
		t.Fatalf("expiry not tracked: eligible=%v next=%v err=%v", eligible, snapshot.nextCheckAt, err)
	}
	done := maintenanceCompletion{version: snapshot.version, path: j.path, nextBefore: snapshot.nextBefore, nextCheckAt: snapshot.nextCheckAt}
	path := j.path
	j.Close()
	r := cleanupForPath(path).reader(ctx)
	r.now = now.Add(30 * time.Minute)
	if skip, err := r.maintenanceUnchanged(done, before, nil); err != nil || !skip {
		t.Fatalf("unchanged unexpired scan not skipped: %v %v", skip, err)
	}
	r.now = sub.ExpiresAt
	if skip, err := r.maintenanceUnchanged(done, before, nil); err != nil || skip {
		t.Fatalf("subscription expiry failed to wake scan: %v %v", skip, err)
	}
	if _, err := cleanupForPath(path).MaintainNext(ctx, time.Time{}, &MaintenanceCursor{completed: done}); err == nil {
		t.Fatal("fast path bypassed cutoff validation")
	}
}
