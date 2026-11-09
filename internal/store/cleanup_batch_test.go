package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Git-on-my-level/agentctl/internal/diagnostic"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"go.etcd.io/bbolt"
)

func cleanupForPath(path string) Cleanup {
	return Cleanup{Open: func(ctx context.Context, readOnly bool) (*Journal, error) {
		return Open(path, Options{ReadOnly: readOnly, LockTimeout: 50 * time.Millisecond})
	}}
}

func addRetentionArtifacts(t *testing.T, j *Journal, e model.Execution, count int) {
	t.Helper()
	// Historical artifact metadata is legal after a terminal and exercises the
	// same event/dedupe/index deletion path as streamed progress.
	for i := 0; i < count; i++ {
		projection := map[string]any{"retention_progress": fmt.Sprint(i), "execution_id": e.ID.String()}
		key, canonical := semanticEvent(t, e.Adapter, projection)
		event := model.Event{ExecutionID: e.ID, Authority: e.Authority, Adapter: e.Adapter, Ordering: model.OrderingObservation, Kind: model.EventArtifact, ObservedAt: *e.TerminalAt, DedupeKey: key, DedupeVersion: 1, Payload: projection}
		if _, _, err := j.AppendEvent(context.Background(), event, canonical); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCleanupPagesReleaseFileLockAndBoundBothModes(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	for i := 0; i < cleanupPageSize+4; i++ {
		seedTerminalForRetention(t, j, now.Add(-72*time.Hour), contracts.MutationKey{})
	}
	path := j.path
	j.Close()
	opens, writes := 0, 0
	cleanup := Cleanup{Open: func(ctx context.Context, ro bool) (*Journal, error) {
		// A fresh writer can acquire between every page, including planning.
		contender, err := Open(path, Options{LockTimeout: time.Millisecond})
		if err != nil {
			t.Fatal("cleanup retained flock between pages", err)
		}
		contender.Close()
		opens++
		if !ro {
			writes++
		}
		return Open(path, Options{ReadOnly: ro})
	}}
	before, _ := InventoryFileState(path)
	plan, err := cleanup.Plan(ctx, now.Add(-24*time.Hour), CleanupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := InventoryFileState(path)
	if before != after {
		t.Fatal("planning wrote journal")
	}
	if !plan.HasMore || len(plan.Eligible) != 8 || writes != 0 || opens < 2 {
		t.Fatalf("plan=%+v opens=%d writes=%d", plan, opens, writes)
	}
	applied, err := cleanup.Apply(ctx, plan.Before, plan.PlanDigest, CleanupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !applied.Applied || writes != 8 {
		t.Fatalf("applied=%+v writes=%d", applied, writes)
	}
	next, err := cleanup.Plan(ctx, plan.Before, CleanupOptions{})
	if err != nil || len(next.Eligible) != 8 || !next.HasMore {
		t.Fatalf("next=%+v err=%v", next, err)
	}
}

func TestCleanupLargeExecutionDrainsInBoundedBatches(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	old := seedTerminalForRetention(t, j, now.Add(-72*time.Hour), contracts.MutationKey{}, cleanupEventBatch*2+1)
	path := j.path
	j.Close()
	cleanup := cleanupForPath(path)
	for batch := 0; batch < 3; batch++ {
		plan, err := cleanup.Plan(ctx, now.Add(-24*time.Hour), CleanupOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Eligible) != 1 || plan.Records.Events > cleanupEventBatch {
			t.Fatalf("unbounded plan: %+v", plan)
		}
		if batch < 2 && (!plan.HasMore || !plan.Eligible[0].Partial || plan.Records.Executions != 0 || plan.Records.Outcomes != 0) {
			t.Fatalf("partial plan claims identity deletion: %+v", plan)
		}
		if _, err := cleanup.Apply(ctx, plan.Before, plan.PlanDigest, CleanupOptions{}); err != nil {
			t.Fatal(err)
		}
		read, err := Open(path, Options{ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		if batch < 2 {
			if _, err := read.GetOutcome(ctx, old.ID); err != nil {
				t.Fatal("partial cleanup lost outcome", err)
			}
			if err := read.db.View(func(tx *bbolt.Tx) error {
				terminal := tx.Bucket(bTerminal).Get([]byte(old.ID.String()))
				if terminal == nil || tx.Bucket(bEvents).Get(terminal) == nil {
					return ErrCorrupt
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		} else if _, err := read.GetExecution(ctx, old.ID); !errors.Is(err, ErrNotFound) {
			t.Fatal("final cleanup retained execution", err)
		}
		// bbolt structure remains sound across partial commits.
		if err := read.db.View(func(tx *bbolt.Tx) error {
			for err := range tx.Check() {
				return err
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		read.Close()
	}
}

func TestCleanupConcurrentProtectionBetweenScanAndCommitDeletesNothing(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	old := seedTerminalForRetention(t, j, now.Add(-72*time.Hour), contracts.MutationKey{})
	path := j.path
	j.Close()
	cleanup := cleanupForPath(path)
	plan, err := cleanup.Plan(ctx, now.Add(-24*time.Hour), CleanupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	injected := false
	cleanup.Open = func(ctx context.Context, ro bool) (*Journal, error) {
		if !ro && !injected {
			injected = true
			writer, err := Open(path, Options{})
			if err != nil {
				t.Fatal(err)
			}
			sub := durableTestSubscription(durableTestID(ids.TypeSubscription, 990))
			sub.Filter.ExecutionIDs = []string{old.ID.String()}
			sub.Filter.Kinds = []string{string(model.EventAttention)}
			if _, _, err := writer.PutSubscription(ctx, sub); err != nil {
				t.Fatal(err)
			}
			writer.Close()
		}
		return Open(path, Options{ReadOnly: ro})
	}
	_, err = cleanup.Apply(ctx, plan.Before, plan.PlanDigest, CleanupOptions{})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("concurrent protection accepted: %v", err)
	}
	read, err := Open(path, Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	if _, err := read.GetOutcome(ctx, old.ID); err != nil {
		t.Fatal("concurrent protected outcome deleted", err)
	}
}

func TestCleanupCancellationReportsCommittedPrefix(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	j, _, now := openTestJournal(t)
	for i := 0; i < 3; i++ {
		seedTerminalForRetention(t, j, now.Add(-72*time.Hour), contracts.MutationKey{})
	}
	path := j.path
	j.Close()
	cleanup := cleanupForPath(path)
	plan, err := cleanup.Plan(ctx, now.Add(-24*time.Hour), CleanupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	writes := 0
	cleanup.Open = func(ctx context.Context, ro bool) (*Journal, error) {
		if writes == 1 {
			cancel()
			return nil, ctx.Err()
		}
		if !ro {
			writes++
		}
		return Open(path, Options{ReadOnly: ro})
	}
	_, err = cleanup.Apply(ctx, plan.Before, plan.PlanDigest, CleanupOptions{})
	var stopped *CleanupInterrupted
	if !errors.As(err, &stopped) || !errors.Is(err, context.Canceled) || stopped.Progress.Executions != 1 || stopped.Progress.Batches != 1 {
		t.Fatalf("lost committed prefix: %v", err)
	}
	read, err := Open(path, Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	rows, err := read.ListExecutions(context.Background(), false)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
}

func TestAutomaticRetentionRequiresAcknowledgementAndPreservesLineage(t *testing.T) {
	ctx := context.Background()
	epoch := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	j := openRetentionJournalWithEpoch(t, epoch)
	// Unacknowledged records before tracking are manually cleanable, but never
	// automatically removable, for both native and Multica authority.
	preEpoch := seedTerminalForRetention(t, j, epoch.Add(-48*time.Hour), contracts.MutationKey{})
	unread := seedTerminalForRetention(t, j, epoch.Add(time.Hour), contracts.MutationKey{})
	old := seedTerminalForRetention(t, j, epoch.Add(2*time.Hour), contracts.MutationKey{}, cleanupEventBatch+1)
	linked := seedTerminalForRetention(t, j, epoch.Add(3*time.Hour), contracts.MutationKey{})
	for _, e := range []model.Execution{old, linked} {
		if _, _, err := j.AcknowledgeExecution(ctx, e.ID, AcknowledgementResult); err != nil {
			t.Fatal(err)
		}
	}
	child := sampleExecution(epoch.Add(4 * time.Hour))
	child.ParentExecutionID = &linked.ID
	if _, _, err := j.CreateExecution(ctx, child, contracts.MutationKey{}); err != nil {
		t.Fatal(err)
	}
	path := j.path
	j.Close()
	cleanup := cleanupForPath(path)
	cutoff := epoch.Add(14 * 24 * time.Hour)
	progress, err := cleanup.Maintain(ctx, cutoff)
	if err != nil || progress.Events != cleanupEventBatch || progress.Executions != 0 || progress.Batches != 1 {
		t.Fatalf("progress=%+v err=%v", progress, err)
	}
	progress, err = cleanup.Maintain(ctx, cutoff)
	if err != nil || progress.Executions != 1 {
		t.Fatalf("progress=%+v err=%v", progress, err)
	}
	read, err := Open(path, Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	for _, e := range []model.Execution{preEpoch, unread, linked} {
		if _, err := read.GetExecution(ctx, e.ID); err != nil {
			t.Fatalf("protected %s removed: %v", e.ID, err)
		}
	}
	if _, err := read.GetExecution(ctx, old.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("eligible execution retained", err)
	}
}

func TestCleanupSnapshotCancellationIsBounded(t *testing.T) {
	j, _, now := openTestJournal(t)
	path := j.path
	j.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := cleanupForPath(filepath.Clean(path)).Plan(ctx, now, CleanupOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestCleanupFinalCommitRejectsAnEventAddedAfterPlanning(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	old := seedTerminalForRetention(t, j, now.Add(-72*time.Hour), contracts.MutationKey{})
	reader := j.cleanupReader(ctx)
	_, targets, err := reader.plan(now.Add(-24*time.Hour), CleanupOptions{}, cleanupEventBatch)
	if err != nil {
		t.Fatal(err)
	}
	addRetentionArtifacts(t, j, old, 1)
	if _, err := reader.remove(now.Add(-24*time.Hour), CleanupOptions{}, targets, 0); !errors.Is(err, ErrConflict) {
		t.Fatalf("late event silently orphaned: %v", err)
	}
	if _, err := j.GetOutcome(ctx, old.ID); err != nil {
		t.Fatal("late event check deleted outcome", err)
	}
}

func TestCleanupFinalCommitRejectsNewDiagnosticCacheSibling(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	old := seedTerminalForRetention(t, j, now.Add(-72*time.Hour), contracts.MutationKey{})
	reader := j.cleanupReader(ctx)
	_, targets, err := reader.plan(now.Add(-24*time.Hour), CleanupOptions{}, cleanupEventBatch)
	if err != nil {
		t.Fatal(err)
	}
	outputs, err := diagnostic.OpenOutputs(filepath.Dir(j.path), true)
	if err != nil {
		t.Fatal(err)
	}
	path, err := outputs.Write(old.ID.String(), []byte("late diagnostic fixture"))
	outputs.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.remove(now.Add(-24*time.Hour), CleanupOptions{}, targets, 0); !errors.Is(err, ErrConflict) {
		t.Fatalf("late cache file orphaned: %v", err)
	}
	if _, err := j.GetOutcome(ctx, old.ID); err != nil {
		t.Fatal("late cache file check deleted outcome", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("late cache file removed without review", err)
	}
}

func TestCleanupPartialDigestCannotAuthorizeTheNextPrefix(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	seedTerminalForRetention(t, j, now.Add(-72*time.Hour), contracts.MutationKey{}, cleanupEventBatch*2+1)
	cutoff := now.Add(-24 * time.Hour)
	plan, err := j.PlanCleanup(ctx, cutoff, CleanupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.ApplyCleanup(ctx, cutoff, plan.PlanDigest, CleanupOptions{}); err != nil {
		t.Fatal(err)
	}
	next, err := j.PlanCleanup(ctx, cutoff, CleanupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.PlanDigest == next.PlanDigest {
		t.Fatal("partial digest did not bind the selected event prefix")
	}
	if _, err := j.ApplyCleanup(ctx, cutoff, plan.PlanDigest, CleanupOptions{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale prefix digest accepted: %v", err)
	}
}

func TestCleanupPartialPreservesSequenceHighWatermark(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	old := seedTerminalForRetention(t, j, now.Add(-72*time.Hour), contracts.MutationKey{}, cleanupEventBatch-1)
	addRetentionArtifacts(t, j, old, 1)
	high, err := j.LastEventSequence(ctx, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	cutoff := now.Add(-24 * time.Hour)
	plan, err := j.PlanCleanup(ctx, cutoff, CleanupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.HasMore {
		t.Fatal("expected partial cleanup")
	}
	if _, err := j.ApplyCleanup(ctx, cutoff, plan.PlanDigest, CleanupOptions{}); err != nil {
		t.Fatal(err)
	}
	after, err := j.LastEventSequence(ctx, old.ID)
	if err != nil || after != high {
		t.Fatalf("sequence regressed %d -> %d: %v", high, after, err)
	}
	projection := map[string]any{"late_after_partial": "artifact"}
	key, canonical := semanticEvent(t, old.Adapter, projection)
	event, _, err := j.AppendEvent(ctx, model.Event{ExecutionID: old.ID, Authority: old.Authority, Adapter: old.Adapter, Ordering: model.OrderingObservation, Kind: model.EventArtifact, ObservedAt: *old.TerminalAt, DedupeKey: key, DedupeVersion: 1, Payload: projection}, canonical)
	if err != nil || event.Sequence <= high {
		t.Fatalf("append reused a deleted sequence: %d <= %d err=%v", event.Sequence, high, err)
	}
}

func TestCleanupPartialKeepsReplayableEventsAndDedupe(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	old := seedTerminalForRetention(t, j, now.Add(-72*time.Hour), contracts.MutationKey{}, cleanupEventBatch+1)
	projection := map[string]any{"retention_artifact": "stable replay"}
	key, canonical := semanticEvent(t, old.Adapter, projection)
	input := model.Event{ExecutionID: old.ID, Authority: old.Authority, Adapter: old.Adapter, Ordering: model.OrderingObservation, Kind: model.EventArtifact, ObservedAt: *old.TerminalAt, DedupeKey: key, DedupeVersion: 1, Payload: projection}
	original, _, err := j.AppendEvent(ctx, input, canonical)
	if err != nil {
		t.Fatal(err)
	}
	cutoff := now.Add(-24 * time.Hour)
	plan, err := j.PlanCleanup(ctx, cutoff, CleanupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.ApplyCleanup(ctx, cutoff, plan.PlanDigest, CleanupOptions{}); err != nil {
		t.Fatal(err)
	}
	sub := durableTestSubscription(durableTestID(ids.TypeSubscription, 120))
	sub.Filter.ExecutionIDs = []string{old.ID.String()}
	sub.Filter.Kinds = []string{string(model.EventArtifact)}
	if _, _, err := j.PutSubscription(ctx, sub); err != nil {
		t.Fatal(err)
	}
	replay, reused, err := j.AppendEvent(ctx, input, canonical)
	if err != nil || !reused || replay.ID != original.ID || replay.Sequence != original.Sequence {
		t.Fatalf("partial cleanup lost replay memory: original=%+v replay=%+v reused=%v err=%v", original, replay, reused, err)
	}
	deliveries, err := j.ListPendingDeliveries(ctx)
	if err != nil || len(deliveries) != 0 {
		t.Fatalf("replay fanned out another delivery: %+v %v", deliveries, err)
	}
}

func TestCleanupDefersOversizedMetadataAndContinuesLaterCandidates(t *testing.T) {
	ctx := context.Background()
	epoch := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	j := openRetentionJournalWithEpoch(t, epoch)
	one := seedTerminalForRetention(t, j, epoch.Add(time.Hour), contracts.MutationKey{})
	two := seedTerminalForRetention(t, j, epoch.Add(2*time.Hour), contracts.MutationKey{})
	oversized, ordinary := one, two
	if oversized.ID.String() > ordinary.ID.String() {
		oversized, ordinary = ordinary, oversized
	}
	for _, e := range []model.Execution{one, two} {
		if _, _, err := j.AcknowledgeExecution(ctx, e.ID, AcknowledgementResult); err != nil {
			t.Fatal(err)
		}
	}
	if err := j.db.Update(func(tx *bbolt.Tx) error {
		for i := 0; i < cleanupPageSize; i++ {
			key := fmt.Sprintf("imported-%d", i)
			raw, err := json.Marshal(mutationRecord{Scope: "fixture", Key: key, InputDigest: hash('a'), ObjectType: "execution", ObjectID: oversized.ID.String()})
			if err != nil {
				return err
			}
			if err := tx.Bucket(bIdempotency).Put([]byte(key), raw); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cutoff := epoch.Add(48 * time.Hour)
	plan, err := j.PlanCleanup(ctx, cutoff, CleanupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Eligible) != 1 || plan.Eligible[0].ExecutionID != ordinary.ID.String() || !containsReason(planReasons(plan, oversized.ID), "cleanup_final_batch_too_large") {
		t.Fatalf("oversized candidate blocked later IDs: %+v", plan)
	}
	path := j.path
	j.Close()
	progress, err := cleanupForPath(path).Maintain(ctx, cutoff)
	if err != nil || progress.Executions != 1 {
		t.Fatalf("automatic retention starved: %+v err=%v", progress, err)
	}
	read, err := Open(path, Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	if _, err := read.GetExecution(ctx, oversized.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := read.GetExecution(ctx, ordinary.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("ordinary candidate remained", err)
	}
}

func TestCleanupDefersReplayableHistoryBeyondOneFinalBatch(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	one := seedTerminalForRetention(t, j, now.Add(-72*time.Hour), contracts.MutationKey{})
	two := seedTerminalForRetention(t, j, now.Add(-71*time.Hour), contracts.MutationKey{})
	oversized, ordinary := one, two
	if oversized.ID.String() > ordinary.ID.String() {
		oversized, ordinary = ordinary, oversized
	}
	addRetentionArtifacts(t, j, oversized, cleanupEventBatch+1)
	plan, err := j.PlanCleanup(ctx, now.Add(-24*time.Hour), CleanupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Eligible) != 1 || plan.Eligible[0].ExecutionID != ordinary.ID.String() || !containsReason(planReasons(plan, oversized.ID), "cleanup_final_batch_too_large") {
		t.Fatalf("replayable history was unsafely selected or starved later IDs: %+v", plan)
	}
}

func TestCleanupPreservesOriginalLegacyBytesForComparison(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	old := seedTerminalForRetention(t, j, now.Add(-72*time.Hour), contracts.MutationKey{})
	if err := j.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bExecutions)
		var document map[string]json.RawMessage
		if err := json.Unmarshal(bucket.Get([]byte(old.ID.String())), &document); err != nil {
			return err
		}
		document["legacy_extension"] = json.RawMessage(`{"unknown":true}`)
		document["task_contract"] = json.RawMessage(`{"objective_summary":"legacy objective","side_effect_boundary":"human reviewed write boundary","expected_artifact_kinds":[],"future_extension":{"schema_version":2}}`)
		document["labels"] = json.RawMessage(`[]`)
		raw, err := json.MarshalIndent(document, "", "  ")
		if err != nil {
			return err
		}
		return bucket.Put([]byte(old.ID.String()), raw)
	}); err != nil {
		t.Fatal(err)
	}
	cutoff := now.Add(-24 * time.Hour)
	plan, err := j.PlanCleanup(ctx, cutoff, CleanupOptions{})
	if err != nil {
		t.Fatalf("unchanged legacy bytes conflicted: %v", err)
	}
	if len(plan.Eligible) != 1 {
		t.Fatalf("legacy row was not selected: %+v", plan)
	}
	if _, err := j.ApplyCleanup(ctx, cutoff, plan.PlanDigest, CleanupOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := j.GetExecution(ctx, old.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestCleanupDefersReplayableHistoryBeyondFinalByteBudget(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	old := seedTerminalForRetention(t, j, now.Add(-72*time.Hour), contracts.MutationKey{})
	for i := 0; i < 3; i++ {
		projection := map[string]any{"artifact_ref": strings.Repeat("a", 50<<10), "index": i}
		key, canonical := semanticEvent(t, old.Adapter, projection)
		if _, _, err := j.AppendEvent(ctx, model.Event{ExecutionID: old.ID, Authority: old.Authority, Adapter: old.Adapter, Ordering: model.OrderingObservation, Kind: model.EventArtifact, ObservedAt: *old.TerminalAt, DedupeKey: key, DedupeVersion: 1, Payload: projection}, canonical); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := j.PlanCleanup(ctx, now.Add(-24*time.Hour), CleanupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Eligible) != 0 || !containsReason(planReasons(plan, old.ID), "cleanup_final_batch_too_large") {
		t.Fatalf("oversized final event values were selected: %+v", plan)
	}
}
