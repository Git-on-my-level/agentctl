package store

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"go.etcd.io/bbolt"
)

func TestCleanupBatchBoundedGenerationCheckedAndReplaySafe(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	for range 35 {
		seedTerminalForRetention(t, j, now.Add(-60*24*time.Hour), contracts.MutationKey{})
	}
	batch, err := j.PlanCleanupBatch(ctx, now.Add(-30*24*time.Hour))
	if err != nil || batch.Executions() != 32 {
		t.Fatalf("batch=%d err=%v", batch.Executions(), err)
	}
	path := j.path
	j.Close()
	// Reopen through the current format; older bbolt opens use the same format
	// and generation. No migration or new bucket is needed by this feature.
	j, err = Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	n, err := j.ApplyCleanupBatch(ctx, batch)
	if err != nil || n != 32 {
		t.Fatalf("applied=%d err=%v", n, err)
	}
	if _, err = j.ApplyCleanupBatch(ctx, batch); !errors.Is(err, ErrConflict) {
		t.Fatalf("replay=%v", err)
	}
	rows, err := j.ListExecutions(ctx, false)
	if err != nil || len(rows) != 3 {
		t.Fatalf("remaining=%d err=%v", len(rows), err)
	}
	if err = j.db.View(func(tx *bbolt.Tx) error {
		for e := range tx.Check() {
			return e
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	next, err := j.PlanCleanupBatch(ctx, now.Add(-30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = j.CreateExecution(ctx, sampleExecution(now), contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = j.ApplyCleanupBatch(ctx, next); !errors.Is(err, ErrConflict) {
		t.Fatalf("concurrent commit not rejected: %v", err)
	}
}

func TestCleanupBatchPreservesUnreadAndWholeLineage(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	old := now.Add(-60 * 24 * time.Hour)
	unread := seedTerminalForRetention(t, j, old, contracts.MutationKey{})
	// Move acknowledgement epoch back so this old terminal is unread evidence.
	if err := j.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bMetadata).Put(keyAckEpoch, []byte(old.Add(-time.Hour).Format(time.RFC3339Nano)))
	}); err != nil {
		t.Fatal(err)
	}
	parent := seedTerminalForRetention(t, j, old, contracts.MutationKey{})
	child := sampleExecution(now)
	child.ParentExecutionID = &parent.ID
	if _, _, err := j.CreateExecution(ctx, child, contracts.MutationKey{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := j.AcknowledgeExecution(ctx, parent.ID, AcknowledgementAwait); err != nil {
		t.Fatal(err)
	}
	b, err := j.PlanCleanupBatch(ctx, now.Add(-30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if b.Executions() != 0 {
		t.Fatalf("protected lineage/unread selected: %+v", b.plan)
	}
	if _, err := j.GetExecution(ctx, unread.ID); err != nil {
		t.Fatal(err)
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := j.PlanCleanupBatch(cancelCtx, now); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
}

func TestCleanupBatchNeverSplitsOversizedComponents(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	old := now.Add(-60 * 24 * time.Hour)
	parent := seedTerminalForRetention(t, j, old, contracts.MutationKey{})
	for range 32 {
		child := seedTerminalForRetention(t, j, old, contracts.MutationKey{})
		child.ParentExecutionID = &parent.ID
		if _, err := j.UpdateExecution(ctx, child, child.Revision); err != nil {
			t.Fatal(err)
		}
	}
	b, err := j.PlanCleanupBatch(ctx, now.Add(-30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if b.Executions() != 0 || len(b.plan.Protected) != 33 {
		t.Fatalf("split oversized component: eligible=%d protected=%d", b.Executions(), len(b.plan.Protected))
	}
}

func TestCleanupBatchEventLimitDefersWholeExecution(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	old := seedTerminalForRetention(t, j, now.Add(-60*24*time.Hour), contracts.MutationKey{})
	// The index limit must reject this target before looking up event bodies.
	// These deliberately missing fixture bodies would be corruption if selected.
	if err := j.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bEventsByExecution).Bucket([]byte(old.ID.String()))
		for i := 0; i < 4097; i++ {
			var key [8]byte
			binary.BigEndian.PutUint64(key[:], uint64(i+100))
			if err := b.Put(key[:], []byte("not-selected")); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	b, err := j.PlanCleanupBatch(ctx, now.Add(-30*24*time.Hour))
	if err != nil || b.Executions() != 0 {
		t.Fatalf("oversized event graph selected: %d %v", b.Executions(), err)
	}
}
